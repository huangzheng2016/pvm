# pvm

Personal QEMU dev VMs in a single binary: create an isolated Linux development machine with one command and route all of its traffic through your own VPS.

## Features

- **Fast, ready-to-use VMs:** Golden images include Docker, common development tools, and a desktop. `pvm create` automatically downloads the base image and bakes a missing golden; the one-time first bake takes about ten minutes, while later VMs are usually ready in about 30 seconds.
- **Fail-closed exit routing:** Route TCP, UDP, and ICMP through WireGuard to your VPS over UDP or TCP, or to Cloudflare WARP over UDP. Only explicit direct/internal rules bypass the selected exit; if the tunnel goes down, traffic is dropped instead of falling back to host egress.
- **Real-IP full-tunnel DNS and split routing:** Default-exit domains resolve through the selected exit to real IPv4 addresses and use direct L3 tunnel forwarding. Only explicit domain proxy rules use fake-ip relays. Direct/internal domains keep their real addresses and direct path, and domain or CIDR rules take effect without a restart.
- **Native browser desktop:** `pvm desktop` serves the VM's own XFCE desktop through TigerVNC and noVNC. It uses a loopback URL by default; `--public` optionally listens on `0.0.0.0` with VNC authentication still required. It is the same machine as SSH, not a container.
- **Host-shaped defaults:** New VMs default to all host CPU cores and half of host RAM, with a balloon driver returning idle memory to the host. Sparse qcow2 disks default to 20 GB, grow with `pvm resize`, and release unused host space with `pvm compact`.
- **Complete VM workflows:** RAM snapshots restore in seconds, clones regenerate machine identifiers, read-only `pvm audit` checks route and TCP/UDP/ICMP/DNS health, and loopback port maps, bidirectional `/share` plus custom host mounts, and virtio-serial exec cover daily development and recovery.

## Installation

pvm requires macOS on arm64 or Linux on amd64. Install the latest release with `curl`:

```bash
curl -fsSL https://raw.githubusercontent.com/huangzheng2016/pvm/master/scripts/install.sh | sh
```

The installer places `pvm` in `~/.local/bin` by default; set `INSTALL_DIR` to change this. On macOS it also installs the bundled QEMU runtime unless `PVM_SKIP_QEMU=1` is set, in which case pvm uses the system QEMU. You can also download an archive from [Releases](https://github.com/huangzheng2016/pvm/releases).

## Quickstart

The daemon starts automatically when needed. The first `create` may download and bake an image; later creates are much faster.

```bash
pvm create --image ubuntu:26.04 --name dev1 --user dev
pvm mount dev1 ~/work/proj:/home/dev/proj
pvm ssh dev1
pvm desktop dev1
pvm map dev1 8080:80
```

### 9p mount profiles

`/share` and extra mounts use the `coherent` profile by default (`cache=none,msize=262144`) for bidirectional host/guest visibility. This is the shared-source baseline, not multi-writer transaction consistency. `pvm mount ls dev1` shows each extra mount's profile.

For a mount dominated by large sequential reads, explicitly select `readahead`:

```bash
pvm mount dev1 --profile readahead ~/work/data:/home/dev/data
```

This uses `cache=readahead` and requests `msize=1048576`; the guest and QEMU negotiate the actual msize, which may be smaller. It does not remove small-file create/stat or other metadata bottlenecks and is not a promise of faster pnpm, Vite, or TypeScript builds. Keep Node/TypeScript dependencies and build caches on the guest's local disk.

If the host rewrites or replaces a file while an old guest file descriptor is open, `readahead` can return old contents even through a newly opened guest descriptor until the old one is closed. Use `coherent` when both sides actively read and write the same files. Only the two named profiles are accepted; arbitrary cache options are not. To change an extra mount's profile, close guest users of the mount and run `pvm mount rm` followed by `pvm mount` with the new profile. Upgrading pvm does not rewrite an existing guest's cloud-init or force a remount.

Register a VPS exit and route the whole VM through it:

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

To route every VM without an explicit route, set the cluster default:

```bash
pvm route default --via my-exit
pvm route ls
```

## Architecture

One `pvm` binary provides the host CLI and daemon, the guest agent, and the VPS exit agent. The daemon owns VM and cluster lifecycle; the managed gateway provides DNS, routing, WireGuard devices, and the fail-closed kill switch. WARP registration and credentials are managed on the host without `warp-cli`, `wgcf`, or extra configuration files.

```text
Control: pvm CLI -> pvm daemon -> gateway VM and dev VMs
Data:    dev VM -> gateway VM -> WireGuard tunnel -> exit-agent on VPS or Cloudflare WARP -> Internet
```

## Supported images and hosts

- Images: `ubuntu:26.04`, `ubuntu:24.04`, `debian:13`, and `debian:12`.
- Hosts: macOS on arm64 (`darwin/arm64`) and Linux on amd64 (`linux/amd64`).
- VMs always use the host's native architecture; cross-architecture emulation is not supported. Linux arm64 release binaries are for VPS exit agents, not for running a pvm host.

## Deploy an exit on a VPS

Download the release archive for the VPS architecture and run the exit agent as a persistent service:

```bash
pvm exit-agent --listen :18443 --token YOUR_TOKEN
```

Then register it on the host with `pvm exit add`. See [SKILL.md](SKILL.md#standard-flow-deploy-an-exit-over-ssh) for the complete SSH deployment, including a systemd unit, fingerprint verification, speed tests, and rollback.

To connect a phone or laptop with a standard WireGuard client, export a separate client profile from a UDP exit:

```bash
pvm exit client-config my-exit --output phone.conf
```

This creates a new client key, enrolls it with the exit agent, writes the config with mode `0600`, and displays a QR code for mobile apps. Run it once per device. Standard clients support UDP only and get a full-tunnel IPv4 VPN, not pvm's gateway DNS or split-routing rules. IPv6 is not tunneled.

## Documentation

- [SKILL.md](SKILL.md): Complete usage manual for agents and people; `pvm skill` prints the embedded copy.
- [SECURITY.md](SECURITY.md): Network, device identity, and risk-control posture; `pvm security` prints the embedded copy.
- [AGENTS.md](AGENTS.md): Repository map, build and test workflow, and development rules.
- `pvm upgrade` upgrades the installed binary, restarts a running daemon, and restores previously running user VMs. Use `--no-restart` to replace only the binary.

## Development

```bash
./script/build.sh
go test ./...
```
