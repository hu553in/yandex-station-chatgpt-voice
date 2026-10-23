import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class BuildTests(unittest.TestCase):
    def test_whitespace_path_fails_before_tool_lookup(self):
        with tempfile.TemporaryDirectory() as directory:
            script = Path(directory) / "checkout with spaces/tools/build-arm.sh"
            script.parent.mkdir(parents=True)
            shutil.copyfile(Path(__file__).with_name("build-arm.sh"), script)
            result = subprocess.run(
                ["sh", str(script), "setup"],
                env={**os.environ, "GO": "station-test-missing-go-command"},
                capture_output=True,
                text=True,
                timeout=5,
            )
        self.assertEqual(result.returncode, 2)
        self.assertEqual(
            result.stderr,
            "Build path must not contain whitespace (Opus/libtool limitation).\n",
        )
