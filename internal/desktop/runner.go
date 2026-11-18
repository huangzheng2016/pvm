package desktop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

// command is one pvm invocation.
type command struct {
	argv []string
	dir  string
}

// runner runs pvm and reports each output line. The error is for a
// command that could not run at all; a command that ran reports its exit
// code.
type runner interface {
	run(c command, line func(string)) (int, error)
}

// pvmPath is the CLI installed next to the app.
func pvmPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	name := "pvm"
	if runtime.GOOS == "windows" {
		name = "pvm.exe"
	}
	path := filepath.Join(filepath.Dir(exe), name)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%s is not next to PVM Desktop (%s); run the pvm installer again", name, filepath.Dir(exe))
	}
	return path, nil
}

type pvmRunner struct{ find func() (string, error) }

// run passes argv as an array, never through a shell, and gives the
// child no console and no stdin, so the CLI asks nothing.
func (r pvmRunner) run(c command, line func(string)) (int, error) {
	path, err := r.find()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(path, c.argv...)
	cmd.Dir = c.dir
	fsutil.ConfigureHiddenCmd(cmd)
	stdout, stderr := &lineWriter{line: line}, &lineWriter{line: line}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// a helper the CLI leaves running must not hold the step open through
	// an inherited pipe
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	stdout.flush()
	stderr.flush()
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode(), nil
	}
	return 0, err
}

// lineWriter splits output at newlines and at the carriage returns that
// progress output uses to redraw a line.
type lineWriter struct {
	line func(string)
	buf  []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	for _, c := range p {
		if c == '\n' || c == '\r' {
			w.flush()
			continue
		}
		w.buf = append(w.buf, c)
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.line(string(w.buf))
		w.buf = w.buf[:0]
	}
}

// commandLine renders pvm arguments for display and copying. An
// argument with a character that cmd.exe or PowerShell acts on is
// quoted by the Windows argument rules; inside the quotes cmd.exe still
// expands %NAME% and PowerShell $name.
//
// No quoting of a double quote is right for both shells: cmd.exe does
// not read the backslash and takes the quote as the end of the quoted
// part. The app therefore takes no double quote in what a person types
// (appTarget), and a Windows path cannot hold one.
func commandLine(argv ...string) string {
	parts := []string{"pvm"}
	for _, arg := range argv {
		if arg == "" || strings.ContainsAny(arg, " \t\"&|;<>^%()'{}@#,") {
			arg = quoteArg(arg)
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}

// quoteArg doubles the backslashes before a double quote and before the
// closing quote, which would otherwise be taken as a literal one.
func quoteArg(arg string) string {
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(arg); i++ {
		switch arg[i] {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteByte(arg[i])
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}
