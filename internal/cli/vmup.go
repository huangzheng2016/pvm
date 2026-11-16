package cli

import (
	"fmt"
	"os"

	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/paths"
)

// ensureDaemon is daemon.Ensure plus a question: a person at a terminal
// sees that the daemon is about to start instead of a pause with no
// explanation. Pipes keep the silent autospawn, and an explicit
// `pvm daemon stop` stays respected.
func ensureDaemon(p *paths.Paths) (*daemon.Client, error) {
	if daemon.NewClient(p).Healthy() {
		return daemon.Ensure(p)
	}
	if _, err := os.Stat(p.DaemonStopMark()); err == nil || !stdinIsTTY() {
		return daemon.Ensure(p)
	}
	if !confirmYes(os.Stdin, os.Stderr, "the pvm daemon is not running. Start it?") {
		return nil, fmt.Errorf("daemon is not running; start it with `pvm daemon start`")
	}
	fmt.Fprintln(os.Stderr, "starting the daemon...")
	return daemon.Ensure(p)
}

// vmUp starts a stopped VM on request. Scripts still fail with the hint
// instead of starting machines by surprise.
func vmUp(c *daemon.Client, name string, yes bool) error {
	var info vmInfoView
	if err := c.Get("/v1/vms/"+name, &info); err != nil {
		return err
	}
	if info.Running {
		return nil
	}
	if !yes {
		if !stdinIsTTY() {
			return fmt.Errorf("vm %s is not running; `pvm start %s`", name, name)
		}
		if !confirmYes(os.Stdin, os.Stderr, fmt.Sprintf("vm %s is stopped. Start it?", name)) {
			return fmt.Errorf("vm %s is not running; `pvm start %s`", name, name)
		}
	}
	if err := c.Post(fmt.Sprintf("/v1/vms/%s/start", name), nil, nil); err != nil {
		return err
	}
	fmt.Printf("start %s\n", name)
	if code := followProvision(c, name, provisionFollowTimeout); code != 0 {
		return fmt.Errorf("vm %s did not become ready; see `pvm logs %s --follow`", name, name)
	}
	return nil
}
