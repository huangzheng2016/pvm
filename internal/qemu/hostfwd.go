package qemu

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// HostForward is one user-network port forward from a host port to the
// same guest port.
type HostForward struct {
	Proto string // "tcp" or "udp"
	Port  int
}

func (f HostForward) String() string { return f.Proto + ":" + strconv.Itoa(f.Port) }

// hmp runs a human monitor command and returns its output; user network
// forwards have no QMP command of their own.
func (q *QMP) hmp(line string) (string, error) {
	raw, err := q.Execute("human-monitor-command", map[string]any{"command-line": line})
	if err != nil {
		return "", err
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("%s: %w", line, err)
	}
	return out, nil
}

// HostForwards lists the forwards of the user netdev from 127.0.0.1.
func (q *QMP) HostForwards() ([]HostForward, error) {
	out, err := q.hmp("info usernet")
	if err != nil {
		return nil, err
	}
	return parseHostForwards(out), nil
}

// parseHostForwards reads `info usernet` rows such as
// "TCP[HOST_FORWARD]  838  127.0.0.1 30000  10.0.2.15 30000  0  0".
func parseHostForwards(out string) []HostForward {
	var fwds []HostForward
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 4 || !strings.HasSuffix(f[0], "[HOST_FORWARD]") || f[2] != "127.0.0.1" {
			continue
		}
		port, err := strconv.Atoi(f[3])
		if err != nil {
			continue
		}
		fwds = append(fwds, HostForward{Proto: strings.ToLower(strings.TrimSuffix(f[0], "[HOST_FORWARD]")), Port: port})
	}
	return fwds
}

// AddHostForward forwards 127.0.0.1:port on the host to port in the guest.
func (q *QMP) AddHostForward(f HostForward) error {
	out, err := q.hmp(fmt.Sprintf("hostfwd_add net0 %s:127.0.0.1:%d-:%d", f.Proto, f.Port, f.Port))
	if err != nil {
		return err
	}
	// success prints nothing
	if s := strings.TrimSpace(out); s != "" {
		return fmt.Errorf("add host forward %s: %s", f, s)
	}
	return nil
}

// RemoveHostForward removes a forward added by AddHostForward.
func (q *QMP) RemoveHostForward(f HostForward) error {
	out, err := q.hmp(fmt.Sprintf("hostfwd_remove net0 %s:127.0.0.1:%d", f.Proto, f.Port))
	if err != nil {
		return err
	}
	if !strings.Contains(out, "removed") {
		return fmt.Errorf("remove host forward %s: %s", f, strings.TrimSpace(out))
	}
	return nil
}
