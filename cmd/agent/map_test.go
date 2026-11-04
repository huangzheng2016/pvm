package main

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/agent"
)

func TestMapControlRequests(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		action     string
		wantReq    any
		wantOutput string
	}{
		{name: "add tcp", args: []string{"add", "18080:80"}, action: agent.ControlMapAdd, wantReq: &agent.MapAddRequest{HostPort: 18080, GuestPort: 80, Proto: "tcp"}, wantOutput: "127.0.0.1:18080/tcp"},
		{name: "add udp", args: []string{"add", "18080:80", "--udp"}, action: agent.ControlMapAdd, wantReq: &agent.MapAddRequest{HostPort: 18080, GuestPort: 80, Proto: "udp"}, wantOutput: "127.0.0.1:18080/udp"},
		{name: "add automatic port", args: []string{"add", "0:80"}, action: agent.ControlMapAdd, wantReq: &agent.MapAddRequest{HostPort: 0, GuestPort: 80, Proto: "tcp"}, wantOutput: "127.0.0.1:18080/tcp"},
		{name: "list", args: []string{"ls"}, action: agent.ControlMapList, wantReq: nil, wantOutput: "127.0.0.1:18080"},
		{name: "remove tcp", args: []string{"rm", "18080"}, action: agent.ControlMapRemove, wantReq: &agent.MapRemoveRequest{HostPort: 18080, Proto: "tcp"}, wantOutput: "removed map 18080/tcp"},
		{name: "remove udp", args: []string{"--udp", "rm", "18080"}, action: agent.ControlMapRemove, wantReq: &agent.MapRemoveRequest{HostPort: 18080, Proto: "udp"}, wantOutput: "removed map 18080/udp"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			call := func(sock, action string, req, resp any, timeout time.Duration) error {
				calls++
				if sock != "/test/control.sock" {
					t.Fatalf("socket = %q", sock)
				}
				if action != tc.action {
					t.Fatalf("action = %q, want %q", action, tc.action)
				}
				if !reflect.DeepEqual(req, tc.wantReq) {
					t.Fatalf("request = %#v, want %#v", req, tc.wantReq)
				}
				if timeout != mapControlTimeout {
					t.Fatalf("timeout = %v", timeout)
				}
				switch out := resp.(type) {
				case *agent.MapAddResponse:
					out.Map = agent.MapEntry{HostPort: 18080, GuestPort: 80, Proto: "tcp"}
					if tc.args[len(tc.args)-1] == "--udp" {
						out.Map.Proto = "udp"
					}
				case *agent.MapListResponse:
					out.Maps = []agent.MapEntry{{HostPort: 18080, GuestPort: 80, Proto: "tcp"}}
				case nil:
				default:
					t.Fatalf("unexpected response type %T", resp)
				}
				return nil
			}
			var stdout, stderr bytes.Buffer
			if code := cmdMap(tc.args, "/test/control.sock", call, &stdout, &stderr); code != 0 {
				t.Fatalf("map returned %d: %s", code, stderr.String())
			}
			if calls != 1 {
				t.Fatalf("control calls = %d, want 1", calls)
			}
			if !strings.Contains(stdout.String(), tc.wantOutput) {
				t.Fatalf("output = %q, want %q", stdout.String(), tc.wantOutput)
			}
		})
	}
}

func TestMapRejectsInvalidArgumentsBeforeControl(t *testing.T) {
	tests := [][]string{
		{},
		{"dev1", "18080:80"},
		{"add"},
		{"add", "dev1", "18080:80"},
		{"add", "18080:80", "extra"},
		{"add", "18080"},
		{"add", "bad:80"},
		{"add", "80:80"},
		{"add", "65536:80"},
		{"add", "18080:0"},
		{"add", "18080:65536"},
		{"add", "18080:80", "--upd"},
		{"add", "18080:80", "--", "--udp"},
		{"ls", "extra"},
		{"ls", "--udp"},
		{"rm"},
		{"rm", "0"},
		{"rm", "bad"},
		{"rm", "18080", "extra"},
		{"rm", "18080", "--upd"},
	}
	for _, args := range tests {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			calls := 0
			call := func(string, string, any, any, time.Duration) error {
				calls++
				return nil
			}
			var stdout, stderr bytes.Buffer
			if code := cmdMap(args, "/test/control.sock", call, &stdout, &stderr); code != 2 {
				t.Fatalf("map %v returned %d, want 2", args, code)
			}
			if calls != 0 {
				t.Fatalf("map %v made %d control calls", args, calls)
			}
			if !strings.Contains(stderr.String(), "usage: pvm map") {
				t.Fatalf("map %v did not print usage: %s", args, stderr.String())
			}
		})
	}
}

func TestMapReportsControlErrors(t *testing.T) {
	call := func(string, string, any, any, time.Duration) error {
		return errors.New("host not connected")
	}
	var stdout, stderr bytes.Buffer
	if code := cmdMap([]string{"ls"}, "/test/control.sock", call, &stdout, &stderr); code != 1 {
		t.Fatalf("map returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "host not connected") {
		t.Fatalf("unexpected error output: %s", stderr.String())
	}
}
