---
name: pvm
description: Create and manage isolated Linux dev VMs with pvm, and route all of a VM's (or the whole cluster's) traffic through a VPS exit-node. Use when you need a clean/disposable dev machine, isolated device or network fingerprints, an environment that exits only via a specific VPS, a browser desktop, port maps, snapshots, or clones.
---

# pvm: isolated dev VMs and egress control

pvm is a QEMU virtual machine system for the local machine (single Go binary). A daemon runs permanently; the CLI auto-spawns it when needed (exception: after an explicit `pvm daemon stop` it stays down until `pvm daemon start`). Everything goes through the `pvm` command — never touch QEMU directly.

## Architecture boundary

pvm supports only the host's native architecture: darwin/arm64 runs arm64 VMs, linux/amd64 runs amd64 VMs. Build artifacts contain guest binaries and the QEMU runtime for the current host architecture only — no cross-arch emulation or packages.

## Prerequisites

- If pvm is on PATH, use `pvm ...`; inside the repo, use `./pvm ...` from the repo root.
- Run `pvm daemon status` first to confirm the daemon is healthy; `pvm list` shows existing VMs.
  - If the status shows `warning: N vm(s) running under tcg fallback`, the host's hardware accelerator is unavailable (on macOS this means Hypervisor.framework is wedged after long uptime — reboot the Mac to restore full speed). VMs work meanwhile, just slowly.
- On first use, `pvm image list` to confirm a base image is present; if missing, `pvm image fetch <distro:version>` (fetch is checksum-cached; `--force` re-downloads). create fails with a clear hint when the base image is absent.
- Example exits: `my-exit` (WireGuard over UDP) and `my-exit-tcp` (WireGuard over TCP). Run `pvm exit ls` to see the names configured on the current host and do not re-add an existing name.
- The `pvm-exit-agent.service` on the VPS must stay running. Never stop it unless you are deliberately testing the kill-switch or upgrading.

## Common choices

- **Images**: `ubuntu:26.04`, `ubuntu:24.04`, `debian:13`, `debian:12`
- **Exit providers**: pvm exit-agent (`udp` default / `tcp` fallback) or Cloudflare WARP (`udp` only); see `pvm exit add-warp` below
- **exit-agent NAT backend**: `auto` (probe) / `nft` / `iptables` / `userspace` (no root, no ICMP)
- **Route targets**: `direct` (host egress) or an exit name such as `my-exit`
- **create flags**: `--image`, `--distro`, `--version`, `--name`, `--user`, `--cpus`, `--mem`, `--disk`, `--mount`, `--tz`, `--locale`, `--git-email`, `--git-name`, `--smbios-spoof`, `--no-start`, `--no-wait`

### Directory mounts (--mount and /share)

- Every VM mounts the host's `~/.pvm/share` at `/share` (9p, **bidirectional read-write**, including subdirectories). Drop files there for quick exchange — zero configuration.
- Extra mounts: `--mount <host-path>:<vm-path>`, repeatable:
  `pvm create --image ubuntu:24.04 --name dev1 --user dev --mount ~/work/proj:/home/dev/proj --mount ~/data:/mnt/data`
- Mounts are virtio-9p (security_model=none + access=any, guest uid aligned with host uid). Good for source trees and documents; not for high-IOPS database directories.
- `--image debian:13` and `--distro debian --version 13` are equivalent; conflicting flags are rejected.
- **`pvm image build` proxy passthrough**: golden builds honor the host's `HTTP_PROXY/HTTPS_PROXY/ALL_PROXY/NO_PROXY` (loopback proxy addresses are rewritten to a VM-reachable one) and clean up thoroughly — no proxy config is baked into the image.

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
- SSH uses a pvm-dedicated keypair, no password. `pvm ssh dev1` opens an interactive shell (a shell command such as `bash` also gets a TTY when run from a terminal); `pvm ssh dev1 '<cmd>'` runs one command. When SSH is unreachable the virtio-serial agent channel takes over automatically (or use `pvm exec dev1 <cmd...>` explicitly — it needs no network).
- create/start pushes the VM's IP/MAC mapping to the gateway DHCP **synchronously** before booting (new VMs are reachable immediately); if the gateway is unreachable, start fails instead of booting a network-less VM.
- Cluster addressing: VMs live in `100.99.<cluster>.0/24` (CGNAT space), the gateway is `100.99.<cluster>.1`.

### 2. Route all of a VM's traffic through the VPS

```bash
pvm exit ls
pvm route dev1 --via my-exit     # UDP preferred
pvm route dev1 --via my-exit-tcp # TCP fallback
pvm ssh dev1 'curl -s ipinfo.io'
```

- The `ipinfo.io` IP, city, and timezone should match the VPS location; the region profile automatically switches the VM's timezone/locale to the exit region (`en_US.UTF-8`, applied over SSH — skipped if the VM's SSH is down; rerun `pvm route` to retry).
- Restore host egress: `pvm route dev1 --via direct`.
- **Fail-closed**: when the exit tunnel is down, VMs bound to it lose all networking — this is intentional leak protection. Do not "fix" it by switching to direct; the VM recovers once the VPS service is back.
- Switching `--via` causes a seconds-long network blip (tunnel enroll/handshake convergence); a dead tunnel rebuilds within ~60s, during which bound VMs stay offline (expected).
- DNS for proxied domains is resolved only through the selected exit (on the VPS or through the WARP tunnel), never through host DNS. Check tunnel health with `pvm exit test my-exit` (stale pvm peers re-enroll automatically).

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
pvm desktop dev1 --stop
```

- Requires a VM built from a **golden image** (XFCE + TigerVNC + noVNC preinstalled). Base images lack the desktop packages and `pvm desktop` says so (a 503 means the VM is still booting — retry shortly).
- This is the VM's own desktop (not a container): the filesystem, users, and processes are the same machine you see over SSH, including the `/share` mount.
- The desktop is passwordless and XFCE screen locking is disabled. The session persists inside the VM (closing the browser changes nothing); timezone/locale follow the VM's region profile. The VNC password rides in the noVNC URL over loopback only. `--open` opens the system browser, `--size 2560x1440` changes resolution.
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

- Domains matching no internal/direct/proxy rule get a fake-ip (198.18.x.x). **Exit-bound VMs**: domains are resolved only through the selected tunnel (by the VPS exit-agent or WARP's in-tunnel resolver). **Direct VMs**: the gateway resolves via the host uplink DNS at connect time and dials on their behalf.
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

### 8. Snapshots and troubleshooting

```bash
pvm snapshot create dev1        # capture RAM+disk state and stop (VM must be running)
pvm start dev1                  # detects the snapshot and restores quickly; processes resume intact
pvm snapshot rm dev1            # VM must be stopped first
pvm logs dev1 --follow
pvm daemon status
pvm daemon stop && pvm daemon start
```

- Snapshots suit machines left unused for days, or a checkpoint before risky operations.
- Restarting the daemon stops user VMs (the gateway recovers automatically); start them again with `pvm start <name>`. `pvm upgrade` does this restart and restores previously running user VMs automatically; use `--no-restart` only when you want to handle that lifecycle yourself.
- If the gateway pvm-gw1 spins at high CPU with SSH unreachable for a long time, recover with `pvm stop pvm-gw1 && pvm start pvm-gw1`. Never delete pvm-gw1 (the daemon recreates it, but cluster networking is down and custom state is lost in the meantime).
- The daemon holds a single-instance lock; on `another pvm daemon is already running`, find the existing daemon instead of spawning a second one.
- A boot watchdog restarts a VM only after two consecutive checks show full CPU with zero console progress; slow TCG boots are left alone.

## Security and identity posture (anti-risk-control)

- Do not set `DISABLE_TELEMETRY` or similar kill switches for the AI client's nonessential traffic: disabling is itself a strong signal concentrated in a specific user population, and it downgrades paid features. Keep defaults inside VMs.
- Do not set `ANTHROPIC_BASE_URL` or reach the service through any AI gateway/proxy (LiteLLM, Helicone, Portkey, etc.) — the gateway host travels upstream in plaintext and gateway-flavored response headers are a fingerprint. pvm already exits via the VPS; stacking a relay only adds signals.
- Keep one service/account on one consistent egress; do not flip between direct and VPS exits. Keep activity hours plausible for the exit's geography — 24/7 nonstop automation lets the server infer your real timezone.
- Never copy the host's `~/.gitconfig`, `~/.ssh`, or personal files into a VM; the pvm keypair is isolated from host SSH.
- VM timezone/locale should match the exit geography; use stock Ubuntu/Debian and official package mirrors, and avoid regional mirrors or CJK locale artifacts.
- gitStatus rides along with every request: beyond `user.name/email`, the branch name, `git status` file names, and the latest ~5 commit subjects are included — keep branch/commit/file names in VMs free of your native language.
- For a fresh device identity, create a new VM (each new VM gets a fresh random device ID in the client's local config dotfile). clone is a full disk copy: machine identifiers (hostname/machine-id/SSH host keys) are regenerated, but user files carry over — a machine that already ran the client stays the same device server-side after cloning. The right order is clone first, then install the client (or delete the client's config dotfile yourself afterwards). Restoring a snapshot means the same device.

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
- Control plane: HTTPS + Bearer token + TOFU fingerprint pinning. Data plane: WireGuard, UDP preferred with TCP fallback.
- Speed probes never touch third-party public servers: the exit-agent serves `/speed?bytes=N` (64MB cap) and `/ping` at `.1:18081` on each tunnel NIC, reachable only inside the tunnel.
- exit-agent NAT backends: `nft` / `iptables` / `userspace`; userspace needs no root but does not forward ICMP ping.
- Tunnel pools use separate `/16` networks: UDP `10.<base>.0.0/16`, TCP `10.<base+1>.0.0/16` (default base `99`). Set a different `--subnet-base` for multiple agents on one VPS or to avoid VPS-internal networks. A larger pool does not prevent address conflicts. Changing the layout drops stale registrations; expect tunnels to re-enroll.

### Add a Cloudflare WARP exit without a VPS

```bash
pvm exit add-warp my-warp
pvm exit test my-warp
pvm route dev1 --via my-warp
```

- `add-warp` registers a device directly through Cloudflare's API. pvm does not install `warp-cli` or `wgcf`, does not generate a `wgcf-profile.conf`, and does not modify any VM image.
- The private key and device token stay in `~/.pvm/exits/my-warp.json` with mode `0600`. Only the referenced exit's WireGuard data-plane parameters are pushed to the managed gateway; the Cloudflare API token is not.
- Registration alone changes no routes. Bind a VM explicitly with `pvm route`, or use `pvm route default --via my-warp` deliberately. `pvm exit rm my-warp` removes the local entry and makes a best-effort Cloudflare device deletion.
- WARP uses UDP WireGuard only. `pvm exit test` checks handshake, in-tunnel DNS, and observed egress IP; VPS-only `--speed` and `client-config` are not available. Fake-IP domains resolve through `1.1.1.1` inside the WARP tunnel, never through host DNS.
- WARP egress location can vary and has no pvm VPS timezone probe. Set the intended region explicitly with `--tz` / `--locale`, or later with `pvm exit set`, before routing a VM.

### Connect a standard WireGuard client

```bash
pvm exit client-config my-exit --output phone.conf
```

- Use a UDP exit; the custom TCP transport is not compatible with standard WireGuard clients.
- The command enrolls a new client key, writes a mode-`0600` config, and prints a QR code for mobile apps. Run it once per device and never share one profile across devices.
- The output path must not already exist. The profile routes IPv4 through the VPS with persistent keepalive; it does not include pvm's fake-IP DNS or split-routing rules. IPv6 is not tunneled; disable client IPv6 if direct IPv6 egress is unacceptable.
- Both the config and QR code contain a private key. Treat terminal scrollback and exported files as secrets.
