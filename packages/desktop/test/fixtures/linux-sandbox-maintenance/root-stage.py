# CI ONLY. Pass this fixed source as argv to OS Python -I -S, never a file path.
import base64
import errno
import fcntl
import select
import time
import hashlib
import json
import os
import re
import stat
import sys

ANCHOR = "/"
PARTS = ("usr", "lib", "dilla-sandbox-maintenance-ci")
LIFE_ANCHOR = "/run"
LIFE_NAME = "dilla-sandbox-maintenance-ci"
LOCK_TIMEOUT = 5.0
INPUT_TIMEOUT = 30.0
OWNER_UID = 0
OWNER_GID = 0
MAX_NATIVE = 33554432
MAX_HELPER = 1048576
MARKER = ".ci-owner.json"
NEXT = ".ci-owner.next"
SAFE_ENV = {"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C", "LC_ALL": "C"}


def require(ok):
    if not ok:
        raise ValueError("unsafe fixture state")


def attrs(fd):
    for name in ("security.capability", "system.posix_acl_default", "system.posix_acl_access"):
        try:
            require(not os.getxattr(fd, name))
        except OSError as error:
            require(error.errno in (errno.ENODATA, errno.ENOTSUP))


def mount_id(fd):
    with open("/proc/self/fdinfo/" + str(fd), "rb") as source:
        raw = source.read(4097)
    require(len(raw) <= 4096)
    found = re.findall(rb"^mnt_id:\s*([0-9]+)$", raw, re.M)
    require(len(found) == 1)
    return int(found[0])


def info(fd):
    s = os.fstat(fd)
    return (s.st_dev, s.st_ino, s.st_mode, s.st_uid, s.st_gid, s.st_nlink, s.st_size, s.st_mtime_ns, s.st_ctime_ns)


def safe_dir(fd, mode=None):
    s = os.fstat(fd)
    require(stat.S_ISDIR(s.st_mode) and s.st_uid == OWNER_UID and s.st_gid == OWNER_GID and s.st_mode & 0o7022 == 0)
    if mode is not None:
        require(stat.S_IMODE(s.st_mode) == mode)
    attrs(fd)


def descend(parent, name, mode):
    fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
    try:
        safe_dir(fd, mode)
        require(os.fstat(parent).st_dev == os.fstat(fd).st_dev and mount_id(parent) == mount_id(fd))
        return fd
    except Exception:
        os.close(fd)
        raise


def parent_directory():
    fd = os.open(ANCHOR, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        safe_dir(fd)
        for name in PARTS[:-1]:
            child = descend(fd, name, None)
            os.close(fd)
            fd = child
        return fd
    except Exception:
        os.close(fd)
        raise


def named(parent, name, fd, snapshot):
    require(info(fd) == snapshot)
    s = os.stat(name, dir_fd=parent, follow_symlinks=False)
    require((s.st_dev, s.st_ino, s.st_mode, s.st_uid, s.st_gid, s.st_nlink, s.st_size, s.st_mtime_ns, s.st_ctime_ns) == snapshot)


def pin(parent, name, modes, maximum):
    try:
        before = os.stat(name, dir_fd=parent, follow_symlinks=False)
    except FileNotFoundError:
        return None
    require(stat.S_ISREG(before.st_mode) and before.st_uid == OWNER_UID and before.st_gid == OWNER_GID and before.st_nlink == 1 and 0 <= before.st_size <= maximum and stat.S_IMODE(before.st_mode) in modes)
    fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK, dir_fd=parent)
    try:
        current = info(fd)
        require(current == (before.st_dev, before.st_ino, before.st_mode, before.st_uid, before.st_gid, before.st_nlink, before.st_size, before.st_mtime_ns, before.st_ctime_ns))
        require(os.fstat(parent).st_dev == before.st_dev and mount_id(parent) == mount_id(fd))
        attrs(fd)
        named(parent, name, fd, current)
        return (fd, current)
    except Exception:
        os.close(fd)
        raise


def read_pin(p, maximum):
    fd, snapshot = p
    chunks = []
    total = 0
    while total <= maximum:
        chunk = os.pread(fd, min(65536, maximum + 1 - total), total)
        if not chunk:
            break
        chunks.append(chunk)
        total += len(chunk)
    require(total == snapshot[6] and total <= maximum and info(fd) == snapshot)
    return b"".join(chunks)


def exact_pairs(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result)
        result[key] = value
    return result


def digest(value):
    require(type(value) is str and re.fullmatch(r"[A-Za-z0-9+/]{86}==", value) is not None)
    require(base64.b64encode(base64.b64decode(value, validate=True)).decode("ascii") == value)
    return value


def marker(root, identity):
    p = pin(root, MARKER, (0o600,), 4096)
    require(p is not None)
    try:
        value = json.loads(read_pin(p, 4096), object_pairs_hook=exact_pairs, parse_constant=lambda _: require(False))
        require(type(value) is dict and set(value) == {"format", "runId", "attempt", "sha", "nonce", "device", "inode", "phase", "nativeSha512"})
        require(type(value["format"]) is int and value["format"] == 1)
        require([value["runId"], value["attempt"], value["sha"]] == identity)
        require(type(value["nonce"]) is str and re.fullmatch(r"[0-9a-f]{64}", value["nonce"]) is not None)
        s = os.fstat(root)
        require(value["device"] == str(s.st_dev) and value["inode"] == str(s.st_ino))
        require(value["phase"] in ("initialized", "verified"))
        if value["phase"] == "verified":
            digest(value["nativeSha512"])
        else:
            require(value["nativeSha512"] == "")
        return value
    finally:
        os.close(p[0])


def write_marker(root, value, first):
    raw = json.dumps(value, separators=(",", ":")).encode("ascii")
    require(len(raw) <= 4096)
    name = MARKER if first else NEXT
    fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=root)
    try:
        os.fchmod(fd, 0o600)
        offset = 0
        while offset < len(raw):
            n = os.write(fd, raw[offset:])
            require(n > 0)
            offset += n
        os.fsync(fd)
    finally:
        os.close(fd)
    if not first:
        os.rename(NEXT, MARKER, src_dir_fd=root, dst_dir_fd=root)
    os.fsync(root)


def initialize(parent, identity, lifecycle):
    # mkdir must fail on any preexisting entry. Never adopt it, even if empty.
    os.mkdir(PARTS[-1], 0o755, dir_fd=parent)
    root = None
    try:
        root = os.open(PARTS[-1], os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
        safe_dir(root)
        require(os.fstat(root).st_dev == os.fstat(parent).st_dev and mount_id(root) == mount_id(parent))
        os.fchmod(root, 0o755)
        s = os.fstat(root)
        value = dict(format=1, runId=identity[0], attempt=identity[1], sha=identity[2], nonce=lifecycle["nonce"], device=str(s.st_dev), inode=str(s.st_ino), phase="initialized", nativeSha512="")
        write_marker(root, value, True)
        os.fsync(parent)
        os.mkdir(".runner", 0o700, dir_fd=root)
        runner = descend(root, ".runner", 0o700)
        os.fsync(runner)
        os.close(runner)
        os.fsync(root)
    except Exception:
        # Only this live invocation has proof for its newly created inode.
        # Do not unlink a replacement or any unexpected child on init failure.
        if root is not None:
            current = os.stat(PARTS[-1], dir_fd=parent, follow_symlinks=False)
            held = os.fstat(root)
            require((current.st_dev, current.st_ino) == (held.st_dev, held.st_ino))
            names = entries(root, (MARKER, ".runner"))
            if set(names) <= {MARKER}:
                if MARKER in names:
                    p = pin(root, MARKER, (0o600,), 4096)
                    require(p is not None)
                    named(root, MARKER, *p)
                    os.unlink(MARKER, dir_fd=root)
                    os.close(p[0])
                os.rmdir(PARTS[-1], dir_fd=parent)
                os.fsync(parent)
        raise
    finally:
        if root is not None:
            os.close(root)


def entries(fd, allowed):
    # listdir is bounded by a fixed, root-only CI namespace, and reject before
    # any unlink. Never descend recursively through an unexpected child.
    names = []
    with os.scandir(fd) as scan:
        for entry in scan:
            require(len(names) < 16 and entry.name in allowed)
            names.append(entry.name)
    require(set(names) <= set(allowed))
    return names


def copied_files(root):
    runner = descend(root, ".runner", 0o700)
    try:
        require(set(entries(runner, ("native.test", "credential-v1", "credential-v2"))) == {"native.test", "credential-v1", "credential-v2"})
        result = []
        for parent, name, mode, limit in ((runner, "native.test", 0o700, MAX_NATIVE), (runner, "credential-v1", 0o555, MAX_HELPER), (runner, "credential-v2", 0o555, MAX_HELPER), (root, "observer", 0o555, MAX_HELPER)):
            p = pin(parent, name, (mode,), limit)
            require(p is not None)
            try:
                raw = read_pin(p, limit)
                require(len(raw) >= 20 and raw[:6] == b"\x7fELF\x02\x01" and raw[18:20] == b"\x3e\x00")
                result.append(base64.b64encode(hashlib.sha512(raw).digest()).decode("ascii"))
            finally:
                os.close(p[0])
        return result
    finally:
        os.close(runner)


def partial_cleanup(parent, root):
    root_names = entries(root, (MARKER, NEXT, ".runner", "observer"))
    pinned = []
    runner = None
    try:
        if ".runner" in root_names:
            runner = descend(root, ".runner", 0o700)
            for name in entries(runner, ("native.test", "credential-v1", "credential-v2")):
                p = pin(runner, name, (0o600, 0o700, 0o555), MAX_NATIVE if name == "native.test" else MAX_HELPER)
                require(p is not None)
                pinned.append((runner, name, p))
        for name in ("observer", NEXT):
            if name in root_names:
                p = pin(root, name, (0o600, 0o700, 0o555) if name == "observer" else (0o600,), MAX_HELPER if name == "observer" else 4096)
                require(p is not None)
                pinned.append((root, name, p))
        # Validate every entry before deleting the first; partial ELF bytes
        # need no hash, but must be non-SUID and have no capability or ACL.
        for folder, name, p in pinned:
            named(folder, name, *p)
        for folder, name, p in pinned:
            named(folder, name, *p)
            os.unlink(name, dir_fd=folder)
            os.fsync(folder)
        if runner is not None:
            require(not entries(runner, ()))
            held = os.fstat(runner)
            current = os.stat(".runner", dir_fd=root, follow_symlinks=False)
            require((current.st_dev, current.st_ino) == (held.st_dev, held.st_ino))
            os.rmdir(".runner", dir_fd=root)
        require(set(entries(root, (MARKER,))) == {MARKER})
        p = pin(root, MARKER, (0o600,), 4096)
        require(p is not None)
        try:
            named(root, MARKER, *p)
            os.unlink(MARKER, dir_fd=root)
            os.fsync(root)
        finally:
            os.close(p[0])
        require(not entries(root, ()))
        held = os.fstat(root)
        current = os.stat(PARTS[-1], dir_fd=parent, follow_symlinks=False)
        require((current.st_dev, current.st_ino) == (held.st_dev, held.st_ino))
        os.rmdir(PARTS[-1], dir_fd=parent)
        os.fsync(parent)
    finally:
        for _, _, p in pinned:
            os.close(p[0])
        if runner is not None:
            os.close(runner)


def cleanup(parent, root, value):
    names = entries(root, (MARKER, NEXT, ".runner", "observer", "appimage", "native"))
    if "appimage" not in names and "native" not in names:
        partial_cleanup(parent, root)
        return
    require(value["phase"] == "verified")
    runner = descend(root, ".runner", 0o700)
    p = None
    try:
        p = pin(runner, "native.test", (0o700,), MAX_NATIVE)
        require(p is not None)
        raw = read_pin(p, MAX_NATIVE)
        require(base64.b64encode(hashlib.sha512(raw).digest()).decode("ascii") == value["nativeSha512"])
        require(len(raw) >= 20 and raw[:6] == b"\x7fELF\x02\x01" and raw[18:20] == b"\x3e\x00")
        named(runner, "native.test", *p)
        require(os.execve in os.supports_fd)
        executable = fcntl.fcntl(p[0], fcntl.F_DUPFD_CLOEXEC, 32)
        inherit_lease()
        os.execve(executable, ["native.test", "-test.run=^TestDisposableRootCleanup$", "-test.timeout=25s", "-test.v"], SAFE_ENV)
    finally:
        if p is not None:
            os.close(p[0])
        os.close(runner)



LEASE = None


def write_lifecycle(control, value, first=False):
    raw = json.dumps(value, separators=(",", ":")).encode("ascii")
    require(len(raw) <= 8192)
    name = "state.json" if first else "state.next"
    fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=control)
    try:
        os.fchmod(fd, 0o600)
        offset = 0
        while offset < len(raw):
            n = os.write(fd, raw[offset:])
            require(n > 0)
            offset += n
        os.fsync(fd)
    finally:
        os.close(fd)
    if not first:
        os.rename("state.next", "state.json", src_dir_fd=control, dst_dir_fd=control)
    os.fsync(control)


def read_lifecycle(control, identity):
    p = pin(control, "state.json", (0o600,), 8192)
    require(p is not None)
    try:
        v = json.loads(read_pin(p, 8192), object_pairs_hook=exact_pairs, parse_constant=lambda _: require(False))
    finally:
        os.close(p[0])
    require(type(v) is dict and set(v) == {"format", "runId", "attempt", "sha", "nonce", "phase", "device", "inode", "hashes", "lockDevice", "lockInode"})
    require(type(v["format"]) is int and v["format"] == 1 and [v["runId"], v["attempt"], v["sha"]] == identity)
    require(type(v["nonce"]) is str and re.fullmatch(r"[0-9a-f]{64}", v["nonce"]) is not None)
    require(v["phase"] in ("initializing", "active", "closed"))
    require(type(v["hashes"]) is list and len(v["hashes"]) in (0, 4))
    if len(v["hashes"]) == 0:
        require(v["phase"] == "closed" and v["device"] == "0" and v["inode"] == "0")
    for h in v["hashes"]:
        digest(h)
    for key in ("device", "inode", "lockDevice", "lockInode"):
        require(type(v[key]) is str and re.fullmatch(r"0|[1-9][0-9]{0,19}", v[key]) is not None)
    return v


def lifecycle(role, identity, hashes):
    global LEASE
    parent = os.open(LIFE_ANCHOR, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
    control = None
    try:
        safe_dir(parent)
        fresh = False
        try:
            control = descend(parent, LIFE_NAME, 0o700)
        except FileNotFoundError:
            require(role in ("init", "cleanup"))
            os.mkdir(LIFE_NAME, 0o700, dir_fd=parent)
            control = descend(parent, LIFE_NAME, 0o700)
            fresh = True
        entries(control, ("lock", "state.json", "state.next"))
        flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK
        if fresh:
            flags |= os.O_CREAT | os.O_EXCL
        fd = os.open("lock", flags, 0o600, dir_fd=control)
        try:
            s = os.fstat(fd)
            require(stat.S_ISREG(s.st_mode) and s.st_uid == OWNER_UID and s.st_gid == OWNER_GID and s.st_nlink == 1 and s.st_size == 0 and stat.S_IMODE(s.st_mode) == 0o600)
            attrs(fd)
            require(s.st_dev == os.fstat(control).st_dev and mount_id(fd) == mount_id(control))
            snapshot = info(fd)
            deadline = time.monotonic() + LOCK_TIMEOUT
            while True:
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    require(role != "cleanup" and time.monotonic() < deadline)
                    time.sleep(0.02)
            named(control, "lock", fd, snapshot)
            if fresh:
                require(role in ("init", "cleanup"))
                # Closing an as-yet absent admission is still durable: a late
                # initializer must not create payload after passing cleanup.
                v = dict(format=1, runId=identity[0], attempt=identity[1], sha=identity[2], nonce=os.urandom(32).hex(), phase="closed" if role == "cleanup" else "initializing", device="0", inode="0", hashes=hashes, lockDevice=str(s.st_dev), lockInode=str(s.st_ino))
                write_lifecycle(control, v, True)
                os.fsync(parent)
            else:
                v = read_lifecycle(control, identity)
                require(role != "init")
                require(v["phase"] != "initializing")
            require(v["lockDevice"] == str(s.st_dev) and v["lockInode"] == str(s.st_ino))
            pending = pin(control, "state.next", (0o600,), 8192)
            if pending is not None:
                try:
                    require(role == "cleanup")
                    named(control, "state.next", *pending)
                    os.unlink("state.next", dir_fd=control)
                    os.fsync(control)
                finally:
                    os.close(pending[0])
            if role != "cleanup":
                require(fresh or v["phase"] == "active")
            LEASE = fd
            return control, v
        except Exception:
            os.close(fd)
            raise
    except Exception:
        if control is not None:
            os.close(control)
        raise
    finally:
        os.close(parent)


def inherit_lease():
    require(LEASE is not None and fcntl.fcntl(LEASE, fcntl.F_GETFL) & os.O_ACCMODE == os.O_RDONLY)
    # Read-only is not authority-free: every propagated FD retains the same
    # flock open-file-description. Only fixed hash-verified helpers receive it.
    duplicate = fcntl.fcntl(LEASE, fcntl.F_DUPFD_CLOEXEC, 32)
    try:
        os.dup2(duplicate, 3, inheritable=True)
    finally:
        os.close(duplicate)


def stage_copy(root, value, state, role):
    require(value["phase"] == "initialized")
    choices = {"stage-native": (0, "native.test", 0o700, MAX_NATIVE), "stage-v1": (1, "credential-v1", 0o555, MAX_HELPER), "stage-v2": (2, "credential-v2", 0o555, MAX_HELPER), "stage-observer": (3, "observer", 0o555, MAX_HELPER)}
    index, name, mode, limit = choices[role]
    folder = root if index == 3 else descend(root, ".runner", 0o700)
    fd = None
    try:
        fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=folder)
        h = hashlib.sha512()
        total = 0
        deadline = time.monotonic() + INPUT_TIMEOUT
        while True:
            remaining = deadline - time.monotonic()
            require(remaining > 0 and select.select([0], [], [], remaining)[0])
            chunk = os.read(0, min(65536, limit + 1 - total))
            if not chunk:
                break
            total += len(chunk)
            require(total <= limit)
            h.update(chunk)
            offset = 0
            while offset < len(chunk):
                n = os.write(fd, chunk[offset:])
                require(n > 0)
                offset += n
        require(base64.b64encode(h.digest()).decode("ascii") == state["hashes"][index])
        os.fchmod(fd, mode)
        os.fsync(fd)
        os.close(fd)
        fd = None
        os.fsync(folder)
    finally:
        if fd is not None:
            os.close(fd)
        if folder != root:
            os.close(folder)


def run():
    require(os.geteuid() == OWNER_UID and len(sys.argv) >= 5)
    role, identity = sys.argv[1], sys.argv[2:5]
    require(role in ("init", "verify", "run", "cleanup", "stage-native", "stage-v1", "stage-v2", "stage-observer"))
    require(all(re.fullmatch(r"[1-9][0-9]{0,19}", x) is not None for x in identity[:2]) and re.fullmatch(r"[0-9a-f]{40}", identity[2]) is not None)
    require(len(sys.argv) == (9 if role in ("init", "verify") else 5))
    hashes = [digest(x) for x in sys.argv[5:]]
    os.umask(0o077)
    control, state = lifecycle(role, identity, hashes)
    parent = None
    root = None
    try:
        parent = parent_directory()
        if role == "init":
            initialize(parent, identity, state)
            root = descend(parent, PARTS[-1], 0o755)
            state["device"], state["inode"], state["phase"] = str(os.fstat(root).st_dev), str(os.fstat(root).st_ino), "active"
            write_lifecycle(control, state)
            return
        try:
            root = descend(parent, PARTS[-1], 0o755)
        except FileNotFoundError:
            require(role == "cleanup" and state["phase"] == "closed")
            os.fsync(control)
            return
        value = marker(root, identity)
        require(value["nonce"] == state["nonce"] and value["device"] == state["device"] and value["inode"] == state["inode"])
        if role == "cleanup":
            # This exclusive lease proves every controlled actor has released
            # its inherited description. Never signal a PID from a record.
            state["phase"] = "closed"
            write_lifecycle(control, state)
            cleanup(parent, root, value)
        elif role == "verify":
            require(hashes == state["hashes"] and copied_files(root) == hashes)
            value["phase"], value["nativeSha512"] = "verified", hashes[0]
            write_marker(root, value, False)
        elif role == "run":
            require(value["phase"] == "verified" and value["nativeSha512"] == state["hashes"][0] and copied_files(root) == state["hashes"])
            inherit_lease()
            os.execve("/usr/bin/unshare", ["unshare", "--mount", "--propagation", "private", "--", "/usr/lib/dilla-sandbox-maintenance-ci/.runner/native.test", "-test.run=^TestDisposableRootTransaction$", "-test.timeout=4m", "-test.v"], SAFE_ENV)
        else:
            stage_copy(root, value, state, role)
    finally:
        if root is not None:
            os.close(root)
        if parent is not None:
            os.close(parent)
        if control is not None:
            os.close(control)
        if LEASE is not None:
            os.close(LEASE)


try:
    run()
except Exception:
    sys.stderr.write("Dilla CI maintenance staging refused unsafe or busy lifecycle.\n")
    sys.exit(1)
