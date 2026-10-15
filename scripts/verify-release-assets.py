#!/usr/bin/env python3
"""Verify a credit-manager release asset directory.

The directory must contain exactly the three native plugin archives
(linux/amd64, linux/arm64, darwin/arm64) and checksums.txt. Archive members,
metadata, the flat layout and the native headers are checked without executing
any library. Only the Python standard library is used.
"""

import argparse
import hashlib
import json
import os
import stat
import struct
import sys
import zipfile

PLUGIN_ID = "cliproxyapi-credit-manager"
PLUGIN_NAME = "Credit Manager"
PLUGIN_ABI = 1
TARGETS = (
    ("linux", "amd64", "so"),
    ("linux", "arm64", "so"),
    ("darwin", "arm64", "dylib"),
)
# e_machine values from the ELF specification.
ELF_MACHINE = {"amd64": 62, "arm64": 183}
# Mach-O filetype cputype CPU_TYPE_ARM64.
MACHO_ARM64 = 0x0100000C
MACHO_MAGIC_64_LE = b"\xcf\xfa\xed\xfe"


def fail(message):
    print("verify-release-assets: {0}".format(message), file=sys.stderr)
    raise SystemExit(1)


def parse_args(argv):
    parser = argparse.ArgumentParser(
        description="Verify credit-manager release assets."
    )
    parser.add_argument("asset_dir", help="directory containing release zips and checksums.txt")
    parser.add_argument("--version", required=True, help="strict numeric X.Y.Z plugin version")
    parser.add_argument("--commit", required=True, help="40-character lowercase git commit SHA")
    parser.add_argument(
        "--sdk",
        required=True,
        help="CLIProxyAPI SDK module and version, e.g. 'github.com/router-for-me/CLIProxyAPI/v8 v8.0.30'",
    )
    return parser.parse_args(argv)


def require_identity(version, commit):
    parts = version.split(".")
    if (
        len(parts) != 3
        or any(not part.isascii() or not part.isdigit() or (len(part) > 1 and part.startswith("0")) for part in parts)
    ):
        fail("version must be strict numeric X.Y.Z")
    if len(commit) != 40 or any(char not in "0123456789abcdef" for char in commit):
        fail("commit must be 40 lowercase hex characters")


def archive_name(version, goos, goarch):
    return "{0}_{1}_{2}_{3}.zip".format(PLUGIN_ID, version, goos, goarch)


def expected_names(version):
    names = [archive_name(version, goos, goarch) for goos, goarch, _ext in TARGETS]
    names.append("checksums.txt")
    return names


def inspect_directory(asset_dir, version):
    if not os.path.isdir(asset_dir) or os.path.islink(asset_dir):
        fail("asset directory is missing")
    try:
        entries = os.listdir(asset_dir)
    except OSError:
        fail("asset directory cannot be listed")
    expected = expected_names(version)
    if sorted(entries) != sorted(expected):
        fail("asset directory must contain exactly the three release archives and checksums.txt")
    for name in expected:
        path = os.path.join(asset_dir, name)
        if os.path.islink(path) or not os.path.isfile(path):
            fail("asset directory must contain regular files only")
    return {name: os.path.join(asset_dir, name) for name in expected}


def parse_checksums(path, version):
    try:
        raw = open(path, "rb").read()
    except OSError:
        fail("checksums.txt cannot be read")
    if raw.startswith(b"\xef\xbb\xbf") or b"\x00" in raw:
        fail("checksums.txt is not plain text")
    try:
        text = raw.decode("utf-8")
    except UnicodeError:
        fail("checksums.txt is not plain text")
    if text.startswith("\ufeff") or "\r" in text:
        fail("checksums.txt must use LF line endings without a BOM")
    lines = text.split("\n")
    if lines and lines[-1] == "":
        lines = lines[:-1]
    if len(lines) != 3 or any(line == "" for line in lines):
        fail("checksums.txt must contain exactly three entries")

    wanted = [archive_name(version, goos, goarch) for goos, goarch, _ext in TARGETS]
    seen = {}
    for line in lines:
        parts = line.split("  ")
        if len(parts) != 2:
            fail("checksums.txt entry must be '<64 lowercase hex>  <archive>'")
        digest, name = parts
        if len(digest) != 64 or any(char not in "0123456789abcdef" for char in digest):
            fail("checksum digest must be 64 lowercase hex characters")
        if (
            name != os.path.basename(name)
            or "/" in name
            or "\\" in name
            or name in (".", "..")
        ):
            fail("checksum path must be a bare archive name")
        if name in seen:
            fail("checksums.txt contains a duplicate archive")
        if name not in wanted:
            fail("checksums.txt names an unexpected archive")
        seen[name] = digest
    if sorted(seen) != sorted(wanted):
        fail("checksums.txt must name exactly the three release archives")
    return seen


def sha256_file(path):
    digest = hashlib.sha256()
    try:
        with open(path, "rb") as handle:
            while True:
                chunk = handle.read(1024 * 1024)
                if not chunk:
                    break
                digest.update(chunk)
    except OSError:
        fail("release archive cannot be read")
    return digest.hexdigest()


def member_names(goos):
    ext = "dylib" if goos == "darwin" else "so"
    return (
        "{0}.{1}".format(PLUGIN_ID, ext),
        "LICENSE",
        "README.md",
        "metadata.json",
    )


def check_zip_entry(info):
    name = info.filename
    if name != info.orig_filename:
        fail("release archive contains an unsafe entry")
    if (
        name.endswith("/")
        or name.startswith("/")
        or "\\" in name
        or "/" in name
        or name in (".", "..")
    ):
        fail("release archive entries must be flat files")
    mode = (info.external_attr >> 16) & 0xFFFF
    if stat.S_ISLNK(mode) or stat.S_ISDIR(mode):
        fail("release archive entries must be regular files")
    if info.flag_bits & 0x1:
        fail("release archive entries must not be encrypted")
    if info.compress_type not in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED):
        fail("release archive uses an unsupported compression method")


def read_member(archive, info):
    try:
        return archive.read(info)
    except (RuntimeError, zipfile.BadZipFile, ValueError, OSError):
        fail("release archive member cannot be read")


def check_elf(data, goarch):
    if len(data) < 20 or data[:4] != b"\x7fELF":
        fail("linux library is not an ELF file")
    ei_class, ei_data = data[4], data[5]
    if ei_class != 2 or ei_data != 1:
        fail("linux library must be ELF64 little-endian")
    machine = struct.unpack_from("<H", data, 18)[0]
    if machine != ELF_MACHINE[goarch]:
        fail("linux library architecture does not match {0}".format(goarch))


def check_macho(data):
    if len(data) < 8 or data[:4] != MACHO_MAGIC_64_LE:
        fail("darwin library is not Mach-O 64-bit little-endian")
    cputype = struct.unpack_from("<I", data, 4)[0]
    if cputype != MACHO_ARM64:
        fail("darwin library architecture is not arm64")


def check_metadata(payload, version, commit, sdk, goos, goarch):
    try:
        text = payload.decode("utf-8")
        meta = json.loads(text)
    except (UnicodeError, ValueError):
        fail("metadata.json is not a JSON object")
    if not isinstance(meta, dict) or any(not isinstance(key, str) for key in meta):
        fail("metadata.json is not a JSON object")
    expected = {
        "id": PLUGIN_ID,
        "name": PLUGIN_NAME,
        "version": version,
        "goos": goos,
        "goarch": goarch,
        "sdk": sdk,
        "commit": commit,
        "abi": PLUGIN_ABI,
    }
    if set(meta) != set(expected):
        fail("metadata.json does not match the requested release identity")
    for key, value in expected.items():
        actual = meta[key]
        if key == "abi":
            # bool is an int subclass; reject true/false for the ABI number.
            if isinstance(actual, bool) or not isinstance(actual, int) or actual != value:
                fail("metadata.json does not match the requested release identity")
            continue
        if not isinstance(actual, str) or actual != value:
            fail("metadata.json does not match the requested release identity")


def verify_archive(path, version, commit, sdk, goos, goarch):
    try:
        archive = zipfile.ZipFile(path)
    except (zipfile.BadZipFile, OSError, ValueError):
        fail("release archive is corrupt")
    try:
        infos = archive.infolist()
        for info in infos:
            check_zip_entry(info)
        names = [info.filename for info in infos]
        expected = member_names(goos)
        if len(names) != len(set(names)):
            fail("release archive contains a duplicate entry")
        if sorted(names) != sorted(expected):
            fail("release archive members are incomplete or unexpected")
        by_name = {info.filename: info for info in infos}
        library_name = expected[0]
        library = read_member(archive, by_name[library_name])
        if not library:
            fail("dynamic library is empty")
        if goos == "linux":
            check_elf(library, goarch)
        else:
            check_macho(library)
        for name in ("LICENSE", "README.md"):
            if not read_member(archive, by_name[name]):
                fail("{0} is empty".format(name))
        check_metadata(read_member(archive, by_name["metadata.json"]), version, commit, sdk, goos, goarch)
    finally:
        archive.close()


def main(argv):
    args = parse_args(argv)
    require_identity(args.version, args.commit)
    paths = inspect_directory(args.asset_dir, args.version)
    checksums = parse_checksums(paths["checksums.txt"], args.version)
    for goos, goarch, _ext in TARGETS:
        name = archive_name(args.version, goos, goarch)
        actual = sha256_file(paths[name])
        if actual != checksums[name]:
            fail("checksum mismatch for {0}".format(name))
        verify_archive(paths[name], args.version, args.commit, args.sdk, goos, goarch)
        print("verify-release-assets: {0}/{1} ok".format(goos, goarch))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
