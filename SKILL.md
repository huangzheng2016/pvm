---
name: pvm
description: Create and manage isolated Linux dev VMs with pvm, and route all of a VM's (or the whole cluster's) traffic through a VPS exit-node. Use when you need a clean/disposable dev machine, separate guest filesystems and controlled egress, an environment that exits only via a specific VPS, a browser desktop, port maps, snapshots, or clones.
---

# pvm: isolated dev VMs and egress control

pvm is a QEMU virtual machine system for the local machine (single Go binary). A daemon runs permanently; the CLI auto-spawns it when needed (exception: after an explicit `pvm daemon stop` it stays down until `pvm daemon start`). Everything goes through the `pvm` command — never touch QEMU directly.

## Security boundary

Read `pvm security` before running sensitive or untrusted workloads. VMs in one cluster share a layer-2 network and gateway; registered IP/MAC pairs do not authenticate a hostile guest. All user VMs share the same read-write `/share`. Extra mounts give guests access to host files with the host user's privileges. Do not treat these VMs as mutually untrusted tenants, and do not expose credentials through shared files. Explicit direct/internal rules are exceptions to a selected exit. A new VM, clone, or route does not guarantee anonymity, a new application identity, or a third-party account outcome.

## Architecture boundary

pvm supports only the host's native architecture: darwin/arm64 runs arm64 VMs, linux/amd64 runs amd64 VMs. Build artifacts contain guest binaries and the QEMU runtime for the current host architecture only — no cross-arch emulation or packages.

## Prerequisites

- If pvm is on PATH, use `pvm ...`; inside the repo, use `./pvm ...` from the repo root.
- Run `pvm daemon status` first to confirm the daemon is healthy; `pvm list` shows existing VMs.
  - If the status shows `warning: N vm(s) running under tcg fallback`, the host's hardware accelerator is unavailable (on macOS this means Hypervisor.framework is wedged after long uptime — reboot the Mac to restore full speed). VMs work meanwhile, just slowly.
- On first use, `pvm image list` shows cached base and golden images. `pvm create` can fetch a missing base and build a missing golden; this first run is much slower. Use `pvm image fetch <distro:version>` and `pvm image build <distro:version>` to prepare them explicitly. Fetch rechecks the distribution checksum; `--force` re-downloads.
- Example exits: `my-exit` (WireGuard over UDP) and `my-exit-tcp` (WireGuard over TCP). Run `pvm exit ls` to see the names configured on the current host and do not re-add an existing name.
- The `pvm-exit-agent.service` on the VPS must stay running. Never stop it unless you are deliberately testing the kill-switch or upgrading.

## Common choices

- **Images**: `ubuntu:26.04`, `ubuntu:24.04`, `debian:13`, `debian:12`
- **Exit providers**: pvm exit-agent (`udp` default / `tcp` fallback) or Cloudflare WARP (`udp` only); see `pvm exit add-warp` below
- **exit-agent NAT backend**: `auto` (probe) / `nft` / `iptables` / `userspace` (no root; ICMP requires ping-socket permission)
- **Route targets**: `direct` (host egress) or an exit name such as `my-exit`
- **create flags**: `--image`, `--distro`, `--version`, `--name`, `--user`, `--cpus`, `--mem`, `--disk`, `--tz`, `--locale`, `--git-email`, `--git-name`, `--smbios-spoof`, `--no-start`, `--no-wait`

### Directory mounts (pvm mount and /share)

- Every VM mounts the host's `~/.pvm/share` at `/share` (9p, **bidirectional read-write**, including subdirectories). Drop files there for quick exchange - zero configuration. `/share` always uses the `coherent` profile.
- Manage extra mounts dynamically on a running VM; no reboot or recreate is needed:
  ```bash
  pvm mount dev1 ~/work/proj:/home/dev/proj
  pvm mount dev1 --profile readahead ~/work/data:/home/dev/data
  pvm mount ls dev1
  pvm mount rm dev1 /home/dev/proj
  ```
- `pvm mount` hot-plugs the 9p device, mounts it in the guest, and persists the configuration and profile in the guest's fstab and VM spec. `pvm mount rm` unmounts and detaches it without deleting host files. Add and remove require the VM to be running and SSH-ready; up to 13 extra mounts are supported. A VM started by an older pvm version needs one restart to reserve the dynamic slots. Clones inherit mount profiles, and `pvm mount ls` displays them.
- Mounts are virtio-9p (security_model=none + access=any, guest uid aligned with host uid). Good for source trees and documents; not for high-IOPS database directories. Extra mounts are managed only by `pvm mount`, not at create time.
- **Mount profiles**:
  - `coherent` is the default for `/share`, new extra mounts, and old specs with no profile. It uses `cache=none,msize=262144` and preserves the bidirectional-visibility baseline for files actively read and written on both sides. It does not provide multi-writer transaction consistency.
  - `readahead` uses `cache=readahead` and requests `msize=1048576`; QEMU and the guest negotiate the actual value, which can be smaller. It targets large sequential reads, not small-file create/stat or other metadata bottlenecks.
  - With `readahead`, if the host rewrites or replaces a file while an old guest file descriptor remains open, even a newly opened guest descriptor can see old contents until the old descriptor is closed. Do not use it when this visibility delay is unacceptable.
- Neither profile removes the metadata cost of source-tree builds. Do not claim a pnpm, Vite, or TypeScript build speedup without measuring it; keep Node/TypeScript dependencies and build caches on the guest's local disk.
- Only `coherent` and `readahead` are accepted; `loose`, `fscache`, and arbitrary mount options are rejected. To switch an extra mount's profile, close guest users of the mount, then use the normal `pvm mount rm` and `pvm mount --profile ...` path. Upgrading pvm does not rewrite existing cloud-init, automatically migrate profiles, force an unmount, or restart the VM.
- `--image debian:13` and `--distro debian --version 13` are equivalent; conflicting flags are rejected.
- **`pvm image build` proxy passthrough**: golden builds honor the host's `HTTP_PROXY/HTTPS_PROXY/ALL_PROXY/NO_PROXY` (loopback addresses are rewritten for the build VM). Successful provisioning removes its temporary proxy configuration. A failed build kept with `--keep-vm` can retain credentials; deletion is not forensic disk erasure. Bake uses host/slirp egress, independently of the final VM's exit route. Concurrent downloads/builds of the same base are rejected with a retry hint, and golden builds are serialized. Abandoned build VMs are preserved for explicit inspection and removal.

## Core workflows

### 1. Create and enter a dev machine

```bash
pvm create --image ubuntu:24.04 --name dev1 --user dev
# alias form: pvm create --distro debian --version 13 --name d1 --user dev
pvm list
pvm ssh dev1 'whoami && ls /share'
```

- Always pass `--user` explicitly. Interactive create prompts when it is omitted; non-interactive create rejects a missing `--user` instead of silently choosing one.
- create follows progress by default (image, disk, boot, cloud-init stages); `--no-wait` returns immediately.
- Golden images come with Docker, common dev tools, and a native XFCE desktop (TigerVNC + noVNC) preinstalled. User VMs get a virtio-gpu display device by default (`/dev/dri` in the guest); gateway and build VMs stay headless.
- SSH uses a pvm-dedicated keypair, no password. `pvm ssh dev1` opens an interactive shell; when both local stdin/stdout are terminals, `pvm ssh dev1 codex` and other interactive commands also get a TTY automatically. Use `pvm ssh -t dev1 <cmd>` to force one or `-T` to disable it. SSH remote commands run through the user's login shell, loading `~/.profile`, Bash login files, or `~/.zprofile`; TTY commands also request interactive mode so `.bashrc`/`.zshrc` follow the shell's normal startup rules. When SSH is unreachable the virtio-serial agent channel takes over automatically (or use `pvm exec dev1 <cmd...>` explicitly - it needs no network).
- create/start pushes the VM's IP/MAC mapping to the gateway DHCP **synchronously** before booting (new VMs are reachable immediately); if the gateway is unreachable, start fails instead of booting a network-less VM.
- Cluster addressing: VMs live in `100.99.<cluster>.0/24` (CGNAT space), the gateway is `100.99.<cluster>.1`.

### 2. Route all of a VM's traffic through the VPS

```bash
pvm exit ls
pvm route dev1 --via my-exit     # UDP preferred
pvm route dev1 --via my-exit-tcp # TCP fallback
pvm audit dev1
pvm exit test my-exit
```

- The region profile automatically switches the VM's timezone/locale to the exit region (`en_US.UTF-8`, applied over SSH — skipped if the VM's SSH is down; rerun `pvm route` to retry). `pvm audit` is read-only: it reports route, TCP, UDP, ICMP, DNS, IPv6, timezone, and locale without starting/stopping the VM or blocking startup.
- Restore host egress: `pvm route dev1 --via direct`.
- **Fail-closed**: when the exit tunnel is down, VMs bound to it lose all networking — this is intentional leak protection. Do not "fix" it by switching to direct; the VM recovers once the VPS service is back.
- Switching `--via` stops the VM first, applies the new route, then starts it again if it was running. A VM that was already stopped is not started automatically.
- The gateway reads actual policy rules and nft state every 10s, repairs missing or incorrect rules, and probes TCP, UDP, and ICMP through the selected exit. Route-only drift is repaired without rebuilding the tunnel. Two failed data-plane checks (~20s) rebuild the tunnel, during which bound VMs stay offline (expected).
- Gateway peer keys persist in the gateway VM, so daemon/gateway restarts re-enroll as the same peer instead of leaking stale registrations. TCP exits use keepalives and bounded writes to recover from half-open connections.
- Default-exit domains resolve only through the selected exit (on the VPS or through the WARP tunnel), return real IPv4 addresses, and use direct L3 tunnel forwarding. Only explicit domain proxy rules use fake-ip relays. `pvm exit test` uses the exit-agent's own TCP, UDP, and ICMP probes; WARP is handshake-only. No third-party STUN or probe service is required.
- TCP, UDP, and ICMP follow the selected exit for both real-IP flows and explicit fake-ip split rules. Only explicit direct/internal rules use the direct path. Unsupported packets and flows with no live tunnel or resolver are dropped; userspace NAT supports TCP, UDP, and ICMP, not arbitrary IP protocols.

### 3. Cluster-wide default route

```bash
pvm route default --via my-exit-tcp # every VM without an explicit route uses this exit
pvm route default --via direct      # back to host egress for unmatched VMs
pvm route ls                     # shows rules, per-VM routes, and the cluster default
```

- Priority: per-VM route > routing rules > cluster default > direct.
- A VM with an explicit `--via direct` keeps host egress even when a cluster default exit is set.
- Same fail-closed semantics as per-VM routes: if the default tunnel dies, every VM following it goes offline.

### 4. Browser desktop (native XFCE)

```bash
pvm desktop dev1
# prints http://127.0.0.1:<port>/vnc.html?password=... — open in a browser, no login page
pvm desktop dev1 --public
# optional: listen on 0.0.0.0 so another device can use http://<host-ip>:<port>/vnc.html?password=...
pvm desktop dev1 --stop
```

- Requires a VM built from a **golden image** (XFCE + TigerVNC + noVNC preinstalled). Base images lack the desktop packages and `pvm desktop` says so (a 503 means the VM is still booting — retry shortly).
- This is the VM's own desktop (not a container): the filesystem, users, and processes are the same machine you see over SSH, including the `/share` mount.
- The desktop is passwordless and XFCE screen locking is disabled. The session persists inside the VM (closing the browser changes nothing); timezone/locale follow the VM's region profile. The VNC password rides in the noVNC URL over loopback by default. `--open` opens the system browser, `--size 2560x1440` changes resolution.
- `--public` makes the host relay listen on `0.0.0.0` and returns an `0.0.0.0` URL; use the host's actual LAN/VPN IP when connecting remotely. Anyone who can reach that port can attempt the VNC password. Switching between public and loopback keeps the same desktop session and VNC password.
- Clipboard is enabled by default in noVNC. Open the Clipboard panel on the left and grant clipboard read/write permission in Chrome when prompted; loopback URLs are considered secure contexts. Golden images include a small CJK font set and an IBus pinyin input method, with `ibus-daemon` started in each desktop session. This only adds Chinese display and input support; the default locale remains `en_US.UTF-8`.

### 5. Port maps

```bash
pvm map dev1 8080:80
curl http://127.0.0.1:8080/
pvm map ls
```

- The host listens on loopback only; the path is host relay -> gateway -> VM.
- Deleting a VM cleans up its relays, desktop state, and agent connections.

### 6. DNS and split-routing rules

```bash
# internal domains: resolved via the host's DNS, traffic goes direct
pvm route add internal --domain-suffix corp.internal
# optionally pin a dedicated internal resolver
pvm route add internal --domain-suffix corp.internal --dns 10.0.0.53
# plain direct rules
pvm route add direct --domain-suffix example.com
pvm route add direct --cidr 10.0.0.0/8
# proxy rules: force a domain/CIDR through a specific exit
pvm route add proxy --domain-suffix example.com --via my-exit
# match types: --domain-suffix / --domain-prefix / --domain-keyword / --cidr
pvm route ls
pvm route rm <index>
pvm dns ls
```

- Explicit proxy-rule domains get a fake-ip (198.18.x.x) and use the policy relays. Direct/internal rules return real addresses and use their direct path. Unmatched domains also return real addresses: exit-bound VMs resolve only through their selected tunnel and forward TCP, UDP, and ICMP directly at L3; direct VMs use the host uplink DNS and normal direct forwarding. Unsupported packets are dropped.
- Non-A/AAAA query types for proxied names return NODATA and are never forwarded to the host resolver. PTR for exit-routed real IPs also returns NODATA because there is no in-tunnel PTR resolver. Direct/internal names and CIDRs may still use their designated direct resolver.
- Domain rules only affect resolution that goes through the gateway DNS; apps with hardcoded DoH bypass it, and rules do not apply to them.
- Rules take effect immediately, but established connections are not retroactively switched.

### 7. Stop, delete, clone

```bash
pvm stop dev1
pvm start dev1
pvm rm dev1 -y                  # or --force
pvm clone dev1 dev2             # source VM must be stopped; --no-start clones without booting; --no-wait returns after boot
```

- `pvm rm` double-checks: interactively type y to confirm; **non-interactive/scripts must pass `-y` (or `--force`)** or the command refuses.
- `pvm start` and starting clones wait for guest provisioning before returning; pass `--no-wait` when you only want to boot QEMU and will poll readiness yourself.
- clone inherits mounts, CPU/memory, user, timezone, SMBIOS, and the GPU device; exit routes are not inherited; port maps are not inherited by default but can be interactively carried over with new ports (`--inherit-ports` / `--no-inherit-ports`).
- On first boot a clone regenerates its hostname, machine-id, and SSH host keys so the two VMs do not share an identity.
- Note: the clone reset does not touch AI-client device IDs (the random userID in the client's local config dotfile) — cloning a VM that already ran the client leaves both machines looking like one device server-side. For a fresh device identity, create a new VM (or clone first, then install the client).

### 8. Disk capacity and host space

- VM disks use sparse qcow2 files. `--disk` at create and `pvm resize` set the virtual capacity ceiling; neither allocates the full size on the host up front. `resize` is grow-only and never shrinks the guest partition or filesystem.
- QEMU enables `discard=unmap` and `detect-zeroes=unmap`. Deleting files alone can leave historical allocations and metadata behind, so run `sudo fstrim -av` in the guest before offline compaction for the best result.
- Reclaim host space with an offline sparse conversion:
  ```bash
  pvm ssh dev1 'sudo fstrim -av'
  pvm stop dev1
  pvm compact dev1
  pvm start dev1
  ```
- `pvm compact` requires a stopped VM and refuses an unconsumed RAM snapshot. It validates the new image, virtual capacity, and backing chain, then atomically replaces the original only when the new file uses less host space. It does not enable compression or reduce virtual capacity.

### 9. Snapshots and troubleshooting

```bash
pvm snapshot create dev1        # capture RAM+disk state and stop (VM must be running)
pvm start dev1                  # detects the snapshot and restores quickly; processes resume intact
pvm snapshot rm dev1            # VM must be stopped first
pvm logs dev1 --follow
pvm daemon status
pvm daemon stop && pvm daemon start
```

- Snapshots suit machines left unused for days, or a checkpoint before risky operations. Existing snapshot artifacts are not overwritten implicitly. After a restore, the active restore overlay and any `.bak` hold VM data: snapshot removal or another snapshot is refused. Clone the stopped VM to flatten its current disk before creating a new checkpoint; do not manually delete backing files.
- Restarting the daemon stops user VMs (the gateway recovers automatically); start them again with `pvm start <name>`. `pvm upgrade` does this restart and restores previously running user VMs automatically; use `--no-restart` only when you want to handle that lifecycle yourself.
- If the gateway pvm-gw1 spins at high CPU with SSH unreachable for a long time, recover with `pvm stop pvm-gw1 && pvm start pvm-gw1`. Never delete pvm-gw1 (the daemon recreates it, but cluster networking is down and custom state is lost in the meantime).
- The daemon holds a single-instance lock; on `another pvm daemon is already running`, find the existing daemon instead of spawning a second one.
- A boot watchdog restarts a VM only after two consecutive checks show full CPU with zero console progress; slow TCG boots are left alone.

## Credentials and application identity

- Never copy or mount the host's `~/.gitconfig`, `~/.ssh`, credential stores, or personal files into a VM. Treat `/share` and extra mounts as an explicit disclosure of their contents.
- Fresh VMs initialize OS identifiers. pvm does not generate third-party application IDs; the application controls enrollment. Clone copies user files and credentials, and snapshots preserve them. Use supported application enrollment and revocation procedures.
- Timezone and locale are convenience settings, not evidence of geography or anonymity. Keep application configuration consistent with its supported deployment and the service's terms.
- pvm cannot guarantee a service's trust score, telemetry behavior, paid features, account separation, or ban outcome. See `pvm security` for the security boundaries and actual checks.

## Adding an exit (only when the user explicitly asks)

### Standard flow: deploy an exit over SSH

pvm release binaries are single files downloaded straight from GitHub Releases (tags publish `pvm_linux_amd64.tar.gz` / `pvm_linux_arm64.tar.gz` plus SHA256SUMS). To deploy a new VPS:

```bash
VPS=root@<vps>                                     # target; ssh must be passwordless
TOKEN=$(openssl rand -hex 24)                      # control-plane token

# 1. Confirm Linux and the architecture (x86_64 -> amd64, aarch64 -> arm64)
ssh $VPS 'uname -s; uname -m'

# 2. Download the release binary on the VPS and install it (keep the old one for rollback)
#    amd64 shown; for arm64 use pvm_linux_arm64.tar.gz instead
ssh $VPS '[ -f /usr/local/bin/pvm ] && cp /usr/local/bin/pvm /usr/local/bin/pvm.prev || true'
ssh $VPS 'curl -fsSL https://github.com/huangzheng2016/pvm/releases/latest/download/pvm_linux_amd64.tar.gz | tar xz -C /tmp && install /tmp/pvm /usr/local/bin/pvm'

# 3. Write and start the systemd unit (reuse an existing unit; never overwrite its token)
ssh $VPS 'cat > /etc/systemd/system/pvm-exit-agent.service <<EOF
[Unit]
Description=pvm exit agent
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=/usr/local/bin/pvm exit-agent --listen :18443 --token $TOKEN
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now pvm-exit-agent'

# 4. Confirm the service and its certificate fingerprint (on upgrades the
#    fingerprint must match the previous one; a mismatch means the cert changed)
ssh $VPS 'systemctl is-active pvm-exit-agent && journalctl -u pvm-exit-agent --since "-30s" --no-pager | grep fingerprint'

# 5. Register locally (--fingerprint is the out-of-band check from step 4;
#    --transport defaults to auto: udp+tcp are probed in parallel (N x 8MB
#    tunnel downloads, averaged) and the fastest wins, preferring UDP unless
#    it is below 80% of TCP; --samples N changes the sample count, default 3)
pvm exit add <name> --server <vps>:18443 --token $TOKEN --fingerprint <sha256-fingerprint>

# 6. Verify
pvm exit test <name>
pvm exit test <name> --speed --samples 5
```

Notes:
- Roll back a failed deploy: `ssh $VPS 'mv /usr/local/bin/pvm.prev /usr/local/bin/pvm && systemctl restart pvm-exit-agent'`.
- Existing production exits are already running — do not redeploy a name that is already registered; upgrading the binary uses the same flow (unit and token/fingerprint unchanged).
- Both amd64 and arm64 binaries come from GitHub Releases — no local build needed; verify with the release's `SHA256SUMS` (`sha256sum -c`) when required.
- Control plane: HTTPS + Bearer token + fingerprint pinning. Verify the initial fingerprint through a trusted channel; trust-on-first-use alone does not authenticate the first connection. Data plane: WireGuard, UDP preferred with TCP fallback.
- Speed probes never touch third-party public servers: the exit-agent serves `/speed?bytes=N` (64MB cap) and `/ping` at `.1:18081` on each tunnel NIC, reachable only inside the tunnel. The gateway uses `/ping` as its data-plane health probe.
- Registrations idle for 24h are reaped by the exit-agent; a gateway returning after sleep re-enrolls automatically with its persisted peer key.
- exit-agent NAT backends: `nft` / `iptables` / `userspace`; userspace needs no root and forwards TCP/UDP/ICMP. ICMP uses Linux ping sockets, so insufficient socket permission drops only that ICMP flow and never falls back to another egress.
- Tunnel pools use separate `/16` networks: UDP `10.<base>.0.0/16`, TCP `10.<base+1>.0.0/16` (default base `99`). Set a different `--subnet-base` for multiple agents on one VPS or to avoid VPS-internal networks. A larger pool does not prevent address conflicts. Changing the layout drops stale registrations; expect tunnels to re-enroll.

### Add a Cloudflare WARP exit without a VPS

```bash
pvm exit add-warp my-warp
pvm exit test my-warp
pvm route dev1 --via my-warp
```

- `add-warp` registers a device directly through Cloudflare's API. pvm does not install `warp-cli` or `wgcf`, does not generate a `wgcf-profile.conf`, and does not modify any VM image.
- The private key and device token stay in `~/.pvm/exits/my-warp.json` with mode `0600`. Only the referenced exit's WireGuard data-plane parameters are pushed to the managed gateway; the Cloudflare API token is not.
- If registration fails and remote rollback also fails, pvm preserves recovery credentials in a mode-`0600` `<name>.warp-pending` file and refuses to register that name again until it is restored or revoked. Treat that file as a secret.
- Registration alone changes no routes. Bind a VM explicitly with `pvm route`, or use `pvm route default --via my-warp` deliberately. `pvm exit rm my-warp` removes the local entry and makes a best-effort Cloudflare device deletion.
- WARP uses UDP WireGuard only. `pvm exit test` is handshake-only for WARP; the self-contained TCP/UDP/ICMP probes and VPS-only `--speed` / `client-config` are not available. Default-exit and explicit fake-ip split domains resolve through `1.1.1.1` inside the WARP tunnel, never through host DNS.
- WARP egress location can vary and has no pvm VPS timezone probe. Set the intended region explicitly with `--tz` / `--locale`, or later with `pvm exit set`, before routing a VM.

### Connect a standard WireGuard client

```bash
pvm exit client-config my-exit --output phone.conf
```

- Use a UDP exit; the custom TCP transport is not compatible with standard WireGuard clients.
- The command enrolls a new client key, writes a mode-`0600` config, and prints a QR code for mobile apps. Run it once per device and never share one profile across devices.
- The output path must not already exist. The profile routes IPv4 through the VPS with persistent keepalive; it does not include pvm's fake-IP DNS or split-routing rules. IPv6 is not tunneled; disable client IPv6 if direct IPv6 egress is unacceptable.
- Both the config and QR code contain a private key. Treat terminal scrollback and exported files as secrets.
