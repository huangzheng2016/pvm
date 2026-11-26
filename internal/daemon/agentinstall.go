package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

const agentUnit = `[Unit]
Description=pvm guest agent
After=multi-user.target

[Service]
ExecStart=/usr/local/bin/pvm agent
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
`

type agentInstallTask struct {
	ctx    context.Context
	cancel context.CancelFunc
	seq    uint64
}

var (
	agentInstallMaxWait   = 60 * time.Minute
	cloudInitIdleTimeout  = 5 * time.Minute
	cloudInitSSHTimeout   = 2 * time.Minute
	cloudInitPollInterval = 3 * time.Second
)

// cloudInitError marks a provisioning failure that persists across guest
// reboots: cloud-init does not rerun a failed runcmd on `stop && start`.
type cloudInitError struct {
	Status string
	Detail string
}

func (e *cloudInitError) Error() string {
	return fmt.Sprintf("cloud-init status %q:\n%s", e.Status, e.Detail)
}

func (s *Server) beginAgentInstall(name string) *agentInstallTask {
	s.progMu.Lock()
	defer s.progMu.Unlock()
	if old := s.installTasks[name]; old != nil {
		old.cancel()
	}
	if s.installTasks == nil {
		s.installTasks = make(map[string]*agentInstallTask)
	}
	if s.progSeq == nil {
		s.progSeq = make(map[string]uint64)
	}
	s.progSeq[name]++
	ctx, cancel := context.WithTimeout(context.Background(), agentInstallMaxWait)
	task := &agentInstallTask{ctx: ctx, cancel: cancel, seq: s.progSeq[name]}
	s.installTasks[name] = task
	return task
}

func (s *Server) finishAgentInstall(name string, task *agentInstallTask) {
	task.cancel()
	s.progMu.Lock()
	if s.installTasks[name] == task {
		delete(s.installTasks, name)
	}
	s.progMu.Unlock()
}

// Called before waiting for the VM lock: an installation holding that
// lock must be interruptible by stop/delete/restart.
func (s *Server) cancelAgentInstall(name string) {
	s.progMu.Lock()
	if task := s.installTasks[name]; task != nil {
		task.cancel()
	}
	s.progMu.Unlock()
}

// A route transaction can fail before restarting a still-running VM.
// Resume its cancelled readiness work after the transaction releases its locks.
func (s *Server) resumeAgentInstallIfRunning(name string) {
	lock := qemu.LockVM(name)
	lock.Lock()
	defer lock.Unlock()
	spec, err := vm.Load(s.P, name)
	if err != nil {
		return
	}
	if st, err := s.instance(spec).Running(); err != nil || st == nil {
		return
	}
	if s.vmReady(name) {
		return
	}
	s.progMu.Lock()
	task := s.installTasks[name]
	active := task != nil && task.ctx.Err() == nil
	s.progMu.Unlock()
	if active {
		return
	}
	s.resetStages(name)
	if spec.IdentityReset {
		s.maybeIdentityReset(spec)
	} else {
		s.ensureAgentAsync(spec)
	}
}

// QEMU startup and guest readiness are separate: installation errors remain
// visible to callers waiting for readiness.
func (s *Server) ensureAgentAsync(spec *vm.Spec) {
	spec = copySpec(spec)
	task := s.beginAgentInstall(spec.Name)
	bin := s.P.AgentBin()
	if _, err := os.Stat(bin); err != nil {
		s.stageIfCurrent(spec.Name, task.seq, "failed", fmt.Sprintf("guest agent asset unavailable; rebuild with script/build.sh, then run `pvm stop %s && pvm start %s`", spec.Name, spec.Name))
		s.finishAgentInstall(spec.Name, task)
		log.Printf("agent: asset unavailable for %s: %v", spec.Name, err)
		return
	}
	go func() {
		defer s.finishAgentInstall(spec.Name, task)
		if err := s.installAgentGeneration(task.ctx, spec, bin, task.seq); err != nil {
			if errors.Is(task.ctx.Err(), context.Canceled) {
				return
			}
			var ciErr *cloudInitError
			if errors.As(err, &ciErr) {
				s.stageIfCurrent(spec.Name, task.seq, "failed", fmt.Sprintf("agent installation failed: %v\nrebooting will not rerun failed cloud-init steps; inspect with `pvm ssh %s` (see /var/log/cloud-init-output.log) or recreate the VM", err, spec.Name))
			} else {
				s.stageIfCurrent(spec.Name, task.seq, "failed", fmt.Sprintf("agent installation failed: %v; run `pvm stop %s && pvm start %s`", err, spec.Name, spec.Name))
			}
			log.Printf("agent: install into %s failed: %v", spec.Name, err)
			return
		}
		log.Printf("agent: installed and reachable in %s", spec.Name)
	}()
}

func (s *Server) installAgentGeneration(ctx context.Context, spec *vm.Spec, bin string, seq uint64) error {
	s.stageIfCurrent(spec.Name, seq, "provision", "waiting for ssh (cloud-init running)")
	if err := s.waitSSHCtx(ctx, spec, 10*time.Minute); err != nil {
		return err
	}
	if err := s.waitGuestProvisionedCtx(ctx, spec, seq); err != nil {
		return err
	}
	if spec.KernelArgsPending {
		if err := s.applyPendingKernelArgs(ctx, spec, seq); err != nil {
			return err
		}
	}
	s.stageIfCurrent(spec.Name, seq, "ssh", "SSH and cloud-init ready")
	phaseCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err := s.withInstallVM(phaseCtx, spec, seq, func(fresh *vm.Spec) error {
		if err := retryTransientSSH(phaseCtx, func() error { return s.configureGuestMountsCtx(phaseCtx, fresh) }); err != nil {
			return err
		}
		if err := retryTransientSSH(phaseCtx, func() error { return s.scpToCtx(phaseCtx, fresh, bin, "/tmp/pvm-agent.new") }); err != nil {
			return fmt.Errorf("scp: %w", err)
		}
		installCmd := fmt.Sprintf(
			"sudo install -m 0755 /tmp/pvm-agent.new /usr/local/bin/pvm && rm -f /tmp/pvm-agent.new && "+
				"echo %s | sudo tee /etc/systemd/system/pvm-agent.service >/dev/null && "+
				"sudo systemctl daemon-reload && sudo systemctl enable pvm-agent && sudo systemctl restart pvm-agent",
			shellQuote(agentUnit))
		var out string
		if err := retryTransientSSH(phaseCtx, func() error {
			var err error
			out, err = s.sshRunCtx(phaseCtx, fresh, installCmd)
			return err
		}); err != nil {
			return fmt.Errorf("install unit: %v\n%s", err, out)
		}
		if err := phaseCtx.Err(); err != nil {
			return err
		}
		s.invalidateAgent(spec.Name)
		return nil
	})
	cancel()
	if err != nil {
		return err
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readyCancel()
	for readyCtx.Err() == nil {
		if _, err := s.agentClientContext(readyCtx, spec.Name); err == nil {
			return s.withInstallVM(readyCtx, spec, seq, func(fresh *vm.Spec) error {
				s.stageIfCurrent(spec.Name, seq, "agent", "guest agent installed")
				s.stageIfCurrent(spec.Name, seq, "ready", "SSH, provisioning and guest agent ready")
				s.ensureControlWatcher(fresh)
				return nil
			})
		}
		if err := waitInstallRetry(readyCtx, time.Second); err != nil {
			return fmt.Errorf("agent not reachable after install: %w", err)
		}
	}
	return fmt.Errorf("agent not reachable after install: %w", readyCtx.Err())
}

// applyPendingKernelArgs cold-boots a VM whose first boot ran without
// spec.KernelArgs, once cloud-init has put them into grub; a -no-reboot
// guest cannot restart itself. It restarts the way pvm start does, so the
// gateway link and mounts are prepared again. spec is updated in place.
func (s *Server) applyPendingKernelArgs(ctx context.Context, spec *vm.Spec, seq uint64) error {
	args := strings.Fields(spec.KernelArgs)
	check := "sync"
	for _, a := range args {
		check += " && sudo grep -qwF -- " + shellQuote(a) + " /boot/grub/grub.cfg"
	}
	if out, err := s.sshRunCtx(ctx, spec, check); err != nil {
		return fmt.Errorf("guest grub lacks kernel args %q: %v%s", spec.KernelArgs, err, indentOutput(out))
	}
	s.stageIfCurrent(spec.Name, seq, "provision", "cold boot to apply kernel args "+spec.KernelArgs)
	err := s.withInstallVM(ctx, spec, seq, func(fresh *vm.Spec) error {
		if err := s.clearKernelArgsPending(fresh); err != nil {
			return err
		}
		log.Printf("vm %s: cold boot to apply guest kernel args %q", fresh.Name, fresh.KernelArgs)
		if err := s.instance(fresh).Stop(); err != nil {
			return fmt.Errorf("stop for kernel args: %w", err)
		}
		s.invalidateAgent(fresh.Name)
		if _, _, err := s.startVM(ctx, fresh); err != nil {
			return fmt.Errorf("restart for kernel args: %w; run `pvm start %s`", err, fresh.Name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	spec.KernelArgsPending = false
	if err := s.waitSSHCtx(ctx, spec, 6*time.Minute); err != nil {
		return fmt.Errorf("after kernel-args cold boot: %w", err)
	}
	cmdline, err := s.sshRunCtx(ctx, spec, "cat /proc/cmdline")
	if err != nil {
		return fmt.Errorf("read guest kernel cmdline: %w", err)
	}
	for _, a := range args {
		if !slices.Contains(strings.Fields(cmdline), a) {
			bootErr := fmt.Errorf("booted without kernel arg %q (cmdline: %s)", a, strings.TrimSpace(cmdline))
			pendingErr := s.setKernelArgsPending(spec, true)
			stopErr := s.instance(spec).Stop()
			if pendingErr == nil && stopErr == nil {
				return fmt.Errorf("%w; vm stopped and safe boot state restored", bootErr)
			}
			return errors.Join(bootErr, pendingErr, stopErr)
		}
	}
	return nil
}

// Readiness probes do not hold the VM lock. Mutations and publication do,
// after checking both the operation generation and the persisted instance.
func (s *Server) withInstallVM(ctx context.Context, spec *vm.Spec, seq uint64, apply func(*vm.Spec) error) error {
	lock := qemu.LockVM(spec.Name)
	for !lock.TryLock() {
		if err := waitInstallRetry(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.progressGeneration(spec.Name) != seq {
		return fmt.Errorf("VM provisioning was superseded")
	}
	fresh, err := vm.Load(s.P, spec.Name)
	if err != nil {
		return err
	}
	if !fresh.CreatedAt.Equal(spec.CreatedAt) {
		return fmt.Errorf("VM instance changed during provisioning")
	}
	return apply(fresh)
}

func waitInstallRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Server) waitSSH(spec *vm.Spec, d time.Duration) error {
	return s.waitSSHCtx(context.Background(), spec, d)
}

func (s *Server) waitSSHCtx(parent context.Context, spec *vm.Spec, d time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	var lastErr error
	for ctx.Err() == nil {
		if _, err := s.sshRunCtx(ctx, spec, "true"); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if err := waitInstallRetry(ctx, 3*time.Second); err != nil {
			return s.sshUnreachableError(spec, err, lastErr)
		}
	}
	return s.sshUnreachableError(spec, ctx.Err(), lastErr)
}

func (s *Server) sshUnreachableError(spec *vm.Spec, cause, lastErr error) error {
	if lines, _, err := tailLines(s.P.VMDir(spec.Name)+"/console.log", 15); err == nil && len(lines) > 0 {
		return fmt.Errorf("ssh not reachable: %w (last error: %v)\nconsole tail:\n  %s", cause, lastErr, strings.Join(lines, "\n  "))
	}
	return fmt.Errorf("ssh not reachable: %w (last error: %v)", cause, lastErr)
}

// cloudInitProbeScript prints three lines: cloud-init status value, the
// output log's byte size and its last line. Polling reconnects naturally
// across sshd restarts, unlike a long-lived `cloud-init status --wait` call.
const cloudInitProbeScript = `if ! command -v cloud-init >/dev/null 2>&1; then printf 'absent\n0\n\n'; exit 0; fi
sudo cloud-init status 2>/dev/null | sed -n 's/^status: //p' | head -n 1
sudo wc -c < /var/log/cloud-init-output.log 2>/dev/null || echo 0
sudo tail -n 1 /var/log/cloud-init-output.log 2>/dev/null | tr -d '\r' || true`

const cloudInitDiagScript = `sudo cloud-init status --long 2>&1 || true
echo '--- /var/log/cloud-init-output.log (tail) ---'
sudo tail -n 25 /var/log/cloud-init-output.log 2>&1 || true
echo '--- failed systemd units ---'
sudo systemctl --failed --no-pager --no-legend 2>&1 || true`

func (s *Server) waitGuestProvisionedCtx(ctx context.Context, spec *vm.Spec, seq uint64) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := s.sshRunCtx(probeCtx, spec, "command -v sudo >/dev/null 2>&1")
		cancel()
		if err == nil {
			break
		}
		if err := waitInstallRetry(ctx, cloudInitPollInterval); err != nil {
			return fmt.Errorf("sudo not available: %w", err)
		}
	}

	lastProgress := time.Now()
	lastSSHOK := time.Now()
	lastSize := ""
	lastLine := ""
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait cloud-init: %w", err)
		}
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		out, err := s.sshRunCtx(probeCtx, spec, cloudInitProbeScript)
		cancel()
		if err != nil {
			// sshd may briefly drop connections while provisioning; tolerate
			// short outages instead of failing an otherwise healthy boot.
			if time.Since(lastSSHOK) < cloudInitSSHTimeout {
				if rerr := waitInstallRetry(ctx, cloudInitPollInterval); rerr == nil {
					continue
				}
			}
			return fmt.Errorf("probe cloud-init: %w", err)
		}
		now := time.Now()
		lastSSHOK = now
		lines := strings.SplitN(strings.ReplaceAll(out, "\r\n", "\n"), "\n", 3)
		status := strings.TrimSpace(lines[0])
		size, last := "0", ""
		if len(lines) > 1 {
			size = strings.TrimSpace(lines[1])
		}
		if len(lines) > 2 {
			last = sanitizeGuestLine(strings.SplitN(lines[2], "\n", 2)[0])
		}
		if size != lastSize {
			lastSize = size
			lastProgress = now
		}
		if last != "" && last != lastLine {
			lastLine = last
		}
		switch status {
		case "absent", "not run", "disabled", "done":
			return nil
		case "degraded done":
			log.Printf("vm %s: cloud-init finished in degraded state: %s", spec.Name, last)
			return nil
		case "error":
			return &cloudInitError{Status: status, Detail: s.cloudInitDiagnostics(ctx, spec)}
		}
		s.stageIfCurrent(spec.Name, seq, "provision", fmt.Sprintf("waiting for cloud-init (status: %s) %s", status, last))
		if now.Sub(lastProgress) >= cloudInitIdleTimeout {
			return &cloudInitError{Status: fmt.Sprintf("stalled (no cloud-init log progress for %s, last status %q)", cloudInitIdleTimeout, status), Detail: s.cloudInitDiagnostics(ctx, spec)}
		}
		if err := waitInstallRetry(ctx, cloudInitPollInterval); err != nil {
			return fmt.Errorf("wait cloud-init: %w", err)
		}
	}
}

func (s *Server) cloudInitDiagnostics(ctx context.Context, spec *vm.Spec) string {
	diagCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := s.sshRunCtx(diagCtx, spec, cloudInitDiagScript)
	text := strings.TrimSpace(out)
	if err != nil {
		if text != "" {
			text += "\n"
		}
		text += fmt.Sprintf("collect diagnostics: %v", err)
	}
	const maxDiag = 4096
	if len(text) > maxDiag {
		text = "...\n" + text[len(text)-maxDiag:]
	}
	return text
}

func sanitizeGuestLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
}

func shellQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, `'"'"'`...)
		} else {
			out = append(out, s[i])
		}
	}
	return string(append(out, '\''))
}
