# pvm Desktop, a graphical app

Status: M0 and M1 are implemented for Windows hosts with the native (WHPX) backend. The app has not been run on Windows yet: so far there are backend unit tests on Linux, a cross-build of `pvm-desktop.exe` and compile checks of both installers. The end-to-end list under [Testing](#testing) is still open.

## Motivation

Everything pvm does is a CLI command today. Creating a VM with the right size, exit, app settings and programs takes several commands and some reading of SKILL.md, and finding a VM's state means `pvm ls` and `pvm logs`. A window in the spirit of Docker Desktop makes the common paths a few clicks: see the VMs and their state, create one with a form, pick exits, and open a terminal, an app or the file manager in a workspace folder.

The app is another client of the same daemon and the same CLI. It adds no behaviour that the CLI lacks; what it needs from the core is added to the CLI and daemon first, so both stay consistent.

## Decisions

- **Toolkit: Wails with WebView2.** A Go backend reuses `internal/daemon.Client` and the store packages; the screens are HTML and TypeScript. On Windows, Wails needs no cgo, so the CI's Linux job can build it with `CGO_ENABLED=0`, like `pvm.exe`. It ships as a separate `pvm-desktop.exe`. The backend is the package `internal/desktop`, which does not import Wails; `cmd/pvm-desktop` holds the window and the frontend.
- **Packaging: the existing Windows installers.** The `_setup.exe` and `_setup.msi` install `pvm-desktop.exe` next to `pvm.exe`, with a Start menu shortcut. `pvm upgrade` replaces only `pvm.exe`, so the app can be older than the CLI until the installer runs again.
- **Backend: native only.** With the WSL2 backend the daemon runs inside WSL, and Windows has no address or token for it. When `cli.ResolveWindowsBackend` (`internal/cli/wsl.go`) returns `wsl`, the app shows a page explaining that and pointing at the CLI, and the backend refuses every other call.
- **App install scripts are built into pvm**, one folder per app, and usable from the CLI too (`pvm app install`).
- **One default exit**, the existing cluster default (`pvm route default --via`). An ordered list with failover is a separate roadmap item, since failing over must keep the kill-switch fail-closed.
- **A stored default VM** in `config.yaml`, used by the CLI and the app alike.

## Scope

M1, the first release of the app:

- **VMs.** List, state, start, stop, delete, open a terminal, open an app, open the file manager, install catalog programs.
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
  - `Get("/v1/status")` gives the version, accelerator and TCG fallback; `tcg_fallback` is a count.
  - `Get("/v1/vms")` gives the VMs; see the core changes below for readiness.
  - `Get("/v1/clusters")` gives the default exit and each VM's exit.
- **Files.** Some state has no daemon route and is read from the store packages:
  - Workspaces from `internal/workspace`'s `Store.List`.
  - Exits from `internal/exit`'s store. Only the name, kind (`agent` for a WireGuard exit agent, or `warp`), endpoint, TZ and locale reach the frontend. Tokens and private keys never leave the backend.
  - Images from `image.Supported()`, each marked when its golden image is cached. `pvm image list` prints file names, not the specs that `pvm create --image` takes.
  - Settings from `internal/config`.
- **Polling.** The frontend drives it: it asks the backend for the VMs every 2 seconds while the window is visible, and every 10 seconds when minimized. Workspaces and exits are re-read after the app changes them and when the window gains focus. A VM's installed catalog apps are read when its app menu opens, as that request asks the guest.

### Changing state

Every change runs `pvm.exe` from the app's own folder as a child process:

- **How it runs.** The argv is passed as an array, never through a shell. Names (VM, exit, workspace, app) are checked against the CLI's own rules before they are passed. A VM whose name the CLI would read as a subcommand of `route` or `app` is refused for those commands.
- **Secrets.** An exit token never goes on the argv. The backend writes it to a private file under the pvm run folder, passes `--token-file`, and deletes the file afterwards, also on failure. The CLI reads the file as it starts, so a file left by an app that was killed during the command is removed when the app next starts or closes. Output lines that contain the token are masked.
- **Output.** Standard output and standard error stream into a progress panel, and the exit code decides success.
- **Prompts.** Without a terminal the CLI asks nothing. Commands that would ask get `-y`, after the app has asked the user itself.
- **Closing the window.** While an action runs, closing asks first and names what is running: the command of the current step would finish on its own, and the steps after it would never run.

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
- **No toolkit log.** Wails would log the whole message of a failed call, which for adding an exit holds the token, so its log is discarded.
- **Never interpreted.** Nothing from VMs or the API is rendered as HTML; text goes through `textContent`.

## Screens

### VMs

The VM list shows, per VM:

- The name.
- The state:
  - Stopped.
  - Starting, with the current stage: `cluster`, `disk+seed`, `boot`, `provision`, `ssh` or `agent`. A running VM with no stage yet, as after a daemon restart, is starting too.
  - Ready.
  - Failed, with the hint of the `failed` stage.
- CPUs and memory.
- The exit.
- Whether it is the default VM.

Gateway VMs (`role: gateway`) are hidden. The list shows as rows or as cards; the window remembers which.

Actions on a VM:

- **Start** runs `pvm start <vm> --no-wait`, then follows the stages until ready or failed. During that wait the VM can still be stopped or deleted and its terminal opened, which ends the wait. Once a stop or delete is sent, the row is locked again until both it and the wait have ended. The wait also ends, with the reason, when the VM list cannot be read for 30 seconds, as when the daemon was stopped.
- **Stop** runs `pvm stop <vm>`.
- **Delete** asks first, naming the VM, then runs `pvm rm <vm> -y`.
- **Terminal** runs `pvm ssh <vm>` in Windows Terminal (`wt.exe`) when installed, else in a new console window.
- **App** asks for a command line or a URL and runs `pvm app <vm> -- <cmd...>` or `pvm app <vm> <url>`. The command is split at spaces and a double quote is refused: it would not group words, and no quoting of it makes the copied command safe in both cmd.exe and PowerShell. It offers the VM's installed catalog apps as one-click entries: a window app runs `pvm app <vm> -- <start>`, a terminal app opens a terminal running `pvm ssh -t <vm> <start>`.
- **Files** runs `pvm app <vm> -- thunar`.
- **App settings** sets clipboard, drop and audio with `pvm app config <vm> ...`. The daemon has no route to read the current values, so a field left empty stays unchanged.
- **Install programs** runs `pvm app install <vm> <id>...` with the catalog programs picked, on a VM that is ready.
- **Set as default** runs `pvm default-vm <vm>`.

### Create

The form asks for:

- The name, checked like the CLI does.
- The image, from the supported specs.
- The user name, `pvm` unless changed: without a terminal `pvm create` needs `--user`.
- CPUs, memory and disk, with defaults from `config.yaml`. Create always passes them, as a running daemon keeps the defaults it started with.
- The exit: `direct`, or one of the exits.
- Clipboard, drop and audio.
- The programs to preinstall, from the app catalog.

Create runs these steps in order, each with its output in the progress panel:

1. `pvm create --name <vm> ... --no-start`. When the golden image is missing, its fetch and bake output shows here; it takes minutes the first time.
2. `pvm route <vm> --via <exit>`, also when the exit is the default one: the VM keeps the exit that was chosen when the default changes later.
3. `pvm app config <vm> --clipboard ... --drop ... --audio ...`.
4. `pvm start <vm> --no-wait`; the app then follows the stages until the VM is ready.
5. `pvm app install <vm> <id>...`, when programs were picked.

The VM is created stopped and boots only in step 4, so its first boot already uses the chosen exit, and a failed route step leaves it stopped. Setting a route pushes the configuration to the gateway VM, and only a user VM's start creates and boots the gateway. For a stopped VM without a running gateway, as for the first VM, the daemon therefore only saves the exit (`switchVMRoute`, `internal/daemon/routes.go`): the start boots the gateway and pushes the configuration before the VM itself boots. The CLI then prints that the route was saved, not that a configuration was pushed. The same holds for `pvm route` on the command line.

The timezone and locale of an exit are set in the guest over SSH, which a stopped VM cannot take. `pvm route` on a stopped VM therefore marks the VM (`region_pending` in its spec), and the start in step 4 sets them once SSH and cloud-init are ready, before the VM counts as ready. When that fails, the start fails with the reason, step 4 fails with it, and the mark stays for the next start.

When a step fails, the app stops there and shows the error. It also shows the remaining steps as commands that can be copied and run by hand. It does not delete the VM by itself.

### Exits

The exit list shows the name, kind, endpoint and the VMs using each exit. The default exit is marked. `pvm exit rm` leaves the cluster default as it is, so the default can name an exit that no longer exists; VMs that follow it are then blocked. The screen warns about that, and `direct` can be set as the default to repair it. The Create form and the "Change exit" dialog warn in the same way.

- **Add** runs `pvm exit add <name> --server <host:port> --token-file <file>`, or `pvm exit add-warp <name>`. The token field is a password field; see Secrets above. The transport stays the CLI's `auto`.
- **Test** runs `pvm exit test <exit>`; the speed test is an option.
- **Remove** warns when the default exit, a VM or a route rule still uses the exit, then runs `pvm exit rm`. The CLI does no such check; the app works it out from `/v1/clusters`.
- **Default** runs `pvm route default --via <exit>`.
- **Per VM** runs `pvm route <vm> --via <exit|direct>`, from the VM list or here. The app warns that a running VM restarts.

### Workspaces

The workspace list shows the id, VM, mode, host folder and last transfer.

- **Open folder** opens the host folder in Explorer.
- **Terminal** runs `pvm ssh` with the host folder as its working directory, so it starts in the matching guest folder.
- **App** runs `pvm app` from the host folder; this needs the app working directory change below.
- **Files** runs `pvm app <vm> -- thunar <guest folder>`.
- **Sync** runs `pvm workspace push` and `pull` for `sync` and `copy` workspaces. A conflict fails with the CLI's own hint; the app offers no conflict options.
- **Create** asks for the id, VM, mode, host folder (a folder picker) and read-only, then runs `pvm workspace create`, with `-y` once the user has confirmed. The command has no transport flag.
- **Remove** asks first, then runs `pvm workspace rm <id> -y`. It removes the record only; the files are not touched.

### Settings

- **Default VM** runs `pvm default-vm <vm>`, or `pvm default-vm --clear`.
- **Start the daemon at login** runs `pvm daemon install` or `pvm daemon uninstall`, which manage the existing `pvm-daemon` scheduled task.
- **Open pvm Desktop at login** writes or removes `HKCU\Software\Microsoft\Windows\CurrentVersion\Run\pvm-desktop`.
- **Language** shows the window in English or Simplified Chinese, or follows the language of Windows. The choice is the window's own: it is kept in the WebView2 profile, not in `config.yaml`, and runs no command. What pvm prints stays as it is in both: task titles, commands, output, error messages and the hint of a failed VM.
- **About** shows the app version, the daemon version, accelerator and TCG fallback warning, and the data folder. When the two versions differ, it says to run the installer again.

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

- **Request.** `appStartRequest` (`internal/daemon/app.go`) gets `cwd`, an absolute guest path, and `cwd_root`, the workspace's guest root that the fallback needs. The daemon accepts only `/workspace/<id>` and a folder inside it.
- **Launch.** `appLaunchScript` passes `--working-directory` to `systemd-run`. Web apps share one Firefox and get no folder.
- **CLI.** `cmdAppStart` (`internal/cli/app.go`) passes the folder `resolveVM` returns, the same rule as `pvm ssh`.
- **Missing folder.** When the folder is missing in the VM, the app starts in the workspace root and says so: the response carries a `note`, which the CLI prints on stderr. When the root is missing too, the start fails.

### Readiness in the VM list

`GET /v1/vms` adds `ready` and the last stage to each entry as `"stage": {"stage": <name>, "detail": <hint>}`, from the same data `GET /v1/vms/{name}` uses. Then the app and `pvm ls` need one request, not one per VM. Stage names are `cluster`, `disk+seed`, `boot`, `provision`, `ssh`, `agent`, `ready` and `failed`; `failed` carries the hint in `detail`. Gateway VMs have no stages and are never `ready`; `pvm ls` shows them as `running`.

### App catalog

- **Files.** One folder per app under `internal/apps/<id>/`, embedded at build time:
  - `app.yaml`: the name, a description, the vendor's download page, whether the build is official or community, whether it runs as a window, and the command to start it.
  - `install.sh`: run as root in the VM; it must be idempotent.
- **Daemon routes.**
  - `GET /v1/apps/catalog` lists the catalog.
  - `POST /v1/vms/{name}/apps/install` takes a list of ids and streams ndjson output, like `apps/prepare`. It runs each script through the agent with a time limit of 15 minutes, and records the installed ids in the VM's `app.json`. The first failure ends the run. A client sends one id per request, as the daemon client limits a whole response to 20 minutes.
  - `GET /v1/apps` and `GET /v1/vms/{name}/apps` answer `{"apps": [...], "installed": {<vm>: [<id>...]}}`.
  - A window app also runs the Xpra preparation first.
- **CLI commands.**
  - `pvm app catalog`.
  - `pvm app install [<vm>] <id>...`.
  - `pvm app ls` shows the installed catalog apps.
- **M1 catalog:** Claude Code CLI, Codex CLI and Claude Desktop.
  - The implementer checks the current official Linux install method for each.
  - ChatGPT Desktop has an official Linux preview, but it is left out for now: its package comes as a plain download with no published checksum or signing key to pin.
  - A program without an official Linux build is left out unless a maintained community build exists. When one is used, it is marked as community in `app.yaml` and in the app.
- **Egress.** Downloads run in the VM and leave through the VM's exit, never through the host.

## Build and release

- **Source.**
  - The backend is `internal/desktop`. Its `App` struct is bound to the window, so the frontend calls `window.go.desktop.App.<Method>`; every exported method of that struct is callable from the frontend, and nothing else is.
  - `cmd/pvm-desktop` holds the Wails window (Windows only; elsewhere the program says so and exits) and the frontend in `cmd/pvm-desktop/frontend` (TypeScript and Vite, no framework).
  - The look of the frontend follows [desktop-design-prompt.md](desktop-design-prompt.md): one stylesheet whose colours and sizes are CSS variables, a light and a dark theme that follow the system, and icons drawn inline. It loads nothing from outside. Headings use one bundled font, Newsreader, from the `@fontsource-variable/newsreader` package (SIL OFL 1.1); the build copies its licence next to it. The text of the window is in `src/en.ts` and `src/zh.ts`.
  - `npm run dev` in the frontend folder serves the page in a browser with a made-up backend (`src/mock.ts`), for working on the screens without Windows. A build leaves that file out.
  - Wails is pinned in `go.mod` and the frontend dependencies in `package-lock.json`. Choose the latest stable versions released at least two weeks before.
  - The built frontend is not committed. It is embedded only with the `production` build tag, so `go build ./...`, `go vet ./...` and `go test ./...` need no Node.
- **Build.** `script/build-desktop.sh` runs on any host with Go and Node:
  - `npm ci` and `npm run build` in the frontend folder.
  - An application manifest (`cmd/pvm-desktop/pvm-desktop.exe.manifest`) turned into a `.syso` with `rsrc`. A plain `go build` skips the resource step of the `wails` CLI, and without the manifest Windows scales the window as a bitmap.
  - `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags desktop,production,wv2runtime.browser -ldflags "-H windowsgui ..."`. `wv2runtime.browser` makes a missing WebView2 open Microsoft's download page; the default would install it without asking.
  - It fails when the result does not contain the built frontend or the manifest.
- **CI.** In the `native` job for `pvm_linux_amd64`:
  - Set up Node at a pinned version and run `script/build-desktop.sh`, after the tests.
  - Upload the result as the raw artifact `raw-pvm-desktop_windows_amd64`; `script/ci/reuse.py` lists it as required.
  - The `package` job checks its Go version, main package and build tags before packing it.
- **Installers.** `script/windows/pack-setup.sh` adds `pvm-desktop.exe` to the NSIS and MSI packages:
  - A per-user Start menu shortcut, "pvm Desktop".
  - An optional "Open pvm Desktop" on the NSIS finish page, off by default: opening the app starts the daemon. The MSI is built with wixl, which has no UI, so it has no finish page.
  - Uninstall removes the program, the shortcut and the `Run` value. The WebView2 profile under the data folder stays, like all user data.
  - A running `pvm-desktop.exe` is found like a running `pvm.exe`: interactive runs offer to close it, silent runs refuse.
- **WebView2.** Windows 10 and 11 normally have it. When it is missing, the app shows a message with Microsoft's download page; the installers do not install it silently.
- **The `windows` CI job** runs the app's backend tests (`./internal/desktop`).

## Testing

- **Backend unit tests** run against a fake daemon (`httptest`) and a fake `pvm` runner. They cover:
  - Mapping stages to states.
  - The argv for every action, and the name checks.
  - Create stopping at the failed step.
  - That tokens and keys never reach the frontend.
- **Frontend.** `TestFrontendAPI` and `TestFrontendTypes` keep `api.ts` equal to the backend's methods and types. The screens have no automated tests; they were looked at in Chromium with the made-up backend of `npm run dev`, not in WebView2.
- **Core change tests:**
  - The resolve order with `default_vm`, including a deleted default VM.
  - The app working directory.
  - Catalog parsing and the install stream.
- **End to end on a native WHPX host**, not done yet:
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
