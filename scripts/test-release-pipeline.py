#!/usr/bin/env python3
"""End-to-end test for the release pipeline.

A throwaway repository skeleton supplies the module manifest, LICENSE, README
and synthetic native libraries; the real scripts/package-plugin.sh packages each
target, checksums.txt is assembled exactly like the release workflow, and the
real scripts/verify-release-assets.py must accept the result. A mutated archive
must then be rejected, proving the SHA256 gate is live. Only the Python standard
library, bash and zip are used.
"""

import hashlib
import json
import os
import shutil
import struct
import subprocess
import sys
import tempfile
import unittest
import zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PACKAGER = os.path.join(ROOT, "scripts", "package-plugin.sh")
VERIFIER = os.path.join(ROOT, "scripts", "verify-release-assets.py")
SCRIPTS_DIR = os.path.join(ROOT, "scripts")
PLUGIN_ID = "cliproxyapi-credit-manager"
VERSION = "0.1.0"
COMMIT = "0123456789abcdef0123456789abcdef01234567"
SDK = "github.com/router-for-me/CLIProxyAPI/v8 v8.0.30"
TARGETS = (
    ("linux", "amd64", "so"),
    ("linux", "arm64", "so"),
    ("darwin", "arm64", "dylib"),
)


def elf(machine):
    header = bytearray(64)
    header[0:4] = b"\x7fELF"
    header[4] = 2
    header[5] = 1
    struct.pack_into("<H", header, 18, machine)
    return bytes(header)


def macho():
    return b"\xcf\xfa\xed\xfe" + struct.pack("<I", 0x0100000C)


def library_for(goos, goarch):
    if goos == "darwin":
        return macho()
    return elf(62 if goarch == "amd64" else 183)


def archive_name(goos, goarch):
    return "{0}_{1}_{2}_{3}.zip".format(PLUGIN_ID, VERSION, goos, goarch)


def need_tools():
    for tool in ("zip", "bash"):
        if shutil.which(tool) is None:
            raise unittest.SkipTest("{0} is required".format(tool))


class ReleasePipelineTest(unittest.TestCase):
    def setUp(self):
        need_tools()
        self.tmp = tempfile.TemporaryDirectory()
        self.repo = os.path.join(self.tmp.name, "repo")
        self.pkg = os.path.join(self.tmp.name, "pkg")
        self._make_repo()

    def tearDown(self):
        self.tmp.cleanup()

    def _make_repo(self):
        os.makedirs(os.path.join(self.repo, "scripts"), exist_ok=True)
        for name in os.listdir(SCRIPTS_DIR):
            if name.endswith(".sh"):
                shutil.copy(os.path.join(SCRIPTS_DIR, name), os.path.join(self.repo, "scripts", name))
        with open(os.path.join(self.repo, "go.mod"), "w", encoding="utf-8") as handle:
            handle.write(
                "module github.com/fengwk/cliproxyapi-credit-manager\n\n"
                "go 1.26.0\n\n"
                "require (\n\tgithub.com/router-for-me/CLIProxyAPI/v8 v8.0.30\n)\n"
            )
        with open(os.path.join(self.repo, "LICENSE"), "w", encoding="utf-8") as handle:
            handle.write("license text\n")
        with open(os.path.join(self.repo, "README.md"), "w", encoding="utf-8") as handle:
            handle.write("# readme\n")
        for goos, goarch, ext in TARGETS:
            target = os.path.join(self.repo, "dist", goos, goarch)
            os.makedirs(target, exist_ok=True)
            with open(os.path.join(target, "{0}.{1}".format(PLUGIN_ID, ext)), "wb") as handle:
                handle.write(library_for(goos, goarch))

    def _package_all(self):
        os.makedirs(self.pkg, exist_ok=True)
        for goos, goarch, _ext in TARGETS:
            result = subprocess.run(
                [
                    "bash",
                    PACKAGER,
                    VERSION,
                    goos,
                    goarch,
                    self.pkg,
                    self.repo,
                ],
                env=dict(os.environ, COMMIT=COMMIT),
                capture_output=True,
                text=True,
            )
            self.assertEqual(result.returncode, 0, result.stderr)

    def _assemble_checksums(self):
        # Mirrors the publish job: concatenate the sidecars, sort by archive name.
        lines = []
        for goos, goarch, _ext in TARGETS:
            sidecar = os.path.join(self.pkg, archive_name(goos, goarch) + ".sha256")
            with open(sidecar, encoding="utf-8") as handle:
                lines.append(handle.read().rstrip("\n"))
            os.remove(sidecar)
        lines.sort(key=lambda line: line.split("  ")[1])
        with open(os.path.join(self.pkg, "checksums.txt"), "w", encoding="utf-8", newline="\n") as handle:
            handle.write("\n".join(lines) + "\n")

    def _run_verifier(self):
        return subprocess.run(
            [
                sys.executable,
                VERIFIER,
                self.pkg,
                "--version",
                VERSION,
                "--commit",
                COMMIT,
                "--sdk",
                SDK,
            ],
            capture_output=True,
            text=True,
        )

    def test_packaged_release_passes_the_verifier(self):
        self._package_all()

        # The packager output must already match the archive contract.
        for goos, goarch, ext in TARGETS:
            archive = os.path.join(self.pkg, archive_name(goos, goarch))
            self.assertTrue(os.path.isfile(archive), archive)
            with open(archive, "rb") as handle:
                digest = hashlib.sha256(handle.read()).hexdigest()
            with open(archive + ".sha256", encoding="utf-8") as handle:
                self.assertEqual(handle.read().strip(), "{0}  {1}".format(digest, archive_name(goos, goarch)))

        self._assemble_checksums()
        result = self._run_verifier()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("linux/amd64 ok", result.stdout)
        self.assertIn("linux/arm64 ok", result.stdout)
        self.assertIn("darwin/arm64 ok", result.stdout)

    def test_packaged_metadata_matches_the_verifier_expectations(self):
        self._package_all()
        # The verifier rejects any extra or missing metadata key, so a passing
        # run plus this explicit check pins the manifest identity.
        archive = os.path.join(self.pkg, archive_name("linux", "amd64"))
        with zipfile.ZipFile(archive) as handle:
            meta = json.loads(handle.read("metadata.json").decode("utf-8"))
        self.assertEqual(
            meta,
            {
                "id": PLUGIN_ID,
                "name": "Credit Manager",
                "version": VERSION,
                "goos": "linux",
                "goarch": "amd64",
                "sdk": SDK,
                "commit": COMMIT,
                "abi": 1,
            },
        )
        self._assemble_checksums()
        self.assertEqual(self._run_verifier().returncode, 0)

    def test_mutated_archive_is_rejected(self):
        self._package_all()
        self._assemble_checksums()
        target = os.path.join(self.pkg, archive_name("linux", "arm64"))
        with open(target, "ab") as handle:
            handle.write(b"\x00")
        result = self._run_verifier()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum mismatch", result.stderr)


if __name__ == "__main__":
    unittest.main()
