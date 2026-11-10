//go:build !windows

package qemu

import (
	"context"
	"fmt"
)

func migrationOutgoing(ramPath string, _ int) (string, <-chan error, func(), func(), error) {
	exe, err := selfExe()
	if err != nil {
		return "", nil, nil, nil, err
	}
	return fmt.Sprintf("exec:%s __snapshot-pipe-out %s", shellQuote(exe), shellQuote(ramPath)), nil, nil, func() {}, nil
}

func migrationRestore(ramPath string) (string, func(context.Context) error, error) {
	exe, err := selfExe()
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("exec:%s __snapshot-pipe-in %s", shellQuote(exe), shellQuote(ramPath)), nil, nil
}
