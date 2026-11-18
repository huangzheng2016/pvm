package desktop

import "strings"

// terminalArgv is the Windows Terminal command line that runs argv in
// dir, when it is set. Windows Terminal splits its arguments at every
// ";" without a backslash before it, also inside a quoted one, so a
// folder or an argument with a ";" would start another command.
func terminalArgv(terminal, dir string, argv []string) []string {
	wt := []string{terminal}
	if dir != "" {
		wt = append(wt, "-d", dir)
	}
	wt = append(wt, argv...)
	for i := 1; i < len(wt); i++ {
		wt[i] = strings.ReplaceAll(wt[i], ";", `\;`)
	}
	return wt
}

// explorerLine is the command line that opens path in Explorer. Explorer
// splits its own command line at "," and "=", as in /select,path, unless
// the path is quoted; a Windows path holds no double quote.
func explorerLine(explorer, path string) string {
	return `"` + explorer + `" "` + path + `"`
}

// system is what the app asks of the desktop itself, apart from pvm.
type system struct {
	// openTerminal runs pvm with args in a new terminal window, in dir
	// when it is set.
	openTerminal func(dir string, args []string) error
	openFolder   func(path string) error
	// appAtLogin is the "pvm-desktop" value of the user's Run key.
	appAtLogin    func() (bool, error)
	setAppAtLogin func(on bool) error
	// daemonAtLogin is whether `pvm daemon install` registered its task.
	daemonAtLogin func() (bool, error)
}
