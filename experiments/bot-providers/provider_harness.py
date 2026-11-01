#!/usr/bin/env python3
"""Development-only provider prerequisite and local Opus checks."""

from __future__ import annotations

import argparse
import json
import math
import os
import re
import selectors
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path


DEFAULT_TIMEOUT = 5.0
MAX_OUTPUT = 65536
MAX_DURATION = 5.0
DOCTOR_TOOLS = ("yt-dlp", "ffmpeg", "ffprobe", "js-runtime")
DEFAULT_NAMES = {
    "yt-dlp": ("yt-dlp",),
    "ffmpeg": ("ffmpeg",),
    "ffprobe": ("ffprobe",),
    "js-runtime": ("deno", "node", "bun"),
}
VERSION_ARGS = {
    "yt-dlp": ("--version",),
    "ffmpeg": ("-version",),
    "ffprobe": ("-version",),
    "js-runtime": ("--version",),
}


class HarnessError(Exception):
    def __init__(self, kind: str, output: bytes = b"") -> None:
        super().__init__(kind)
        self.kind = kind
        self.output = output


@dataclass(frozen=True)
class ProcessResult:
    returncode: int
    output: bytes


@dataclass(frozen=True)
class ToolResult:
    available: bool
    version: str = ""
    reason: str = ""


def _group_exists(pgid: int) -> bool:
    try:
        os.killpg(pgid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def _signal_group(pgid: int, signum: int) -> None:
    try:
        os.killpg(pgid, signum)
    except ProcessLookupError:
        pass
    except PermissionError:
        # The child is started in a new session, so this fallback is only for
        # an unusual platform restriction. It still reaps the direct child.
        pass


def _terminate_and_reap(process: subprocess.Popen[bytes]) -> None:
    """Stop the entire invocation group, even when its original leader exited."""
    pgid = process.pid
    _signal_group(pgid, signal.SIGTERM)
    grace_deadline = time.monotonic() + 0.2
    while _group_exists(pgid) and time.monotonic() < grace_deadline:
        try:
            process.wait(timeout=min(0.025, max(0, grace_deadline - time.monotonic())))
        except subprocess.TimeoutExpired:
            pass
        time.sleep(0.01)
    if _group_exists(pgid):
        _signal_group(pgid, signal.SIGKILL)
    try:
        process.wait(timeout=0.5)
    except subprocess.TimeoutExpired:
        process.kill()
        try:
            process.wait(timeout=0.5)
        except subprocess.TimeoutExpired:
            pass


def run_bounded(
    argv: list[str] | tuple[str, ...],
    *,
    timeout: float = DEFAULT_TIMEOUT,
    output_limit: int = MAX_OUTPUT,
) -> ProcessResult:
    """Run an argv vector without a shell, bounding time and captured bytes."""
    if not argv or not all(isinstance(arg, str) for arg in argv):
        raise ValueError("argv must be a non-empty string sequence")
    try:
        process = subprocess.Popen(
            list(argv),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            shell=False,
            start_new_session=True,
        )
    except (FileNotFoundError, PermissionError, OSError):
        raise HarnessError("unavailable") from None

    assert process.stdout is not None
    output = bytearray()
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ)
    deadline = time.monotonic() + timeout
    try:
        while selector.get_map():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise HarnessError("timeout", bytes(output))
            events = selector.select(min(remaining, 0.1))
            for key, _ in events:
                chunk = os.read(key.fd, min(4096, output_limit + 1 - len(output)))
                if not chunk:
                    selector.unregister(key.fileobj)
                    continue
                output.extend(chunk)
                if len(output) > output_limit:
                    raise HarnessError("output limit", bytes(output[:output_limit]))
        remaining = max(0, deadline - time.monotonic())
        try:
            returncode = process.wait(timeout=remaining)
        except subprocess.TimeoutExpired:
            raise HarnessError("timeout", bytes(output)) from None
        return ProcessResult(returncode, bytes(output))
    finally:
        selector.close()
        process.stdout.close()
        _terminate_and_reap(process)


def _resolve_tool(name: str, configured: str | None = None) -> str | None:
    if configured:
        return configured
    for candidate in DEFAULT_NAMES[name]:
        path = shutil.which(candidate)
        if path:
            return path
    return None


def _version_token(text: str) -> str:
    first_line = text.splitlines()[0] if text else ""
    match = re.search(r"\b(?:version\s+)?[nNvV]?(\d+\.\d+(?:\.\d+){0,3}(?:[-+][A-Za-z0-9.-]+)?)", first_line)
    return match.group(1) if match else "version reported"


def probe_tool(name: str, path: str) -> ToolResult:
    result = run_bounded([path, *VERSION_ARGS[name]], timeout=DEFAULT_TIMEOUT)
    if result.returncode != 0:
        return ToolResult(False, reason="version command failed")
    # Only a version-shaped token leaves this process. Raw tool output can
    # contain paths, URLs, or other operator data and is never printed.
    version = _version_token(result.output.decode("utf-8", "replace"))
    return ToolResult(True, version=version)


def doctor(overrides: dict[str, str | None] | None = None) -> int:
    overrides = overrides or {}
    missing = False
    for name in DOCTOR_TOOLS:
        path = _resolve_tool(name, overrides.get(name))
        if not path:
            print(f"{name}: missing")
            missing = True
            continue
        try:
            result = probe_tool(name, path)
        except HarnessError as error:
            result = ToolResult(False, reason=error.kind)
        if result.available:
            print(f"{name}: available (version {result.version})")
        else:
            print(f"{name}: unavailable ({result.reason})")
            missing = True
    print("provider qualification: unverified")
    return 1 if missing else 0


def encode_opus(ffmpeg: str, destination: Path, *, duration: float = 1.0) -> None:
    if not math.isfinite(duration) or duration <= 0 or duration > MAX_DURATION:
        raise ValueError(f"duration must be >0 and <= {MAX_DURATION:g} seconds")
    argv = [
        ffmpeg,
        "-hide_banner",
        "-loglevel",
        "error",
        "-f",
        "lavfi",
        "-i",
        f"sine=frequency=997:sample_rate=48000:duration={duration:g}",
        "-map_metadata",
        "-1",
        "-ac",
        "2",
        "-ar",
        "48000",
        "-c:a",
        "libopus",
        "-application",
        "audio",
        "-frame_duration",
        "20",
        "-b:a",
        "96k",
        "-vbr",
        "off",
        "-dtx",
        "0",
        "-f",
        "ogg",
        "-y",
        str(destination),
    ]
    result = run_bounded(argv, timeout=20, output_limit=MAX_OUTPUT)
    if result.returncode != 0:
        raise HarnessError("command failed", result.output)
    if not destination.is_file() or destination.stat().st_size == 0:
        raise HarnessError("empty output")


def validate_opus(ffprobe: str, audio: Path) -> int:
    if not audio.is_file() or audio.stat().st_size == 0:
        raise HarnessError("invalid stream")
    argv = [
        ffprobe,
        "-v",
        "error",
        "-select_streams",
        "a:0",
        "-show_entries",
        "stream=codec_name,sample_rate,channels,bit_rate:packet=size,duration_time",
        "-show_packets",
        "-of",
        "json",
        str(audio),
    ]
    result = run_bounded(argv, timeout=10, output_limit=MAX_OUTPUT)
    if result.returncode != 0:
        raise HarnessError("invalid stream", result.output)
    try:
        data = json.loads(result.output)
        streams = data["streams"]
        packets = data["packets"]
        stream = streams[0]
        if (
            stream.get("codec_name") != "opus"
            or str(stream.get("sample_rate")) != "48000"
            or int(stream.get("channels", 0)) != 2
            or len(packets) < 2
        ):
            raise ValueError
        for index, packet in enumerate(packets):
            duration = float(packet["duration_time"])
            if int(packet.get("size", 0)) != 240:
                raise ValueError
            if index < len(packets) - 1 and abs(duration - 0.02) > 0.0005:
                raise ValueError
            if index == len(packets) - 1 and not (0 < duration <= 0.0205):
                raise ValueError
    except (KeyError, IndexError, TypeError, ValueError, json.JSONDecodeError):
        raise HarnessError("invalid stream", result.output[:256]) from None
    return len(packets)


def _path_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--yt-dlp-bin")
    parser.add_argument("--ffmpeg-bin")
    parser.add_argument("--ffprobe-bin")
    parser.add_argument("--js-runtime-bin")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    doctor_parser = commands.add_parser("doctor", help="check bounded provider prerequisites")
    _path_args(doctor_parser)
    smoke_parser = commands.add_parser("opus-smoke", help="encode and inspect a short local Opus test tone")
    smoke_parser.add_argument("--ffmpeg-bin")
    smoke_parser.add_argument("--ffprobe-bin")
    smoke_parser.add_argument("--duration", type=float, default=1.0)
    args = parser.parse_args(argv)
    try:
        if args.command == "doctor":
            overrides = {name: getattr(args, name.replace("-", "_") + "_bin") for name in DOCTOR_TOOLS}
            return doctor(overrides)
        ffmpeg = _resolve_tool("ffmpeg", args.ffmpeg_bin)
        ffprobe = _resolve_tool("ffprobe", args.ffprobe_bin)
        if not ffmpeg or not ffprobe:
            raise HarnessError("ffmpeg and ffprobe are required")
        with tempfile.TemporaryDirectory(prefix="bot-provider-opus-") as temp:
            output = Path(temp) / "smoke.ogg"
            encode_opus(ffmpeg, output, duration=args.duration)
            packet_count = validate_opus(ffprobe, output)
            print(f"opus encoding: valid Opus stream; {packet_count} packets; 48 kHz stereo; 20 ms frame cadence; 96 kbps CBR")
            print("provider playback and browser SFrame decryption: unverified")
        return 0
    except HarnessError as error:
        print(f"error: {error.kind}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
