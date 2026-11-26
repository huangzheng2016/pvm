#!/usr/bin/env python3
"""Collect third-party Go module and bundled runtime license texts into THIRD_PARTY_LICENSES."""
import json
import subprocess
import sys
from pathlib import Path

LICENSE_NAMES = ("LICENSE", "LICENSE.txt", "LICENSE.md", "COPYING", "COPYING.txt", "NOTICE")

BUNDLED = [
    (
        "Bundled QEMU runtime (QEMU, SeaBIOS, EDK2/OVMF, iPXE, libslirp)",
        "GPL-2.0 / LGPL-2.1 / BSD",
        "https://gitlab.com/qemu-project/qemu/-/blob/master/COPYING",
        "Bundled as independent executables and firmware in ~/.pvm/runtime/qemu.",
    ),
    (
        "Xpra client and server",
        "GPL-2.0-or-later",
        "https://github.com/Xpra-org/xpra/blob/master/COPYING",
        "Used as independent processes for `pvm app`.",
    ),
    (
        "Mihomo proxy core",
        "GPL-3.0",
        "https://github.com/MetaCubeX/mihomo/blob/main/LICENSE",
        "Downloaded at pinned SHA-256 and run as an independent gateway process; never linked into pvm.",
    ),
    (
        "noVNC and TigerVNC",
        "MPL-2.0 / GPL-2.0",
        "https://github.com/novnc/noVNC/blob/master/LICENSE.txt",
        "Served inside the guest for `pvm desktop`.",
    ),
]


def collect(root: Path) -> str:
    out = subprocess.check_output(["go", "list", "-m", "-json", "all"], cwd=root, text=True)
    dec = json.JSONDecoder()
    idx, modules = 0, []
    while idx < len(out):
        while idx < len(out) and out[idx].isspace():
            idx += 1
        if idx >= len(out):
            break
        obj, end = dec.raw_decode(out, idx)
        modules.append(obj)
        idx = end

    parts = ["THIRD_PARTY_LICENSES\n===================\n"]
    seen = set()
    for m in sorted(modules, key=lambda x: x.get("Path", "")):
        path, ver, mdir = m.get("Path", ""), m.get("Version", ""), m.get("Dir", "")
        if path == "github.com/huangzheng2016/pvm" or path in seen:
            continue
        seen.add(path)
        parts.append(f"\n--- {path} {ver} ---\n")
        found = False
        if mdir:
            base = Path(mdir)
            for name in LICENSE_NAMES:
                cand = base / name
                if cand.is_file():
                    parts.append(cand.read_text(encoding="utf-8", errors="replace").strip() + "\n")
                    found = True
                    break
        if not found:
            parts.append("(License text not found in module cache; see upstream module repository.)\n")
    parts.append("\n===================\nBundled runtime components\n===================\n")
    for name, lic, url, note in BUNDLED:
        parts.append(f"\n--- {name} ---\nLicense: {lic}\nUpstream: {url}\n{note}\n")
    return "\n".join(line.rstrip() for line in "\n".join(parts).splitlines()) + "\n"


def main() -> int:
    root = Path(__file__).resolve().parents[2]
    target = root / "THIRD_PARTY_LICENSES"
    text = collect(root)
    if "--check" in sys.argv:
        if not target.is_file() or target.read_text(encoding="utf-8") != text:
            print("THIRD_PARTY_LICENSES is out of date; run python3 script/ci/collect_licenses.py", file=sys.stderr)
            return 1
        print("THIRD_PARTY_LICENSES is up to date")
        return 0
    target.write_text(text, encoding="utf-8")
    print(f"wrote {target}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
