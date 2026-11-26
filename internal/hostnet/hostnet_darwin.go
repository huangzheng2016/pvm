//go:build darwin

package hostnet

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
)

const ipBoundIF = 25 // IP_BOUND_IF on macOS

func parseHardwarePorts(out string) map[string]bool {
	devs := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if dev, ok := strings.CutPrefix(strings.TrimSpace(line), "Device: "); ok {
			dev = strings.TrimSpace(dev)
			if !IsVirtual(dev) {
				devs[dev] = true
			}
		}
	}
	return devs
}

var scutilIfaceRE = regexp.MustCompile(`^\s*([A-Za-z0-9_.-]+)\s*:\s*flags.*IPv4`)

func parseScutilIPv4Order(out string) []string {
	var order []string
	inV4 := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "IPv4 network interface information") {
			inV4 = true
			continue
		}
		if strings.Contains(line, "IPv6 network interface information") || strings.HasPrefix(line, "Network interfaces:") {
			break
		}
		if !inV4 {
			continue
		}
		if m := scutilIfaceRE.FindStringSubmatch(line); m != nil {
			order = append(order, m[1])
		}
	}
	return order
}

func selectPhysical() (*Interface, error) {
	hwOut, err := exec.Command("/usr/sbin/networksetup", "-listallhardwareports").Output()
	if err != nil {
		return nil, err
	}
	nwiOut, err := exec.Command("/usr/sbin/scutil", "--nwi").Output()
	if err != nil {
		return nil, err
	}
	hw := parseHardwarePorts(string(hwOut))
	for _, name := range parseScutilIPv4Order(string(nwiOut)) {
		if hw[name] && !IsVirtual(name) {
			return &Interface{Name: name}, nil
		}
	}
	return nil, fmt.Errorf("no reachable physical IPv4 interface found")
}

func bindControl(name string, index int) func(string, string, syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		if skipBindForAddress(address) {
			return nil
		}
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipBoundIF, index)
		}); err != nil {
			return err
		}
		return serr
	}
}
