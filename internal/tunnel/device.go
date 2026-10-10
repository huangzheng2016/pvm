// Package tunnel wires WireGuard devices over UDP or TCP transports.
package tunnel

import (
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

// BindFor returns the bind for a transport name ("udp" or "tcp").
func BindFor(transport string) conn.Bind {
	if transport == "tcp" {
		return NewTCPBind()
	}
	return conn.NewStdNetBind()
}

// NewDevice creates a WireGuard device and brings it up.
func NewDevice(t tun.Device, bind conn.Bind, verbose bool) (*device.Device, error) {
	level := device.LogLevelError
	if verbose {
		level = device.LogLevelVerbose
	}
	logger := device.NewLogger(level, "pvm-wg: ")
	dev := device.NewDevice(t, bind, logger)
	if err := dev.Up(); err != nil {
		return nil, err
	}
	return dev, nil
}
