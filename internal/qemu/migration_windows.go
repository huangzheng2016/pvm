package qemu

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

type finishReader struct {
	conn net.Conn
	done <-chan struct{}
}

func (r *finishReader) Read(p []byte) (int, error) {
	_ = r.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	n, err := r.conn.Read(p)
	if n > 0 {
		return n, nil
	}
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			select {
			case <-r.done:
				return 0, io.EOF
			default:
				return 0, nil
			}
		}
		return 0, err
	}
	return 0, nil
}

// Windows QEMU could not launch pvm's exec: migration helper. Loopback TCP
// keeps the stream in this process and finishes it explicitly after migration.
func migrationOutgoing(ramPath string, _ int) (string, <-chan error, func(), func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, nil, err
	}
	done := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(done) }) }
	accepted := make(chan net.Conn, 1)
	cleanup := func() {
		finish()
		_ = ln.Close()
		select {
		case conn := <-accepted:
			_ = conn.Close()
		default:
		}
	}
	helperDone := make(chan error, 1)
	go func() {
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			helperDone <- err
			return
		}
		accepted <- conn
		defer conn.Close()
		f, err := os.OpenFile(ramPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			helperDone <- err
			return
		}
		defer f.Close()
		if err := snapCompress(f, &finishReader{conn: conn, done: done}); err != nil {
			helperDone <- err
			return
		}
		helperDone <- f.Sync()
	}()
	return "tcp:" + ln.Addr().String(), helperDone, finish, cleanup, nil
}

func migrationRestore(ramPath string) (string, func(context.Context) error, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		return "", nil, err
	}
	start := func(ctx context.Context) error {
		var conn net.Conn
		for {
			conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			if err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("connect restore stream: %w (last error: %v)", ctx.Err(), err)
			case <-time.After(100 * time.Millisecond):
			}
		}
		defer conn.Close()
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
		defer stop()
		f, err := os.Open(ramPath)
		if err != nil {
			return err
		}
		defer f.Close()
		return snapDecompress(conn, f)
	}
	return "tcp:" + addr, start, nil
}
