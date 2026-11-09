"""Minimal in-namespace HTTP CONNECT relay and yt-dlp/ffmpeg pipeline."""

import json
import os
import resource
import selectors
import signal
import socket
import struct
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
MAX_ACTIVE = 64


def load_ytdlp(ytdlp):
    """yt-dlp as a library, from the configured yt-dlp itself: its zipapp, or the conventional venv it is installed
    in (its own site-packages, for this interpreter's version). Anything else, or an import failure, answers None and
    the worker runs one yt-dlp process per lookup instead. The code imported is the code the process would run."""
    try:
        import zipfile
        path = os.path.realpath(ytdlp)
        if zipfile.is_zipfile(path):
            sys.path.insert(0, path)
        else:
            bindir = os.path.dirname(path)
            root = os.path.dirname(bindir)
            if os.path.basename(bindir) != "bin" or not os.path.isfile(os.path.join(root, "pyvenv.cfg")):
                return None
            site = os.path.join(root, "lib", "python%d.%d" % sys.version_info[:2], "site-packages")
            if not os.path.isdir(site):
                return None
            sys.path.insert(0, site)
        import yt_dlp
        if not callable(getattr(yt_dlp, "main", None)):
            return None
        # The extractor classes load now, once, so no lookup child imports them again.
        if hasattr(yt_dlp, "extractor") and callable(getattr(yt_dlp.extractor, "gen_extractor_classes", None)):
            yt_dlp.extractor.gen_extractor_classes()
        return yt_dlp
    except Exception:
        return None


# The JS challenge solver kept alive across lookups (Bot v2 Task 19 fix round 4, Jonas's decision of 2026-11-09).
#
# yt-dlp solves YouTube's JS challenges by starting `node --permission -` per lookup with the solver's lib and core
# scripts and the player JS on stdin (about 0.46 s, most of it parsing the player). The worker instead runs one node,
# the same binary the lookup names with --js-runtimes, inside the same sandbox (same mounts, same egress: none), under
# the same --permission flag, listening on a socket in the worker's private /tmp. Each request runs in a new vm
# context: no global, object or closure of one lookup is seen by another, whichever community asked. What is kept
# between requests is immutable and the same for every caller: the compiled solver script, and each player's
# preprocessed form keyed by the player's own sha256 (a pure function of YouTube's public player JS). The worker
# starts the node on the first lookup that names it and starts it again when it has exited; a lookup that cannot use
# it (no node, a failed request, a yt-dlp whose stdin template changed) runs yt-dlp's own per-lookup node instead.
JSC_SOCKET = "/tmp/dilla-jsc.sock"
JSC_MAX_FRAME = 64 * 1024 * 1024
JSC_TIMEOUT = 20.0
JSC_RESTART_BACKOFF = 5.0
JSC_SERVER = r"""
'use strict';
const net = require('net');
const vm = require('vm');
const crypto = require('crypto');
const SOCKET = process.argv[1];
const MAX_REQUEST = 64 * 1024 * 1024;
const TIMEOUT_MS = 15000;
const scripts = new Map();
const players = new Map();
const hash = (text) => crypto.createHash('sha256').update(text).digest('hex');
function remember(map, key, value, limit) {
  if (map.size >= limit) map.delete(map.keys().next().value);
  map.set(key, value);
}
function solve(request) {
  if (!request || typeof request.lib !== 'string' || typeof request.core !== 'string' || !request.data || typeof request.data !== 'object') {
    throw new Error('malformed request');
  }
  const source = request.lib + '\nObject.assign(globalThis, lib);\n' + request.core + '\n';
  const scriptKey = hash(source);
  let script = scripts.get(scriptKey);
  if (!script) {
    script = new vm.Script(source, { filename: 'jsc-solver.js' });
    remember(scripts, scriptKey, script, 2);
  }
  let data = request.data;
  let playerKey = null;
  if (data.type === 'player' && typeof data.player === 'string') {
    playerKey = hash(data.player);
    const preprocessed = players.get(playerKey);
    if (preprocessed !== undefined) {
      data = { type: 'preprocessed', preprocessed_player: preprocessed, requests: data.requests };
      playerKey = null;
    }
  }
  const quiet = () => {};
  const context = vm.createContext({ URL, URLSearchParams, TextEncoder, TextDecoder, atob, btoa,
    console: { log: quiet, warn: quiet, error: quiet, info: quiet, debug: quiet } });
  script.runInContext(context, { timeout: TIMEOUT_MS });
  context.__dillaRequest = JSON.stringify(data);
  const text = vm.runInContext('JSON.stringify(jsc(JSON.parse(__dillaRequest)))', context, { timeout: TIMEOUT_MS });
  if (playerKey !== null) {
    const result = JSON.parse(text);
    if (result && result.type === 'result' && typeof result.preprocessed_player === 'string') {
      remember(players, playerKey, result.preprocessed_player, 4);
    }
  }
  return text + '\n';
}
function frame(value) {
  const payload = Buffer.from(JSON.stringify(value), 'utf8');
  const head = Buffer.alloc(4);
  head.writeUInt32BE(payload.length, 0);
  return Buffer.concat([head, payload]);
}
const server = net.createServer((conn) => {
  const chunks = [];
  let size = 0;
  let expected = -1;
  conn.setTimeout(30000, () => conn.destroy());
  conn.on('error', () => {});
  conn.on('data', (chunk) => {
    chunks.push(chunk);
    size += chunk.length;
    if (expected < 0 && size >= 4) {
      expected = Buffer.concat(chunks, 4).readUInt32BE(0);
      if (expected > MAX_REQUEST) { conn.destroy(); return; }
    }
    if (expected >= 0 && size >= 4 + expected) {
      conn.removeAllListeners('data');
      let reply;
      try {
        const body = Buffer.concat(chunks, 4 + expected).subarray(4);
        reply = { ok: true, out: solve(JSON.parse(body.toString('utf8'))) };
      } catch (error) {
        reply = { ok: false, error: String((error && error.message) || error).slice(0, 2000) };
      }
      conn.end(frame(reply));
    }
  });
});
server.on('error', () => process.exit(1));
server.listen(SOCKET, () => process.stdout.write('ready\n'));
"""


def runtime_node(args):
    """The node a lookup names (--js-runtimes node:<path>), or None."""
    for i, arg in enumerate(args[:-1]):
        if arg == "--js-runtimes" and args[i + 1].startswith("node:"):
            node = args[i + 1][len("node:"):]
            return node if os.path.isabs(node) else None
    return None


class JscServer:
    """The worker's one long-lived challenge solver: started on demand, started again when it has exited."""

    def __init__(self, env, children):
        self.lock = threading.Lock()
        self.env = env
        self.children = children
        self.proc = None
        self.node = None
        self.failed_at = None

    def _stop_locked(self):
        if self.proc is None:
            return
        try:
            self.proc.kill()
            self.proc.wait(timeout=2)
        except (OSError, subprocess.TimeoutExpired):
            pass
        try:
            self.children.remove(self.proc)
        except ValueError:
            pass
        self.proc = None

    def ensure(self, node):
        if node is None:
            return
        with self.lock:
            if self.proc is not None and self.proc.poll() is None and self.node == node:
                return
            self._stop_locked()
            if self.failed_at is not None and time.monotonic() - self.failed_at < JSC_RESTART_BACKOFF:
                return
            try:
                os.unlink(JSC_SOCKET)
            except OSError:
                pass
            try:
                proc = subprocess.Popen([node, "--permission", "-e", JSC_SERVER, JSC_SOCKET], stdin=subprocess.DEVNULL,
                                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=self.env, close_fds=True,
                                        start_new_session=True)
            except OSError:
                self.failed_at = time.monotonic()
                return
            ready = False
            sel = selectors.DefaultSelector()
            try:
                sel.register(proc.stdout, selectors.EVENT_READ)
                if sel.select(5):
                    ready = os.read(proc.stdout.fileno(), 64) == b"ready\n"
            except OSError:
                ready = False
            finally:
                sel.close()
                proc.stdout.close()
            if not ready:
                try:
                    proc.kill()
                    proc.wait(timeout=2)
                except (OSError, subprocess.TimeoutExpired):
                    pass
                self.failed_at = time.monotonic()
                return
            self.proc, self.node, self.failed_at = proc, node, None
            self.children.append(proc)


def jsc_request(request):
    """One request to the worker's solver; any failure raises (the caller runs node itself)."""
    payload = json.dumps(request, ensure_ascii=False).encode("utf-8")
    if len(payload) > JSC_MAX_FRAME:
        raise ValueError("request too large")
    conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    conn.settimeout(JSC_TIMEOUT)
    try:
        conn.connect(JSC_SOCKET)
        conn.sendall(struct.pack(">I", len(payload)) + payload)

        def exact(n):
            buf = bytearray()
            while len(buf) < n:
                chunk = conn.recv(min(1 << 20, n - len(buf)))
                if not chunk:
                    raise OSError("solver closed")
                buf.extend(chunk)
            return bytes(buf)

        (length,) = struct.unpack(">I", exact(4))
        if length > JSC_MAX_FRAME:
            raise ValueError("reply too large")
        reply = json.loads(exact(length).decode("utf-8"))
    finally:
        conn.close()
    if not isinstance(reply, dict) or reply.get("ok") is not True or not isinstance(reply.get("out"), str):
        raise ValueError("solver error")
    return reply["out"]


def install_jsc_client():
    """Routes yt-dlp's node challenge solving to the worker's solver. The request is built from the same parts as
    yt-dlp's stdin, and used only when yt-dlp's stdin is exactly that template: any other yt-dlp keeps its own path."""
    try:
        from yt_dlp.extractor.youtube.jsc._builtin import ejs as ejs_module
        from yt_dlp.extractor.youtube.jsc._builtin import node as node_module
    except Exception:
        return False
    original_construct = getattr(ejs_module.EJSBaseJCP, "_construct_stdin", None)
    original_run = getattr(node_module.NodeJCP, "_run_js_runtime", None)
    if original_construct is None or original_run is None:
        return False

    def construct(self, player, preprocessed, requests, /):
        stdin = original_construct(self, player, preprocessed, requests)
        self._dilla_jsc = None
        try:
            json_requests = [{"type": request.type.value, "challenges": request.input.challenges} for request in requests]
            data = {"type": "preprocessed", "preprocessed_player": player, "requests": json_requests} if preprocessed else {
                "type": "player", "player": player, "requests": json_requests, "output_preprocessed": True}
            lib, core = self._lib_script.code, self._core_script.code
            expected = ("        " + lib + "\n        Object.assign(globalThis, lib);\n        " + core +
                        "\n        console.log(JSON.stringify(jsc(" + json.dumps(data) + ")));\n        ")
            if stdin == expected:
                self._dilla_jsc = (stdin, {"lib": lib, "core": core, "data": data})
        except Exception:
            self._dilla_jsc = None
        return stdin

    def run(self, stdin, /):
        pending = getattr(self, "_dilla_jsc", None)
        self._dilla_jsc = None
        if pending is not None and pending[0] is stdin:
            try:
                return jsc_request(pending[1])
            except Exception:
                pass
        return original_run(self, stdin)

    ejs_module.EJSBaseJCP._construct_stdin = construct
    node_module.NodeJCP._run_js_runtime = run
    return True


class ForkedLookup:
    """One lookup child: its own session, so its process group (with any node it starts) is killed as one."""

    def __init__(self, pid):
        self.pid = pid

    def terminate(self):
        for kill in (os.killpg, os.kill):
            try:
                kill(self.pid, signal.SIGTERM)
            except OSError:
                pass


def lookup_child(library, args, env, out_fd, err_fd):
    """The child's whole life: yt-dlp's own command line entry, with this lookup's argv, its stdout and stderr on the
    parent's pipes. It never returns."""
    code = 1
    try:
        os.setsid()
        signal.signal(signal.SIGTERM, signal.SIG_DFL)
        signal.signal(signal.SIGINT, signal.SIG_DFL)
        null = os.open(os.devnull, os.O_RDONLY)
        os.dup2(null, 0)
        os.dup2(out_fd, 1)
        os.dup2(err_fd, 2)
        # Nothing else of the worker's: not its reply channel, not another lookup's pipes, not the relay's sockets.
        os.closerange(3, resource.getrlimit(resource.RLIMIT_NOFILE)[0])
        os.environ.clear()
        os.environ.update(env)
        sys.stdin = open(0, "r", closefd=False)
        sys.stdout = open(1, "w", encoding="utf-8", errors="replace", closefd=False)
        sys.stderr = open(2, "w", encoding="utf-8", errors="replace", closefd=False)
        try:
            library.main(args)
            code = 0
        except SystemExit as exit_:
            code = exit_.code if isinstance(exit_.code, int) else (0 if exit_.code is None else 1)
        except BaseException as failure:  # noqa: BLE001 - this lookup's failure, reported as its exit code
            sys.stderr.write("ERROR: %s\n" % type(failure).__name__)
            code = 1
        for stream in (sys.stdout, sys.stderr):
            try:
                stream.flush()
            except (OSError, ValueError):
                pass
    finally:
        os._exit(code & 0xFF)


def serve(ytdlp, limit, env, children, stopping, stop):
    """Long-lived lookup worker: one JSON request per stdin line, one JSON reply per stdout line.

    Only the configured yt-dlp ever runs, with the same environment (proxy, no
    credentials) and the same network namespace as the per-request worker. The
    reply carries yt-dlp's exit code, its stdout (bounded) and the tail of its
    stderr so the caller can classify a failure per request.
    """
    out_lock = threading.Lock()
    slots = threading.BoundedSemaphore(MAX_CONCURRENT)
    # The worker imports yt-dlp once; each lookup is a child forked after the import (Bot v2 Task 19 fix round 3). A
    # lookup pays no interpreter start and no module import and, with the worker's cache dir in its private /tmp, no
    # player download after the first; it still gets a fresh YoutubeDL (no error state, cookie jar or visitor identity
    # from another lookup), is killed with its process group on cancel or timeout, and runs beside the others up to
    # MAX_CONCURRENT. The parent never constructs a YoutubeDL and never makes a request.
    library = load_ytdlp(ytdlp)
    jsc = None
    if library is not None:
        import warnings
        warnings.filterwarnings("ignore", category=DeprecationWarning, message=".*fork.*")
        if install_jsc_client():
            jsc = JscServer(env, children)

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

    state_lock = threading.Lock()
    active = set()  # ids dispatched and not finished
    procs = {}  # id -> running yt-dlp
    cancelled = set()  # ids cancelled before their yt-dlp started

    def kill_group(proc):
        # The whole group: yt-dlp may have started node, and an orphan would keep the stdout pipe open. A forked
        # lookup that has not reached its setsid yet is killed by pid.
        for kill in (os.killpg, os.kill):
            try:
                kill(proc.pid, signal.SIGKILL)
            except OSError:
                pass

    def drain_tail(fd, sink):
        try:
            while True:
                chunk = os.read(fd, 4096)
                if not chunk:
                    return
                sink.extend(chunk)
                if len(sink) > MAX_STDERR_REPLY:
                    del sink[: len(sink) - MAX_STDERR_REPLY]
        except OSError:
            return

    def run_library(rid, args):
        with state_lock:
            if rid in cancelled:
                reply(rid, -9)
                return
        if jsc is not None:
            jsc.ensure(runtime_node(args))
        try:
            out_r, out_w = os.pipe()
            err_r, err_w = os.pipe()
        except OSError:
            reply(rid, 1)
            return
        try:
            # Under out_lock (M3-2): no other thread is between a reply's write and its flush, so the child's copy of
            # the reply buffer is empty.
            with out_lock:
                pid = os.fork()
        except OSError:
            for fd in (out_r, out_w, err_r, err_w):
                os.close(fd)
            reply(rid, 1)
            return
        if pid == 0:
            lookup_child(library, args, env, out_w, err_w)
        reaped = False
        proc = ForkedLookup(pid)
        try:
            os.close(out_w)
            os.close(err_w)
            with state_lock:
                procs[rid] = proc
                was_cancelled = rid in cancelled
            children.append(proc)
            if was_cancelled:
                kill_group(proc)
            err_tail = bytearray()
            err_thread = threading.Thread(target=drain_tail, args=(err_r, err_tail), daemon=True)
            err_thread.start()
            # The output is capped as it is read: a lookup that prints past the limit is killed there.
            data = bytearray()
            too_large = False
            try:
                while True:
                    chunk = os.read(out_r, 32768)
                    if not chunk:
                        break
                    data.extend(chunk)
                    if len(data) > limit:
                        too_large = True
                        kill_group(proc)
                        break
            except OSError:
                kill_group(proc)
            _, status = os.waitpid(pid, 0)
            reaped = True
            err_thread.join(1)
            if too_large:
                reply(rid, 75)
                return
            code = os.waitstatus_to_exitcode(status)
            reply(rid, code, bytes(data).decode("utf-8", "replace"), bytes(err_tail).decode("utf-8", "replace"))
        finally:
            if not reaped:
                # Something failed between the fork and the wait (M3-3): the child is killed and reaped here.
                kill_group(proc)
                try:
                    os.waitpid(pid, 0)
                except OSError:
                    pass
            for fd in (out_r, err_r):
                try:
                    os.close(fd)
                except OSError:
                    pass
            with state_lock:
                procs.pop(rid, None)
            try:
                children.remove(proc)
            except ValueError:
                pass

    def run(rid, args):
        if library is not None:
            run_library(rid, args)
            return
        with state_lock:
            if rid in cancelled:
                reply(rid, -9)
                return
        try:
            proc = subprocess.Popen([ytdlp] + args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, close_fds=True, start_new_session=True)
        except OSError:
            reply(rid, 1)
            return
        with state_lock:
            procs[rid] = proc
            was_cancelled = rid in cancelled
        children.append(proc)
        try:
            if was_cancelled:
                kill_group(proc)
            err_bytes = bytearray()
            err_thread = threading.Thread(target=drain, args=(proc.stderr, err_bytes), daemon=True)
            err_thread.start()
            try:
                data = proc.stdout.read(limit + 1)
                if len(data) > limit:
                    kill_group(proc)
                    proc.wait()
                    reply(rid, 75)
                    return
                code = proc.wait()
                err_thread.join(1)
                reply(rid, code, data.decode("utf-8", "replace"), bytes(err_bytes).decode("utf-8", "replace"))
            except (OSError, ValueError):
                kill_group(proc)
                reply(rid, 1)
        finally:
            with state_lock:
                procs.pop(rid, None)
            try:
                children.remove(proc)
            except ValueError:
                pass

    def handle(rid, args):
        # The slot and the bookkeeping are released on every path, Popen failures included.
        slots.acquire()
        try:
            run(rid, args)
        finally:
            slots.release()
            with state_lock:
                active.discard(rid)
                cancelled.discard(rid)

    for raw in sys.stdin.buffer:
        if stopping.is_set():
            break
        if len(raw) > MAX_REQUEST:
            continue
        try:
            request = json.loads(raw)
            if not isinstance(request, dict):
                continue
        except ValueError:
            continue
        if "cancel" in request:
            rid = request["cancel"]
            if not isinstance(rid, int):
                continue
            proc = None
            with state_lock:
                if rid in active:
                    proc = procs.get(rid)
                    if proc is None:
                        cancelled.add(rid)
            if proc is not None:
                kill_group(proc)
            continue
        rid = request.get("id")
        args = request.get("args")
        if not isinstance(rid, int) or isinstance(rid, bool):
            continue
        if not isinstance(args, list) or len(args) > MAX_ARGS or not all(isinstance(a, str) and "\0" not in a for a in args):
            reply(rid, 125)
            continue
        with state_lock:
            if len(active) >= MAX_ACTIVE or rid in active:
                full = True
            else:
                full = False
                active.add(rid)
        if full:
            reply(rid, 125)
            continue
        # A thread per request, so this loop keeps reading (and can see a cancel) while all slots are busy.
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
