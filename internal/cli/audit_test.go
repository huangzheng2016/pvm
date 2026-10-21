package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintAuditFailExitsNonZero(t *testing.T) {
	var buf bytes.Buffer
	code := printAudit(&buf, []auditCheck{
		{Name: "vm", Status: "ok", Detail: "running (pid 42, uptime 1m0s)"},
		{Name: "route", Status: "ok", Detail: "via my-exit (per-vm)"},
		{Name: "tcp", Status: "ok", Detail: "/ping via pvm.probe:18081"},
		{Name: "udp", Status: "fail", Detail: "no valid echo: exit status 1"},
		{Name: "icmp", Status: "ok", Detail: "echo reply via pvm.probe"},
		{Name: "dns", Status: "ok", Detail: "pvm.probe -> 203.0.113.5 (real-ip)"},
		{Name: "ipv6", Status: "warn", Detail: "enabled"},
		{Name: "timezone", Status: "skipped", Detail: "no tz in spec"},
		{Name: "locale", Status: "skipped", Detail: "no locale in spec"},
	})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 when any check fails", code)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("got %d lines, want 9: %q", len(lines), buf.String())
	}
	want := []string{
		"vm=ok: running (pid 42, uptime 1m0s)",
		"route=ok: via my-exit (per-vm)",
		"tcp=ok: /ping via pvm.probe:18081",
		"udp=fail: no valid echo: exit status 1",
		"icmp=ok: echo reply via pvm.probe",
		"dns=ok: pvm.probe -> 203.0.113.5 (real-ip)",
		"ipv6=warn: enabled",
		"timezone=skipped: no tz in spec",
		"locale=skipped: no locale in spec",
	}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("line %d = %q, want %q", i, lines[i], w)
		}
	}
}

func TestPrintAuditWarnAndSkippedExitZero(t *testing.T) {
	var buf bytes.Buffer
	code := printAudit(&buf, []auditCheck{
		{Name: "vm", Status: "warn", Detail: "stopped"},
		{Name: "route", Status: "warn", Detail: "via gone-exit (per-vm) is not registered"},
		{Name: "udp", Status: "skipped", Detail: "vm not running"},
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for warn/skipped", code)
	}
}

func TestPrintAuditEmpty(t *testing.T) {
	var buf bytes.Buffer
	if code := printAudit(&buf, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected output %q", buf.String())
	}
}
