# Fixed literal privileged bootstrap. No path/code is read from caller input.
import base64
import datetime
import errno
import fcntl
import hashlib
import json
import os
import re
import resource
import select
import stat
import struct
import sys
import time

ANCHOR = "/"
ROOT = "/usr/lib/dilla/appimage"
OWNER_UID = 0
OWNER_GID = 0
INPUT_TIMEOUT = 30.0
LOCK_TIMEOUT = 5.0
CONFIG = __DILLA_CONFIGURATION__
SAFE_ENV = {"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C", "LC_ALL": "C"}
MAX_INTEGER = 9007199254740991
MAX_RECORD = 131072


def require(ok):
    if not ok:
        raise ValueError("unsafe bootstrap state")


def no_writable_acl(fd):
    for name in ("security.capability", "system.posix_acl_default"):
        try:
            require(not os.getxattr(fd, name))
        except OSError as error:
            require(error.errno in (errno.ENODATA, errno.ENOTSUP))
    try:
        raw = os.getxattr(fd, "system.posix_acl_access")
    except OSError as error:
        require(error.errno in (errno.ENODATA, errno.ENOTSUP))
        return
    require(len(raw) >= 4 and (len(raw) - 4) % 8 == 0)
    require(struct.unpack_from("<I", raw)[0] == 2)
    for offset in range(4, len(raw), 8):
        tag, permissions, identity = struct.unpack_from("<HHI", raw, offset)
        require(tag in (1, 2, 4, 8, 16, 32) and permissions <= 7)
        if permissions & 2:
            require(tag in (1, 16) or tag == 2 and identity == OWNER_UID)


def safe_directory(fd, private=False):
    info = os.fstat(fd)
    require(stat.S_ISDIR(info.st_mode))
    require(info.st_uid in (0, OWNER_UID) and not info.st_mode & 0o6022)
    if private:
        require(info.st_uid == OWNER_UID and info.st_gid == OWNER_GID and stat.S_IMODE(info.st_mode) == 0o700)
    no_writable_acl(fd)
    return info


def descend(parent, name, create_mode=None):
    created = False
    try:
        fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
    except FileNotFoundError:
        require(create_mode is not None)
        os.mkdir(name, create_mode, dir_fd=parent)
        fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
        created = True
    if created:
        os.fchown(fd, OWNER_UID, OWNER_GID)
        os.fchmod(fd, create_mode)
        os.fsync(fd)
        os.fsync(parent)
    safe_directory(fd, create_mode == 0o700)
    if create_mode is not None:
        require(stat.S_IMODE(os.fstat(fd).st_mode) == create_mode)
    return fd


def root_directory():
    prefix = ANCHOR.rstrip("/") + "/"
    require(ROOT.startswith(prefix))
    parts = ROOT[len(prefix):].split("/")
    fd = os.open(ANCHOR, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    safe_directory(fd)
    for index, part in enumerate(parts):
        require(part not in ("", ".", ".."))
        child = descend(fd, part, 0o755 if index >= len(parts) - 2 else None)
        os.close(fd)
        fd = child
    require(os.fstat(fd).st_uid == OWNER_UID)
    return fd


def existing(parent, name):
    try:
        return os.stat(name, dir_fd=parent, follow_symlinks=False)
    except FileNotFoundError:
        return None


def safe_file(info, mode, limit):
    require(stat.S_ISREG(info.st_mode) and info.st_uid == OWNER_UID and info.st_gid == OWNER_GID)
    require(info.st_nlink == 1 and stat.S_IMODE(info.st_mode) == mode and 0 <= info.st_size <= limit)


def pin_file(parent, name, mode, limit):
    before = existing(parent, name)
    if before is None:
        return None
    safe_file(before, mode, limit)
    fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK, dir_fd=parent)
    try:
        after = os.fstat(fd)
        safe_file(after, mode, limit)
        require((before.st_dev, before.st_ino) == (after.st_dev, after.st_ino))
        no_writable_acl(fd)
        return fd
    except BaseException:
        os.close(fd)
        raise


def read_bytes(fd, limit):
    info = os.fstat(fd)
    require(info.st_size <= limit)
    data = bytearray()
    while len(data) <= limit:
        chunk = os.pread(fd, min(65536, limit + 1 - len(data)), len(data))
        if not chunk:
            break
        data.extend(chunk)
    after = os.fstat(fd)
    require(len(data) == info.st_size and len(data) <= limit)
    require((info.st_size, info.st_mtime_ns, info.st_ctime_ns) == (after.st_size, after.st_mtime_ns, after.st_ctime_ns))
    return bytes(data)


def file_digest(fd, limit):
    info = os.fstat(fd)
    require(info.st_size <= limit)
    digest = hashlib.sha512()
    offset = 0
    while offset <= info.st_size:
        chunk = os.pread(fd, min(65536, info.st_size + 1 - offset), offset)
        if not chunk:
            break
        digest.update(chunk)
        offset += len(chunk)
    after = os.fstat(fd)
    require(offset == info.st_size)
    require((info.st_size, info.st_mtime_ns, info.st_ctime_ns) == (after.st_size, after.st_mtime_ns, after.st_ctime_ns))
    return base64.b64encode(digest.digest()).decode("ascii")


def pairs(items):
    result = {}
    for key, value in items:
        require(key not in result)
        result[key] = value
    return result


def integer(text):
    require(re.fullmatch(r"0|[1-9][0-9]*", text) is not None)
    value = int(text)
    require(value <= MAX_INTEGER)
    return value


def reject_number(_):
    raise ValueError("invalid numeric encoding")


def decode_json(raw):
    return json.loads(raw.decode("utf-8"), object_pairs_hook=pairs, parse_int=integer, parse_float=reject_number, parse_constant=reject_number)


def exact(value, names):
    require(type(value) is dict and set(value) == set(names.split()))
    return value


def natural(value, positive=True):
    require(type(value) is int and (1 if positive else 0) <= value <= MAX_INTEGER)
    return value


def b64(value, maximum, length=None):
    require(type(value) is str and len(value) <= 4 * ((maximum + 2) // 3))
    data = base64.b64decode(value, validate=True)
    require(len(data) <= maximum and base64.b64encode(data).decode("ascii") == value)
    require(length is None or len(data) == length)
    return data


def digest(value):
    b64(value, 64, 64)
    return value


def identity(value, maximum, nullable=False):
    if value is None:
        require(nullable)
        return None
    exact(value, "size sha512 device inode")
    require(natural(value["size"]) <= maximum)
    digest(value["sha512"])
    for key in ("device", "inode"):
        text = value[key]
        require(type(text) is str and re.fullmatch(r"0|[1-9][0-9]{0,19}", text) is not None)
        require(int(text) <= 18446744073709551615 and (key != "inode" or int(text) > 0))
    return value


def manifest(raw):
    require(0 < len(raw) <= 65536)
    value = exact(decode_json(raw), "format kind generation platform arch compatibilityEpoch sandboxApi publishedAt expiresAt maintainer helper")
    require(value["format"] == 1 and type(value["format"]) is int and value["kind"] == "dilla-linux-sandbox")
    require(value["platform"] == "linux" and value["arch"] == "x64")
    require(value["compatibilityEpoch"] == 1 and type(value["compatibilityEpoch"]) is int and value["sandboxApi"] == 1 and type(value["sandboxApi"]) is int)
    natural(value["generation"])
    for key, maximum in (("maintainer", 33554432), ("helper", 8388608)):
        item = exact(value[key], "size sha512")
        require(natural(item["size"]) <= maximum)
        digest(item["sha512"])
    pub, exp = stamp(value["publishedAt"]), stamp(value["expiresAt"])
    require(pub < exp and exp - pub <= 93 * 86400 * 1000000000)
    return value


def stamp(value):
    require(type(value) is str)
    match = re.fullmatch(r"([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\.([0-9]{1,9}))?Z", value)
    require(match is not None)
    parsed = datetime.datetime.strptime(match[1], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=datetime.timezone.utc)
    seconds = (parsed - datetime.datetime(1970, 1, 1, tzinfo=datetime.timezone.utc)).days * 86400 + parsed.hour * 3600 + parsed.minute * 60 + parsed.second
    return seconds * 1000000000 + int((match[2] or "").ljust(9, "0"))


def admission(value, metadata):
    natural(value)
    require(value <= 253402300799999)
    at = value * 1000000
    require(stamp(metadata["publishedAt"]) <= at + 300000000000 and at < stamp(metadata["expiresAt"]))


def record(parent, name, limit):
    fd = pin_file(parent, name, 0o644, limit)
    if fd is None:
        return None
    try:
        return decode_json(read_bytes(fd, limit))
    finally:
        os.close(fd)


def receipt(value):
    exact(value, "format authorization initialAnchorSha512 manifest signature acceptedAt maximumGeneration maintainerSha512 helperSha512 compatibilityEpoch sandboxApi state completedOperation transactionSha512")
    require(type(value["format"]) is int and value["format"] == 1)
    raw = b64(value["manifest"], 65536)
    meta = manifest(raw)
    require(value["maximumGeneration"] == meta["generation"] and type(value["maximumGeneration"]) is int)
    require(value["compatibilityEpoch"] == 1 and type(value["compatibilityEpoch"]) is int and value["sandboxApi"] == 1 and type(value["sandboxApi"]) is int)
    require(value["maintainerSha512"] == meta["maintainer"]["sha512"] and value["helperSha512"] == meta["helper"]["sha512"])
    digest(value["initialAnchorSha512"])
    digest(value["transactionSha512"])
    require(value["state"] in ("active", "removed") and value["completedOperation"] == ("install" if value["state"] == "active" else "remove"))
    require(value["authorization"] in ("initial-digest", "maintenance-signature"))
    b64(value["signature"], 64, 0 if value["authorization"] == "initial-digest" else 64)
    admission(value["acceptedAt"], meta)
    return value


def intent(value):
    require(type(value) is dict and type(value.get("format")) is int and value["format"] == 1)
    if value.get("operation") == "remove":
        exact(value, "format operation maximumGeneration acceptedAt previousReceiptSha512 currentMaintainer currentHelper")
        natural(value["maximumGeneration"])
        natural(value["acceptedAt"])
        require(value["acceptedAt"] <= 253402300799999)
        digest(value["previousReceiptSha512"])
        identity(value["currentMaintainer"], 33554432)
        identity(value["currentHelper"], 8388608, True)
        return value
    exact(value, "format operation generation acceptedAt authorization manifest signature oldMaintainer newMaintainer oldHelper newHelper previousReceiptSha512")
    require(value["operation"] == "install" and value["authorization"] in ("initial-anchor", "maintenance-signature"))
    meta = manifest(b64(value["manifest"], 65536))
    require(type(value["generation"]) is int and value["generation"] == meta["generation"])
    admission(value["acceptedAt"], meta)
    b64(value["signature"], 64, 0 if value["authorization"] == "initial-anchor" else 64)
    for key, maximum in (("oldMaintainer", 33554432), ("newMaintainer", 33554432), ("oldHelper", 8388608), ("newHelper", 8388608)):
        identity(value[key], maximum, key.startswith("old"))
    require(value["oldMaintainer"] is not None or value["oldHelper"] is None)
    for key, target in (("newMaintainer", "maintainer"), ("newHelper", "helper")):
        require(value[key]["size"] == meta[target]["size"] and value[key]["sha512"] == meta[target]["sha512"])
    if value["previousReceiptSha512"] is not None:
        digest(value["previousReceiptSha512"])
    return value


def anchor(value):
    exact(value, "format kind operation generation acceptedAt manifestSha512 compatibilityEpoch sandboxApi capsule maintainer")
    require(type(value["format"]) is int and value["format"] == 1 and value["kind"] == "dilla-linux-sandbox-bootstrap" and value["operation"] == "initial")
    natural(value["generation"])
    natural(value["acceptedAt"])
    require(value["acceptedAt"] <= 253402300799999)
    require(type(value["compatibilityEpoch"]) is int and value["compatibilityEpoch"] == 1 and type(value["sandboxApi"]) is int and value["sandboxApi"] == 1)
    digest(value["manifestSha512"])
    identity(value["capsule"], 50331648)
    identity(value["maintainer"], 33554432)
    return value


def read_anchor(stage):
    fd = pin_file(stage, "bootstrap-anchor.json", 0o600, 4096)
    if fd is None:
        return None
    try:
        return anchor(decode_json(read_bytes(fd, 4096)))
    finally:
        os.close(fd)


def acquire_lock(root):
    before = existing(root, ".sandbox-lock")
    if before is not None:
        safe_file(before, 0o600, 0)
    fd = os.open(".sandbox-lock", os.O_RDWR | os.O_CREAT | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600, dir_fd=root)
    info = os.fstat(fd)
    safe_file(info, 0o600, 0)
    if before is not None:
        require((before.st_dev, before.st_ino) == (info.st_dev, info.st_ino))
    no_writable_acl(fd)
    deadline = time.monotonic() + LOCK_TIMEOUT
    while True:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return fd
        except BlockingIOError:
            require(time.monotonic() < deadline)
            time.sleep(0.025)


def match_file(fd, expected, maximum):
    info = os.fstat(fd)
    require(info.st_size == expected["size"] and str(info.st_dev) == expected["device"] and str(info.st_ino) == expected["inode"])
    require(file_digest(fd, maximum) == expected["sha512"])


def execute(program, lock, capsule=None):
    require(os.execve in os.supports_fd and os.pread(program, 4, 0) == b"\x7fELF")
    if program == 3:
        program = fcntl.fcntl(program, fcntl.F_DUPFD_CLOEXEC, 4)
    if capsule == 3:
        capsule = fcntl.fcntl(capsule, fcntl.F_DUPFD_CLOEXEC, 4)
    if capsule is not None:
        os.dup2(capsule, 0, inheritable=True)
        os.lseek(0, 0, os.SEEK_SET)
    else:
        os.set_inheritable(0, True)
    os.dup2(lock, 3, inheritable=True)
    require(fcntl.fcntl(3, fcntl.F_GETFL) & os.O_ACCMODE == os.O_RDWR)
    os.set_inheritable(program, False)
    limit = resource.getrlimit(resource.RLIMIT_NOFILE)[0]
    require(type(limit) is int and 4 <= limit <= 1048576)
    os.closerange(4, program)
    os.closerange(program + 1, limit)
    os.chdir("/")
    os.execve(program, ["sandbox-maintainer"], SAFE_ENV)


def write_all(fd, data):
    offset = 0
    while offset < len(data):
        count = os.write(fd, data[offset:])
        require(count > 0)
        offset += count


def fresh_date():
    now = time.time_ns() // 1000000
    admission(now, {"publishedAt": CONFIG["publishedAt"], "expiresAt": CONFIG["expiresAt"]})
    return now


def copy_input(fd):
    kind = os.fstat(0).st_mode
    require(stat.S_ISREG(kind) or stat.S_ISFIFO(kind))
    require(fcntl.fcntl(0, fcntl.F_GETFL) & os.O_ACCMODE != os.O_WRONLY)
    os.set_blocking(0, False)
    deadline = time.monotonic() + INPUT_TIMEOUT
    expected = CONFIG["capsuleSize"]
    offset = 0
    while True:
        remaining = deadline - time.monotonic()
        require(remaining > 0)
        ready, _, _ = select.select([0], [], [], remaining)
        require(ready)
        chunk = os.read(0, min(65536, expected + 1 - offset))
        if not chunk:
            require(offset == expected)
            break
        offset += len(chunk)
        require(offset <= expected)
        write_all(fd, chunk)
    os.fsync(fd)


def create_file(stage, name, mode):
    return os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, mode, dir_fd=stage)


def orphan_cleanup(root, stage):
    require(set(os.listdir(root)) <= {".sandbox-lock", ".sandbox-staging"})
    require(set(os.listdir(stage)) <= {"bootstrap-capsule", "bootstrap-maintainer", "bootstrap-anchor.next"})
    for name in os.listdir(stage):
        mode = 0o700 if name == "bootstrap-maintainer" else 0o600
        fd = pin_file(stage, name, mode, 50331648)
        require(fd is not None)
        os.close(fd)
        os.unlink(name, dir_fd=stage)
    os.fsync(stage)


def capsule_metadata(fd):
    header = os.pread(fd, 32, 0)
    require(len(header) == 32 and header[:8] == b"DLSBOX1\x00")
    metadata_size, signature_size, program_size, helper_size = struct.unpack_from("<IIQQ", header, 8)
    require(0 < metadata_size <= 65536 and signature_size == 0 and 0 < program_size <= 33554432 and 0 < helper_size <= 8388608)
    require(32 + metadata_size + program_size + helper_size == os.fstat(fd).st_size)
    raw = os.pread(fd, metadata_size, 32)
    require(len(raw) == metadata_size)
    value = manifest(raw)
    require(value["maintainer"]["size"] == program_size and value["helper"]["size"] == helper_size)
    return value, base64.b64encode(hashlib.sha512(raw).digest()).decode("ascii"), 32 + metadata_size


def new_anchor(root, stage):
    fresh_date()
    orphan_cleanup(root, stage)
    writable = create_file(stage, "bootstrap-capsule", 0o600)
    try:
        copy_input(writable)
    finally:
        os.close(writable)
    capsule = pin_file(stage, "bootstrap-capsule", 0o600, 50331648)
    require(capsule is not None and file_digest(capsule, 50331648) == CONFIG["capsuleSha512"])
    meta, metadata_hash, program_offset = capsule_metadata(capsule)
    require(metadata_hash == CONFIG["manifestSha512"] and meta["generation"] == CONFIG["generation"])
    require(meta["publishedAt"] == CONFIG["publishedAt"] and meta["expiresAt"] == CONFIG["expiresAt"])
    require(program_offset == CONFIG["maintainerOffset"] and meta["maintainer"]["size"] == CONFIG["maintainerSize"] and meta["maintainer"]["sha512"] == CONFIG["maintainerSha512"])
    writable = create_file(stage, "bootstrap-maintainer", 0o700)
    try:
        offset = CONFIG["maintainerOffset"]
        left = CONFIG["maintainerSize"]
        while left:
            chunk = os.pread(capsule, min(65536, left), offset)
            require(chunk)
            write_all(writable, chunk)
            offset += len(chunk)
            left -= len(chunk)
        os.fsync(writable)
    finally:
        os.close(writable)
    program = pin_file(stage, "bootstrap-maintainer", 0o700, 33554432)
    require(program is not None and file_digest(program, 33554432) == CONFIG["maintainerSha512"])
    os.fsync(stage)
    def observed(fd, sha):
        info = os.fstat(fd)
        return {"size": info.st_size, "sha512": sha, "device": str(info.st_dev), "inode": str(info.st_ino)}
    value = {"format": 1, "kind": "dilla-linux-sandbox-bootstrap", "operation": "initial", "generation": CONFIG["generation"], "acceptedAt": fresh_date(), "manifestSha512": CONFIG["manifestSha512"], "compatibilityEpoch": 1, "sandboxApi": 1, "capsule": observed(capsule, CONFIG["capsuleSha512"]), "maintainer": observed(program, CONFIG["maintainerSha512"])}
    raw = json.dumps(value, separators=(",", ":"), sort_keys=True).encode("ascii")
    require(len(raw) <= 4096)
    writable = create_file(stage, "bootstrap-anchor.next", 0o600)
    try:
        write_all(writable, raw)
        os.fsync(writable)
    finally:
        os.close(writable)
    require(existing(stage, "bootstrap-anchor.json") is None)
    os.rename("bootstrap-anchor.next", "bootstrap-anchor.json", src_dir_fd=stage, dst_dir_fd=stage)
    os.fsync(stage)
    os.fsync(root)
    return program, capsule


def run():
    require(os.geteuid() == OWNER_UID and len(sys.argv) == 1)
    os.umask(0o077)
    root = root_directory()
    lock = acquire_lock(root)
    previous = record(root, "sandbox-receipt.json", MAX_RECORD)
    if previous is not None:
        previous = receipt(previous)
    pending = record(root, "sandbox-transaction.json", MAX_RECORD)
    if pending is not None:
        pending = intent(pending)
    stage = descend(root, ".sandbox-staging", 0o700)
    require(os.fstat(root).st_dev == os.fstat(stage).st_dev)
    initial = read_anchor(stage)
    resident = pin_file(root, "sandbox-maintainer", 0o755, 33554432)
    if resident is not None:
        # Select only previously ROOT-admitted code. The native maintainer
        # must recheck crypto, floor, current helper and terminal-operation
        # bindings before it performs any privileged mutation.
        expected = set()
        if previous is not None:
            expected.add(previous["maintainerSha512"])
        if pending is not None:
            names = ("oldMaintainer", "newMaintainer") if pending["operation"] == "install" else ("currentMaintainer",)
            for name in names:
                if pending[name] is not None:
                    expected.add(pending[name]["sha512"])
        if initial is not None and previous is None:
            expected.add(initial["maintainer"]["sha512"])
        require(file_digest(resident, 33554432) in expected)
        # A committed receipt can dispatch trusted resident cleanup without
        # reopening bootstrap stages that cleanup has already deleted.
        execute(resident, lock)
    if initial is not None:
        require(previous is None)
        if pending is not None:
            require(pending["operation"] == "install" and pending["authorization"] == "initial-anchor" and pending["generation"] == initial["generation"] and pending["previousReceiptSha512"] is None)
        program = pin_file(stage, "bootstrap-maintainer", 0o700, 33554432)
        capsule = pin_file(stage, "bootstrap-capsule", 0o600, 50331648)
        require(program is not None and capsule is not None)
        match_file(program, initial["maintainer"], 33554432)
        match_file(capsule, initial["capsule"], 50331648)
        meta, metadata_hash, _ = capsule_metadata(capsule)
        require(metadata_hash == initial["manifestSha512"] and meta["generation"] == initial["generation"])
        require(meta["maintainer"]["size"] == initial["maintainer"]["size"] and meta["maintainer"]["sha512"] == initial["maintainer"]["sha512"])
        admission(initial["acceptedAt"], meta)
        require(initial["acceptedAt"] <= time.time_ns() // 1000000 + 300000)
        execute(program, lock, capsule)
    require(previous is None and pending is None)
    program, capsule = new_anchor(root, stage)
    execute(program, lock, capsule)


try:
    run()
except Exception:
    sys.stderr.write("Dilla sandbox bootstrap refused unsafe or incomplete setup.\n")
    sys.exit(1)
