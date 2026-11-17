# pvm Desktop, a graphical app

Status: design, not implemented. Windows hosts with the native (WHPX) backend first.

## Motivation

Everything pvm does is a CLI command today. Creating a VM with the right size, exit, app settings and programs takes several commands and some reading of SKILL.md, and finding a VM's state means `pvm ls` and `pvm logs`. A window in the spirit of Docker Desktop makes the common paths a few clicks: see the VMs and their state, create one with a form, pick exits, and open a terminal, an app or the file manager in a workspace folder.

The app is another client of the same daemon and the same CLI. It adds no behaviour that the CLI lacks; what it needs from the core is added to the CLI and daemon first, so both stay consistent.

## Decisions

- **Toolkit: Wails with WebView2.** A Go backend reuses `internal/daemon.Client` and the store packages; the screens are HTML and TypeScript. On Windows, Wails needs no cgo, so the CI's Linux job can build it with `CGO_ENABLED=0`, like `pvm.exe`. It ships as a separate `pvm-desktop.exe`.
- **Packaging: the existing Windows installers.** The `_setup.exe` and `_setup.msi` install `pvm-desktop.exe` next to `pvm.exe`, with a Start menu shortcut.
- **Backend: native only.** With the WSL2 backend the daemon runs inside WSL, and Windows has no address or token for it. When `cli.ResolveWindowsBackend` (`internal/cli/wsl.go`) returns `wsl`, the app shows a page explaining that and pointing at the CLI.
- **App install scripts are built into pvm**, one folder per app, and usable from the CLI too (`pvm app install`).
- **One default exit**, the existing cluster default (`pvm route default --via`). An ordered list with failover is a separate roadmap item, since failing over must keep the kill-switch fail-closed.
- **A stored default VM** in `config.yaml`, used by the CLI and the app alike.

## Scope

M1, the first release of the app:

- **VMs.** List, state, start, stop, delete, open a terminal, open an app, open the file manager.
- **Create.** A form for a new VM, including the programs to preinstall.
- **Exits and routes.** List, add, remove and test exits; set the default exit; change a VM's exit.
- **Workspaces.** List and create; open the host folder; open a terminal, an app or the file manager in the matching guest folder.
- **Settings.** The default VM, starting the daemon at login and opening the app at login.

Not in M1:

- A tray icon.
- The WSL2 backend.
- macOS and Linux.
- Exit lists with failover.
- Screens for snapshots, port maps, mounts and logs.
- A push or event API; the app polls.

## Architecture

```
pvm-desktop.exe
  frontend (TypeScript, WebView2) --Wails IPC--> backend (Go)
                                                  |-- reads:  daemon HTTP API (internal/daemon.Client)
                                                  |           store files (workspace, exit, config)
                                                  `-- writes: pvm.exe child processes
```

### Reading state

- **Daemon.** The backend uses `daemon.NewClient` (`internal/daemon/client.go`).
  - On Windows it already finds the daemon through `run/daemon.addr` and authenticates with `run/admin.token`.
  - `Healthy` reports whether the daemon is up.
  - `Get("/v1/status")` gives the version, accelerator and TCG fallback.
  - `Get("/v1/vms")` gives the VMs; see the core changes below for readiness.
  - `Get("/v1/clusters")` gives the default exit and each VM's exit.
- **Files.** Some state has no daemon route and is read from the store packages:
  - Workspaces from `internal/workspace`'s `Store.List`.
  - Exits from `internal/exit`'s store. Only the name, kind (WireGuard or WARP), endpoint, TZ and locale reach the frontend. Tokens and private keys never leave the backend.
  - Settings from `internal/config`.
- **Polling.** The app polls `/v1/vms` every 2 seconds while a window is visible, and every 10 seconds when minimized. Workspaces and exits are re-read after the app changes them and when the window gains focus.

### Changing state

Every change runs `pvm.exe` from the app's own folder as a child process:

- **How it runs.** The argv is passed as an array, never through a shell. Names (VM, exit, workspace, app) are checked against the CLI's own rules before they are passed.
- **Output.** Standard output and standard error stream into a progress panel, and the exit code decides success.
- **Prompts.** Without a terminal the CLI asks nothing. Commands that would ask get `--yes`, after the app has asked the user itself.

The reason is that several operations exist only in the CLI process:

- Fetching and baking the golden image on create (`CmdCreate`, `internal/cli/vm.go`).
- Adding, removing and testing exits (`internal/cli/exit.go`).
- Creating workspaces (`internal/cli/workspace.go`).

Calling the CLI keeps one implementation of each. Moving these into the daemon is possible later and is not needed for M1.

### Daemon start

When the daemon is not healthy at start, the app runs `pvm daemon start` and shows its output.

If the `run/daemon.stopped` mark exists, the user stopped the daemon on purpose. The app then shows a "Start the daemon" button instead of starting it.

### Security

- **No extra listener.** In production mode Wails serves the frontend from memory over its own IPC and opens no network port.
- **Same rights as the CLI.** The app runs as the user, with the same rights as the CLI, and holds the admin token only in the backend.
- **Never interpreted.** Nothing from VMs or the API is rendered as HTML; text goes through `textContent`.

## Screens

### VMs

The VM list shows, per VM:

- The name.
- The state:
  - Stopped.
  - Starting, with the current stage: `boot`, `provision`, `ssh` or `agent`.
  - Ready.
  - Failed, with the stage's hint.
- CPUs and memory.
- The exit.
- Whether it is the default VM.

Gateway VMs (`role: gateway`) are hidden.

Actions on a VM:

- **Start** runs `pvm start <vm>`, then follows the stages until ready or failed.
- **Stop** runs `pvm stop <vm>`.
- **Delete** asks first, naming the VM, then runs `pvm rm <vm> --yes`.
- **Terminal** runs `pvm ssh <vm>` in Windows Terminal (`wt.exe`) when installed, else in a new console window.
- **App** asks for a command line or a URL and runs `pvm app <vm> -- <cmd...>` or `pvm app <vm> <url>`. It offers the VM's installed catalog apps as one-click entries.
- **Files** runs `pvm app <vm> -- thunar`.
- **App settings** sets clipboard, drop and audio with `pvm app config <vm> ...`.
- **Set as default** runs `pvm default-vm <vm>`.

### Create

The form asks for:

- The name, checked like the CLI does.
- The image, from `pvm image list`.
- CPUs, memory and disk, with defaults from `config.yaml`.
- The exit: `direct`, or one of the exits.
- Clipboard, drop and audio.
- The programs to preinstall, from the app catalog.

Create runs these steps in order, each with its output in the progress panel:

1. `pvm create --name <vm> ... --no-wait`. When the golden image is missing, its fetch and bake output shows here; it takes minutes the first time.
2. `pvm route <vm> --via <exit>`, when the exit is not the default. The implementer checks whether this can happen before the first boot without a restart, and if not, sets it right after create.
3. `pvm app config <vm> --clipboard ... --drop ... --audio ...`.
4. The app follows the stages until the VM is ready.
5. `pvm app install <vm> <id>...`, when programs were picked.

When a step fails, the app stops there and shows the error. It also shows the remaining steps as commands that can be copied and run by hand. It does not delete the VM by itself.

### Exits

The exit list shows the name, kind, endpoint and the VMs using each exit. The default exit is marked.

- **Add** runs `pvm exit add` with the server address and token, or `pvm exit add-warp`. The token field is a password field and is passed to the child process without being logged.
- **Test** runs `pvm exit test <exit>`; the speed test is an option.
- **Remove** warns when a VM or route rule still uses the exit, then runs `pvm exit rm`.
- **Default** runs `pvm route default --via <exit>`.
- **Per VM** runs `pvm route <vm> --via <exit|direct>`, from the VM list or here. The app warns that a running VM restarts.

### Workspaces

The workspace list shows the id, VM, mode, host folder and last transfer.

- **Open folder** opens the host folder in Explorer.
- **Terminal** runs `pvm ssh` with the host folder as its working directory, so it starts in the matching guest folder.
- **App** runs `pvm app` from the host folder; this needs the app working directory change below.
- **Files** runs `pvm app <vm> -- thunar <guest folder>`.
- **Sync** runs `pvm workspace push` and `pull` for `sync` and `copy` workspaces.
- **Create** asks for the id, VM, mode, host folder (a folder picker), read-only and transport, then runs `pvm workspace create`.
- **Remove** runs `pvm workspace rm`. It removes the record only; the files are not touched.

### Settings

- **Default VM** runs `pvm default-vm <vm>`, or `pvm default-vm --clear`.
- **Start the daemon at login** runs `pvm daemon install` or `pvm daemon uninstall`, which manage the existing `pvm-daemon` scheduled task.
- **Open pvm Desktop at login** writes or removes `HKCU\Software\Microsoft\Windows\CurrentVersion\Run\pvm-desktop`.
- **About** shows the pvm version, daemon version, accelerator and TCG fallback warning, and the data folder.

## Core changes first (M0)

These land in the CLI and daemon before the app. Each is its own commit, with tests and the docs it touches: SKILL.md, both READMEs and the usage text.

### Default VM

- **Setting.** `config.yaml` gets `default_vm` in `internal/config`; unknown keys are already rejected, so the field is declared.
- **Resolve order.** `resolveVM` (`internal/cli/defaultvm.go`) uses:
  1. The explicit name.
  2. The workspace of the current directory.
  3. `default_vm`.
  4. The only user VM.
- **A deleted default VM** is an error that names it and the command to clear it. It does not fall back to the only VM.
- **Command.** `pvm default-vm [<vm>|--clear]` prints, sets or clears the setting.
- **Removal.** `pvm rm` of the default VM clears the setting and says so.

### Apps start in the workspace folder

- **Request.** `appStartRequest` (`internal/daemon/app.go`) gets `cwd`, an absolute guest path.
- **Launch.** `appLaunchScript` passes `--working-directory` to `systemd-run`.
- **CLI.** `cmdAppStart` (`internal/cli/app.go`) passes the folder `resolveVM` returns, the same rule as `pvm ssh`.
- **Missing folder.** When the folder is missing in the VM, the app starts in the workspace root and says so.

### Readiness in the VM list

`GET /v1/vms` adds `ready` and the last stage (name and hint) to each entry, from the same data `GET /v1/vms/{name}` uses. Then the app and `pvm ls` need one request, not one per VM.

### App catalog

- **Files.** One folder per app under `internal/apps/<id>/`, embedded at build time:
  - `app.yaml`: the name, a description, the vendor's download page, whether the build is official or community, whether it runs as a window, and the command to start it.
  - `install.sh`: run as root in the VM; it must be idempotent.
- **Daemon routes.**
  - `GET /v1/apps/catalog` lists the catalog.
  - `POST /v1/vms/{name}/apps/install` takes a list of ids and streams ndjson output, like `apps/prepare`. It runs each script through the agent with a time limit, and records the installed ids in the VM's `app.json`.
  - A window app also runs the Xpra preparation first.
- **CLI commands.**
  - `pvm app catalog`.
  - `pvm app install [<vm>] <id>...`.
  - `pvm app ls` shows the installed catalog apps.
- **M1 catalog:** Claude Code CLI, Codex CLI and Claude Desktop.
  - The implementer checks the current official Linux install method for each.
  - A program without an official Linux build, such as ChatGPT Desktop at the time of writing, is left out unless a maintained community build exists. When one is used, it is marked as community in `app.yaml` and in the app.
- **Egress.** Downloads run in the VM and leave through the VM's exit, never through the host.

## Build and release

- **Source.**
  - The app lives in `cmd/pvm-desktop` (Go) with the frontend in `cmd/pvm-desktop/frontend` (TypeScript and Vite, no large framework).
  - Wails is pinned in `go.mod` and the frontend dependencies in a lockfile. Choose the latest stable versions released at least two weeks before.
  - The built frontend is not committed.
- **CI.** In the `native` job for `pvm_linux_amd64`:
  - Set up Node at a pinned version and build the frontend.
  - Build `pvm-desktop.exe` with `CGO_ENABLED=0 GOOS=windows GOARCH=amd64`, the Wails production build tags and `-ldflags "-H windowsgui"`.
  - Upload it as a raw artifact next to `pvm.exe`; `script/ci/reuse.py` lists it as required.
  - The implementer confirms the Wails tags and asset embedding for a plain `go build`, rather than the `wails` CLI.
- **Installers.** `script/windows/pack-setup.sh` adds `pvm-desktop.exe` to the NSIS and MSI packages:
  - A Start menu shortcut.
  - An optional "Open pvm Desktop" on the finish page.
  - Uninstall removes the shortcut and the `Run` value.
- **WebView2.** Windows 10 and 11 normally have it. When it is missing, the app shows a message with Microsoft's download page; the installers do not install it silently.
- **The `windows` CI job** runs the app's backend tests.

## Testing

- **Backend unit tests** run against a fake daemon (`httptest`) and a fake `pvm` runner. They cover:
  - Mapping stages to states.
  - The argv for every action, and the name checks.
  - Create stopping at the failed step.
  - That tokens and keys never reach the frontend.
- **Core change tests:**
  - The resolve order with `default_vm`, including a deleted default VM.
  - The app working directory.
  - Catalog parsing and the install stream.
- **End to end on a native WHPX host:**
  - Install with both installers and open the app from the Start menu.
  - Create a VM with Claude Code preinstalled, from an empty data folder (golden bake) and again with the golden present.
  - Start, stop and delete; a failed provision shows its hint.
  - Terminal, app and file manager from the VM list and from a workspace, landing in the right guest folder.
  - Add, test and remove an exit; set the default; switch a running VM's exit and see it restart.
  - The default VM takes effect in `pvm ssh` from a folder outside workspaces.
  - Both login options survive a sign-out.
  - With the WSL2 backend, the explanation page shows.
  - Uninstalling leaves no shortcut, `Run` value or files behind; the data folder stays.

## Milestones

1. **M0:** the core changes above, each released with the CLI.
2. **M1:** the app, the installers and the CI build.
3. **Later:**
   - A tray icon with VM state.
   - A daemon event stream in place of polling.
   - Screens for snapshots, port maps, mounts and logs.
   - The WSL2 backend through a bridge to the daemon in WSL.
   - Exit lists with failover.
   - macOS and Linux builds.
