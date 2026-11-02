# CI ONLY. Pass this fixed source as argv to OS Python -I -S, never a file path.
import base64
import errno
import hashlib
import json
import os
import re
import stat
import sys

ANCHOR = "/"
PARTS = ("usr", "lib", "dilla-sandbox-maintenance-ci")
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


def initialize(parent, identity):
    # mkdir must fail on any preexisting entry. Never adopt it, even if empty.
    os.mkdir(PARTS[-1], 0o755, dir_fd=parent)
    root = None
    try:
        root = os.open(PARTS[-1], os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
        safe_dir(root)
        require(os.fstat(root).st_dev == os.fstat(parent).st_dev and mount_id(root) == mount_id(parent))
        os.fchmod(root, 0o755)
        s = os.fstat(root)
        value = dict(format=1, runId=identity[0], attempt=identity[1], sha=identity[2], nonce=os.urandom(32).hex(), device=str(s.st_dev), inode=str(s.st_ino), phase="initialized", nativeSha512="")
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
        os.execve(p[0], ["native.test", "-test.run=^TestDisposableRootCleanup$", "-test.timeout=25s", "-test.v"], SAFE_ENV)
    finally:
        if p is not None:
            os.close(p[0])
        os.close(runner)


def run():
    require(os.geteuid() == OWNER_UID and len(sys.argv) >= 5)
    role, identity = sys.argv[1], sys.argv[2:5]
    require(role in ("init", "verify", "cleanup"))
    require(all(re.fullmatch(r"[1-9][0-9]{0,19}", x) is not None for x in identity[:2]) and re.fullmatch(r"[0-9a-f]{40}", identity[2]) is not None)
    require(len(sys.argv) == (9 if role == "verify" else 5))
    os.umask(0o077)
    parent = parent_directory()
    root = None
    try:
        if role == "init":
            initialize(parent, identity)
            return
        try:
            root = descend(parent, PARTS[-1], 0o755)
        except FileNotFoundError:
            require(role == "cleanup")
            return
        value = marker(root, identity)
        if role == "verify":
            expected = [digest(x) for x in sys.argv[5:]]
            require(copied_files(root) == expected)
            value["phase"], value["nativeSha512"] = "verified", expected[0]
            write_marker(root, value, False)
        else:
            cleanup(parent, root, value)
    finally:
        if root is not None:
            os.close(root)
        os.close(parent)


try:
    run()
except Exception:
    sys.stderr.write("Dilla CI maintenance staging refused unsafe state.\n")
    sys.exit(1)
