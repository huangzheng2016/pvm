package daemon

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/huangzheng2016/pvm/internal/agent"
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

// ensureAgentAsync pushes the guest agent once ssh is reachable. Runs
// in the background; failures are logged, never fatal to the VM.
func (s *Server) ensureAgentAsync(spec *vm.Spec) {
	bin := s.P.AgentBin(spec.Arch)
	if _, err := os.Stat(bin); err != nil {
		log.Printf("agent: %s not packed (run script/build.sh); skipping agent install for %s", bin, spec.Name)
		return
	}
	go func() {
		if err := s.installAgent(spec, bin); err != nil {
			log.Printf("agent: install into %s failed: %v", spec.Name, err)
			return
		}
		log.Printf("agent: installed and reachable in %s", spec.Name)
	}()
}

func (s *Server) installAgent(spec *vm.Spec, bin string) error {
	s.stage(spec.Name, "provision", "waiting for ssh (cloud-init running)")
	if err := s.waitSSH(spec, 6*time.Minute); err != nil {
		return err
	}
	s.stage(spec.Name, "ready", "ssh reachable")
	if err := s.scpTo(spec, bin, "/tmp/pvm-agent.new"); err != nil {
		return fmt.Errorf("scp: %w", err)
	}
	installCmd := fmt.Sprintf(
		"sudo install -m 0755 /tmp/pvm-agent.new /usr/local/bin/pvm && rm -f /tmp/pvm-agent.new && "+
			"echo %s | sudo tee /etc/systemd/system/pvm-agent.service >/dev/null && "+
			"sudo systemctl daemon-reload && sudo systemctl enable pvm-agent && sudo systemctl restart pvm-agent",
		shellQuote(agentUnit))
	if out, err := s.sshRun(spec, installCmd); err != nil {
		return fmt.Errorf("install unit: %v\n%s", err, out)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ac, err := agent.DialSock(s.P.AgentSock(spec.Name))
		if err == nil {
			_, perr := ac.Ping()
			ac.Close()
			if perr == nil {
				s.stage(spec.Name, "agent", "guest agent installed")
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("agent not reachable on virtio channel after install")
}

func (s *Server) waitSSH(spec *vm.Spec, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := s.sshRun(spec, "true"); err == nil {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("ssh not reachable within %v", d)
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
