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

- Only the host opens streams, by naming a target. The guest agent connects only targets on its allowlist: `novnc` (the desktop's websockify), `xpra:<uid>` (that user's Xpra socket), `clip:<uid>:<kind>` (sets that user's clipboard), `clipout:<uid>:files` (copies the files on that user's clipboard) and three measurement targets. The guest cannot open a stream, reach host addresses or reach the network through it.
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

The guest runs Xpra 6.5 from xpra.org's APT repository, pinned to the 6.5 line, with the signing key fingerprint checked. Golden images include it, and `pvm app` installs it on first use in older VMs. The host needs the matching Windows client, 6.5.x. The Windows installers include it in `%LOCALAPPDATA%\pvm\xpra`: the release job downloads a pinned zip from xpra.org (`script/windows/fetch-xpra-client.sh`), checks its SHA256 and its signature by the same key as the APT repository, and ships it unmodified with its GPL text and a note on where to get the source.

### Session model

- One Xpra server per VM, on display `:100`, as the VM user, run as the user systemd unit `pvm-xpra`. Linger keeps it running without a login session.
- Every app is its own user unit `pvm-app-<id>` on that display. Stopping an app stops its whole process tree; stopping the last one stops the server.
- Apps in one VM share the display, so they can copy and drag between each other inside the VM.
- Web apps are windows of one Firefox per VM (the user unit `pvm-webapps`) with one profile, so they share logins, cookies and cache; Firefox allows one process per profile. The profile is separate from the desktop's Firefox. It hides the tab strip and toolbars, skips first-run, terms and onboarding pages, and opens links meant for a new tab in a new window. Popups, which Firefox marks `popup-window`, keep their read-only address bar, without the bookmark star, so a sign-in window shows where it is.
- pvm records each web app's X window and tracks it with wmctrl: `pvm app stop` closes that window, `--kiosk` makes it full screen, and the shared Firefox stops with the last web app.
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

## Input methods

Text a host input method commits, such as Chinese from Microsoft Pinyin, reaches the Xpra server as a key event with keycode 0 and the character as its string. The server only presses keycodes its keymap already has, and the keymap it builds from the client fills every keycode, so it dropped these characters. pvm starts the server through `xpra_start.py`, which hands such presses to `xdotool type`. xdotool needs one empty keycode to borrow for each character, so keycode 254 (`XF86WWAN`) is cleared for it. The characters of one commit arrive milliseconds apart and are typed in one call, 40 ms apart; shorter gaps lost characters, as applications look the borrowed keycode up after the event. Measured on native Windows with WHPX, typing `nihao` and `zhongwen` with Microsoft Pinyin into a GTK text box: most commits arrive intact, but the latest run still lost a character from the first commit and showed stray Latin characters; the cause is not confirmed. Plain keys are unchanged.

Synthetic Unicode input (`SendInput` with `KEYEVENTF_UNICODE`, used by some on-screen keyboards and automation tools) still does not arrive: the Xpra Windows client turns it into an empty key before it sends it.

Characters outside the Basic Multilingual Plane, such as emoji from the Windows emoji panel, reach GTK on Windows as two key events, one per UTF-16 surrogate. The Xpra 6.5.5 client then failed to encode the lone surrogate, dropped the connection and lost every window. The Windows installers add `xpra/client/gui/keyboard_helper.py` to the client (`script/windows/xpra-patch`), which Python loads before the compiled module of the same name; it runs the original and joins the two halves into one key event, which the server types like other committed text. Measured: an emoji from the emoji panel arrived in gedit as `U+1F924`, and the client stayed connected. Clients installed some other way still drop the connection.

## Sound

The server runs its own PulseAudio (`xpra-audio-server`) with `--speaker=on --microphone=on`. The guest session runs a PulseAudio of its own, with only a null sink, which apps would otherwise use; the server script puts `PULSE_SERVER` for the server's in the user manager's environment, which app units inherit, and starts the server without it, as its PulseAudio refuses to start while one is named. The client decides, from `pvm app config <vm> --audio`:

- `out` (default): `--speaker=on --microphone=no`; VM sound plays on the host's default output device.
- `both`: also `--microphone=on`; the host's default input device records into the VM's default source.
- `off`: neither.

Both directions use Opus. Measured on a Windows host: a 440 Hz tone played in the VM with `paplay` was recorded from the host's output device through WASAPI loopback at about 440 Hz, and a recording in the VM picked up the host's default microphone. Before `PULSE_SERVER` was set, apps played into the session's null sink and the host heard nothing.

## Startup

GTK programs started through D-Bus activation services that need the display, such as the desktop portal's GTK backend. The user manager had no `DISPLAY`, so the backend exited, the portal hung until its start timed out, and gedit and Thunar took 50 to 60 seconds to show a window. The server script now puts `DISPLAY` in the activation environment and clears the failed units; both open in about one second.

Apps such as Claude Desktop keep secrets in the Secret Service. gnome-keyring locks it until a desktop login unlocks it, and the app display has no login, so a keyring dialog asked for a password on every start. When the server starts, `keyring_unlock.py` creates a `login` keyring with an empty password if there is none, makes it the default, and unlocks it, as a passwordless login would. Its contents are then stored unencrypted, readable only by the VM user. A keyring the user gave a password is left locked. Measured in a VM: the keyring was locked after a reboot and unlocked once `pvm app` started the server, and a secret was stored and read back without a prompt.

The first app in a VM installs what came after the VM's Xpra: `gtk3-nocsd`, `xpra-audio-server` and the Noto fonts for all major scripts and color emoji (about 25 MB). Packages the release lacks do not block, and an install that fails leaves a warning. `pvm app` shows the install's output.

## Clipboard

Measured: the Xpra Windows client sends the host clipboard to the server even while none of its windows has focus, so anything copied on the host, including passwords, would enter the VM. pvm therefore splits the directions:

- Guest to host: Xpra's own clipboard, `--clipboard-direction=to-client` on both ends. Text and images.
- Host to guest: pvm's bridge, `pvm app __clipboard`, started with the client. It sends the host clipboard only when one of the VM's windows comes to the front, and when the clipboard changes while it is there. It skips content the Xpra client or the bridge put on the clipboard, which came from the guest, and content marked `ExcludeClipboardContentFromMonitorProcessing` by password managers. Text and images (PNG, or DIB converted to PNG) go to the `clip` target, which sets the guest clipboard with xclip. Windows hosts only so far.

Files, which neither Xpra nor xclip carries, go through the same bridge on Windows hosts, as a tar of folders and regular files, up to 256 MB and 20000 entries; links and special files are left out.

- Host to guest: files copied in Explorer (`CF_HDROP`) go to `clip:files`. The agent unpacks them as the VM user into a new folder under `~/.cache/pvm/clipboard`, removing the previous copy, and `clip_files.py` owns the clipboard with them as `x-special/gnome-copied-files` (Thunar, file dialogs), `text/uri-list` (browsers, Electron) and their paths as text. xclip offers one target only, and GTK's API for several is not available from Python.
- Guest to host: when guest content reaches the host clipboard, and when focus leaves the VM's windows, the bridge opens `clipout:files`. The agent answers with a fingerprint of the files on the clipboard, by path, size and time; the bridge asks for them only when that changed, so a host copy made since is not overwritten by the same files. It unpacks them under `vms/<vm>/app/clipboard` in pvm's data, keeping only the latest copy, and puts them on the host clipboard as `CF_HDROP`, owned by a message-only window. Names Windows cannot hold, reserved device names and paths leaving the folder are skipped. Files that came from the host are not offered back.

`pvm app config <vm> --clipboard off|to-guest|to-host|both` sets the policy per VM; the default is `off`. The daemon refuses `clip` streams unless the policy allows host to guest, and `clipout` streams unless it allows guest to host.

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
pvm app config dev1 --clipboard both --drop on --audio both
```

Closing a host window closes that app in the guest. Quitting the client detaches without stopping apps. `pvm stop` and `pvm rm` stop the VM's Xpra server first, so the client hears it shut down and closes its windows; before, it kept reconnecting with the windows left on the host. A VM restart ends its apps; `pvm app ls` reports them as exited once.

## Not done

- macOS and Linux hosts: the client starts, but the clipboard bridge, which carries host content into the VM and files either way, is Windows only.
- WSL2 backend: not supported.
- Drops into a specific application, and dragging out of the VM.
- Firefox memory caps per web app.
