package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/agent"
	"github.com/huangzheng2016/pvm/internal/daemon"
)

// probeEchoPort is the guest port of the temporary echo server.
const probeEchoPort = 47123

// probeEchoServer echoes with TCP_NODELAY, one connection at a time, and
// exits after a minute without a connection.
const probeEchoServer = `import socket,sys
s=socket.socket()
s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind(("0.0.0.0",int(sys.argv[1])))
s.listen(4)
s.settimeout(60)
while True:
    c,_=s.accept()
    c.settimeout(None)
    c.setsockopt(socket.IPPROTO_TCP,socket.TCP_NODELAY,1)
    while True:
        d=c.recv(65536)
        if not d:
            break
        if d[:1]==b"R":
            c.sendall(b"y"*int(d[1:9]))
        else:
            c.sendall(d)
    c.close()
`

// probeLoopback measures round trips over loopback inside the guest, which
// shows the guest's own scheduling delay without any host or network hop.
const probeLoopback = `import socket,threading,time,sys
n,iv,size=int(sys.argv[1]),float(sys.argv[2]),int(sys.argv[3])
s=socket.socket()
s.bind(("127.0.0.1",0))
s.listen(1)
def serve():
    c,_=s.accept()
    c.setsockopt(socket.IPPROTO_TCP,socket.TCP_NODELAY,1)
    while True:
        d=c.recv(65536)
        if not d:
            return
        c.sendall(d)
threading.Thread(target=serve,daemon=True).start()
k=socket.create_connection(s.getsockname())
k.setsockopt(socket.IPPROTO_TCP,socket.TCP_NODELAY,1)
msg=b"x"*size
for _ in range(n):
    t=time.perf_counter()
    k.sendall(msg)
    got=0
    while got<size:
        got+=len(k.recv(65536))
    print("%.3f"%((time.perf_counter()-t)*1000))
    time.sleep(iv)
`

// probeGuestInfo prints the clocksource and the local timer interrupt
// rate summed over all CPUs, sampled over two seconds.
const probeGuestInfo = `echo "clocksource: $(cat /sys/devices/system/clocksource/clocksource0/current_clocksource)"
echo "available: $(cat /sys/devices/system/clocksource/clocksource0/available_clocksource)"
loc() { awk '$1=="LOC:"{s=0;for(i=2;i<=NF;i++)if($i~/^[0-9]+$/)s+=$i;print s}' /proc/interrupts; }
a=$(loc); sleep 2; b=$(loc)
echo "LOC interrupts/s: $(( (b-a)/2 )) over $(nproc) cpus"
`

// CmdProbeRTT is a hidden measurement tool: it reports small-message round
// trip times on the paths a remote display would use.
func CmdProbeRTT(args []string) int {
	const usage = "pvm __probe-rtt <vm> [--via all|relay|stream] [--n 1000] [--interval 20ms] [--size 64]"
	fs := newFlagSet("__probe-rtt", usage)
	n := fs.Int("n", 1000, "samples per path")
	interval := fs.Duration("interval", 20*time.Millisecond, "pause between samples")
	size := fs.Int("size", 64, "message size in bytes")
	via := fs.String("via", "all", "all, relay or stream")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return 1
	}
	if fs.NArg() != 1 || *n < 1 || *size < 1 || *size > 65536 || (*via != "all" && *via != "relay" && *via != "stream") {
		return fatalf("usage: %s", usage)
	}
	vmName := fs.Arg(0)
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}

	if *via != "stream" {
		if code := probeGuestAndRelay(c, vmName, *n, *interval, *size); code != 0 {
			return code
		}
	}
	if *via != "relay" {
		if err := probeStream(c, vmName, *n, *interval, *size); err != nil {
			return fatalf("stream: %v", err)
		}
	}
	return 0
}

func probeGuestAndRelay(c *daemon.Client, vmName string, n int, interval time.Duration, size int) int {
	info, _, err := probeExec(c, vmName, []string{"sh", "-c", probeGuestInfo})
	if err != nil {
		return fatalf("guest info: %v", err)
	}
	fmt.Print(info)

	out, _, err := probeExec(c, vmName, []string{"python3", "-c", probeLoopback,
		strconv.Itoa(n), strconv.FormatFloat(interval.Seconds(), 'f', -1, 64), strconv.Itoa(size)})
	if err != nil {
		return fatalf("guest loopback: %v", err)
	}
	var loop []float64
	for _, l := range strings.Fields(out) {
		if v, err := strconv.ParseFloat(l, 64); err == nil {
			loop = append(loop, v)
		}
	}
	printRTT("guest loopback", loop)

	relay, err := probeRelay(c, vmName, n, interval, size)
	if err != nil {
		return fatalf("relay: %v", err)
	}
	printRTT("host relay -> gateway -> vm", relay)
	for _, reply := range []int{64 << 10, 1 << 20} {
		bulk, err := probeRelayBulk(c, vmName, 50, interval, reply)
		if err != nil {
			return fatalf("relay bulk: %v", err)
		}
		printRTT(fmt.Sprintf("relay, 64 B request -> %d KiB reply", reply>>10), bulk)
	}
	return 0
}

// probeStream measures the pvm.stream path through the guest's in-process
// echo target.
func probeStream(c *daemon.Client, vmName string, n int, interval time.Duration, size int) error {
	path := fmt.Sprintf("/v1/vms/%s/stream?target=echo", vmName)
	conn, err := c.Upgrade(path)
	if err != nil {
		return err
	}
	msg := bytes.Repeat([]byte("x"), size)
	buf := make([]byte, size)
	var rtt []float64
	for i := 0; i < n; i++ {
		t := time.Now()
		if _, err := conn.Write(msg); err != nil {
			conn.Close()
			return err
		}
		if _, err := io.ReadFull(conn, buf); err != nil {
			conn.Close()
			return fmt.Errorf("after %d samples: %w", i, err)
		}
		rtt = append(rtt, float64(time.Since(t).Microseconds())/1000)
		time.Sleep(interval)
	}
	printRTT("host daemon -> pvm.stream -> guest", rtt)

	// an echo turns a 1 MiB send into a 1 MiB reply, so each sample moves
	// 1 MiB each way
	const chunk = 1 << 20
	big := bytes.Repeat([]byte("y"), chunk)
	got := make([]byte, chunk)
	var bulk []float64
	for i := 0; i < 50; i++ {
		t := time.Now()
		werr := make(chan error, 1)
		go func() { _, err := conn.Write(big); werr <- err }()
		if _, err := io.ReadFull(conn, got); err != nil {
			conn.Close()
			return err
		}
		if err := <-werr; err != nil {
			conn.Close()
			return err
		}
		bulk = append(bulk, float64(time.Since(t).Microseconds())/1000)
	}
	printRTT("pvm.stream, 1024 KiB echoed", bulk)
	conn.Close()

	// sustained throughput, one direction counted
	conn, err = c.Upgrade(path)
	if err != nil {
		return err
	}
	defer conn.Close()
	const total = 64 << 20
	t := time.Now()
	go func() {
		for sent := 0; sent < total; sent += chunk {
			if _, err := conn.Write(big); err != nil {
				return
			}
		}
	}()
	if _, err := io.CopyN(io.Discard, conn, total); err != nil {
		return err
	}
	el := time.Since(t).Seconds()
	fmt.Printf("pvm.stream throughput: %d MiB echoed in %.2fs = %.1f MB/s each way\n", total>>20, el, float64(total)/el/1e6)
	return probeStreamOneWay(c, vmName)
}

// probeStreamOneWay measures each direction alone.
func probeStreamOneWay(c *daemon.Client, vmName string) error {
	const total = 128 << 20
	up, err := c.Upgrade(fmt.Sprintf("/v1/vms/%s/stream?target=discard", vmName))
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	t := time.Now()
	for sent := 0; sent < total; sent += len(buf) {
		if _, err := up.Write(buf); err != nil {
			up.Close()
			return err
		}
	}
	el := time.Since(t).Seconds()
	up.Close()
	fmt.Printf("pvm.stream host -> guest: %d MiB in %.2fs = %.1f MB/s\n", total>>20, el, float64(total)/el/1e6)

	down, err := c.Upgrade(fmt.Sprintf("/v1/vms/%s/stream?target=source", vmName))
	if err != nil {
		return err
	}
	defer down.Close()
	t = time.Now()
	if _, err := io.CopyN(io.Discard, down, total); err != nil {
		return err
	}
	el = time.Since(t).Seconds()
	fmt.Printf("pvm.stream guest -> host: %d MiB in %.2fs = %.1f MB/s\n", total>>20, el, float64(total)/el/1e6)
	return nil
}

// reorderFlags moves flags before positionals so flag.Parse sees them.
func reorderFlags(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(append(flags, "--"), pos...)
}

func probeRelayWith(c *daemon.Client, vmName string, measure func(net.Conn) ([]float64, error)) ([]float64, error) {
	// setsid: the agent ends the exec session's process group on return
	start := fmt.Sprintf("setsid -f python3 -c %s %d >/dev/null 2>&1 </dev/null", sshShellQuote(probeEchoServer), probeEchoPort)
	if _, _, err := probeExec(c, vmName, []string{"sh", "-c", start}); err != nil {
		return nil, fmt.Errorf("start guest echo server: %w", err)
	}
	defer probeExec(c, vmName, []string{"sh", "-c", fmt.Sprintf("fuser -k %d/tcp >/dev/null 2>&1; true", probeEchoPort)})

	var m mapView
	if err := c.Post("/v1/maps", map[string]any{"vm": vmName, "host_port": 0, "guest_port": probeEchoPort, "proto": "tcp"}, &m); err != nil {
		return nil, fmt.Errorf("temporary map: %w", err)
	}
	defer c.Delete(fmt.Sprintf("/v1/maps/%d/tcp", m.HostPort), nil)

	// the relay accepts before the gateway forwards the port, so retry
	// until a byte comes back
	conn, err := probeDial(m.HostPort, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return measure(conn)
}

func probeRelay(c *daemon.Client, vmName string, n int, interval time.Duration, size int) ([]float64, error) {
	return probeRelayWith(c, vmName, func(conn net.Conn) ([]float64, error) {
		msg := bytes.Repeat([]byte("x"), size)
		buf := make([]byte, size)
		var rtt []float64
		for i := 0; i < n; i++ {
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			t := time.Now()
			if _, err := conn.Write(msg); err != nil {
				return rtt, err
			}
			if _, err := io.ReadFull(conn, buf); err != nil {
				return rtt, fmt.Errorf("after %d samples: %w", i, err)
			}
			rtt = append(rtt, float64(time.Since(t).Microseconds())/1000)
			time.Sleep(interval)
		}
		return rtt, nil
	})
}

// probeRelayBulk times a small request answered by a large reply, the
// shape of a framebuffer update.
func probeRelayBulk(c *daemon.Client, vmName string, n int, interval time.Duration, reply int) ([]float64, error) {
	return probeRelayWith(c, vmName, func(conn net.Conn) ([]float64, error) {
		req := []byte(fmt.Sprintf("R%08d", reply))
		buf := make([]byte, reply)
		var rtt []float64
		for i := 0; i < n; i++ {
			conn.SetDeadline(time.Now().Add(30 * time.Second))
			t := time.Now()
			if _, err := conn.Write(req); err != nil {
				return rtt, err
			}
			if _, err := io.ReadFull(conn, buf); err != nil {
				return rtt, err
			}
			rtt = append(rtt, float64(time.Since(t).Microseconds())/1000)
			time.Sleep(interval)
		}
		return rtt, nil
	})
}

func probeDial(port int, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
		if err == nil {
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			var b [1]byte
			if _, err = conn.Write([]byte("x")); err == nil {
				if _, err = io.ReadFull(conn, b[:]); err == nil {
					return conn, nil
				}
			}
			conn.Close()
		}
		last = err
		time.Sleep(250 * time.Millisecond)
	}
	return nil, fmt.Errorf("echo server not reachable through the relay: %v", last)
}

// probeExec runs a command through the guest agent and returns stdout.
func probeExec(c *daemon.Client, vmName string, cmd []string) (string, int, error) {
	resp, err := c.PostStream(fmt.Sprintf("/v1/vms/%s/exec", vmName), map[string]any{"cmd": cmd})
	if err != nil {
		return "", -1, err
	}
	defer resp.Body.Close()
	var stdout, stderr strings.Builder
	code := -1
	dec := json.NewDecoder(resp.Body)
	for {
		var f agent.Frame
		if err := dec.Decode(&f); err == io.EOF {
			break
		} else if err != nil {
			return "", -1, err
		}
		switch {
		case f.Error != "":
			return "", -1, fmt.Errorf("%s", f.Error)
		case f.Done:
			code = f.ExitCode
		case f.Stream == agent.StreamStdout || f.Stream == agent.StreamStderr:
			data, _ := base64.StdEncoding.DecodeString(f.DataB64)
			if f.Stream == agent.StreamStdout {
				stdout.Write(data)
			} else {
				stderr.Write(data)
			}
		}
	}
	if code != 0 {
		return stdout.String(), code, fmt.Errorf("exit %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), code, nil
}

func printRTT(name string, ms []float64) {
	if len(ms) == 0 {
		fmt.Printf("%s: no samples\n", name)
		return
	}
	s := slices.Clone(ms)
	slices.Sort(s)
	q := func(p float64) float64 { return s[min(len(s)-1, int(p*float64(len(s))))] }
	spikes := 0
	for _, v := range s {
		if v >= 150 {
			spikes++
		}
	}
	fmt.Printf("%s: n=%d p50=%.2fms p95=%.2fms p99=%.2fms max=%.2fms >=150ms:%d\n",
		name, len(s), q(0.50), q(0.95), q(0.99), s[len(s)-1], spikes)
}
