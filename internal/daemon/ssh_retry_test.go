package daemon

import (
	"context"
	"errors"
	"testing"
)

func TestRetryTransientSSH(t *testing.T) {
	calls := 0
	err := retryTransientSSH(context.Background(), func() error {
		calls++
		if calls == 1 {
			return errors.New("exit status 255: Connection timed out during banner exchange")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("retry: calls=%d, err=%v", calls, err)
	}
	permanent := errors.New("mount: unknown filesystem")
	calls = 0
	if err := retryTransientSSH(context.Background(), func() error { calls++; return permanent }); !errors.Is(err, permanent) || calls != 1 {
		t.Fatalf("permanent error retried: calls=%d, err=%v", calls, err)
	}
}
