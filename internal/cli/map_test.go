package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/huangzheng2016/pvm/internal/paths"
)

func TestMapRejectsInvalidArgumentsBeforeSideEffects(t *testing.T) {
	for _, args := range [][]string{
		{}, {"dev1", "8080:80", "--upd"}, {"rm", "8080", "--upd"},
		{"dev1", "8080:80", "extra"}, {"rm", "8080", "extra"},
		{"dev1", "8080:80", "--", "--udp"}, {"rm", "8080", "--", "--udp"},
		{"ls", "extra"}, {"ls", "--udp"}, {"rm"},
	} {
		t.Run(filepath.Join(args...), func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "pvm")
			t.Setenv("PVM_HOME", home)
			if got := CmdMap(args); got == 0 {
				t.Fatal("invalid arguments succeeded")
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatalf("invalid arguments created state directory: %v", err)
			}
		})
	}
}

func TestMapUDPRequests(t *testing.T) {
	// Unix sockets have a short path limit, including on macOS.
	home, err := os.MkdirTemp("", "pvm-map-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("PVM_HOME", home)
	p := paths.New(home)
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p.DaemonSock())
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 4)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			w.Write([]byte(`{"ready":true}`))
		case "/v1/maps":
			var body mapView
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			requests <- body.Proto
			json.NewEncoder(w).Encode(body)
		default:
			requests <- r.Method + " " + r.URL.Path
			w.Write([]byte(`{}`))
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	for _, tc := range []struct {
		args  []string
		proto string
	}{
		{[]string{"dev1", "8080:80", "--udp"}, "udp"},
		{[]string{"--udp", "dev1", "8080:80"}, "udp"},
		{[]string{"--udp", "--", "dev1", "8080:80"}, "udp"},
		{[]string{"dev1", "8080:80", "--"}, "tcp"},
		{[]string{"dev1", "8080:80", "--udp=false"}, "tcp"},
	} {
		if code := CmdMap(tc.args); code != 0 {
			t.Fatalf("map returned %d", code)
		}
		if got := <-requests; got != tc.proto {
			t.Fatalf("protocol = %q", got)
		}
	}
	if code := CmdMap([]string{"rm", "8080", "--udp"}); code != 0 {
		t.Fatalf("map rm returned %d", code)
	}
	if got := <-requests; got != "DELETE /v1/maps/8080/udp" {
		t.Fatalf("request = %q", got)
	}
}
