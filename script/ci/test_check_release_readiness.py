#!/usr/bin/env python3
import json
import unittest
from pathlib import Path

from check_release_readiness import MACOS_SIGNING_VARS, check_version_text, check_windows_qemu_pin_data, signing_warnings


class ReleaseReadinessTests(unittest.TestCase):
    def test_repo_pin_is_valid(self):
        root = Path(__file__).resolve().parents[2]
        pin = json.loads((root / "script" / "qemu" / "windows-qemu.json").read_text(encoding="utf-8"))
        self.assertEqual(check_windows_qemu_pin_data(pin), [])
        version = (root / "VERSION").read_text(encoding="utf-8").strip()
        self.assertEqual(check_version_text(version, "v" + version), [])

    def test_version_and_tag_mismatch(self):
        self.assertEqual(check_version_text("1.2.3", "master"), [])
        self.assertEqual(check_version_text("v1.2.3", "v1.2.3"), [])
        self.assertTrue(check_version_text("1.2.3", "v1.2.4"))
        self.assertTrue(check_version_text("v1.2.3", "v1.2.4"))
        self.assertTrue(check_version_text("latest", "v1.2.3"))

    def test_pin_rejects_unpinned_or_mismatched_inputs(self):
        base = {"version": "20260811", "url": "https://qemu.weilnetz.de/w64/2026/qemu-w64-setup-20260811.exe", "sha512": "a" * 128}
        self.assertEqual(check_windows_qemu_pin_data(base), [])
        self.assertTrue(check_windows_qemu_pin_data({**base, "url": "http://example.com/qemu-w64-setup-20260811.exe"}))
        self.assertTrue(check_windows_qemu_pin_data({**base, "url": "https://example.com/qemu-w64-setup-20260101.exe"}))
        self.assertTrue(check_windows_qemu_pin_data({**base, "sha512": "deadbeef"}))

    def test_signing_warnings_never_leak_secret_values(self):
        warns = signing_warnings({})
        self.assertEqual(len(warns), 1)
        self.assertIn("MACOS_CERTIFICATE_P12_BASE64", warns[0])
        env = {k: "YOUR_TOKEN_VALUE" for k in MACOS_SIGNING_VARS}
        self.assertEqual(signing_warnings(env), [])
        for w in signing_warnings({MACOS_SIGNING_VARS[0]: "YOUR_TOKEN_VALUE"}):
            self.assertNotIn("YOUR_TOKEN_VALUE", w)


if __name__ == "__main__":
    unittest.main()
