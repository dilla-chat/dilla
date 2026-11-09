import { spawnSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import {
  copyFileSync, cpSync, existsSync, linkSync, lstatSync, mkdirSync, readFileSync, readdirSync, readlinkSync, renameSync, rmSync, symlinkSync,
  unlinkSync, writeFileSync,
} from 'node:fs';
import { createServer } from 'node:net';
import { createSocket } from 'node:dgram';
import { tmpdir } from 'node:os';
import { basename, dirname, join } from 'node:path';

/**
 * The side-effecting helpers of scripts/ci-local.mjs, each taking its directories as arguments so the tests drive them
 * in a temporary directory: the job environment, the per-user lane locks, the go-release snapshot, the clean tree and the
 * port probe.
 */

// ---------------------------------------------------------------- the job environment

/**
 * Variables a caller's shell may carry that change what a test touches or where a build goes. CI sets none of them
 * unless the workflow says so, so every job starts without them and gets only what the workflow sets:
 * - DILLA_*: the test switches (DILLA_TEST_PG points the store and API tests at a server they CREATE/DROP databases on,
 *   DILLA_TESTKIT, DILLA_METRICS_TOKEN, the e2e port pins, …);
 * - PG* and DATABASE_URL: libpq and driver defaults;
 * - GO, NPM, CARGO, WASM_PACK, CARGO_TARGET_DIR: the e2e helpers' tool overrides and cargo's output directory.
 */
export const SCRUBBED_ENV = /^(DILLA_[A-Z0-9_]*|PG[A-Z0-9_]*|DATABASE_URL|GO|NPM|CARGO|WASM_PACK|CARGO_TARGET_DIR)$/;

/** The caller's environment without SCRUBBED_ENV. */
export function scrubEnv(env) {
  const out = {};
  for (const [k, v] of Object.entries(env)) if (!SCRUBBED_ENV.test(k)) out[k] = v;
  return out;
}

// ---------------------------------------------------------------- per-user lane locks

/** The start time of a process (clock ticks since boot, /proc/<pid>/stat field 22), or null when it is gone. */
export function processStart(pid) {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, 'utf8');
    return stat.slice(stat.lastIndexOf(')') + 2).split(' ')[19] ?? null;
  } catch {
    return null;
  }
}

/** Whether the owner of a lock still runs: its pid exists and, where /proc says, started when the lock says. */
export function ownerAlive(owner) {
  if (!owner || !Number.isInteger(owner.pid)) return false;
  try {
    process.kill(owner.pid, 0);
  } catch (e) {
    if (e.code !== 'EPERM') return false;
  }
  if (owner.start === null || owner.start === undefined || !existsSync('/proc/self/stat')) return true;
  return processStart(owner.pid) === owner.start;
}

function readOwner(path) {
  try {
    return JSON.parse(readFileSync(path, 'utf8'));
  } catch {
    return null;
  }
}

/**
 * This user's lock directory: `$XDG_RUNTIME_DIR/dilla-ci-local-locks` when the session has one, else
 * `<tmpdir>/dilla-ci-local-locks-<uid>`. The locks only coordinate this user's lanes; other users' listeners are what the
 * port probe catches.
 */
export function lockDirPath({ env = process.env, tmp = tmpdir(), uid = process.getuid() } = {}) {
  return env.XDG_RUNTIME_DIR ? join(env.XDG_RUNTIME_DIR, 'dilla-ci-local-locks') : join(tmp, `dilla-ci-local-locks-${uid}`);
}

/**
 * The lock directory, created 0700 when missing (never chmodded). Before use it must be a real directory, not a
 * symlink, owned by this uid and writable by no group or other user; anything else fails closed, because whoever
 * controls the directory controls the locks.
 */
export function lockDir(dir, { uid = process.getuid(), stat = lstatSync } = {}) {
  try {
    mkdirSync(dir, { mode: 0o700 });
  } catch (e) {
    if (e.code !== 'EEXIST') throw e;
  }
  const st = stat(dir);
  const why = st.isSymbolicLink() ? 'is a symlink'
    : !st.isDirectory() ? 'is not a directory'
      : st.uid !== uid ? `is owned by uid ${st.uid}, not ${uid}`
        : (st.mode & 0o022) !== 0 ? `is writable by group or others (mode ${(st.mode & 0o777).toString(8)})`
          : null;
  if (why !== null) throw new Error(`ci-local: refusing the lock directory ${dir}: it ${why}. Remove it (or fix its owner and mode to 0700) and run again.`);
  return dir;
}

/**
 * Takes the lock file `<dir>/<name>.lock` for this process, or returns null while another live process holds it.
 * The claim is atomic (a fully written owner file hard-linked into place, which fails if the name exists). A holder
 * that is gone (pid dead, or reused by a process that started later) is taken over by renaming its file to a name of
 * our own (one renamer wins) and checking that what was renamed is the stale owner we judged; a live lock renamed by
 * mistake is linked back. A lock that cannot be read or moved (another user's) counts as held.
 */
export function tryLock(dir, name, info = {}, self = { pid: process.pid, start: processStart(process.pid) }) {
  const path = join(dir, `${name}.lock`);
  const token = randomBytes(8).toString('hex');
  const tmp = `${path}.${token}.tmp`;
  writeFileSync(tmp, JSON.stringify({ ...info, pid: self.pid, start: self.start, token }), { mode: 0o644 });
  try {
    for (let attempt = 0; attempt < 5; attempt++) {
      try {
        linkSync(tmp, path);
        return { path, token };
      } catch (e) {
        if (e.code !== 'EEXIST') throw e;
      }
      const owner = readOwner(path);
      if (owner === null) {
        // Unreadable: being replaced right now, or not ours to read. Held either way.
        if (existsSync(path)) return null;
        continue;
      }
      if (ownerAlive(owner)) return null;
      const claim = `${path}.${token}.stale`;
      try {
        renameSync(path, claim);
      } catch (e) {
        if (e.code === 'ENOENT') continue;
        if (e.code === 'EPERM' || e.code === 'EACCES') return null;
        throw e;
      }
      const moved = readOwner(claim);
      if (moved?.token !== owner.token) {
        // A live lock was created between the read and the rename: put it back, unless a third run holds the name now.
        try { linkSync(claim, path); } catch { /* the name is held again */ }
        unlinkSync(claim);
        return null;
      }
      unlinkSync(claim);
    }
    return null;
  } finally {
    rmSync(tmp, { force: true });
  }
}

/** Releases a lock taken by tryLock, only while it is still ours. */
export function releaseLock(lock) {
  if (lock === null) return;
  if (readOwner(lock.path)?.token === lock.token) rmSync(lock.path, { force: true });
}

// ---------------------------------------------------------------- the go-release snapshot

const marker = (snapDir, p) => join(snapDir, `${p.replaceAll('/', '__')}.pending.json`);
const copyOf = (snapDir, p) => join(snapDir, p.replaceAll('/', '__'));

/**
 * Copies `<root>/<p>` aside before a job that rewrites it. The pending marker is written only once the copy is complete,
 * so a marker always names a whole snapshot; a marker left by a run that died is restored first (recoverSnapshots).
 */
export function takeSnapshot(root, snapDir, p) {
  recoverSnapshots(root, snapDir);
  mkdirSync(snapDir, { recursive: true });
  const from = join(root, p);
  const to = copyOf(snapDir, p);
  rmSync(to, { recursive: true, force: true });
  const existed = existsSync(from);
  if (existed) cpSync(from, to, { recursive: true, verbatimSymlinks: true });
  const m = marker(snapDir, p);
  writeFileSync(`${m}.tmp`, JSON.stringify({ path: p, existed }));
  renameSync(`${m}.tmp`, m);
}

/**
 * Puts `<root>/<p>` back as the snapshot holds it: copied beside it, then swapped in by rename, so a failed copy leaves
 * the tree as it was and the marker in place for the next run.
 */
export function restoreSnapshot(root, snapDir, p) {
  const m = marker(snapDir, p);
  if (!existsSync(m)) return false;
  const { existed } = JSON.parse(readFileSync(m, 'utf8'));
  const target = join(root, p);
  const tag = `${process.pid}-${randomBytes(4).toString('hex')}`;
  const incoming = join(dirname(target), `.${basename(target)}.ci-local-restore-${tag}`);
  const outgoing = join(dirname(target), `.${basename(target)}.ci-local-old-${tag}`);
  if (existed) cpSync(copyOf(snapDir, p), incoming, { recursive: true, verbatimSymlinks: true });
  if (existsSync(target)) renameSync(target, outgoing);
  if (existed) renameSync(incoming, target);
  rmSync(outgoing, { recursive: true, force: true });
  rmSync(m, { force: true });
  rmSync(copyOf(snapDir, p), { recursive: true, force: true });
  return true;
}

/** Restores every snapshot a run left pending (it died during go-release) and removes half-done restore copies. */
export function recoverSnapshots(root, snapDir) {
  const restored = [];
  if (!existsSync(snapDir)) return restored;
  for (const f of readdirSync(snapDir)) {
    if (!f.endsWith('.pending.json')) continue;
    const { path } = JSON.parse(readFileSync(join(snapDir, f), 'utf8'));
    for (const sib of existsSync(dirname(join(root, path))) ? readdirSync(dirname(join(root, path))) : []) {
      // Half-done restore copies: the snapshot, which the marker vouches for, is restored whole below instead.
      if (sib.startsWith(`.${basename(path)}.ci-local-restore-`) || sib.startsWith(`.${basename(path)}.ci-local-old-`)) {
        rmSync(join(dirname(join(root, path)), sib), { recursive: true, force: true });
      }
    }
    if (restoreSnapshot(root, snapDir, path)) restored.push(path);
  }
  return restored;
}

// ---------------------------------------------------------------- the clean tree

/**
 * A copy of `files` (paths relative to `root`) in `dir`, a git index over them, and `root`'s node_modules linked in:
 * what a runner's checkout plus npm ci holds, without local build output.
 */
export function buildCleanTree(root, dir, files) {
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  for (const f of files) {
    const from = join(root, f);
    let st;
    try { st = lstatSync(from); } catch { continue; }
    mkdirSync(dirname(join(dir, f)), { recursive: true });
    if (st.isSymbolicLink()) symlinkSync(readlinkSync(from), join(dir, f));
    else if (st.isFile()) copyFileSync(from, join(dir, f));
  }
  if (existsSync(join(root, 'node_modules'))) symlinkSync(join(root, 'node_modules'), join(dir, 'node_modules'));
  const git = (a) => {
    const r = spawnSync('git', a, { cwd: dir, encoding: 'utf8' });
    if (r.status !== 0) throw new Error(`git ${a.join(' ')} in the clean tree failed: ${r.stderr}`);
  };
  git(['init', '-q']);
  // The node_modules link is a file to git (`/node_modules/` ignores only a directory): keep it out of the index.
  writeFileSync(join(dir, '.git', 'info', 'exclude'), '/node_modules\n', { flag: 'a' });
  git(['add', '-A']);
  // HEAD must name this copy's own files, not the source repository's commit: the checkout a runner builds is one commit
  // of exactly these files, and the tests that read `git rev-parse HEAD` compare against it.
  git(['-c', 'user.name=ci-local', '-c', 'user.email=ci-local@localhost', '-c', 'commit.gpgsign=false', 'commit', '-q', '--allow-empty', '--no-verify', '-m', 'ci-local clean tree']);
  return dir;
}

// ---------------------------------------------------------------- ports

function probe(kind, port) {
  return new Promise((done) => {
    if (kind === 'tcp') {
      const srv = createServer();
      srv.once('error', () => done(false));
      srv.listen({ port, host: '127.0.0.1', exclusive: true }, () => srv.close(() => done(true)));
    } else {
      const sock = createSocket('udp4');
      sock.once('error', () => { sock.close(); done(false); });
      sock.bind({ port, address: '127.0.0.1', exclusive: true }, () => sock.close(() => done(true)));
    }
  });
}

/** The ports of [from, to] (TCP, and UDP where `udp`) that something on this machine already binds. */
export async function busyPorts(from, to, { udp = false } = {}) {
  const busy = [];
  for (let port = from; port <= to; port += 64) {
    const batch = [];
    for (let p = port; p <= Math.min(to, port + 63); p++) {
      batch.push(probe('tcp', p).then((ok) => (ok ? null : `${p}/tcp`)));
      if (udp) batch.push(probe('udp', p).then((ok) => (ok ? null : `${p}/udp`)));
    }
    busy.push(...(await Promise.all(batch)).filter(Boolean));
  }
  return busy;
}
