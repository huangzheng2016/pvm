---
name: pvm-guest
description: Use the guest-local pvm service and manage port maps for the current VM.
---

# pvm guest

The Linux guest has a small `pvm` binary for the agent service and port maps for the VM where the command runs. The systemd service starts it as `/usr/local/bin/pvm agent`.

Run `pvm skill` to print this guide.

## Current VM port maps

Map a host loopback port to a port in this VM:

```bash
pvm map add 8080:80
pvm map add 8080:80 --udp
```

Use host port `0` to ask the host to choose a free port:

```bash
pvm map add 0:80
```

List and remove this VM's maps:

```bash
pvm map ls
pvm map rm 8080
pvm map rm 8080 --udp
```

TCP is the default. Add `--udp` for UDP. Map commands do not accept a VM name and always operate on the current VM.
