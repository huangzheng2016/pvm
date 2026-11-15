# VM apps as host windows

Status: implemented on master, not yet released. Tested on native Windows with WHPX; macOS and Linux hosts are untested.

## Motivation

`pvm desktop` shows the whole XFCE desktop in a browser tab through noVNC. Two things were missing:

- Single applications as their own host windows: a web page in the VM's Firefox as a web app, and ordinary X11 programs such as an editor. The page renders in the VM, so cookies, fingerprint and egress stay with the VM and its exit; the host only receives pixels.
- Latency and clipboard. Pointer feedback in `pvm desktop` lagged by hundreds of milliseconds, and noVNC carries text only.

## Measurements

Measured with `pvm __probe-rtt` on native Windows with WHPX:

| Path | 64 B round trip p50 / p99 | Throughput |
|---|---|---|
| Inside the guest, loopback | 0.09 / 0.36 ms | - |
| Host relay, gateway VM, user VM (port maps, old desktop) | 1.4 / 2.5 ms | about 12 MB/s |
| `pvm.stream` | 0.6 / 1.6 ms | 171 MB/s guest to host, 41 MB/s host to guest |

Small messages were never slow and there were no 200 ms Nagle stalls. The port map path is throughput bound: a 1 MiB reply took 85 ms, so every large framebuffer update in `pvm desktop` cost tens to hundreds of milliseconds.

## The `pvm.stream` channel

Every VM has a second virtio-serial port, `pvm.stream`, next to the agent's. It carries multiplexed binary streams with per-stream credit flow control (`internal/stream`).

- Only the host opens streams, by naming a target. The guest agent connects only targets on its allowlist: `novnc` (the desktop's websockify), `xpra:<uid>` (that user's Xpra socket), `clip:<uid>:<kind>` (sets that user's clipboard) and three measurement targets. The guest cannot open a stream, reach host addresses or reach the network through it.
- The agent runs as root, so it connects a user socket only when the runtime directory and the socket belong to that uid, are not symlinks and are closed to other users.
- The daemon keeps one session per VM, because QEMU's chardev socket accepts one host connection. A handshake skips bytes left on the port by an earlier host connection, so a daemon restart recovers while the VM runs.
- RAM snapshots record whether the VM had the port. A VM restored from an older snapshot runs without it until its next cold boot.

`pvm desktop` uses it too: a daemon listener forwards to the `novnc` target, so the desktop no longer needs a port map or the gateway VM. The listener keeps its port across daemon restarts.

## Engine: Xpra

Xpra was the only option on a native Windows host that gives per-window host windows, text and image clipboard both ways and file drops, and sends only damaged regions.

Rejected:

- RDP RemoteApp: no working Linux server.
- Waypipe: no Windows client.
- ssh -X to an X server on Windows: weak isolation, text-only clipboard.
- SPICE, Selkies, KasmVNC, Guacamole and noVNC: whole desktops in a browser tab.

The guest runs Xpra 6.5 from xpra.org's APT repository, pinned to the 6.5 line, with the signing key fingerprint checked. Golden images include it, and `pvm app` installs it on first use in older VMs. The host needs the matching Windows client, 6.5.x.

### Session model

- One Xpra server per VM, on display `:100`, as the VM user, run as the user systemd unit `pvm-xpra`. Linger keeps it running without a login session.
- Every app is its own user unit `pvm-app-<id>` on that display. Stopping an app stops its whole process tree; stopping the last one stops the server.
- Apps in one VM share the display, so they can copy and drag between each other inside the VM.
- A web app is Firefox with its own profile per app id. The profile hides the tab strip and toolbars and skips first-run, terms and onboarding pages.
- The server listens only on a unix socket under `$XDG_RUNTIME_DIR/pvm`, mode 0700, with password authentication. The password is per VM: the host copy is in `~/.pvm/vms/<vm>/app/xpra.secret` and it reaches the guest over ssh stdin.
- The host client connects to a daemon loopback listener that forwards to the `xpra:<uid>` target. Window titles end in `[<vm>]`.

### Encoding

Server: `--speed=100 --min-speed=80 --quality=80 --min-quality=50 --encodings=rgb,webp,jpeg,png --video-encoders=none`. The client repeats the minimums, because its defaults override the server's.

Measured on a 1280x993 Firefox canvas animation, with the server reporting:

| Setup | Frame latency | Frames per second | Guest Xpra CPU |
|---|---|---|---|
| No video encoder, client OpenGL on | 58 ms | 22 | 18% |
| vpx | 55 ms | 22 | 64% |
| openh264 | 66 ms | 22 | 36% |
| No video encoder, client OpenGL off | 43 ms | 50 | - |

The video encoders gave no gain under WHPX, where the guest has no AVX, and cost CPU. Client OpenGL painting on Windows held the frame rate down, because the server paces updates by client acknowledgements. The Windows client runs with `--opengl=no`; it used about 9% of one core.

For small updates (a terminal printing every 30 ms) the frame latency was 10 to 15 ms at 105 fps.

## Clipboard

Measured: the Xpra Windows client sends the host clipboard to the server even while none of its windows has focus, so anything copied on the host, including passwords, would enter the VM. pvm therefore splits the directions:

- Guest to host: Xpra's own clipboard, `--clipboard-direction=to-client` on both ends. Text and images.
- Host to guest: pvm's bridge, `pvm app __clipboard`, started with the client. It sends the host clipboard only when one of the VM's windows comes to the front, and when the clipboard changes while it is there. It skips content the Xpra client put on the clipboard, which came from the guest, and content marked `ExcludeClipboardContentFromMonitorProcessing` by password managers. Text and images (PNG, or DIB converted to PNG) go to the `clip` target, which sets the guest clipboard with xclip. Windows hosts only so far.

`pvm app config <vm> --clipboard off|to-guest|to-host|both` sets the policy per VM; the default is `off`. The daemon refuses `clip` streams unless the policy allows host to guest.

## Drops

Xpra uses one setting for both directions of file transfer. The server always accepts uploads (`--file-transfer=yes --open-files=no --file-size-limit=100M`), and the client decides:

- `--drop off` (default): the client runs with `--file-transfer=no`, so drops do nothing and files the guest sends are refused.
- `--drop on`: the client runs with `--file-transfer=ask`. Files dropped on a VM window upload into `~/Downloads` in the VM and are never opened. A file the guest sends waits for the user to accept it in a host dialog.

A dropped file lands in the VM's download folder, not in the application under the pointer; Xpra does not support that yet. Dragging out of the VM is not supported.

## Commands

```bash
pvm app dev1 -- gedit                      # an X11 program
pvm app dev1 https://mail.example.com      # a web app; --kiosk for full screen
pvm app dev1 --name notes --no-attach -- gedit notes.txt
pvm app ls [dev1]
pvm app attach dev1                        # reopen the windows after closing the client
pvm app stop dev1 notes | --all
pvm app config dev1 --clipboard both --drop on
```

Closing a host window closes that app in the guest. Quitting the client detaches without stopping apps. A VM restart ends its apps; `pvm app ls` reports them as exited once.

## Not done

- macOS and Linux hosts: the client starts, but the host-to-guest clipboard bridge is Windows only.
- WSL2 backend: not supported.
- Drops into a specific application, and dragging out of the VM.
- Firefox memory caps per web app.
