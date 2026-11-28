"""JadeView Playground 启动入口。

JadeView.dll 是原生库，必须与 Python 解释器位数一致（x86=32位，x64=64位）。
启动前会读取目标 DLL 的 PE 头位数；若当前 Python 位数不匹配，
则自动在本机查找匹配位数的 Python 解释器并重新拉起，避免 WinError 193。
"""
from __future__ import annotations

import glob
import os
from pathlib import Path
import re
import struct
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
DEFAULT_DLL = ROOT / "runtime" / "JadeView.dll"
CACHE_DIR = ROOT / ".data"
REEXEC_ENV = "JADE_PLAYGROUND_REEXEC"

MACHINE_BITS = {0x014C: 32, 0x8664: 64, 0xAA64: 64}


def resolve_target_dll(argv: list[str] | None = None) -> Path:
    """按与 app.main 相同的优先级解析待检查的 Windows DLL 路径。"""
    args = sys.argv[1:] if argv is None else argv
    for index, token in enumerate(args):
        if token == "--dll" and index + 1 < len(args):
            return Path(args[index + 1])
        if token.startswith("--dll="):
            return Path(token.split("=", 1)[1])
    env_lib = os.environ.get("JADEVIEW_LIBRARY")
    if env_lib:
        return Path(env_lib)
    return DEFAULT_DLL


def dll_bits(path: Path) -> int | None:
    """读取 PE 头，返回 JadeView.dll 需要的解释器位数（32 或 64）。"""
    try:
        with path.open("rb") as handle:
            head = handle.read(0x40)
            if len(head) < 0x40 or head[:2] != b"MZ":
                return None
            offset = struct.unpack_from("<I", head, 0x3C)[0]
            handle.seek(offset)
            if handle.read(4) != b"PE\x00\x00":
                return None
            machine = struct.unpack("<H", handle.read(2))[0]
    except OSError:
        return None
    return MACHINE_BITS.get(machine)


def _probe_python(executable: str | Path, expr: str) -> str | None:
    try:
        result = subprocess.run(
            [str(executable), "-c", expr],
            capture_output=True,
            text=True,
            timeout=20,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if result.returncode != 0:
        return None
    lines = result.stdout.strip().splitlines()
    return lines[-1] if lines else None


def python_bits(executable: str | Path) -> int | None:
    """探测解释器位数（32 或 64）。"""
    out = _probe_python(executable, "import struct;print(struct.calcsize('P')*8)")
    if out is None:
        return None
    try:
        return int(out)
    except ValueError:
        return None


def _version_of(executable: str | Path) -> tuple[int, int]:
    out = _probe_python(executable, "import sys;print('%d.%d' % sys.version_info[:2])")
    if out is None:
        return (0, 0)
    try:
        major, minor = out.split(".")
        return int(major), int(minor)
    except ValueError:
        return (0, 0)


def _cache_path(required: int) -> Path:
    return CACHE_DIR / f"runtime-python-{required}.txt"


def candidate_interpreters(required: int) -> list[Path]:
    found: list[Path] = []

    def add(path: str | Path | None) -> None:
        if not path:
            return
        try:
            resolved = Path(path).resolve()
        except OSError:
            return
        if resolved.is_file() and resolved not in found:
            found.append(resolved)

    cache_file = _cache_path(required)
    if cache_file.is_file():
        try:
            add(cache_file.read_text(encoding="utf-8").strip())
        except OSError:
            pass

    add(sys.executable)

    # py launcher 注册过的版本（含 32 位与 64 位）
    try:
        listing = subprocess.run(
            ["py", "-0p"],
            capture_output=True,
            text=True,
            timeout=20,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
        )
        for line in listing.stdout.splitlines():
            match = re.search(r"([A-Za-z]:\\.*?\.exe)", line)
            if match:
                add(match.group(1))
    except (OSError, subprocess.SubprocessError):
        pass

    patterns: list[str] = []
    local_appdata = os.environ.get("LOCALAPPDATA")
    appdata = os.environ.get("APPDATA")
    if local_appdata:
        patterns.append(f"{local_appdata}\\Programs\\Python\\Python3*\\python.exe")
    if appdata:
        uv_arch = "x86" if required == 32 else "x86_64"
        patterns.append(f"{appdata}\\uv\\python\\cpython-3*-windows-{uv_arch}-none\\python.exe")
    patterns.append("C:\\Python3*\\python.exe")
    for pattern in patterns:
        for match in sorted(glob.glob(pattern)):
            add(match)
    return found


def pick_interpreter(required: int) -> Path | None:
    scored: list[tuple[tuple[int, int], Path]] = []
    for candidate in candidate_interpreters(required):
        if python_bits(candidate) != required:
            continue
        version = _version_of(candidate)
        if version < (3, 10):
            continue
        scored.append((version, candidate))
    if not scored:
        return None
    scored.sort(key=lambda item: (item[0] in ((3, 12), (3, 13)), item[0]), reverse=True)
    return scored[0][1]


def remember(required: int, path: Path) -> None:
    try:
        cache_file = _cache_path(required)
        cache_file.parent.mkdir(parents=True, exist_ok=True)
        cache_file.write_text(str(path), encoding="utf-8")
    except OSError:
        pass


def ensure_matching_interpreter() -> int | None:
    """若需要切换解释器并已完成子进程执行，返回其退出码；否则返回 None 继续当前进程。"""
    if os.name != "nt" or getattr(sys, "frozen", False):
        return None
    target_dll = resolve_target_dll()
    required = dll_bits(target_dll)
    current = struct.calcsize("P") * 8
    if not required or required == current:
        return None
    arch_label = "x86 (32 位)" if required == 32 else "x64 (64 位)"
    if os.environ.get(REEXEC_ENV) == "1":
        print(
            f"[run] 已切换解释器但仍不匹配：{target_dll.name} 需要 {arch_label}，当前 {current} 位。\n"
            f"[run] 请手动使用 {required} 位 Python 运行，或替换对应位数的 JadeView.dll。",
            file=sys.stderr,
        )
        return 2
    replacement = pick_interpreter(required)
    if replacement is None:
        print(
            f"[run] {target_dll.name} 为 {arch_label}，当前 Python 为 {current} 位，"
            f"且未找到可用的 {required} 位 Python（>=3.10）。\n"
            f"[run] 请安装 {required} 位 Python（或执行 uv python install）后重试。",
            file=sys.stderr,
        )
        return 2
    print(
        f"[run] 当前解释器为 {current} 位，{target_dll.name} 为 {arch_label} → 切换到 {replacement}",
        file=sys.stderr,
        flush=True,
    )
    remember(required, replacement)
    env = dict(os.environ, **{REEXEC_ENV: "1"})
    script = str(Path(sys.argv[0]).resolve()) if sys.argv and sys.argv[0] else str(ROOT / "run.py")
    try:
        return subprocess.call([str(replacement), script, *sys.argv[1:]], cwd=str(ROOT), env=env)
    except KeyboardInterrupt:
        return 130


def launch() -> int:
    reexec_code = ensure_matching_interpreter()
    if reexec_code is not None:
        return reexec_code
    from hosts.python.app import main as app_main

    return app_main()
