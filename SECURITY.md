---
name: pvm-security
description: Review pvm's host, guest, image, credential, and network security boundaries, and verify the configuration of a running VM.
---

# pvm security: boundaries and verification

`pvm security` prints this guide. pvm provides local QEMU development VMs and configurable network routing. It does not guarantee anonymity, an undetectable VM, a new third-party application identity, or any service's account or risk-control outcome.

## Threat model

The host user, host OS, pvm daemon, QEMU, guest kernel, gateway, image publishers, and selected exit provider are trusted components. Host administrators can read VM disks, memory and credentials. A compromised gateway or exit can observe network destinations and unencrypted traffic; application TLS remains necessary.

VMs in one cluster share a gateway and a layer-2 network. The gateway validates configured source IP/MAC pairs, but these are not cryptographic identities or per-port source bindings. A guest administrator can imitate another registered VM's pair. Do not mix mutually untrusted guests or rely on these checks to contain a hostile guest with root access. Use separate host environments and independently controlled networks for that threat model.

## Host and guest privileges

The host `pvm` binary exposes the complete pvm control plane and acts with the host user's filesystem and process permissions. It can manage all local VMs, cluster routes, host-path mounts, exit registrations, and stored credentials. Access to the host account or pvm data directory is therefore not limited to one guest.

The Linux guest uses a separate binary with only `agent`, `map`, and `skill`. It has no host lifecycle, route, mount, exit, or `security` commands. Every local guest user can use `map`; requests carry no VM name. The daemon derives the VM identity from that guest's virtio-serial channel and limits list, add, and remove operations to maps owned by that VM. These maps still create host loopback listeners into the current VM.

This reduced command surface is not an OS privilege sandbox. The guest agent service runs as root and accepts exec requests from the host, so the host retains full control of the guest. `pvm security` is a host command and is intentionally absent from the guest binary.

## Shared files and cache consistency

All user VMs receive a read-write `/share` backed by the same pvm share directory. Extra mounts use `security_model=none` and `access=any`, with the guest uid aligned to the host uid, and expose host files with the host user's privileges. Shared files can transmit secrets, executable code, and identifiers between VMs. Neither a separate disk nor a fresh hostname makes shared content safe. Mount only data that the guest is allowed to read and change; do not place host SSH keys, git configuration, credentials, or personal files in shared directories.

The `coherent` mount profile uses `cache=none` and preserves the bidirectional-visibility baseline for files actively read and written on both sides. It does not provide multi-writer transaction consistency.

With `readahead`, if the host rewrites or replaces a file while an old guest file descriptor remains open, even a newly opened guest descriptor can see old contents until the old descriptor is closed. Do not use this profile when that visibility delay is unacceptable.

## Network policy and its limits

- With an exit selected, unmatched IPv4 traffic is intended to use that tunnel. A failed tunnel must leave that traffic offline rather than switch it to direct egress. A successful WireGuard handshake alone does not prove DNS, TCP, UDP, or ICMP forwarding works.
- Explicit `direct`, `internal`, and bypass rules are exceptions. They may send traffic and DNS through the host uplink even when the VM has a default exit. Inspect the complete rule set before treating a VM as exit-only.
- Default-exit domains resolve through the exit to real IPv4 addresses. Explicit proxy-domain rules use fake-IP mappings. Supported exit DNS queries must not silently fall back to the host resolver.
- The managed cluster is IPv4-only and drops forwarded IPv6. Exported standard WireGuard client configurations cover IPv4 only; the client must separately disable or contain IPv6.
- Domain rules affect only resolution through the gateway DNS. Applications with hardcoded DoH can bypass those rules. Blocking known public DoH/DoQ resolver addresses cannot block every encrypted DNS service, and custom HTTPS resolvers may still be reachable through the selected route. pvm does not decrypt HTTPS to classify it.
- Loopback host port maps are accessible to other processes running on the host.
- The desktop session has no separate XFCE login and screen locking is disabled; the VNC password is carried in the noVNC URL. `pvm desktop --public` explicitly exposes the desktop to the network, and anyone who can reach that port can attempt the password. Its HTTP/noVNC connection is not TLS-protected by pvm; use a trusted network or an authenticated encrypted tunnel. Desktop URLs contain secrets and must not be shared.

Routing policy is a security goal with regression coverage, not a proof that every host/kernel/QEMU configuration is leak-free. Test the actual deployment, including tunnel loss, before relying on it for sensitive traffic.

## Images and bake

Base downloads are checked against checksums fetched from the distribution over HTTPS. This detects corrupt or inconsistent downloads; pvm does not independently verify a distribution signing key, and trusts the HTTPS endpoint and host trust store. Golden images additionally contain packages installed from the base image's configured repositories. Bake does not turn an untrusted base image into a trusted one. For manual release verification, check a downloaded release archive against the release's `SHA256SUMS` before installing it.

A build VM uses host/slirp egress and may use explicitly configured host proxy environment variables. It does not inherit the final VM's selected exit route. Proxy credentials must not be printed by shell tracing and temporary proxy configuration is removed before a successful image is sealed. Failed builds kept with `--keep-vm` can retain credentials and provisioning logs; treat them as sensitive. Removing a file inside qcow2 is not forensic erasure of all backing blocks.

A successful bake resets SSH host keys, machine-id and cloud-init state and removes the build user. It does not reset arbitrary application enrollment, revoke credentials, or certify every file in the image. Interrupted build VMs can contain sensitive state and are preserved for explicit inspection and removal rather than automatically deleted based on a name prefix.

Golden versions are retained as backing images. Do not edit, replace, or remove a golden image while any VM or snapshot depends on it. A forced guest shutdown must not be treated as successful image sealing.

## Credentials and identity

pvm stores its dedicated host SSH key under the pvm data directory. It does not automatically copy the host's `~/.ssh` or `~/.gitconfig` into guests. Explicit mounts, files placed in `/share`, SSH options, and commands run by the user can still expose host data. Managed SSH, SCP and gateway hops ignore personal SSH configuration, offer only the pvm key, and disable host-agent use and forwarding. Manually invoked SSH can use different options; review its actual command and environment.

The exit control plane uses HTTPS, a bearer token, and certificate fingerprint pinning; the data plane uses WireGuard. Verify the initial fingerprint through a trusted channel: trust-on-first-use alone does not authenticate the first connection. During upgrades, preserve the existing service unit's token and fingerprint, and investigate a certificate fingerprint mismatch rather than blindly pinning the new value.

Exit certificates, WireGuard keys, API tokens, desktop passwords, and snapshots are secrets. Keep the pvm data directory private, back it up securely, and never publish raw configuration files, `wg` private-key output, or full diagnostic dumps. Existing credentials must not be silently replaced when unreadable or inconsistent; investigate and restore the expected files.

WARP registration stores the private key and device token in `~/.pvm/exits/<name>.json` with mode `0600`. Only the referenced exit's WireGuard data-plane parameters are pushed to the managed gateway; the Cloudflare API token is not. If registration fails and remote rollback also fails, pvm preserves recovery credentials in a mode-`0600` `~/.pvm/exits/<name>.warp-pending` file and refuses to register that name again until the device is restored or revoked. Treat both files as secrets and follow the recovery instruction instead of deleting the pending state or reusing the name.

Each exported standard WireGuard client profile and its QR code contain a private key. Use a separate profile for each device and never share one profile across devices. Treat the exported file, terminal scrollback, and QR code as secrets.

A fresh VM initializes OS machine identifiers. Clone regenerates its hostname, machine-id, and SSH host keys on first boot, but copies user files and may preserve application-specific identifiers such as an AI client's random userID, tokens, browser profiles, and cache state. A snapshot restores the same identity and secrets. To avoid copying an existing client enrollment, create a fresh VM or clone before installing the client, then use the application's supported enrollment and credential-management process. Do not assume clone or a MAC/hostname change provides a new application identity. pvm cannot prevent account correlation through OAuth, billing, IP reputation, shared files, or application telemetry.

Timezone and locale are convenience settings, not evidence of geography or anonymity. Keep application configuration consistent with its supported deployment and the service's terms. pvm cannot guarantee a service's trust score, telemetry behavior, paid features, account separation, or ban outcome.

## Verify a running environment

Use mock names below with the actual deployment's names. Keep real addresses, hostnames, tokens and command output private.

```bash
pvm daemon status
pvm list
pvm route ls
pvm exit ls
pvm mount ls dev1
pvm audit dev1
pvm exit test my-exit
```

Check the route's default, per-VM override, explicit domain/CIDR exceptions, registered exit, and mount source paths. `pvm audit` checks several network protocols, but is a point-in-time test and does not simulate all outages. WARP testing has narrower coverage than a pvm VPS exit.

For an offline guest, `pvm exec dev1 <cmd...>` uses the agent channel and requires no guest networking. Readiness states differ: a running QEMU process is not proof that cloud-init, SSH, the exec agent, or the cluster gateway is ready.

For tunnel-loss testing, use a disposable environment and an exit you control. Observe IPv4 traffic and DNS at the host uplink while the exit is unreachable; check that exit-bound flows fail and no direct fallback occurs. Test IPv6, non-A/AAAA DNS, hardcoded resolver requests, split rules, and long-lived connections. Do not stop a shared production exit as an audit shortcut. Restore the exit rather than switching a sensitive VM to `direct`.

Review guest enrollment and secrets before sharing a clone or snapshot. Review `/share` and every extra mount before running untrusted code. Keep QEMU, base images, guest packages, and pvm current; rebuild golden images to update future VMs, and update existing guests separately.
