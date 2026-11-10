import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class FamilyBootstrapTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(
            prefix="family-bootstrap-check-", dir=os.environ.get("KIROCREW_SCRATCH")
        )
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        (self.root / "scripts").mkdir()
        (self.root / "server").mkdir()
        (self.root / "bin").mkdir()
        source = Path(__file__).resolve().parents[2] / "scripts/bootstrap.sh"
        shutil.copyfile(source, self.root / "scripts/bootstrap.sh")
        self.docker_calls = self.root / "docker-calls"
        docker = self.root / "bin/docker"
        docker.write_text("#!/bin/sh\nprintf 'called\\n' >> \"$FHD_DOCKER_CALLS\"\nexit 91\n")
        docker.chmod(0o700)
        self.env = {
            **os.environ,
            "PATH": str(self.root / "bin") + os.pathsep + os.environ["PATH"],
            "FHD_DOCKER_CALLS": str(self.docker_calls),
        }
        self.settings = self.root / "server/.env"

    def run_bootstrap(self, *args):
        return subprocess.run(
            ["bash", str(self.root / "scripts/bootstrap.sh"), *args],
            env=self.env, text=True, capture_output=True, check=False,
        )

    def test_configure_creates_only_non_secret_settings_without_docker(self):
        result = self.run_bootstrap(
            "--url", "https://viewer.example:18443", "--time-zone", "Europe/Berlin", "--configure-only"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            self.settings.read_text(),
            "WEB_PUBLIC_URL=https://viewer.example:18443\n"
            "PULS_PUBLIC_URL=https://viewer.example:18443\n"
            "PULS_TIME_ZONE=Europe/Berlin\nTRUST_PROXY_HEADERS=false\n",
        )
        self.assertEqual(self.settings.stat().st_mode & 0o777, 0o600)
        self.assertFalse(self.docker_calls.exists())

    def test_existing_settings_are_preserved_and_not_claimed_valid(self):
        self.settings.write_text("legacy-private-sentinel\n")
        result = self.run_bootstrap("--configure-only")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.settings.read_text(), "legacy-private-sentinel\n")
        self.assertNotIn("legacy-private-sentinel", result.stdout + result.stderr)
        self.assertFalse(self.docker_calls.exists())

    def test_legacy_flags_and_unsafe_values_fail_without_writes(self):
        for args in [
            ("--lan",), ("--print-pairing",), ("--issue-device", "Phone"),
            ("synthetic-private-value",),
            ("--url", "http://viewer.example", "--configure-only"),
            ("--url", "https://viewer.example\nDATABASE_URL=private", "--configure-only"),
            ("--url", "https://viewer.example", "--time-zone", "../bad", "--configure-only"),
        ]:
            with self.subTest(args=args):
                result = self.run_bootstrap(*args)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("synthetic-private-value", result.stdout + result.stderr)
                self.assertFalse(self.settings.exists())
                self.assertFalse(self.docker_calls.exists())

    def test_settings_symlink_is_not_followed_or_replaced(self):
        target = self.root / "outside"
        target.write_text("retained")
        self.settings.symlink_to(target)
        result = self.run_bootstrap()
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(self.settings.is_symlink())
        self.assertEqual(target.read_text(), "retained")
        self.assertFalse(self.docker_calls.exists())
