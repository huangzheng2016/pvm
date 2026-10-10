package exit

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// SpeedPort serves /speed and /ping inside each tunnel (server .1 only).
const SpeedPort = 18081

const maxSpeedBytes = 64 << 20

func speedHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /speed", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
		if err != nil || n < 0 {
			n = 8 << 20
		}
		if n > maxSpeedBytes {
			n = maxSpeedBytes
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = io.CopyN(w, &randReader{}, n)
	})
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, `{"ok":true,"ts":%d}`, time.Now().UnixNano())
	})
	return mux
}

// randReader streams crypto-random bytes.
type randReader struct{}

func (*randReader) Read(p []byte) (int, error) {
	return rand.Read(p)
}

// serveSpeedConn answers one HTTP request on a raw connection (userspace
// NAT interception path, where no host listener exists).
func serveSpeedConn(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Minute))
	req, err := http.ReadRequest(bufio.NewReader(c))
	if err != nil {
		return
	}
	speedHandler().ServeHTTP(&connResponseWriter{conn: c, header: http.Header{}}, req)
}

// connResponseWriter adapts a raw conn to http.ResponseWriter for one
// request, buffering headers until first Write.
type connResponseWriter struct {
	conn        net.Conn
	header      http.Header
	wroteHeader bool
}

func (w *connResponseWriter) Header() http.Header { return w.header }

func (w *connResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if w.header.Get("Content-Length") == "" {
		w.header.Set("Content-Length", "0")
	}
	w.header.Set("Connection", "close")
	fmt.Fprintf(w.conn, "HTTP/1.1 %d %s\r\n", code, http.StatusText(code))
	_ = w.header.Write(w.conn)
	fmt.Fprintf(w.conn, "\r\n")
}

func (w *connResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.conn.Write(b)
}
