"""Compile the Python host entry (`run.py`) into a standalone native executable via Nuitka.

External runtime artifacts (`runtime/JadeView.dll`, `web/dist/`, `assets/`) are intentionally
NOT bundled inside the executable so they can be updated or swapped independently.
"""
from __future__ import annotations

import argparse
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]


def main() -> int:
    parser = argparse.ArgumentParser(description="Compile JadeView Playground host to native exe")
    parser.add_argument("--output-dir", type=Path, default=ROOT / "dist")
    parser.add_argument("--name", default="playground.exe")
    parser.add_argument("--onefile", action=argparse.BooleanOptionalAction, default=True)
    parser.add_argument("--console", action="store_true", help="Keep console window attached")
    args = parser.parse_args()

    out_dir = args.output_dir.resolve()
    out_dir.mkdir(parents=True, exist_ok=True)
    icon = ROOT / "web" / "public" / "favicon.ico"

    cmd = [
        sys.executable,
        "-m",
        "nuitka",
        f"--mode={'onefile' if args.onefile else 'standalone'}",
        "--msvc=latest",
        "--lto=yes",
        "--remove-output",
        "--assume-yes-for-downloads",
        "--include-package=hosts.python",
        f"--output-dir={out_dir}",
        f"--output-filename={args.name}",
    ]
    if not args.console:
        cmd.append("--windows-console-mode=disable")
    if icon.is_file():
        cmd.append(f"--windows-icon-from-ico={icon}")
    cmd.append(str(ROOT / "run.py"))

    print("Running:", " ".join(cmd), flush=True)
    return subprocess.call(cmd, cwd=ROOT)


if __name__ == "__main__":
    raise SystemExit(main())
