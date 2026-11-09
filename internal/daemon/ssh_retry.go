package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func retryTransientSSH(ctx context.Context, op func() error) error {
	for {
		err := op()
		if err == nil {
			return nil
		}
		if !transientSSHError(err) {
			return err
		}
		if waitErr := waitInstallRetry(ctx, 3*time.Second); waitErr != nil {
			return fmt.Errorf("%w (last SSH error: %v)", waitErr, err)
		}
	}
}

func transientSSHError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, fragment := range []string{"Connection timed out", "banner exchange", "Connection reset", "Connection refused", "kex_exchange_identification"} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}
