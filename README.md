# pvm

English | [简体中文](README.zh-CN.md)

Personal QEMU dev VMs in a single binary: create an isolated Linux development machine with one command and route all of its traffic through your own VPS.

## Highlights

- **A ready dev box in one command.** Docker, common tools, Firefox and a desktop come preinstalled. After a one-time image bake, new VMs are ready in about 30 seconds. See [Quickstart](#quickstart).
- **All traffic through your own exit, and none without it.** Route a VM through your VPS over WireGuard, or through Cloudflare WARP with nothing to deploy. If the tunnel drops, the VM goes offline instead of leaking your host's IP. See [Fail-closed exit routing](#fail-closed-exit-routing).
- **Split routing by domain or network.** Send some domains direct or through another exit; rules apply at once, without a restart.
- **VM programs as native windows.** `pvm app dev1 -- gedit` or a web page opens as its own window on your desktop, while cookies, fingerprint and egress stay in the VM. Type with your own input method. See [Apps as host windows](#apps-as-host-windows).
- **The full desktop in a browser tab.** `pvm desktop` opens the VM's own desktop, the same machine you reach over SSH. See [Browser desktop](#browser-desktop).
- **Your project folders in the VM.** Mount a folder, keep two synced copies, or upload once and download the result. In a project folder, `pvm ssh` opens a shell in the same folder in its VM, and offers to start the VM first. See [Workspaces](#workspaces).
- **Snapshots in seconds and clones**, loopback port maps, and a read-only `pvm audit` of routing and DNS.
- **Windows without administrator rights.** One installer brings pvm, QEMU and the Xpra client. See [Windows installers](#windows-installers).

## Roadmap

Shipped:

- [x] Per-user Windows installers (v1.1.6)
- [x] Workspaces: `mount`, `sync` and `copy` (v1.1.7)
- [x] Firefox and sshfs in golden images (v1.1.7)
- [x] `pvm app`: VM programs and web pages as host windows (v1.2.0)
- [x] The `pvm.stream` channel and a faster `pvm desktop` (v1.2.0)
- [x] UDP and UDP exits on native Windows (v1.2.0)
- [x] Xpra client in the Windows installers (v1.2.0)
- [x] The VM of the current workspace by default, `pvm ssh` in the matching folder, and an offer to start a stopped VM (next release)

Planned:

- [ ] `pvm exit-agent install|uninstall` as a systemd service
- [ ] `pvm app` on macOS, Linux and WSL2 hosts, and reliable input method text
- [ ] Input method candidates that follow the app's text cursor
- [ ] `pvm desktop` as a host window like `pvm app`, opening the workspace folder in the VM's file manager

Candidates:

- [ ] `pvm ssh-config` for VS Code Remote-SSH, rsync and similar tools
- [ ] An egress probe in `pvm exit test`
- [ ] A default image
- [ ] Port maps over `pvm.stream` instead of the gateway VM
- [ ] Fix the gateway rule drift false positive

Details are in [docs/roadmap.md](docs/roadmap.md).

## Installation

pvm runs on macOS on arm64, Linux on amd64, and Windows on amd64.

### Windows

1. Download `pvm_windows_amd64_setup.exe` or `pvm_windows_amd64_setup.msi` from [Releases](../../releases). Optionally verify it first: `gh attestation verify pvm_windows_amd64_setup.exe --repo BitterOrange/pvm`.
2. Run it. No administrator rights are needed.
3. Open a new terminal, so the `PATH` change applies, and run `pvm doctor` to check acceleration and the backend.

Both installers put `pvm.exe` in `%LOCALAPPDATA%\pvm`, add it to the user `PATH`, install the Xpra client to `%LOCALAPPDATA%\pvm\xpra`, and install the QEMU runtime to `%USERPROFILE%\.pvm\runtime\qemu\x86_64`, or under `PVM_HOME` when it is set. Use one installer type per machine; `pvm upgrade` updates either.

- Files in use by a running pvm or VM cannot be replaced. Interactive runs offer to stop the daemon, which also stops running VMs; silent runs (`/S`, `msiexec /qn`) refuse, so run `pvm daemon stop` first.
- Silent MSI install: `msiexec /i pvm_windows_amd64_setup.msi /qn`. Pass `PVMHOME=D:\pvm-home` to choose the data location; upgrades and uninstalls reuse it.
- Uninstalling from Settings > Apps removes pvm, the runtime, the Xpra client and the `PATH` entry. VM disks, keys and configuration under `~/.pvm` are kept.

<details>
<summary>Known limitations of the Windows installers</summary>

- Installing, and uninstalling from Settings > Apps, are tested while pvm is stopped. The prompt that offers to stop a running daemon has not been tested end to end. If it does not appear, or setup still reports that pvm is running, cancel, run `pvm daemon stop`, and run setup again.
- Stopping the daemon from setup waits for running VMs to shut down, up to about three minutes, and setup shows no progress meanwhile.
- VMs left running without a daemon cannot be stopped by setup. Setup lists them and refuses; shut them down, then run setup again.
- Antivirus software briefly locks freshly unpacked files. Setup retries each file move for up to 10 seconds; if the lock lasts longer, setup stops and keeps the previous version. Run setup again.
- After `pvm upgrade`, Settings > Apps still shows the version the installer put there.

</details>

Windows has two backends, chosen with `PVM_BACKEND` or `backend` in `~/.pvm/config.yaml`:

- `native`: the Windows daemon runs Windows QEMU directly, with WHPX when Windows Hypervisor Platform is enabled. Windows QEMU has no virtio-9p, so `/share` is off; use [workspaces](#workspaces) for host folders.
- `wsl`: commands go to `pvm` inside WSL2, with full `/share`, extra mounts and KVM where available.
- `auto` (default): a running native daemon or native QEMU first, otherwise WSL2 when pvm is installed there.

Acceleration, DNS and WSL2 nested virtualization settings are in [SKILL.md](SKILL.md).

### macOS and Linux

Install GitHub CLI (`gh`) and Python 3, then:

```bash
curl -fsSL https://raw.githubusercontent.com/BitterOrange/pvm/master/scripts/install.sh | sh
```

The script installs `pvm` to `~/.local/bin` (set `INSTALL_DIR` to change it) together with the bundled QEMU runtime (set `PVM_SKIP_QEMU=1` to use the system QEMU). It checks the release checksum and its GitHub build attestation before replacing anything, and keeps the previous binary and runtime. The script also works in Git Bash on Windows, but the installers are the recommended way there.

### Quickstart

The daemon starts on demand. The first `create` downloads and bakes an image, which takes about ten minutes once.

```bash
pvm create --image ubuntu:26.04 --name dev1 --user dev
pvm ssh dev1
pvm desktop dev1
pvm map dev1 8080:80
pvm exit add-warp my-warp
pvm route dev1 --via my-warp
```

Each VM gets a private host folder at `/share` (`~/.pvm/vms/<name>/share`) where 9p is available; `pvm mount` adds more, with the mount profiles described in [SKILL.md](SKILL.md).

## Features

### Fail-closed exit routing

Register a VPS exit and route the whole VM through it. TCP, UDP and ICMP go through WireGuard over UDP or TCP, and if the tunnel goes down, traffic is dropped instead of falling back to the host's connection.

```bash
pvm exit add my-exit --server vps.example.com:18443 --token YOUR_TOKEN
pvm route dev1 --via my-exit
pvm ssh dev1 'curl -s ipinfo.io'
pvm route dev1 --via direct
```

Or use Cloudflare WARP without deploying a VPS or installing anything in the VM:

```bash
pvm exit add-warp my-warp
pvm route dev1 --via my-warp
```

Route every VM that has no explicit route, and add split-routing rules by domain or network:

```bash
pvm route default --via my-exit
pvm route add direct --domain-suffix example.com
pvm route add proxy --domain-suffix example.com --via my-exit
pvm route ls
```

Only explicit direct and internal rules bypass the selected exit. To run your own exit, see [Deploy an exit on a VPS](#deploy-an-exit-on-a-vps).

### Apps as host windows

`pvm app` shows single programs or web pages from a VM as host windows through [Xpra](https://xpra.org). The host needs the Xpra 6.5 client, which the Windows installers include; the first app in a VM installs the Xpra server there from xpra.org's repository.

```bash
pvm app dev1 -- gedit
pvm app dev1 https://mail.example.com --kiosk
pvm app ls
pvm app config dev1 --clipboard both --drop on
pvm app attach dev1
pvm app stop dev1 --all
```

Clipboard and drops are off until you enable them per VM. With `--clipboard to-guest` or `both`, host content enters the VM only when one of its windows comes to the front or you copy while it is there, and content that password managers mark as excluded is never sent. Text typed with a host input method, such as Chinese with Microsoft Pinyin, reaches the app as well. See [docs/design/app.md](docs/design/app.md).

### Browser desktop

`pvm desktop dev1` serves the VM's own XFCE desktop through TigerVNC and noVNC on a loopback URL, resized to the browser window. `--public` listens on `0.0.0.0` and still requires VNC authentication.

### Workspaces

A workspace binds one host folder to one VM at `/workspace/<id>`. Pick a mode when you create it:

- `mount`: one shared copy. Linux, macOS and WSL2 use 9p; the native Windows backend uses reverse sshfs over the existing SSH connection. Host changes raise no inotify events in the VM, so file watchers need polling (for example `CHOKIDAR_USEPOLLING=1`).
- `sync`: a full copy on each side. `push` and `pull` move changes and stop on conflicts until you pass `--prefer host`, `--prefer vm` or `--keep-both`.
- `copy`: upload once, work in the VM, then download the result with `clone`.

```bash
cd ~/src/web
pvm workspace create web dev1 --mode sync
pvm ssh 'npm install && npm test'
pvm workspace pull web
pvm workspace clone web ~/src/web-result
pvm workspace rm web
```

Inside a workspace folder, commands such as `pvm ssh`, `pvm exec`, `pvm app` and `pvm map` can leave out the VM name and use the workspace's VM; `pvm ssh` starts in the matching directory. Outside a workspace they use the only VM, if there is one. At a terminal pvm asks before starting a stopped VM.

Uploads skip `.gitignore` matches and `node_modules`; a `.pvmignore` file adds or re-includes paths. pvm refuses to upload or mount the home directory, a file system root, its own data directory, or a tree with credentials such as `.ssh` or `.aws`, and `pvm workspace rm` never deletes host files. See [docs/design/workspace.md](docs/design/workspace.md).

### Windows installers

Releases ship `pvm_windows_amd64_setup.exe` and `pvm_windows_amd64_setup.msi`: per-user installers that need no administrator rights and bring pvm, the bundled QEMU runtime and the Xpra client for `pvm app` in one step. They detect a running pvm before replacing files and keep your VMs on uninstall. See [Installation on Windows](#windows).

### Architecture

The host `pvm` binary provides the CLI and daemon, and also runs as the VPS exit agent. A separate guest binary built from `cmd/agent` carries only the guest agent, current-VM port maps and the guest skill. The daemon owns VM and cluster lifecycle; the managed gateway VM provides DNS, routing, WireGuard devices and the fail-closed kill switch. WARP registration and credentials are managed on the host without `warp-cli`, `wgcf` or extra configuration files.

```text
Control: pvm CLI -> pvm daemon -> gateway VM and dev VMs
Data:    dev VM -> gateway VM -> WireGuard tunnel -> exit-agent on VPS or Cloudflare WARP -> Internet
```

## Supported images and hosts

- Images: `ubuntu:26.04`, `ubuntu:24.04`, `debian:13`, and `debian:12`.
- Hosts: macOS on arm64 (`darwin/arm64`), Linux on amd64 (`linux/amd64`), and Windows on amd64 (`windows/amd64` via WSL2 bridge or native QEMU/WHPX; selectable with `PVM_BACKEND=auto|wsl|native` or `backend` in `config.yaml`).
- VMs always use the host's native architecture; cross-architecture emulation is not supported. Linux arm64 release binaries are for VPS exit agents, not for running a pvm host.

## Deploy an exit on a VPS

Verify the release archive for the VPS architecture and create an owner-only token file, then run the exit agent as a persistent service:

```bash
pvm exit-agent --listen :18443 --token-file /etc/pvm-exit/token
```

Then register it on the host with `pvm exit add`. See [SKILL.md](SKILL.md#standard-flow-deploy-an-exit-over-ssh) for the complete SSH deployment, including a systemd unit, fingerprint verification, speed tests, and rollback.

The VPS firewall must accept TCP 18443 (control plane, `--listen`), UDP 18443 (WireGuard over UDP, `--udp-port`) and TCP 18444 (WireGuard over TCP, `--tcp-port`). With the `nft` or `iptables` NAT backend it must also forward traffic from the agent's TUN devices: `pvmexit0` for the UDP transport and `pvmexit1` for the TCP transport (the prefix follows `--tun-prefix`). The agent's own nftables table cannot override a drop elsewhere in the forward path, such as ufw's default `deny (routed)`. In that case `pvm exit test` still passes, because it only probes the agent itself, but VMs routed through the exit cannot open connections to the internet, although ping may still work. With ufw, allow the forwarding, replacing `eth0` with the VPS's internet-facing interface:

```bash
ufw route allow in on pvmexit0 out on eth0
ufw route allow in on pvmexit1 out on eth0
```

To connect a phone or laptop with a standard WireGuard client, export a separate client profile from a UDP exit:

```bash
pvm exit client-config my-exit --output phone.conf
```

This creates a new client key, enrolls it with the exit agent, writes the config with mode `0600`, and displays a QR code for mobile apps. Run it once per device. Standard clients support UDP only and get a full-tunnel IPv4 VPN, not pvm's gateway DNS or split-routing rules. IPv6 is not tunneled.

## Documentation

- [SKILL.md](SKILL.md): Complete usage manual for agents and people; `pvm skill` prints the embedded copy.
- [SECURITY.md](SECURITY.md): Host/guest trust boundaries, credentials, image provenance, and network verification; `pvm security` prints the embedded copy.
- [AGENTS.md](AGENTS.md): Repository map, build and test workflow, and development rules.
- `pvm upgrade` verifies the release, saves a recovery point, upgrades the installed binary and bundled QEMU runtime, restarts a running daemon, and attempts to restore every previously running user VM. Use `--no-restart` to replace only the binary and runtime, or `pvm upgrade --rollback` to restore the saved binary, QEMU runtime, and VM recovery plan. On Windows, `--no-restart` requires no running VMs when QEMU is included; omit it so pvm can stop the daemon before replacing the runtime and restore the VM plan afterwards. Set `PVM_SKIP_QEMU=1` to upgrade only pvm.
- `pvm audit dev1 --privacy`, `pvm doctor`, and `pvm route explain dev1 example.com` inspect local configuration without starting a daemon or making network probes; their output redacts identifying values.
- Automatic update checks are off by default. Set `update_check: true` in `~/.pvm/config.yaml` or `PVM_UPDATE_CHECK=1` to opt in; `PVM_NO_UPDATE_CHECK=1` disables them.
- For independently trusted image signing keys, use `pvm image fetch ubuntu:26.04 --keyring /trusted/ubuntu-cloudimage.gpg`. This requires `gpgv`; pvm does not download or trust keys automatically.

## Development

A full build uses Go 1.27.4 and garble v0.18.0, with both versions pinned in CI. The build uses the dedicated `cmd/agent` entry point and reproducible obfuscation settings.

```bash
go install mvdan.cc/garble@v0.18.0
./script/build.sh
go test ./...
```

On Windows, install Git for Windows, Go 1.27.4 and garble v0.18.0, then run the wrapper from PowerShell. It runs `script/build.sh` through Git Bash and writes `pvm.exe`. Bundled QEMU is not packed on Windows (`script/qemu/pack-windows-runtime.sh` builds that runtime separately), so the wrapper defaults to `PVM_PACK_QEMU=0` and the runtime already installed under `~/.pvm/runtime` keeps being used.

```powershell
go install mvdan.cc/garble@v0.18.0
.\script\build.ps1
.\pvm.exe version
```

Do not run the full `go test ./...` on a Windows host. The test suite is written for Unix (file permission bits, paths such as `/tmp`, symlinks, and tools like `ip`, `gh` and `bash`), so many tests fail there for reasons unrelated to your change, and the `internal/cli` tests keep respawning themselves and leave `cli.test.exe` processes behind. On Windows, run only targeted tests for what you changed, for example `go test ./internal/qemu -run Accel`, and rely on Linux or macOS (CI or WSL2) for the full suite.

CI tests and builds each native platform once per commit, caches Go and build tools, and reserves QEMU packaging and signing for releases. Releases can reuse verified artifacts from a successful master run of the exact same commit. Linux namespace tests must run rather than skip.

Security boundaries: user VMs share a layer-2 network; global shares and extra host mounts are explicit trust boundaries. They are not isolated hostile tenants. Explicit direct/internal rules can bypass a default exit. See [SECURITY.md](SECURITY.md) for guarantees, limitations, and deployment checks.
