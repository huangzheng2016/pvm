//go:build !windows

package desktop

import "errors"

var errWindowsOnly = errors.New("PVM Desktop runs on Windows only")

func hostSystem() system {
	return system{
		openTerminal:  func(string, []string) error { return errWindowsOnly },
		openFolder:    func(string) error { return errWindowsOnly },
		appAtLogin:    func() (bool, error) { return false, errWindowsOnly },
		setAppAtLogin: func(bool) error { return errWindowsOnly },
		daemonAtLogin: func() (bool, error) { return false, errWindowsOnly },
	}
}
