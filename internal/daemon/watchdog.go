package daemon

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

type stallInput struct {
	SSHReady        bool
	ConsoleProgress bool
	CPUPercent      float64
	Attempts        int
}

type stallAction int

const (
	stallContinue stallAction = iota
	stallRestart
	stallWarn
)

// decideStall only restarts a VM that is both spinning at full CPU and
// making no serial-console progress. A slow-but-progressing TCG boot is
// left alone, and a second identical stall is only reported.
func decideStall(in stallInput) stallAction {
	if in.SSHReady || in.ConsoleProgress || in.CPUPercent < 95 {
		return stallContinue
	}
	if in.Attempts < 1 {
		return stallRestart
	}
	return stallWarn
}

func (s *Server) nextWatchSeq(name string) uint64 {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	if s.watchSeq == nil {
		s.watchSeq = map[string]uint64{}
	}
	s.watchSeq[name]++
	return s.watchSeq[name]
}

func (s *Server) currentWatchSeq(name string) uint64 {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	return s.watchSeq[name]
}

// watchVM starts a best-effort boot-stall watchdog for one VM. A newer
// start of the same VM supersedes older watchers.
func (s *Server) watchVM(spec *vm.Spec) {
	seq := s.nextWatchSeq(spec.Name)
	go s.watchVMLoop(spec, seq)
}

func (s *Server) watchVMLoop(spec *vm.Spec, seq uint64) {
	threshold := 90 * time.Second
	if spec.Role == "gateway" {
		threshold = 180 * time.Second
	}
	deadline := time.Now().Add(threshold)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	var lastConsoleSize int64 = -1
	attempts := 0
	for range ticker.C {
		if s.currentWatchSeq(spec.Name) != seq {
			return
		}
		inst := s.instance(spec)
		st, err := inst.Running()
		if err != nil || st == nil {
			return
		}
		if time.Now().Before(deadline) {
			continue
		}
		if _, err := s.sshRun(spec, "true"); err == nil {
			return
		}

		size := consoleSize(s.P.VMDir(spec.Name) + "/console.log")
		progress := lastConsoleSize >= 0 && size > lastConsoleSize
		lastConsoleSize = size
		cpu, err := qemuCPUPercent(st.PID)
		if err != nil {
			continue
		}
		action := decideStall(stallInput{
			SSHReady: false, ConsoleProgress: progress, CPUPercent: cpu, Attempts: attempts,
		})
		switch action {
		case stallRestart:
			log.Printf("vm %s: possible UEFI boot stall (cpu %.0f%%, no console progress); restarting once", spec.Name, cpu)
			if err := s.restartStalledVM(spec); err != nil {
				log.Printf("vm %s: restart after boot stall failed: %v", spec.Name, err)
				return
			}
			attempts++
			deadline = time.Now().Add(threshold)
			lastConsoleSize = -1
		case stallWarn:
			log.Printf("vm %s: boot still stalled after one restart (cpu %.0f%%); giving up", spec.Name, cpu)
			return
		}
	}
}

func (s *Server) restartStalledVM(spec *vm.Spec) error {
	lock := qemu.LockVM(spec.Name)
	lock.Lock()
	defer lock.Unlock()
	inst := s.instance(spec)
	if err := inst.Stop(); err != nil {
		return err
	}
	return inst.Start()
}

func consoleSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

func qemuCPUPercent(pid int) (float64, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "%cpu=").Output()
	if err != nil {
		return 0, err
	}
	return parseCPUPercent(string(out))
}

func parseCPUPercent(out string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err != nil {
		return 0, fmt.Errorf("parse ps cpu %q: %w", strings.TrimSpace(out), err)
	}
	return v, nil
}
