import os
import shutil
import subprocess  # nosec B404 # explicit local argv, no host shell
import tempfile
import unittest
from pathlib import Path


class BuildTests(unittest.TestCase):
    def test_whitespace_path_fails_before_tool_lookup(self):
        with tempfile.TemporaryDirectory() as directory:
            script = Path(directory) / "checkout with spaces/tools/build-arm.sh"
            script.parent.mkdir(parents=True)
            shutil.copyfile(Path(__file__).with_name("build-arm.sh"), script)
            result = subprocess.run(  # nosec B603 B607 # local test fixture or fixed tool argv
                ["sh", str(script), "build"],
                env={**os.environ, "GO": "station-test-missing-go-command"},
                capture_output=True,
                text=True,
                timeout=5,
                check=False,
            )
        self.assertEqual(result.returncode, 2)
        self.assertEqual(
            result.stderr, "Build path must not contain whitespace (Opus/libtool limitation).\n"
        )
