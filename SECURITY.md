---
name: pvm-security
description: Audit and remediate a pvm VM's risk-control posture. Use when you need to audit or harden a VM against AI-client risk control, switch the VM to a different account, provision a fresh device identity or environment, or investigate a suspected account-linkage ban.
---

# pvm security: risk-control posture and audit manual

How AI coding tools' client-side risk control works, what pvm's design already does about it, what remains your responsibility, and how to audit a running VM. Field and mechanism names are kept for verifiability. `pvm security` prints this document.

## The one rule that matters

Blend in, don't vanish. These systems score deviation from a normal paying user, not any single fingerprint. The strongest signals are the ones you create by trying to hide:

- `DISABLE_TELEMETRY` and friends cascade into feature-flag loss (paid-tier features silently degrade) and are themselves a trust-tier signal.
- Killing the attribution header makes auto mode entirely unusable - the off switch is deliberately expensive.
- Zero telemetry with continuous API usage is a louder signal than telemetry ever was.

## The signal layers (what the client reports)

- **Attribution.** Every API request carries an attribution header: `cc_version` (version + 3-char fingerprint derived from a hardcoded salt, characters of the first user message, and the version), `cc_entrypoint`, and `cch` - a native attestation computed by the client binary that modified clients and proxies cannot reproduce. Cross-repo attribution exists (the official kill switch proves it).
- **Identity.** The device ID (64-hex in the client's local config dotfile) persists across sessions AND accounts. `account_uuid` merges devices into one account server-side; the same account on many device IDs is the "account sharing" verdict. Device trust goes further: enrolled-device requirements, revocable device keys in the OS keychain, per-event signatures, trusted-device tokens, and host profiles (OS, arch, GPU, installed dev tools, MCP server names, hostname).
- **Telemetry channels.** Datadog, first-party event logging, GrowthBook (feature flags + user profile: email, deviceID, accountUUID, subscriptionType, apiBaseUrlHost), plus RUM and OTel exports. These connect directly from the client to official endpoints - no relay or proxy can intercept them, which is why "just use a gateway" never works.
- **Environment probing.** Platform/arch/terminal/distro/package manager/runtimes; region-specific lists (npmmirror, aliyun, tuna mirrors, Aliyun metadata addresses); cloud metadata probing; git status collection; compliance taints (a tainted session loses model-chosen URL egress); geo-consistency checks at login (public IP vs declared network).
- **Proxy/relay detection.** Known gateway response headers (litellm, helicone, portkey, cloudflare, kong, ...); base-URL hostname extraction (setting `ANTHROPIC_BASE_URL` to your own gateway is classified as a custom endpoint and hard-blocked for orgs); Cloudflare mitigation signals; prompt-cache billing inconsistencies that expose relays; TLS/HTTP2 fingerprinting.
- **Content-level anti-cheat.** The client scans assistant output for third-party harness markers, runs classifiers over your config files for "delegate work to subagents" patterns, sends auto-mode command context to server-side review (fail-closed), and distinguishes human-typed prompts from automation.
- **Behavior.** Activity hours, request cadence, 24/7 uptime, concurrency shape, cache-hit patterns. The layer no byte-editing can fix - and the layer most bans ultimately come from.

## What pvm already does for you

- **A real machine, not a container.** Environment detection looks for `/.dockerenv` (docker signal) and `/sys/hypervisor/uuid` (Xen/EC2 signal); pvm's QEMU VMs have neither and present as an ordinary Ubuntu machine. Run tools in the VM's main userland - NOT inside a docker container in the VM (that reintroduces `/.dockerenv`). `pvm desktop` is the same machine as SSH, not a container.
- **One consistent egress, fail-closed.** `pvm route <vm> --via <exit>` sends all VM traffic through the selected VPS or Cloudflare WARP tunnel. Tunnel down = VM offline; it never falls back to the host egress (no IP jumps). Proxied domains are resolved by the exit-agent on the VPS or through the WARP tunnel's resolver; the gateway never uses host DNS for them. Well-known public DoH/DoQ resolvers are blocked on the bridge (their :53 is hijacked to the gateway, so ordinary resolution is unaffected; only hardcoded DoH channels close). The cluster is IPv4-only (IPv6 egress dropped). Bridge-to-uplink is default-deny: only registered MAC/IP pairs of direct VMs and bypass destinations pass, so a guest forging its IP/MAC cannot bypass the kill-switch.
- **Real-IP full-tunnel DNS.** Default-exit domains resolve through the selected exit to real IPv4 addresses and use direct L3 tunnel forwarding; only explicit domain proxy rules use fake-ip. Ordinary full-tunnel DNS logs therefore do not show the `198.18.0.0/16` fake-ip fingerprint, although routing, resolver, and exit-IP fingerprints still remain.
- **Internal DNS split-horizon.** `pvm route add internal --domain-suffix corp.example` resolves internal domains via the host's own DNS (works naturally inside office networks/VPNs); `--dns <srv>` optionally pins a resolver - only matching queries ever reach it. Internal domains and their traffic stay direct and never enter the tunnel.
- **Region profile.** `pvm exit add/set <name> --tz <iana> --locale en_US.UTF-8` registers an exit's geography; routing a VM through it applies the exit's timezone and locale over SSH (timezone appears in model prompts - it must match the exit IP's geography). `--via direct` restores the previous values. `pvm create --tz/--locale` sets them at create time; defaults are neutral (UTC + en_US.UTF-8).
- **Identity hygiene.** pvm never reads or propagates host `~/.gitconfig` or `~/.ssh`; VM git identity comes only from explicit `--git-email/--git-name`. Every new VM gets a fresh device identity (hostname, machine-id, SSH host keys, and the client's random device ID). Clone regenerates machine identifiers but copies user files - clone first, then install the client (see "Fresh device identity"). Keep VM names neutral - trusted-device display names carry the hostname. `--smbios-spoof` presents commodity PC DMI (evidence says these fields do not travel upstream; off by default). OpenSSH's `SendEnv LANG LC_*` can leak a host's CJK locale into interactive shells - override with `ssh -o SetEnv=LANG=en_US.UTF-8` when it matters.
- **Neutral images.** Golden images provision from official package mirrors only - no regional mirrors, no regional locale artifacts.
- **Non-QEMU MACs.** Cluster MACs are assigned by the daemon with a Dell OUI (10:65:30), not QEMU's default 52:54:00.

## What only you can do

- Do not disable telemetry: no `DISABLE_TELEMETRY`, the client's nonessential-traffic kill switches, OTel kill switches, `DO_NOT_TRACK`, or DNS-blocking telemetry domains. Keep client defaults.
- Do not set `ANTHROPIC_BASE_URL` or use LiteLLM/Helicone/Portkey/one-api relays inside the VM. The pvm exit already puts you on a clean IP; a relay adds plaintext fingerprints for zero benefit (the relay host travels upstream via GrowthBook, and gateway response headers are detected).
- One account, one egress, one VM. Don't hop a VM between direct and exit routes, don't run one account from many environments, don't share one VM across accounts. Device identity cannot save you from account-layer correlation (OAuth identity, payment, IP reputation).
- Keep activity plausible: working hours that match the exit's timezone, pauses between sessions, no 24/7 unattended automation (the server infers your real timezone from activity distribution regardless of client timezone), and no quota-wall ramming - when limited, wait for reset; retries are themselves a signal.
- Neutral content habits: English branch names, commit subjects and file names in VM repos (branch, dirty files, and the last ~5 commit subjects ride every request via gitStatus); a noreply git email; no third-party harnesses wrapping the client; no fabricated dialogue turns; normal config-file wording (classifiers flag "delegate to subagents" patterns).
- Update the client - version floors (`version_below_minimum`) retire old clients, and modified clients fail integrity checks.

## Audit a VM

Run guest checks through `pvm ssh <vm> '<cmd>'`; when the VM's network is down (kill-switch engaged), use `pvm exec <vm> <cmd...>` instead - it needs no networking. Each item: check -> read -> fix. A clean item needs no action.

### 1. Egress route and exit IP

```bash
pvm route ls                       # host: the VM's --via target
pvm ssh dev1 'curl -s ipinfo.io'   # guest: ip, city, timezone
```

- Check: the route shows the intended exit, not `direct`; the ipinfo.io city and timezone match the VPS location.
- Fix: `pvm route dev1 --via <exit>` (also re-applies the region profile); `pvm exit test <exit>` if the tunnel looks dead. Never "fix" an offline exit-bound VM by switching to direct - that is the fail-closed kill-switch working.

### 2. Timezone vs exit region

```bash
pvm exit ls                             # host: the exit's TZ column
pvm ssh dev1 'cat /etc/timezone; date'  # guest: IANA name and wall clock
```

- Check: `/etc/timezone` equals the exit's TZ; the wall clock shows plausible working hours in that region (timezone appears in model prompts and must match the exit IP's geography).
- Fix: `pvm exit set <exit> --tz <iana>`, then `pvm route dev1 --via <exit>` again to re-apply (the apply is skipped if the VM's SSH was down at route time - rerunning `pvm route` retries it).

### 3. Locale

```bash
pvm ssh dev1 'locale'
```

- Check: `LANG=en_US.UTF-8` (or the exit's registered locale); no CJK locale leaked in through SSH `SendEnv LANG LC_*`.
- Fix: `pvm exit set <exit> --locale en_US.UTF-8` and re-route; for interactive shells use `ssh -o SetEnv=LANG=en_US.UTF-8`.

### 4. Forbidden environment variables

```bash
pvm ssh dev1 'grep -sE "DISABLE_TELEMETRY|DO_NOT_TRACK|ANTHROPIC_BASE_URL|OTEL_" /etc/environment ~/.bashrc ~/.profile /etc/profile.d/* 2>/dev/null'
```

- Check: empty output. Telemetry kill switches, OTel exporter overrides, and `ANTHROPIC_BASE_URL` must not exist anywhere.
- Fix: delete the offending lines and re-login; keep client defaults.

### 5. Regional mirror sources

```bash
pvm ssh dev1 'grep -rnsE "npmmirror|aliyun|tuna|ustc" /etc/apt/sources.list /etc/apt/sources.list.d/ ~/.npmrc /etc/pip.conf ~/.pip/pip.conf ~/.condarc 2>/dev/null'
```

- Check: empty output. Regional mirrors are on the client's region-probing lists and contradict the exit's declared geography.
- Fix: point the source back to the official mirror (`archive.ubuntu.com`, `deb.debian.org`, registry.npmjs.org, pypi.org). Golden images ship clean - a hit here means someone edited the VM.

### 6. /etc/hosts telemetry blocking

```bash
pvm ssh dev1 'cat /etc/hosts'
```

- Check: only the default `127.0.0.1 localhost` and `127.0.1.1 <hostname>` entries. No service or telemetry domains pinned to `0.0.0.0` or `127.0.0.1` - DNS-blocking telemetry domains is a compliance taint.
- Fix: remove the pinned lines.

### 7. AI relay processes

```bash
pvm ssh dev1 'ps aux | grep -Ei "litellm|helicone|portkey|one-api|new-api" | grep -v grep'
```

- Check: empty output. No local gateway or relay between the client and the service - the pvm exit already puts the VM on a clean IP.
- Fix: kill the process and remove its unit if it persists across reboots (`systemctl list-unit-files | grep -i <name>`); also clear `ANTHROPIC_BASE_URL` (item 4).

### 8. git identity

```bash
pvm ssh dev1 'git config --global user.email; git config --global user.name'
```

- Check: a noreply email (the `--git-email` value); a neutral name. A real email ties the VM to a person.
- Fix: `git config --global user.email <noreply-address>`; keep branch names, file names, and commit subjects in English - they ride every request via gitStatus.

### 9. Host-sensitive mounts and files

```bash
pvm mount ls dev1
pvm ssh dev1 'grep 9p /proc/mounts'   # expect pvmshare /share + declared pvm mount targets
pvm ssh dev1 'ls ~/.ssh ~/.gitconfig 2>/dev/null'
```

- Check: 9p mounts show only `pvmshare /share` and the targets declared by `pvm mount` - no mount maps host `~/.ssh`, `~/.gitconfig`, or other personal paths. The VM's own `~/.ssh` holds only VM-generated keys; `~/.gitconfig` exists only if it was set inside the VM.
- Fix: remove the offending mount with `pvm mount rm dev1 <guest-path>`; delete any copied host files. pvm itself never propagates host `~/.ssh` or `~/.gitconfig`.

### 10. MAC OUI

```bash
pvm ssh dev1 'cat /sys/class/net/*/address'
```

- Check: every interface starts with `10:65:30` (the daemon-assigned OUI). `52:54:00` is QEMU's default OUI - a bare virtualization fingerprint.
- Fix: nothing to change inside the guest (MACs are assigned by the daemon); report the finding.

## Fresh device identity

Device identity lives in the client, not the OS. The client stores a random device ID (64-hex) in a config dotfile in the user's home and persists more state under `~/.cache`; revocable device keys also land in the OS keychain. Machine identifiers (hostname, machine-id, SSH host keys) are NOT the device ID - regenerating them does not create a new device.

Correct order when switching accounts or starting clean:

1. Preferred: `pvm create` a new VM and install the client there - the first run generates a fresh random device ID.
2. Reusing a prepared VM: clone FIRST, then install the client (`pvm clone` regenerates hostname/machine-id/SSH host keys but copies user files). Cloning after the client ran leaves both VMs as one device server-side.
3. Fixing an already-duplicated VM: stop the client, delete its config dotfile in the client's home directory and its persisted data under `~/.cache`, then relaunch - it re-enrolls as a new device. The revocable device key in the OS keychain must be removed by the user manually; an agent cannot do it for them.

A RAM snapshot restore is the same device by design - snapshot/restore never mints a new identity. If the old device is burned, do not restore its snapshots onto the new account's path.

## What cannot be fixed (accept it)

`cch` attestation, account-layer correlation (OAuth, payment fingerprint, IP reputation), and the server-side remote control plane (kill switches, signed config documents, managed model catalogs) are outside any local or relay-side fix. Compliant, ordinary usage on a clean machine with a consistent egress is the only stable strategy - which is exactly what pvm automates.
