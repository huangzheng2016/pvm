package agent

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/stream"
)

// runStream serves the pvm.stream port. VMs started before the port
// existed have none, and run without it.
func runStream(sysfsRoot string) {
	dev, err := FindVirtioPort(sysfsRoot, stream.PortName)
	if err != nil {
		log.Printf("agent: %s unavailable: %v", stream.PortName, err)
		return
	}
	log.Printf("agent: serving %s on %s", stream.PortName, dev)
	// the port opens only once at a time, so one fd serves every session
	var f *os.File
	for {
		if f == nil {
			f, err = os.OpenFile(dev, os.O_RDWR, 0)
			if err != nil {
				log.Printf("agent: open %s: %v (retry in 2s)", dev, err)
				time.Sleep(2 * time.Second)
				continue
			}
		}
		start := time.Now()
		err = stream.Serve(keepOpen{f}, streamTarget)
		// an idle port returns EOF at once; only log sessions with traffic
		if err != nil && time.Since(start) > 2*time.Second {
			log.Printf("agent: %s session ended: %v", stream.PortName, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// keepOpen stops a session from closing the port it runs on.
type keepOpen struct{ *os.File }

func (keepOpen) Close() error { return nil }

// streamTarget connects an allowed stream target.
func streamTarget(target string) (io.ReadWriteCloser, error) {
	if rest, ok := strings.CutPrefix(target, "clip:"); ok {
		uid, kind, _ := strings.Cut(rest, ":")
		n, err := strconv.Atoi(uid)
		if err != nil || strconv.Itoa(n) != uid {
			return nil, fmt.Errorf("bad uid in target %q", target)
		}
		return clipboardSink(n, kind)
	}
	if uid, ok := strings.CutPrefix(target, "clipout:"); ok {
		uid, ok = strings.CutSuffix(uid, ":files")
		n, err := strconv.Atoi(uid)
		if !ok || err != nil || strconv.Itoa(n) != uid {
			return nil, fmt.Errorf("bad clipboard target %q", target)
		}
		return clipFilesSource(n)
	}
	if uid, ok := strings.CutPrefix(target, "xpra:"); ok {
		n, err := strconv.Atoi(uid)
		if err != nil || strconv.Itoa(n) != uid {
			return nil, fmt.Errorf("bad uid in target %q", target)
		}
		return dialUserSocket("/", n, xpraSocket)
	}
	if portStr, ok := strings.CutPrefix(target, "tcp:"); ok {
		// Host port maps over pvm.stream: only guest loopback is reachable,
		// so the VM's egress route and kill-switch are unchanged.
		n, err := strconv.Atoi(portStr)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != portStr {
			return nil, fmt.Errorf("bad tcp target %q", target)
		}
		return net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", portStr), 5*time.Second)
	}
	switch target {
	case "novnc":
		// pvm desktop's websockify, which serves noVNC and proxies RFB
		return net.DialTimeout("tcp", "127.0.0.1:6080", 5*time.Second)
	case "echo":
		return newEchoConn(), nil
	case "discard":
		// reads everything and never answers, for one-way throughput
		r, w := io.Pipe()
		go io.Copy(io.Discard, r)
		return &echoConn{r: emptyReader(), w: w}, nil
	case "source":
		// sends zeros until the host closes, for one-way throughput
		r, w := io.Pipe()
		go func() {
			buf := make([]byte, 64<<10)
			for {
				if _, err := w.Write(buf); err != nil {
					return
				}
			}
		}()
		_, sink := io.Pipe()
		return &echoConn{r: r, w: sink}, nil
	}
	return nil, fmt.Errorf("unknown target %q", target)
}

// echoConn echoes in process, for measuring the channel.
type echoConn struct {
	r *io.PipeReader
	w *io.PipeWriter
}

// emptyReader blocks until closed, so the stream stays open until the
// host is done writing.
func emptyReader() *io.PipeReader {
	r, _ := io.Pipe()
	return r
}

func newEchoConn() *echoConn {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		io.Copy(outW, inR)
		outW.Close()
	}()
	return &echoConn{r: outR, w: inW}
}

func (c *echoConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *echoConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *echoConn) CloseWrite() error           { return c.w.Close() }
func (c *echoConn) Close() error {
	c.w.Close()
	return c.r.Close()
}
