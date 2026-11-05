package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
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
			s.stageIfCurrent(spec.Name, task.seq, "failed", fmt.Sprintf("agent installation failed: %v; run `pvm stop %s && pvm start %s`", err, spec.Name, spec.Name))
			log.Printf("agent: install into %s failed: %v", spec.Name, err)
			return
		}
		log.Printf("agent: installed and reachable in %s", spec.Name)
	}()
}

func (s *Server) installAgentGeneration(ctx context.Context, spec *vm.Spec, bin string, seq uint64) error {
	s.stageIfCurrent(spec.Name, seq, "provision", "waiting for ssh (cloud-init running)")
	if err := s.waitSSHCtx(ctx, spec, 6*time.Minute); err != nil {
		return err
	}
	if err := s.waitGuestProvisionedCtx(ctx, spec, 6*time.Minute); err != nil {
		return err
	}
	s.stageIfCurrent(spec.Name, seq, "ssh", "SSH and cloud-init ready")
	phaseCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err := s.withInstallVM(phaseCtx, spec, seq, func(fresh *vm.Spec) error {
		if err := s.configureGuestMountsCtx(phaseCtx, fresh); err != nil {
			return err
		}
		if err := s.scpToCtx(phaseCtx, fresh, bin, "/tmp/pvm-agent.new"); err != nil {
			return fmt.Errorf("scp: %w", err)
		}
		installCmd := fmt.Sprintf(
			"sudo install -m 0755 /tmp/pvm-agent.new /usr/local/bin/pvm && rm -f /tmp/pvm-agent.new && "+
				"echo %s | sudo tee /etc/systemd/system/pvm-agent.service >/dev/null && "+
				"sudo systemctl daemon-reload && sudo systemctl enable pvm-agent && sudo systemctl restart pvm-agent",
			shellQuote(agentUnit))
		if out, err := s.sshRunCtx(phaseCtx, fresh, installCmd); err != nil {
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
			return fmt.Errorf("ssh not reachable: %w (last error: %v)", err, lastErr)
		}
	}
	return fmt.Errorf("ssh not reachable: %w (last error: %v)", ctx.Err(), lastErr)
}

func (s *Server) waitGuestProvisionedCtx(parent context.Context, spec *vm.Spec, d time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.sshRunCtx(ctx, spec, "command -v sudo >/dev/null 2>&1"); err == nil {
			break
		}
		if err := waitInstallRetry(ctx, 3*time.Second); err != nil {
			return fmt.Errorf("sudo not available: %w", err)
		}
	}
	if _, err := s.sshRunCtx(ctx, spec, "if command -v cloud-init >/dev/null; then sudo cloud-init status --wait >/dev/null; fi"); err != nil {
		return fmt.Errorf("wait cloud-init: %w", err)
	}
	return nil
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
