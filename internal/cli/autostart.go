package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

func confirmYesDefault(r io.Reader, w io.Writer, prompt string) bool {
	fmt.Fprintf(w, "%s [Y/n] ", prompt)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "" || ans == "y" || ans == "yes"
}

// offerStartVM starts a stopped VM after interactive confirmation (TTY only)
// or when assumeYes is set. Scripts without a TTY never start VMs by surprise.
func offerStartVM(c *daemon.Client, name string, assumeYes bool) (*vmInfoView, error) {
	var info vmInfoView
	if err := c.Get("/v1/vms/"+name, &info); err != nil {
		return nil, err
	}
	if info.Running {
		return &info, nil
	}
	if !assumeYes {
		if !stdinIsTTY() || !isTTY(os.Stdout) {
			return nil, fmt.Errorf("vm %s is not running; run `pvm start %s`", name, name)
		}
		if !confirmYesDefault(os.Stdin, os.Stderr, fmt.Sprintf("%s is stopped. Start it?", name)) {
			return nil, fmt.Errorf("vm %s is not running; run `pvm start %s`", name, name)
		}
	}
	if err := c.Post("/v1/vms/"+name+"/start", nil, nil); err != nil {
		return nil, err
	}
	if rc := followProvision(c, name, provisionFollowTimeout); rc != 0 {
		return nil, fmt.Errorf("vm %s did not become ready after start", name)
	}
	if err := c.Get("/v1/vms/"+name, &info); err != nil {
		return nil, err
	}
	return &info, nil
}
