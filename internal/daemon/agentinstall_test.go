package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stubCloudInitSSH(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	script := "#!/bin/sh\n" +
		"count=0\n[ ! -f '" + counter + "' ] || count=$(cat '" + counter + "')\n" +
		"count=$((count+1))\nprintf '%s' \"$count\" > '" + counter + "'\n" +
		"for last do :; done\n" + body
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func shortenCloudInitWaits(t *testing.T, idle time.Duration) {
	t.Helper()
	oldIdle, oldSSH, oldPoll := cloudInitIdleTimeout, cloudInitSSHTimeout, cloudInitPollInterval
	cloudInitIdleTimeout = idle
	cloudInitSSHTimeout = 200 * time.Millisecond
	cloudInitPollInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		cloudInitIdleTimeout, cloudInitSSHTimeout, cloudInitPollInterval = oldIdle, oldSSH, oldPoll
	})
}

func TestWaitGuestProvisionedReportsCloudInitError(t *testing.T) {
	shortenCloudInitWaits(t, time.Second)
	stubCloudInitSSH(t, `case "$last" in
  *cloudInitProbeScript*|*"sed -n"*) printf 'error\n120\nCloud-init v. 24.1 running modules-final\n' ;;
  *"status --long"*) printf 'status: error\n--- /var/log/cloud-init-output.log (tail) ---\nE: Unable to locate package foo\n--- failed systemd units ---\ncloud-final.service loaded failed failed\n' ;;
  *) exit 0 ;;
esac
`)
	s, spec, _ := newMountTestServer(t)
	err := s.waitGuestProvisionedCtx(context.Background(), spec, 1)
	var ciErr *cloudInitError
	if !errors.As(err, &ciErr) {
		t.Fatalf("err = %v, want *cloudInitError", err)
	}
	if !strings.Contains(err.Error(), "Unable to locate package foo") || !strings.Contains(err.Error(), "cloud-final.service loaded failed failed") {
		t.Fatalf("missing guest diagnostics in error: %v", err)
	}
}

func TestWaitGuestProvisionedIdleTimeout(t *testing.T) {
	shortenCloudInitWaits(t, 40*time.Millisecond)
	stubCloudInitSSH(t, `case "$last" in
  *"sed -n"*) printf 'running\n512\nWaiting for network\n' ;;
  *"status --long"*) printf 'status: running\nstill waiting\n' ;;
  *) exit 0 ;;
esac
`)
	s, spec, _ := newMountTestServer(t)
	err := s.waitGuestProvisionedCtx(context.Background(), spec, 1)
	var ciErr *cloudInitError
	if !errors.As(err, &ciErr) || !strings.Contains(ciErr.Status, "stalled") {
		t.Fatalf("err = %v, want stalled *cloudInitError", err)
	}
}

func TestWaitGuestProvisionedProgressResetsIdle(t *testing.T) {
	shortenCloudInitWaits(t, 35*time.Millisecond)
	stubCloudInitSSH(t, `case "$last" in
  *"sed -n"*)
    if [ "$count" -lt 6 ]; then
      printf 'running\n%s00\napt step %s\n' "$count" "$count"
    else
      printf 'done\n900\nfinished\n'
    fi ;;
  *) exit 0 ;;
esac
`)
	s, spec, _ := newMountTestServer(t)
	start := time.Now()
	if err := s.waitGuestProvisionedCtx(context.Background(), spec, 1); err != nil {
		t.Fatalf("progressing cloud-init failed: %v", err)
	}
	if time.Since(start) <= cloudInitIdleTimeout {
		t.Fatalf("test did not exceed idle timeout (%s)", time.Since(start))
	}
}

func TestWaitGuestProvisionedToleratesTransientSSHDrop(t *testing.T) {
	shortenCloudInitWaits(t, time.Second)
	stubCloudInitSSH(t, `case "$last" in
  *"sed -n"*)
    if [ "$count" -lt 3 ]; then
      echo 'Connection reset by peer' >&2
      exit 255
    fi
    printf 'done\n100\nfinished\n' ;;
  *) exit 0 ;;
esac
`)
	s, spec, _ := newMountTestServer(t)
	if err := s.waitGuestProvisionedCtx(context.Background(), spec, 1); err != nil {
		t.Fatalf("transient ssh drop should be retried: %v", err)
	}
}
