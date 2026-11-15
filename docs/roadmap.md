# Roadmap

Planned work for the next release of this fork. Items move to the release notes once they ship.

## Planned

- **`pvm exit-agent install|uninstall`.** Register the exit agent as a systemd service with one command, like `pvm daemon install` on the host. It writes `pvm-exit-agent.service` with the given `exit-agent` flags, refuses when another agent already holds the ports, keeps the existing token file and certificate so registered hosts need no re-enrollment, and runs `systemctl enable --now`. `uninstall` stops and removes the unit and leaves the token, certificate and registrations in place. Today the unit is written by hand; see [SKILL.md](../SKILL.md#standard-flow-deploy-an-exit-over-ssh).
- **`pvm app`: VM programs and web pages as host windows.** Implemented on master through Xpra over the new `pvm.stream` channel, with focus-gated clipboard and opt-in drops; `pvm desktop` moved to `pvm.stream` as well. Tested on native Windows with WHPX. Still open: include the Xpra client in the Windows installers (needs a pinned, signature-checked download in the release job and a test of both installers), the host-to-guest clipboard bridge for macOS and Linux hosts, WSL2 support, and testing on macOS and Linux hosts. See [docs/design/app.md](design/app.md).

## Candidates

Not yet scheduled.

- **UDP transport under the native Windows backend.** The gateway VM's QEMU user-mode network forwards the whole 100-port map pool for both TCP and UDP, about 200 host sockets. Windows QEMU polls sockets with `select()`, which handles 64 by default, and listening TCP sockets come first, which would explain why UDP replies never arrive. This is not yet confirmed. Confirm by shrinking the forwarded range, then add forwards on demand through QMP `hostfwd_add` when a map is created.
- **`pvm ssh-config <vm>`.** Print an OpenSSH config entry for a VM, with its key, user and a `ProxyJump` through the gateway, so tools such as VS Code Remote-SSH, Mutagen and rsync can reach it by name. It prints only; pvm still never writes to `~/.ssh`.
- **Exit egress probe in `pvm exit test`.** The test only probes the agent itself, so it passes while a VPS firewall drops forwarded traffic. Add a probe that opens a connection to the internet through the tunnel.
- **A default image and a default VM.** `pvm create --image ubuntu:26.04 --name dev2` repeats the same image on every create, and commands that take a VM name repeat it on every call.
  - Default image: `pvm image default ubuntu:26.04` records it, and `pvm create dev2` then needs only a name, given as a positional argument. With no default set and exactly one distro and version having a golden image, use that one; otherwise fail and list the choices. The create output names the image and golden version it used.
  - Default VM: `pvm default dev2` records it, and VM commands such as `ssh`, `exec`, `desktop`, `map`, `mount` and `workspace` use it when the name is omitted. With no default set and exactly one user VM, use that one. Never guess for destructive commands: `rm`, `stop`, `snapshot restore` and `compact` always require the name.
  - Both live in one host config file written with `fsutil.AtomicWrite`; `pvm rm` of the default VM clears it, and a default image whose base and golden are gone is reported, not silently replaced.
  - One default VM per host, not per directory. Commands that act on an existing workspace already know its VM from the workspace record and use that; the default VM only fills in the name when a workspace is created.
- **Gateway rule drift false positive.** The gateway installs `to <server>/32 iif lo fwmark 0 lookup main`. `ip rule` omits a zero fwmark, so the drift check never matches and re-adds the rule every 10 seconds, logging `restored missing rule` each time.
