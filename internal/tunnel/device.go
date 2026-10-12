package tunnel

import (
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

func BindFor(transport string) conn.Bind {
	if transport == "tcp" {
		return NewTCPBind()
	}
	return conn.NewStdNetBind()
}

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
