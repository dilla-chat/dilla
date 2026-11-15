import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import test, { after } from 'node:test';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const script = join(root, 'scripts', 'fetch-libwebrtc.sh');
const source = readFileSync(script, 'utf8');
const dir = mkdtempSync(join(tmpdir(), 'dilla-libwebrtc-'));
after(() => rmSync(dir, { recursive: true, force: true }));

const run = (...args) => spawnSync('bash', [script, ...args], { encoding: 'utf8' });

test('a target with no pin is refused before any download', () => {
  const result = run('riscv64gc-unknown-linux-gnu', dir);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /no pinned libwebrtc/);
});

test('a changed archive is refused before it is unpacked', () => {
  const archive = join(dir, 'wrong.zip');
  writeFileSync(archive, 'not the reviewed archive');
  for (const target of ['x86_64-unknown-linux-gnu', 'aarch64-apple-darwin', 'x86_64-pc-windows-msvc']) {
    const result = run(target, join(dir, 'out'), '--archive', archive);
    assert.equal(result.status, 1, target);
    assert.match(result.stderr, /SHA-256 mismatch/, target);
  }
});

test('bad usage is refused', () => {
  assert.equal(run().status, 1);
  assert.equal(run('x86_64-unknown-linux-gnu', dir, '--other', 'x').status, 1);
});

test('each of the three release targets has its own 64-hex pin', () => {
  const pins = [...source.matchAll(/^\s+sha256=([0-9a-f]+)$/gm)].map((m) => m[1]);
  assert.equal(pins.length, 3);
  assert.equal(new Set(pins).size, 3);
  for (const pin of pins) assert.match(pin, /^[0-9a-f]{64}$/);
});

// The pins name the archive webrtc-sys-build fetches. A Cargo.lock bump of it may change WEBRTC_TAG (and so the
// archive): this fails until the script's tag and digests are re-reviewed.
test('the pinned webrtc-sys-build version is the one Cargo.lock resolves', () => {
  const lock = readFileSync(join(root, 'Cargo.lock'), 'utf8');
  const locked = /\[\[package\]\]\nname = "webrtc-sys-build"\nversion = "([^"]+)"/.exec(lock)?.[1];
  const pinned = /^WEBRTC_SYS_BUILD_VERSION=(\S+)$/m.exec(source)?.[1];
  assert.ok(locked, 'webrtc-sys-build is in Cargo.lock');
  assert.equal(pinned, locked);
  assert.match(source, /^WEBRTC_TAG=webrtc-a545de4$/m);
});
