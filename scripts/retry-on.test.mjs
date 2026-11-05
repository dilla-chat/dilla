import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { parseArgs, retryOn } from './retry-on.mjs';

const patterns = [/proxy\.golang\.org/, /Error 68/];
const sequence = (...results) => {
  let calls = 0;
  return { run: () => Promise.resolve(results[calls++]), calls: () => calls };
};
const noSleep = () => Promise.resolve();

test('a matching failure is retried and its success returned', async () => {
  const s = sequence({ code: 1, output: 'dial proxy.golang.org: timeout' }, { code: 0, output: 'ok' });
  const result = await retryOn({ patterns, attempts: 3, waitMs: 0, run: s.run, sleep: noSleep });
  assert.deepEqual(result, { code: 0, attempts: 2 });
});

test('a non-matching failure is not retried', async () => {
  const s = sequence({ code: 7, output: 'compile error' }, { code: 0, output: 'ok' });
  const result = await retryOn({ patterns, attempts: 3, waitMs: 0, run: s.run, sleep: noSleep });
  assert.deepEqual(result, { code: 7, attempts: 1 });
  assert.equal(s.calls(), 1);
});

test('a matching failure stops after the attempt limit', async () => {
  const s = sequence({ code: 1, output: 'Error 68' }, { code: 2, output: 'Error 68' }, { code: 3, output: 'Error 68' }, { code: 0, output: 'ok' });
  const result = await retryOn({ patterns, attempts: 3, waitMs: 0, run: s.run, sleep: noSleep });
  assert.deepEqual(result, { code: 3, attempts: 3 });
});

test('success on the first try runs once', async () => {
  const s = sequence({ code: 0, output: 'proxy.golang.org mentioned in a passing run' });
  const result = await retryOn({ patterns, attempts: 3, waitMs: 0, run: s.run, sleep: noSleep });
  assert.deepEqual(result, { code: 0, attempts: 1 });
  assert.equal(s.calls(), 1);
});

test('each retry waits waitMs and says which attempt failed on which pattern', async () => {
  const waits = [];
  const lines = [];
  const s = sequence({ code: 1, output: 'Error 68' }, { code: 0, output: '' });
  await retryOn({ patterns, attempts: 3, waitMs: 20000, run: s.run, sleep: (ms) => { waits.push(ms); return Promise.resolve(); }, log: (l) => lines.push(l) });
  assert.deepEqual(waits, [20000]);
  assert.equal(lines.length, 1);
  assert.match(lines[0], /attempt 1 of 3.*Error 68/);
});

test('parseArgs reads patterns, attempts, wait and the command after --', () => {
  const parsed = parseArgs(['--pattern', 'a|b', '--pattern', 'c', '--attempts', '2', '--wait', '5', '--', 'go', 'mod', '--pattern', 'x']);
  assert.deepEqual(parsed.patterns.map(String), ['/a|b/', '/c/']);
  assert.equal(parsed.attempts, 2);
  assert.equal(parsed.waitMs, 5000);
  assert.deepEqual(parsed.command, ['go', 'mod', '--pattern', 'x']);
});

test('parseArgs rejects a call without a pattern or a command', () => {
  assert.throws(() => parseArgs(['--', 'go']), /--pattern/);
  assert.throws(() => parseArgs(['--pattern', 'x']), /command/);
});

const script = fileURLToPath(new URL('./retry-on.mjs', import.meta.url));
const flaky = (dir) => {
  const file = join(dir, 'flaky.mjs');
  // Fails once with a network-looking line, then succeeds; the marker file survives between processes.
  writeFileSync(file, `import { existsSync, writeFileSync } from 'node:fs';
const marker = process.argv[2];
if (!existsSync(marker)) { writeFileSync(marker, '1'); console.error('connection reset by peer'); process.exit(3); }
console.log('second try fine');`);
  return file;
};

test('the CLI retries a matching failure once, streams the output and exits 0', () => {
  const dir = mkdtempSync(join(tmpdir(), 'retry-on-'));
  const r = spawnSync(process.execPath, [script, '--pattern', 'connection reset', '--wait', '0', '--', process.execPath, flaky(dir), join(dir, 'marker')], { encoding: 'utf8' });
  assert.equal(r.status, 0);
  assert.match(r.stderr, /connection reset by peer/);
  assert.match(r.stdout, /second try fine/);
  assert.match(r.stderr, /retry-on: attempt 1 of 3 failed.*connection reset/);
  assert.equal(readFileSync(join(dir, 'marker'), 'utf8'), '1');
});

test('the CLI stops at once on a non-matching failure with the same exit code', () => {
  const r = spawnSync(process.execPath, [script, '--pattern', 'connection reset', '--wait', '0', '--', process.execPath, '-e', 'console.error("syntax problem"); process.exit(4)'], { encoding: 'utf8' });
  assert.equal(r.status, 4);
  assert.doesNotMatch(r.stderr, /retry-on/);
});
