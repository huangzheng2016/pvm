#!/usr/bin/env python3
"""Run privileged network regressions and reject missing or skipped cases."""
import os
import subprocess
import tempfile

cases = {
    "./internal/gateway": ["TestKillSwitchUplinkE2ELinux", "TestFakeICMPNetnsRelayLinux", "TestFakeUDPTransparentRelayLinux", "TestRealIPForwardE2ELinux", "TestFakeICMPRealEchoLinux", "TestFakeUDPProxyRelayLinux", "TestOriginalDstUDPLinux"],
    "./internal/exit": ["TestUserspaceNATICMP"],
}
with tempfile.TemporaryDirectory(prefix="pvm-netns-") as directory:
    for index, (package, names) in enumerate(cases.items()):
        binary = os.path.join(directory, str(index) + ".test")
        subprocess.run(["go", "test", "-c", "-o", binary, package], check=True)
        pattern = "^(" + "|".join(names) + ")$"
        listed = subprocess.check_output([binary, "-test.list", pattern], text=True).splitlines()
        if not set(names).issubset(listed):
            raise SystemExit("required network test missing")
        result = subprocess.run(["sudo", binary, "-test.v", "-test.timeout=5m", "-test.run", pattern], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        print(result.stdout, flush=True)
        if result.returncode or "--- SKIP:" in result.stdout:
            raise SystemExit("required network regressions failed or skipped")
