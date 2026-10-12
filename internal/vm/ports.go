package vm

import (
	"net"
	"strconv"
)

const SSHPortBase = 10022

func AllocSSHPort(used map[int]bool, base int) (int, error) {
	for p := base; p < base+2000; p++ {
		if used[p] {
			continue
		}
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			continue // bound by something else on the host
		}
		l.Close()
		return p, nil
	}
	return 0, errNoPort
}

type errT string

func (e errT) Error() string { return string(e) }

const errNoPort = errT("no free host port available")
