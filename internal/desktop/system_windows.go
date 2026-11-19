//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

const (
	runKey      = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue    = "pvm-desktop"
	daemonTask  = "pvm-daemon" // the task `pvm daemon install` creates
	windowsTerm = "wt.exe"
)

func hostSystem() system {
	return system{
		openTerminal:  openTerminal,
		openFolder:    openFolder,
		appAtLogin:    appAtLogin,
		setAppAtLogin: setAppAtLogin,
		daemonAtLogin: daemonAtLogin,
	}
}

// spawn starts a process that outlives the app. It calls CreateProcess
// itself because os/exec always hands the child its own standard
// handles: a console program started that way would see the null device
// instead of its new console, and `pvm ssh` would not be interactive.
func spawn(dir string, argv []string, flags uint32) error {
	return spawnLine(dir, windows.ComposeCommandLine(argv), flags)
}

// spawnLine takes the command line as the program will read it.
func spawnLine(dir, commandLine string, flags uint32) error {
	line, err := windows.UTF16PtrFromString(commandLine)
	if err != nil {
		return err
	}
	var cwd *uint16
	if dir != "" {
		if cwd, err = windows.UTF16PtrFromString(dir); err != nil {
			return err
		}
	}
	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation
	// the program is a full path, so nothing is searched for
	if err := windows.CreateProcess(nil, line, nil, nil, false, flags, nil, cwd, &si, &pi); err != nil {
		return fmt.Errorf("start %s: %w", commandLine, err)
	}
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(pi.Process)
	return nil
}

// openTerminal prefers Windows Terminal and falls back to a console
// window of the command itself.
func openTerminal(dir string, args []string) error {
	pvm, err := pvmPath()
	if err != nil {
		return err
	}
	argv := append([]string{pvm}, args...)
	terminal, err := exec.LookPath(windowsTerm)
	if err != nil {
		return spawn(dir, argv, windows.CREATE_NEW_CONSOLE)
	}
	return spawn("", terminalArgv(terminal, dir, argv), 0)
}

func openFolder(path string) error {
	if fi, err := os.Stat(path); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%s is not a folder", path)
	}
	// by its full path: a bare name is looked up in the app's current
	// directory before the Windows directory
	windir, err := windows.GetWindowsDirectory()
	if err != nil {
		return err
	}
	return spawnLine("", explorerLine(filepath.Join(windir, "explorer.exe"), path), 0)
}

func appAtLogin() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false, err
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValue)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func setAppAtLogin(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(runValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	return k.SetStringValue(runValue, `"`+exe+`"`)
}

func daemonAtLogin() (bool, error) {
	cmd := exec.Command("schtasks", "/Query", "/TN", daemonTask)
	fsutil.ConfigureHiddenCmd(cmd)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return err == nil, err
}
