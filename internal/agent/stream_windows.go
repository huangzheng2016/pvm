package agent

import (
	"fmt"
	"io"
)

const xpraSocket = "pvm/xpra.sock"

func dialUserSocket(string, int, string) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("user sockets are only served in linux guests")
}

func clipboardSink(int, string) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("the clipboard is only served in linux guests")
}
