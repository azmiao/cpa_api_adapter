#!/usr/bin/env python3
"""Package one native CPA plugin library as a plugin-store release asset."""

import argparse
import pathlib
import re
import zipfile

PLUGIN_ID = "cpa-api-adapter"
PLATFORMS = {
    "linux_amd64": f"{PLUGIN_ID}.so",
    "linux_arm64": f"{PLUGIN_ID}.so",
    "darwin_amd64": f"{PLUGIN_ID}.dylib",
    "darwin_arm64": f"{PLUGIN_ID}.dylib",
    "windows_amd64": f"{PLUGIN_ID}.dll",
}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("version")
    parser.add_argument("platform", choices=PLATFORMS)
    parser.add_argument("library", type=pathlib.Path)
    args = parser.parse_args()

    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", args.version):
        parser.error("version must be in X.Y.Z format without a leading v")
    expected_name = PLATFORMS[args.platform]
    if args.library.name != expected_name or not args.library.is_file():
        parser.error(f"library must be an existing {expected_name} file")
    if args.library.stat().st_size == 0:
        parser.error("library is empty")

    output = pathlib.Path("dist") / f"{PLUGIN_ID}_{args.version}_{args.platform}.zip"
    output.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(output, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        archive.write(args.library, expected_name)
    with zipfile.ZipFile(output) as archive:
        if archive.namelist() != [expected_name] or archive.testzip() is not None:
            raise SystemExit("invalid plugin archive")
        if archive.getinfo(expected_name).file_size != args.library.stat().st_size:
            raise SystemExit("plugin archive size mismatch")
    print(output)


if __name__ == "__main__":
    main()
