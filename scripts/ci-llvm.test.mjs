import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const script = join(dirname(fileURLToPath(import.meta.url)), 'ci-llvm.sh');
const PACKAGES = ['clang-23', 'llvm-23', 'libllvm23', 'libclang-cpp23', 'libclang1-23', 'libclang-common-23-dev', 'llvm-23-linker-tools', 'llvm-23-runtime'];
const FPR = '6084F3CF814B57C1CF12EFD515CF4D18AF4F7421';
const ARCHIVE_BUILD = '1:23.1.0~++20261103081500+0123456789ab-1~exp1~20261103201612.42';
const CACHED_BUILD = '1:23.1.0~++20261020081500+fedcba987654-1~exp1~20261020201612.17';
const debName = (p, v) => `${p}_${v.replace(':', '%3a')}_amd64.deb`;

/** A runner with fake tools on PATH: the fakes record every apt-get call and act out one archive state. */
function runner({ codename = 'noble', archive = 'up', candidate = ARCHIVE_BUILD, cached = null, fingerprint = FPR } = {}) {
  const root = mkdtempSync(join(tmpdir(), 'dilla-llvm-test-'));
  const dirs = { bin: 'bin', llvm: 'llvm-bin', cache: 'cache', archives: 'archives', keyrings: 'keyrings', sources: 'sources', conf: 'apt.conf.d', temp: 'temp', state: 'state' };
  for (const d of Object.values(dirs)) mkdirSync(join(root, d), { recursive: true });
  const fake = (name, body) => { const f = join(root, 'bin', name); writeFileSync(f, `#!/usr/bin/env bash\n${body}\n`); chmodSync(f, 0o755); };
  const state = join(root, 'state');
  fake('sudo', 'exec "$@"');
  fake('lsb_release', `echo ${codename}`);
  fake('curl', archive === 'unreachable'
    ? 'echo "curl: (7) Failed to connect" >&2; exit 7'
    : 'while [ $# -gt 0 ]; do if [ "$1" = -o ]; then echo "ARMORED KEY" > "$2"; fi; shift; done');
  fake('gpg', `case "$1" in
  --dearmor) echo "KEYRING BYTES" > "$3" ;;
  --show-keys) printf 'pub:-:255:22:15CF4D18AF4F7421:::::::::\\nfpr:::::::::${fingerprint}:\\n' ;;
  *) exit 1 ;;
esac`);
  fake('apt-get', `echo "$*" >> "${state}/apt-get.log"
case "$1" in
  update) ${archive === 'index-down' ? 'echo "E: The repository https://apt.llvm.org/noble llvm-toolchain-noble-23 Release no longer has a Release file." >&2; exit 100' : 'exit 0'} ;;
  install)
    case "$*" in
      *.deb*) v=$(basename "$(printf '%s\\n' "$@" | grep -m1 '/clang-23_')" | sed -e 's/^clang-23_//' -e 's/_amd64\\.deb$//' -e 's/%3a/:/'); echo "$v" > "${state}/installed" ;;
      *) v=$(printf '%s\\n' "$@" | sed -n 's/^clang-23=//p'); echo "$v" > "${state}/installed"
         for p in ${PACKAGES.join(' ')}; do : > "${root}/archives/\${p}_$(printf '%s' "$v" | sed 's/:/%3a/')_amd64.deb"; done ;;
    esac ;;
esac`);
  fake('apt-cache', `case "$*" in "policy clang-23") printf 'clang-23:\\n  Installed: (none)\\n  Candidate: ${candidate}\\n' ;; *) echo "policy: $*" ;; esac`);
  fake('dpkg-query', `cat "${state}/installed"`);
  for (const tool of ['clang-23', 'llvm-ar-23']) {
    const f = join(root, 'llvm-bin', tool);
    writeFileSync(f, `#!/usr/bin/env bash\nv=$(cat "${state}/installed"); echo "clang version $(printf '%s' "$v" | sed -e 's/^1://' -e 's/~.*//')"\n`); chmodSync(f, 0o755);
  }
  if (cached) for (const p of PACKAGES) writeFileSync(join(root, 'cache', debName(p, cached)), `cached ${p}`);
  const output = join(root, 'state', 'output');
  const run = () => {
    const result = spawnSync('bash', [script, 'install'], {
      encoding: 'utf8',
      env: {
        PATH: `${join(root, 'bin')}:${process.env.PATH}`, HOME: root, RUNNER_TEMP: join(root, 'temp'), GITHUB_OUTPUT: output,
        LLVM_DEB_CACHE_DIR: join(root, 'cache'), LLVM_APT_ARCHIVES: join(root, 'archives'), LLVM_KEYRING: join(root, 'keyrings', 'apt-llvm.gpg'),
        LLVM_SOURCE_LIST: join(root, 'sources', 'apt-llvm-23.list'), LLVM_APT_CONF: join(root, 'apt.conf.d'), LLVM_BIN: join(root, 'llvm-bin'),
      },
    });
    const outputs = Object.fromEntries((existsSync(output) ? readFileSync(output, 'utf8') : '').trim().split('\n').filter(Boolean).map((l) => l.split('=')));
    const aptGet = existsSync(join(state, 'apt-get.log')) ? readFileSync(join(state, 'apt-get.log'), 'utf8').trim().split('\n') : [];
    return { ...result, outputs, aptGet, cache: readdirSync(join(root, 'cache')).sort(), sourceListed: existsSync(join(root, 'sources', 'apt-llvm-23.list')), installed: existsSync(join(state, 'installed')) ? readFileSync(join(state, 'installed'), 'utf8').trim() : null };
  };
  return { run, dispose: () => rmSync(root, { recursive: true, force: true }) };
}

test('a reachable archive installs its pinned 23.1 build and caches exactly its eight debs', () => {
  const r = runner();
  try {
    const result = r.run();
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.installed, ARCHIVE_BUILD);
    assert.deepEqual(result.outputs, { source: 'archive', build: ARCHIVE_BUILD.replace(/[^A-Za-z0-9._-]/g, '-'), changed: 'true' });
    assert.deepEqual(result.cache, PACKAGES.map((p) => debName(p, ARCHIVE_BUILD)).sort());
    const install = result.aptGet.find((l) => l.startsWith('install '));
    for (const p of PACKAGES) assert.ok(install.includes(` ${p}=${ARCHIVE_BUILD}`), `${p} pinned in: ${install}`);
    assert.equal(result.sourceListed, false, 'the archive stays out of later apt-get update calls');
    assert.match(result.stdout, /installed from the archive/);
  } finally { r.dispose(); }
});

test('a cache that already holds the archive build is reported unchanged, and a stale one is replaced', () => {
  const same = runner({ cached: ARCHIVE_BUILD });
  try { const result = same.run(); assert.equal(result.status, 0, result.stderr); assert.equal(result.outputs.changed, 'false'); } finally { same.dispose(); }
  const stale = runner({ cached: CACHED_BUILD });
  try {
    const result = stale.run();
    assert.equal(result.status, 0, result.stderr); assert.equal(result.outputs.changed, 'true');
    assert.deepEqual(result.cache, PACKAGES.map((p) => debName(p, ARCHIVE_BUILD)).sort(), 'the old build leaves the cache');
  } finally { stale.dispose(); }
});

for (const [what, archive] of [['answers nothing', 'unreachable'], ['serves no package index', 'index-down']]) {
  test(`an archive that ${what} installs the cached build from its debs`, () => {
    const r = runner({ archive, cached: CACHED_BUILD });
    try {
      const result = r.run();
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.installed, CACHED_BUILD);
      assert.deepEqual(result.outputs, { source: 'cache', build: CACHED_BUILD.replace(/[^A-Za-z0-9._-]/g, '-'), changed: 'false' });
      const install = result.aptGet.find((l) => l.startsWith('install '));
      for (const p of PACKAGES) assert.ok(install.includes(`/${debName(p, CACHED_BUILD)}`), `${p} installed from its cached deb: ${install}`);
      assert.equal(result.sourceListed, false);
      assert.match(result.stderr, /apt.llvm.org is unreachable/);
    } finally { r.dispose(); }
  });
}

test('an unreachable archive with no cached build fails with the reason', () => {
  const r = runner({ archive: 'unreachable' });
  try {
    const result = r.run();
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /no cached \.deb of clang-23 .*no earlier run cached its build/);
    assert.equal(result.installed, null);
  } finally { r.dispose(); }
});

test('a reachable archive whose candidate left the 23.1 line fails closed even with a cached build', () => {
  const r = runner({ candidate: '1:23.2.0~++20261201081500+0123456789ab-1~exp1~20261201201612.1', cached: CACHED_BUILD });
  try {
    const result = r.run();
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /expected clang 23\.1\.x candidate, got 1:23\.2/);
    assert.equal(result.installed, null, 'nothing is installed, from the archive or the cache');
  } finally { r.dispose(); }
});

test('an archive key with another fingerprint is refused before apt sees the source', () => {
  const r = runner({ fingerprint: '0'.repeat(40), cached: CACHED_BUILD });
  try {
    const result = r.run();
    assert.notEqual(result.status, 0);
    assert.equal(result.aptGet.length, 0);
    assert.equal(result.installed, null);
  } finally { r.dispose(); }
});

test('another Ubuntu release is refused', () => {
  const r = runner({ codename: 'jammy' });
  try { const result = r.run(); assert.notEqual(result.status, 0); assert.equal(result.aptGet.length, 0); } finally { r.dispose(); }
});

test('the usage line answers anything but install', () => {
  const result = spawnSync('bash', [script], { encoding: 'utf8' });
  assert.equal(result.status, 2); assert.match(result.stderr, /usage/);
});
