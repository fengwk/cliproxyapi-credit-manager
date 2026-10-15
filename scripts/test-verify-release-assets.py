#!/usr/bin/env python3
"""Static tests for scripts/verify-release-assets.py.

Synthetic ELF and Mach-O headers stand in for the native libraries so the
release gate is exercised without a C toolchain or any third-party package.
"""

import hashlib
import io
import json
import os
import stat
import struct
import subprocess
import sys
import tempfile
import unittest
import warnings
import zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SCRIPT = os.path.join(ROOT, "scripts", "verify-release-assets.py")
PLUGIN_ID = "cliproxyapi-credit-manager"
PLUGIN_NAME = "Credit Manager"
VERSION = "1.2.3"
COMMIT = "0123456789abcdef0123456789abcdef01234567"
SDK = "github.com/router-for-me/CLIProxyAPI/v8 v8.0.30"
TARGETS = (
    ("linux", "amd64", "so"),
    ("linux", "arm64", "so"),
    ("darwin", "arm64", "dylib"),
)


def elf(machine):
    # EI_CLASS=2 (64-bit), EI_DATA=1 (little-endian), e_machine at offset 18.
    header = bytearray(64)
    header[0:4] = b"\x7fELF"
    header[4] = 2
    header[5] = 1
    struct.pack_into("<H", header, 18, machine)
    return bytes(header)


def macho(cputype=0x0100000C):
    # MH_MAGIC_64 little-endian followed by cputype.
    return b"\xcf\xfa\xed\xfe" + struct.pack("<I", cputype)


def library_for(goos, goarch):
    if goos == "darwin":
        return macho()
    machine = 62 if goarch == "amd64" else 183
    return elf(machine)


def archive_name(version, goos, goarch):
    return "{0}_{1}_{2}_{3}.zip".format(PLUGIN_ID, version, goos, goarch)


def metadata(version=VERSION, commit=COMMIT, sdk=SDK, goos="linux", goarch="amd64"):
    return json.dumps(
        {
            "id": PLUGIN_ID,
            "name": PLUGIN_NAME,
            "version": version,
            "goos": goos,
            "goarch": goarch,
            "sdk": sdk,
            "commit": commit,
            "abi": 1,
        },
        indent=2,
    ).encode("utf-8") + b"\n"


def mark_encrypted(blob, member):
    # Python's zip writer cannot emit a traditional encrypted entry, so the
    # general-purpose encryption bit is set on the local and central headers.
    data = bytearray(blob)
    encoded = member.encode("utf-8")
    for signature, name_offset, flag_offset in (
        (b"PK\x03\x04", 26, 6),
        (b"PK\x01\x02", 28, 8),
    ):
        start = 0
        while True:
            index = data.find(signature, start)
            if index < 0:
                break
            name_len = struct.unpack_from("<H", data, index + name_offset)[0]
            name_at = index + (30 if signature == b"PK\x03\x04" else 46)
            if data[name_at:name_at + name_len] == encoded:
                flags = struct.unpack_from("<H", data, index + flag_offset)[0] | 0x1
                struct.pack_into("<H", data, index + flag_offset, flags)
            start = index + 4
    return bytes(data)


def build_zip(goos, goarch, library=None, meta=None, members=None, symlink=None, encrypt=None):
    ext = "dylib" if goos == "darwin" else "so"
    lib_name = "{0}.{1}".format(PLUGIN_ID, ext)
    payload = {
        lib_name: library if library is not None else library_for(goos, goarch),
        "LICENSE": b"license\n",
        "README.md": b"# readme\n",
        "metadata.json": meta if meta is not None else metadata(goos=goos, goarch=goarch),
    }
    if members is not None:
        payload = members
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        for name, data in payload.items():
            info = zipfile.ZipInfo(name)
            info.external_attr = (stat.S_IFREG | 0o644) << 16
            if symlink and name == symlink:
                info.external_attr = (stat.S_IFLNK | 0o777) << 16
            archive.writestr(info, data)
    blob = buffer.getvalue()
    if encrypt:
        blob = mark_encrypted(blob, encrypt)
    return blob


def checksum_line(name, data):
    return "{0}  {1}".format(hashlib.sha256(data).hexdigest(), name)


class ReleaseAssetsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.asset_dir = self.tmp.name

    def tearDown(self):
        self.tmp.cleanup()

    def run_verifier(self, version=VERSION, commit=COMMIT, sdk=SDK, asset_dir=None):
        cmd = [
            sys.executable,
            SCRIPT,
            asset_dir or self.asset_dir,
            "--version",
            version,
            "--commit",
            commit,
            "--sdk",
            sdk,
        ]
        return subprocess.run(cmd, capture_output=True, text=True)

    def write_release(self, archives=None, checksums=None, extra=None):
        if archives is None:
            archives = {
                archive_name(VERSION, goos, goarch): build_zip(goos, goarch)
                for goos, goarch, _ext in TARGETS
            }
        for name, data in archives.items():
            with open(os.path.join(self.asset_dir, name), "wb") as handle:
                handle.write(data)
        if checksums is None:
            # checksums.txt is sorted by archive name, matching the release workflow.
            lines = [
                checksum_line(name, data)
                for name, data in sorted(archives.items(), key=lambda item: item[0])
            ]
            checksums = "\n".join(lines) + "\n"
        with open(os.path.join(self.asset_dir, "checksums.txt"), "w", encoding="utf-8", newline="\n") as handle:
            handle.write(checksums)
        if extra:
            for name, data in extra.items():
                path = os.path.join(self.asset_dir, name)
                if isinstance(data, bytes):
                    with open(path, "wb") as handle:
                        handle.write(data)
                else:
                    os.symlink(data, path)

    def assert_rejected(self, result, needle):
        # Failure must be non-zero and must not echo archive bodies.
        self.assertNotEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("license", result.stdout + result.stderr)
        self.assertNotIn("# readme", result.stdout + result.stderr)
        self.assertIn(needle, result.stderr)

    def test_valid_release_passes(self):
        # Three matching archives and checksums are the only success path.
        self.write_release()
        result = self.run_verifier()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("linux/amd64 ok", result.stdout)
        self.assertIn("linux/arm64 ok", result.stdout)
        self.assertIn("darwin/arm64 ok", result.stdout)

    def test_help_is_available(self):
        result = subprocess.run([sys.executable, SCRIPT, "--help"], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--version", result.stdout)
        self.assertIn("--commit", result.stdout)
        self.assertIn("--sdk", result.stdout)

    def test_missing_archive_is_rejected(self):
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
            if not (goos == "linux" and goarch == "arm64")
        }
        self.write_release(archives=archives)
        result = self.run_verifier()
        self.assert_rejected(result, "exactly the three release archives")

    def test_extra_file_is_rejected(self):
        self.write_release(extra={"notes.txt": b"nope\n"})
        result = self.run_verifier()
        self.assert_rejected(result, "exactly the three release archives")

    def test_symlink_in_directory_is_rejected(self):
        self.write_release(extra={"extra.zip": "checksums.txt"})
        result = self.run_verifier()
        self.assert_rejected(result, "exactly the three release archives")

    def test_checksum_mismatch_is_rejected(self):
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        lines = []
        for name, data in archives.items():
            digest = hashlib.sha256(data).hexdigest()
            if name.endswith("linux_amd64.zip"):
                digest = "0" * 64
            lines.append("{0}  {1}".format(digest, name))
        self.write_release(archives=archives, checksums="\n".join(lines) + "\n")
        result = self.run_verifier()
        self.assert_rejected(result, "checksum mismatch")

    def test_duplicate_checksum_is_rejected(self):
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        repeated = archive_name(VERSION, "linux", "amd64")
        lines = [checksum_line(repeated, archives[repeated]) for _index in range(3)]
        self.write_release(archives=archives, checksums="\n".join(lines) + "\n")
        result = self.run_verifier()
        self.assert_rejected(result, "duplicate")

    def test_checksum_path_traversal_is_rejected(self):
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        lines = []
        for name, data in archives.items():
            shown = "../" + name if name.endswith("linux_amd64.zip") else name
            lines.append(checksum_line(shown, data))
        self.write_release(archives=archives, checksums="\n".join(lines) + "\n")
        result = self.run_verifier()
        self.assert_rejected(result, "bare archive name")

    def test_wrong_version_is_rejected(self):
        self.write_release()
        result = self.run_verifier(version="1.2.4")
        self.assert_rejected(result, "exactly the three release archives")

    def test_wrong_sdk_is_rejected(self):
        self.write_release()
        result = self.run_verifier(sdk=SDK + "-wrong")
        self.assert_rejected(result, "metadata.json")

    def test_wrong_commit_is_rejected(self):
        self.write_release()
        result = self.run_verifier(commit="a" * 40)
        self.assert_rejected(result, "metadata.json")

    def test_wrong_plugin_name_is_rejected(self):
        renamed = json.loads(metadata())
        renamed["name"] = "Credit Manager Pro"
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(
                goos,
                goarch,
                meta=json.dumps(renamed).encode("utf-8") + b"\n",
            )
            if (goos, goarch) == ("linux", "amd64")
            else build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        self.write_release(archives=archives)
        result = self.run_verifier()
        self.assert_rejected(result, "metadata.json")

    def test_boolean_abi_is_rejected(self):
        # True == 1 in Python; the ABI must be the integer 1, not a boolean.
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        abi_meta = json.loads(metadata())
        abi_meta["abi"] = True
        archives[archive_name(VERSION, "linux", "amd64")] = build_zip(
            "linux", "amd64", meta=json.dumps(abi_meta).encode("utf-8") + b"\n"
        )
        self.write_release(archives=archives)
        result = self.run_verifier()
        self.assert_rejected(result, "metadata.json")

    def test_wrong_platform_metadata_is_rejected(self):
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(
                goos,
                goarch,
                meta=metadata(goos="windows", goarch="amd64"),
            )
            if goos == "linux" and goarch == "amd64"
            else build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        self.write_release(archives=archives)
        result = self.run_verifier()
        self.assert_rejected(result, "metadata.json")

    def test_wrong_library_architecture_is_rejected(self):
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(
                goos, goarch, library=elf(183) if goarch == "amd64" else library_for(goos, goarch)
            )
            for goos, goarch, _ext in TARGETS
        }
        self.write_release(archives=archives)
        result = self.run_verifier()
        self.assert_rejected(result, "architecture")

    def test_duplicate_zip_entry_is_rejected(self):
        base = [
            ("{0}.so".format(PLUGIN_ID), library_for("linux", "amd64")),
            ("LICENSE", b"license\n"),
            ("README.md", b"# readme\n"),
            ("metadata.json", metadata(goos="linux", goarch="amd64")),
            ("LICENSE", b"license\n"),
        ]
        buffer = io.BytesIO()
        with zipfile.ZipFile(buffer, "w") as archive:
            # The duplicate name is intentional; silence only the writer warning.
            for name, data in base:
                with warnings.catch_warnings():
                    warnings.simplefilter("ignore", UserWarning)
                    archive.writestr(name, data)
        archives = {
            archive_name(VERSION, goos, goarch): buffer.getvalue()
            if (goos, goarch) == ("linux", "amd64")
            else build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        self.write_release(archives=archives)
        result = self.run_verifier()
        self.assert_rejected(result, "duplicate entry")

    def test_nested_zip_path_is_rejected(self):
        # The library entry keeps its basename but is stored under a directory,
        # so the flat-file layout check runs.
        lib = "{0}.so".format(PLUGIN_ID)
        members = {
            "dir/" + lib: library_for("linux", "amd64"),
            "LICENSE": b"license\n",
            "README.md": b"# readme\n",
            "metadata.json": metadata(),
        }
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(goos, goarch, members=members)
            if (goos, goarch) == ("linux", "amd64")
            else build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        self.write_release(archives=archives)
        result = self.run_verifier()
        self.assert_rejected(result, "flat files")

    def test_versioned_library_name_is_rejected(self):
        # The packager emits the bare <id>.so; a <id>-vX.Y.Z.so member is a
        # layout the host would accept but this repository never produces, so
        # the release gate keeps the archive unique and predictable.
        members = {
            "{0}-v{1}.so".format(PLUGIN_ID, VERSION): library_for("linux", "amd64"),
            "LICENSE": b"license\n",
            "README.md": b"# readme\n",
            "metadata.json": metadata(),
        }
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(goos, goarch, members=members)
            if (goos, goarch) == ("linux", "amd64")
            else build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        self.write_release(archives=archives)
        result = self.run_verifier()
        self.assert_rejected(result, "unexpected")

    def test_extra_zip_member_is_rejected(self):
        members = {
            "{0}.so".format(PLUGIN_ID): library_for("linux", "amd64"),
            "LICENSE": b"license\n",
            "README.md": b"# readme\n",
            "metadata.json": metadata(),
            "extra.txt": b"no\n",
        }
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(goos, goarch, members=members)
            if (goos, goarch) == ("linux", "amd64")
            else build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        self.write_release(archives=archives)
        result = self.run_verifier()
        self.assert_rejected(result, "unexpected")

    def test_zip_symlink_is_rejected(self):
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(
                goos, goarch, symlink="{0}.so".format(PLUGIN_ID)
            )
            if (goos, goarch) == ("linux", "amd64")
            else build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        self.write_release(archives=archives)
        result = self.run_verifier()
        self.assert_rejected(result, "regular files")

    def test_empty_library_is_rejected(self):
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(goos, goarch, library=b"")
            if (goos, goarch) == ("linux", "amd64")
            else build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        self.write_release(archives=archives)
        result = self.run_verifier()
        self.assert_rejected(result, "empty")

    def test_encrypted_member_is_rejected(self):
        # The encryption bit is rejected before any member body can be echoed.
        archives = {
            archive_name(VERSION, goos, goarch): build_zip(goos, goarch, encrypt="LICENSE")
            if (goos, goarch) == ("linux", "amd64")
            else build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        self.write_release(archives=archives)
        result = self.run_verifier()
        self.assert_rejected(result, "encrypted")
        self.assertNotIn("SECRET", result.stdout + result.stderr)

    def test_corrupt_zip_is_rejected(self):
        archives = {
            archive_name(VERSION, goos, goarch): b"not a zip"
            if (goos, goarch) == ("linux", "amd64")
            else build_zip(goos, goarch)
            for goos, goarch, _ext in TARGETS
        }
        self.write_release(archives=archives)
        result = self.run_verifier()
        self.assert_rejected(result, "corrupt")

    def test_non_numeric_version_is_rejected(self):
        result = self.run_verifier(version="1.2")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("X.Y.Z", result.stderr)

    def test_uppercase_commit_is_rejected(self):
        result = self.run_verifier(commit="A" * 40)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("lowercase hex", result.stderr)

    def test_unicode_digits_are_rejected(self):
        result = self.run_verifier(version="\u0661.\u0662.\u0663")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("X.Y.Z", result.stderr)


if __name__ == "__main__":
    unittest.main()
