#!/usr/bin/env python3
"""Validate release pins, version metadata and signing readiness without printing secrets."""
import json
import os
import re
import sys
from pathlib import Path

MACOS_SIGNING_VARS = (
    "MACOS_CERTIFICATE_P12_BASE64",
    "MACOS_CERTIFICATE_PASSWORD",
    "MACOS_CODESIGN_IDENTITY",
)


def check_version_text(version, ref_name):
    errors = []
    bare = version.lstrip("v")
    if not re.fullmatch(r"\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?", bare):
        errors.append(f"VERSION {version!r} is not a semantic version")
    if ref_name.startswith("v") and ref_name.lstrip("v") != bare:
        errors.append(f"tag {ref_name!r} does not match VERSION {version!r}")
    return errors


def check_windows_qemu_pin_data(pin):
    errors = []
    if not isinstance(pin, dict):
        return ["windows QEMU pin must be a JSON object"]
    url, sha, ver = pin.get("url", ""), pin.get("sha512", ""), pin.get("version", "")
    if not isinstance(ver, str) or not ver.isdigit():
        errors.append("windows QEMU pin version must be a date string")
    if not isinstance(url, str) or not url.startswith("https://") or not url.endswith(f"qemu-w64-setup-{ver}.exe"):
        errors.append("windows QEMU pin url must be HTTPS and match its version")
    if not isinstance(sha, str) or not re.fullmatch(r"[0-9a-f]{128}", sha):
        errors.append("windows QEMU pin sha512 must be 128 lowercase hex characters")
    return errors


def signing_warnings(env):
    missing = [name for name in MACOS_SIGNING_VARS if not env.get(name, "").strip()]
    if not missing:
        return []
    return ["macOS Developer ID signing is not fully configured (missing " + ", ".join(missing) + "); builds fall back to ad-hoc signing"]


def main(argv):
    require_signing = "--require-signing" in argv or os.environ.get("PVM_REQUIRE_SIGNING") == "1"
    root = Path(__file__).resolve().parents[2]
    errors = []
    try:
        version = (root / "VERSION").read_text(encoding="utf-8").strip()
        errors += check_version_text(version, os.environ.get("GITHUB_REF_NAME", ""))
    except OSError as e:
        errors.append(f"VERSION unreadable: {e}")
    try:
        pin = json.loads((root / "script" / "qemu" / "windows-qemu.json").read_text(encoding="utf-8"))
        errors += check_windows_qemu_pin_data(pin)
    except (OSError, ValueError) as e:
        errors.append(f"windows QEMU pin unreadable: {e}")

    warnings = signing_warnings(os.environ)
    for w in warnings:
        print(f"::warning::{w}")
    if require_signing:
        errors += warnings
    for e in errors:
        print(f"::error::{e}", file=sys.stderr)
    if errors:
        return 1
    print("release readiness checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
