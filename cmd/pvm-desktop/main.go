//go:build windows

// pvm-desktop is the graphical app: a WebView2 window over
// internal/desktop.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	"github.com/wailsapp/wails/v2/pkg/runtime"
	syswin "golang.org/x/sys/windows"

	"github.com/huangzheng2016/pvm/internal/desktop"
	"github.com/huangzheng2016/pvm/internal/paths"
)

// quiet drops the toolkit's log. On a failed call Wails logs the whole
// call message, and the arguments of "add exit" hold the token.
type quiet struct{}

func (quiet) Print(string)   {}
func (quiet) Trace(string)   {}
func (quiet) Debug(string)   {}
func (quiet) Info(string)    {}
func (quiet) Warning(string) {}
func (quiet) Error(string)   {}
func (quiet) Fatal(string)   { os.Exit(1) }

var _ logger.Logger = quiet{}

func main() {
	if err := run(); err != nil {
		// a windowed program has no console to print to
		text, _ := syswin.UTF16PtrFromString(err.Error())
		title, _ := syswin.UTF16PtrFromString("pvm Desktop")
		syswin.MessageBox(0, text, title, syswin.MB_ICONERROR)
		os.Exit(1)
	}
}

func run() error {
	p, err := paths.Default()
	if err == nil {
		err = p.Ensure()
	}
	if err != nil {
		return fmt.Errorf("pvm data folder: %w", err)
	}

	// the window's context exists only once it has started
	var window atomic.Pointer[context.Context]
	app := desktop.New(p, desktop.Options{
		Emit: func(event string, data any) {
			if ctx := window.Load(); ctx != nil {
				runtime.EventsEmit(*ctx, event, data)
			}
		},
		PickFolder: func() (string, error) {
			ctx := window.Load()
			if ctx == nil {
				return "", nil
			}
			return runtime.OpenDirectoryDialog(*ctx, runtime.OpenDialogOptions{Title: "Choose a folder"})
		},
	})

	messages := windows.DefaultMessages()
	messages.DownloadPage = "pvm Desktop needs the Microsoft Edge WebView2 Runtime, which is not installed. Press OK to open Microsoft's download page. Minimum version: "
	// what wails.Run returns after that dialog; main shows it
	messages.FailedToInstall = "Install the WebView2 Runtime, then open pvm Desktop again."
	return wails.Run(&options.App{
		Title:       "pvm Desktop",
		Width:       1100,
		Height:      720,
		MinWidth:    800,
		MinHeight:   540,
		AssetServer: &assetserver.Options{Assets: assets},
		Logger:      quiet{},
		// the single-instance lock is held by now, so no other app is
		// in the middle of `pvm exit add`
		OnStartup: func(ctx context.Context) {
			window.Store(&ctx)
			desktop.RemoveTokenFiles(p)
		},
		// Closing ends the app with the command of the current step still
		// running and the steps after it never run, so the user decides.
		OnBeforeClose: func(ctx context.Context) bool {
			tasks := desktop.RunningTasks(app)
			if len(tasks) == 0 {
				return false
			}
			answer, err := runtime.MessageDialog(ctx, runtime.MessageDialogOptions{
				Type:  runtime.QuestionDialog,
				Title: "pvm Desktop is still working",
				Message: "Still running: " + strings.Join(tasks, ", ") + ".\n\n" +
					"If you close now, the pvm command that is running finishes on its own, the steps after it are not run, and nothing reports the result.\n\nClose anyway?",
				DefaultButton: "No",
			})
			return err == nil && answer != "Yes"
		},
		OnShutdown: func(context.Context) { desktop.RemoveTokenFiles(p) },
		Bind:       []any{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "pvm-desktop",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) {
				if ctx := window.Load(); ctx != nil {
					runtime.WindowUnminimise(*ctx)
					runtime.WindowShow(*ctx)
				}
			},
		},
		Windows: &windows.Options{
			Messages:            messages,
			WebviewUserDataPath: filepath.Join(p.Home, "desktop", "webview"),
		},
	})
}
