"""Minimal in-namespace HTTP CONNECT relay and yt-dlp/ffmpeg pipeline."""

import json
import os
import resource
import selectors
import signal
import socket
import subprocess
import sys
import threading
import time

READY = b"DILLA-EGRESS-READY /1.0\r\n\r\n"
ACK = b"DILLA-EGRESS-ACK /1.0\r\n\r\n"
MAX_HEADER = 16 * 1024
MAX_STDERR = 64 * 1024


def relay(client):
    client.settimeout(10)
    header = bytearray()
    while len(header) <= MAX_HEADER:
        chunk = client.recv(min(2048, MAX_HEADER + 1 - len(header)))
        if not chunk:
            return
        header.extend(chunk)
        if header.endswith(b"\r\n\r\n"):
            break
    else:
        return
    broker = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    broker.settimeout(10)
    try:
        broker.connect("/run/egress.sock")
        broker.sendall(header)
        response = bytearray()
        while len(response) < 4096:
            chunk = broker.recv(1024)
            if not chunk:
                return
            response.extend(chunk)
            if b"\r\n\r\n" in response:
                boundary = response.index(b"\r\n\r\n") + 4
                client.sendall(response[:boundary])
                if not response.startswith(b"HTTP/1.1 200 "):
                    return
                if response[boundary:]:
                    client.sendall(response[boundary:])
                break
        client.settimeout(None)
        broker.settimeout(None)
        sel = selectors.DefaultSelector()
        sel.register(client, selectors.EVENT_READ, broker)
        sel.register(broker, selectors.EVENT_READ, client)
        while True:
            for key, _ in sel.select(10):
                data = key.fileobj.recv(32768)
                if not data:
                    return
                key.data.sendall(data)
    except OSError:
        return
    finally:
        client.close()
        broker.close()


MAX_REQUEST = 256 * 1024
MAX_ARGS = 128
MAX_STDERR_REPLY = 8 * 1024
MAX_CONCURRENT = 4


def serve(ytdlp, limit, env, children, stopping, stop):
    """Long-lived lookup worker: one JSON request per stdin line, one JSON reply per stdout line.

    Only the configured yt-dlp ever runs, with the same environment (proxy, no
    credentials) and the same network namespace as the per-request worker. The
    reply carries yt-dlp's exit code, its stdout (bounded) and the tail of its
    stderr so the caller can classify a failure per request.
    """
    out_lock = threading.Lock()
    slots = threading.BoundedSemaphore(MAX_CONCURRENT)

    def reply(rid, code, out="", err=""):
        line = json.dumps({"id": rid, "code": code, "out": out, "err": err}, ensure_ascii=True)
        with out_lock:
            sys.stdout.write(line + "\n")
            sys.stdout.flush()

    def drain(stream, sink):
        try:
            while True:
                chunk = stream.read(4096)
                if not chunk:
                    return
                if len(sink) < MAX_STDERR_REPLY:
                    sink.extend(chunk[: MAX_STDERR_REPLY - len(sink)])
        except (OSError, ValueError):
            return

    def handle(rid, args):
        try:
            proc = subprocess.Popen([ytdlp] + args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, close_fds=True)
        except OSError:
            reply(rid, 1)
            return
        children.append(proc)
        err_bytes = bytearray()
        err_thread = threading.Thread(target=drain, args=(proc.stderr, err_bytes), daemon=True)
        err_thread.start()
        try:
            data = proc.stdout.read(limit + 1)
            if len(data) > limit:
                proc.kill()
                proc.wait()
                reply(rid, 75)
                return
            code = proc.wait()
            err_thread.join(1)
            reply(rid, code, data.decode("utf-8", "replace"), bytes(err_bytes).decode("utf-8", "replace"))
        except (OSError, ValueError):
            try:
                proc.kill()
            except OSError:
                pass
            reply(rid, 1)
        finally:
            try:
                children.remove(proc)
            except ValueError:
                pass
            slots.release()

    for raw in sys.stdin.buffer:
        if stopping.is_set():
            break
        if len(raw) > MAX_REQUEST:
            continue
        try:
            request = json.loads(raw)
            rid = request["id"]
            args = request["args"]
        except (ValueError, KeyError, TypeError):
            continue
        if not isinstance(rid, int) or not isinstance(args, list) or len(args) > MAX_ARGS or not all(isinstance(a, str) and "\0" not in a for a in args):
            if isinstance(rid, int):
                reply(rid, 125)
            continue
        slots.acquire()
        threading.Thread(target=handle, args=(rid, args), daemon=True).start()
    stop(signal.SIGTERM, None)
    stopping.set()
    return 0


def main(argv):
    if not argv:
        return 125
    mode = argv[0]
    if mode == "pcm" and len(argv) in (6, 7):
        ytdlp, ffmpeg, node, source, max_pcm = argv[1:6]
        start_at = argv[6] if len(argv) == 7 else "0.000"
        command = None
        output_limit = max_pcm
    elif mode == "serve" and len(argv) == 3:
        output_limit = argv[1]
        command = None
        ytdlp = argv[2]
        ffmpeg = node = source = max_pcm = None
    elif mode == "exec" and len(argv) >= 3:
        output_limit = argv[1]
        command = argv[2:]
        ytdlp = ffmpeg = node = source = max_pcm = None
    else:
        return 125
    try:
        limit = int(output_limit)
        resource.setrlimit(resource.RLIMIT_AS, (2 * 1024**3, 2 * 1024**3))
        resource.setrlimit(resource.RLIMIT_NOFILE, (128, 128))
        resource.setrlimit(resource.RLIMIT_FSIZE, (limit, limit))
    except (ValueError, OSError):
        return 125

    ready = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    ready.settimeout(2)
    try:
        ready.connect("/run/egress.sock")
        ready.sendall(READY)
        if ready.recv(len(ACK)) != ACK:
            return 125
    except OSError:
        return 125
    finally:
        ready.close()

    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        server.bind(("127.0.0.1", 3128))
        server.listen(32)
        server.settimeout(0.2)
    except OSError:
        server.close()
        return 125
    stopping = threading.Event()
    children = []

    def stop(_signum, _frame):
        stopping.set()
        for child in children:
            try:
                child.terminate()
            except OSError:
                pass

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)

    def accept_loop():
        while not stopping.is_set():
            try:
                client, _ = server.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            threading.Thread(target=relay, args=(client,), daemon=True).start()

    threading.Thread(target=accept_loop, daemon=True).start()
    env = {
        "PATH": "/usr/bin:/bin",
        "HOME": "/tmp",
        "TMPDIR": "/tmp",
        "LANG": "C.UTF-8",
        "LC_ALL": "C.UTF-8",
        "HTTP_PROXY": "http://127.0.0.1:3128",
        "HTTPS_PROXY": "http://127.0.0.1:3128",
        "http_proxy": "http://127.0.0.1:3128",
        "https_proxy": "http://127.0.0.1:3128",
        "ALL_PROXY": "http://127.0.0.1:3128",
        "all_proxy": "http://127.0.0.1:3128",
        "NO_PROXY": "",
        "no_proxy": "",
        "PYTHONDONTWRITEBYTECODE": "1",
        "SSL_CERT_FILE": "/etc/ssl/cert.pem",
    }
    if mode == "serve":
        return serve(ytdlp, limit, env, children, stopping, stop)
    if command is not None:
        try:
            child = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=None, env=env, close_fds=True)
            children.append(child)
            total = 0
            too_large = False
            while True:
                # os.read returns what is available: a buffered read(32768) would hold a
                # streaming child's first 32 KB (seconds of audio) back until it filled.
                chunk = os.read(child.stdout.fileno(), 32768)
                if not chunk:
                    break
                if total + len(chunk) > limit:
                    allowed = max(0, limit - total)
                    if allowed:
                        sys.stdout.buffer.write(chunk[:allowed])
                        sys.stdout.buffer.flush()
                    too_large = True
                    stop(signal.SIGTERM, None)
                    break
                total += len(chunk)
                sys.stdout.buffer.write(chunk)
                sys.stdout.buffer.flush()
            child.stdout.close()
            code = child.wait()
            if too_large:
                return 75
            return 0 if code == 0 and not stopping.is_set() else 1
        except OSError:
            stop(signal.SIGTERM, None)
            return 1
        finally:
            stopping.set()
            server.close()
    args = [
        ytdlp, "--ignore-config", "--no-plugin-dirs", "--no-remote-components",
        "--no-warnings", "--no-progress", "--socket-timeout", "10",
        "--extractor-retries", "1", "--format", "bestaudio", "--output", "-",
        "--", source,
    ]
    if node:
        args[1:1] = ["--js-runtimes", "node:" + node]
    def mark(stage):
        print("DILLA_YT_TIMING", stage, time.monotonic_ns(), file=sys.stderr, flush=True)

    try:
        mark("spawn")
        yt = subprocess.Popen(args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=None, env=env, close_fds=True)
        children.append(yt)
        ff = subprocess.Popen(
            [ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-ss", start_at, "-f", "s16le", "-ar", "44100", "-ac", "2", "pipe:1"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=None, env=env, close_fds=True,
        )
        children.append(ff)
        def watch_failure(child):
            if child.wait() != 0 and not stopping.is_set():
                os._exit(1)
        for child in (yt, ff):
            threading.Thread(target=watch_failure, args=(child,), daemon=True).start()
        def pump_ytdlp():
            first = True
            try:
                while True:
                    chunk = os.read(yt.stdout.fileno(), 32768)
                    if not chunk:
                        break
                    if first:
                        mark("first_yt_dlp_byte")
                        first = False
                    ff.stdin.write(chunk)
                    ff.stdin.flush()
            except (OSError, BrokenPipeError):
                pass
            finally:
                yt.stdout.close()
                try:
                    ff.stdin.close()
                except OSError:
                    pass
        threading.Thread(target=pump_ytdlp, daemon=True).start()
        total = 0
        too_large = False
        while True:
            chunk = os.read(ff.stdout.fileno(), 32768)
            if not chunk:
                break
            if total == 0:
                mark("first_pcm")
            if total + len(chunk) > limit:
                allowed = max(0, limit - total)
                if allowed:
                    sys.stdout.buffer.write(chunk[:allowed])
                    sys.stdout.buffer.flush()
                too_large = True
                stop(signal.SIGTERM, None)
                break
            total += len(chunk)
            sys.stdout.buffer.write(chunk)
            sys.stdout.buffer.flush()
        ff.stdout.close()
        ff_code = ff.wait()
        yt_code = yt.wait()
        if too_large:
            return 75
        if stopping.is_set() or ff_code != 0 or yt_code != 0:
            return 1
        return 0
    except OSError:
        stop(signal.SIGTERM, None)
        for child in children:
            try:
                child.wait(timeout=1)
            except subprocess.TimeoutExpired:
                child.kill()
        return 1
    finally:
        stopping.set()
        server.close()


raise SystemExit(main(sys.argv[1:]))
