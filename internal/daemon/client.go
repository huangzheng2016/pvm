package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/paths"
)

// Client talks to the daemon over the unix socket.
type Client struct {
	P    *paths.Paths
	http *http.Client
}

func NewClient(p *paths.Paths) *Client {
	return &Client{
		P: p,
		http: &http.Client{
			Timeout: 120 * time.Second, // VM stop can take up to the 30s powerdown grace
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", p.DaemonSock())
				},
			},
		},
	}
}

// Healthy reports whether the daemon answers /v1/health quickly.
func (c *Client) Healthy() bool {
	req, err := http.NewRequest("GET", "http://d/v1/health", nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// Ensure returns a client to a running daemon, auto-spawning it if
// needed. A daemon explicitly stopped via `pvm daemon stop` is not
// auto-restarted (marker file) — the caller gets a clear hint instead.
func Ensure(p *paths.Paths) (*Client, error) {
	c := NewClient(p)
	if c.Healthy() {
		return c, nil
	}
	if _, err := os.Stat(p.DaemonStopMark()); err == nil {
		return nil, fmt.Errorf("daemon is stopped; run `pvm daemon start`")
	}
	lock, err := fsutil.TryLock(p.DaemonLock())
	if err != nil {
		// Someone else is starting it; just wait.
		if waitHealthy(c, 15*time.Second) {
			return c, nil
		}
		return nil, fmt.Errorf("daemon not reachable and start in progress elsewhere")
	}
	defer lock.Unlock()

	if c.Healthy() { // double-check after acquiring the lock
		return c, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(p.DaemonLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "daemon", "run")
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn daemon: %w", err)
	}
	if !waitHealthy(c, 15*time.Second) {
		return nil, fmt.Errorf("daemon spawned (pid %d) but did not become healthy; see %s", cmd.Process.Pid, p.DaemonLog())
	}
	return c, nil
}

func waitHealthy(c *Client, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c.Healthy() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func (c *Client) Get(path string, out any) error {
	resp, err := c.http.Get("http://d" + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResp(resp, out)
}

func (c *Client) Post(path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(data)
	}
	resp, err := c.http.Post("http://d"+path, "application/json", rdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResp(resp, out)
}

func (c *Client) Delete(path string, out any) error {
	req, err := http.NewRequest("DELETE", "http://d"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResp(resp, out)
}

func decodeResp(resp *http.Response, out any) error {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("daemon http %d: %s", resp.StatusCode, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
