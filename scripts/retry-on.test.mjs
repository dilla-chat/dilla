import assert from 'node:assert/strict';
import { mkdtempSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
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

// The pattern sets the workflows really pass, read from every workflow file: each wrapped command must carry one set
// everywhere, so these tests hold whatever ships.
const WORKFLOWS_DIR = fileURLToPath(new URL('../.github/workflows/', import.meta.url));
function shippedPatterns(command) {
  const sets = new Map();
  for (const file of readdirSync(WORKFLOWS_DIR).filter((name) => name.endsWith('.yml'))) {
    for (const line of readFileSync(join(WORKFLOWS_DIR, file), 'utf8').split('\n')) {
      const m = /node scripts\/retry-on\.mjs (.*?) -- (.*)$/.exec(line.trim());
      if (m === null || !m[2].startsWith(command)) continue;
      const flags = [...m[1].matchAll(/--pattern '([^']*)'/g)].flatMap((p) => ['--pattern', p[1]]);
      const key = JSON.stringify(flags);
      sets.set(key, [...(sets.get(key) ?? []), file]);
    }
  }
  assert.equal(sets.size, 1, `${command}: one pattern set everywhere, got ${[...sets.keys()].join(' / ')}`);
  const [[flags, files]] = sets;
  return { patterns: parseArgs([...JSON.parse(flags), '--', 'x']).patterns, files: [...new Set(files)].sort() };
}
const GO = shippedPatterns('go mod download');
const STAPLE = shippedPatterns('npm run dist');
const ELECTRON = shippedPatterns('node node_modules/electron/install.js');
const GRADLE = shippedPatterns('packages/mobile/android/gradlew');
const ANDROID_SDK = shippedPatterns('bash scripts/ci-android.sh sdk');
const SPM = shippedPatterns('xcodebuild -resolvePackageDependencies');
const retried = async (patterns, output) => (await retryOn({ patterns, attempts: 3, waitMs: 0, run: sequence({ code: 1, output }, { code: 0, output: '' }).run, sleep: noSleep })).attempts > 1;

test('the shipped pattern sets come from the workflows that run the wrapped commands', () => {
  assert.deepEqual(GO.files, ['desktop-ci.yml', 'desktop.yml', 'mobile.yml', 'release.yml', 'server.yml']);
  assert.deepEqual(STAPLE.files, ['desktop.yml']);
  assert.deepEqual(ELECTRON.files, ['desktop-ci.yml', 'desktop.yml']);
  for (const set of [GRADLE, ANDROID_SDK, SPM]) assert.deepEqual(set.files, ['mobile.yml']);
});

test('the Go download retries a proxy 5xx, a rate limit and transport errors', async () => {
  for (const line of [
    'go: example.com/m@v1.2.3: reading https://proxy.golang.org/example.com/m/@v/v1.2.3.info: 502 Bad Gateway',
    'go: example.com/m@v1.2.3: reading https://proxy.golang.org/example.com/m/@v/v1.2.3.zip: 503 Service Unavailable',
    'go: example.com/m@v1.2.3: reading https://proxy.golang.org/example.com/m/@v/v1.2.3.mod: 429 Too Many Requests',
    'go: example.com/m@v1.2.3: Get "https://proxy.golang.org/example.com/m/@v/v1.2.3.zip": stream error: stream ID 5; INTERNAL_ERROR; received from peer',
    'go: example.com/m@v1.2.3: Get "https://proxy.golang.org/example.com/m/@v/v1.2.3.zip": net/http: TLS handshake timeout',
    'go: example.com/m@v1.2.3: read tcp 10.1.0.4:443: connection reset by peer',
    'go: example.com/m@v1.2.3: dial tcp 142.250.74.17:443: i/o timeout',
  ]) assert.ok(await retried(GO.patterns, line), line);
});

test('the Go download does not retry a module that is not there or a real failure', async () => {
  for (const line of [
    'go: example.com/x@v1.2.3: reading https://proxy.golang.org/example.com/x/@v/v1.2.3.info: 404 Not Found',
    'go: example.com/x@v1.2.3: reading https://proxy.golang.org/example.com/x/@v/v1.2.3.info: 410 Gone',
    'verifying example.com/x@v1.2.3: checksum mismatch\n\tdownloaded: h1:abc=\n\tgo.sum:     h1:def=',
    '--- FAIL: TestDownload (0.01s)\n    download_test.go:12: got 502 from proxy.golang.org mock, want 200\nFAIL\tgithub.com/dilla/x\t0.02s',
  ]) assert.ok(!(await retried(GO.patterns, line)), line);
});

test('the macOS build retries only the stapling and Apple service errors', async () => {
  for (const line of [
    'The staple and validate action failed! Error 65.',
    'Could not validate ticket: Error Domain=NSURLErrorDomain Code=-1001 "The request timed out."',
    'CloudKit query for Dilla.app (2/abc) failed due to "Record not found". The staple and validate action failed! Error 68.',
  ]) assert.ok(await retried(STAPLE.patterns, line), line);
  for (const line of [
    '  status: Invalid\n  message: Processing complete',
    'Notarization failed: Error 680',
    'Error 6800: something else',
    'npm ERR! code ELIFECYCLE',
  ]) assert.ok(!(await retried(STAPLE.patterns, line)), line);
});

test('the Electron download retries a gateway error, a rate limit and transport errors', async () => {
  for (const line of [
    'HTTPError: Response code 504 (Gateway Time-out) for https://github.com/electron/electron/releases/download/v45.2.1/electron-v45.2.1-darwin-arm64.zip',
    'HTTPError: Response code 502 (Bad Gateway) for https://github.com/electron/electron/releases/download/v45.2.1/electron-v45.2.1-linux-x64.zip',
    'HTTPError: Response code 429 (Too Many Requests) for https://github.com/electron/electron/releases/download/v45.2.1/electron-v45.2.1-win32-x64.zip',
    'RequestError: connect ETIMEDOUT 140.82.121.4:443',
    'RequestError: read ECONNRESET',
    'RequestError: getaddrinfo EAI_AGAIN github.com',
    'RequestError: socket hang up',
  ]) assert.ok(await retried(ELECTRON.patterns, line), line);
});

test('the Electron download does not retry a missing release or a checksum mismatch', async () => {
  for (const line of [
    'HTTPError: Response code 404 (Not Found) for https://github.com/electron/electron/releases/download/v0.0.0/electron-v0.0.0-linux-x64.zip',
    'Error: Response code 5040 (odd) for https://github.com/electron/electron/releases/download/v45.2.1/x.zip',
    'Error: checksum mismatch: sha256 of electron-v45.2.1-linux-x64.zip does not match SHASUMS256.txt',
    'Error: Failed to verify the sha256 checksum of the Electron download',
  ]) assert.ok(!(await retried(ELECTRON.patterns, line)), line);
});

test('the Gradle build retries a repository 5xx, a rate limit and transport errors', async () => {
  for (const line of [
    "> Could not resolve androidx.core:core:1.17.0.\n   > Could not GET 'https://dl.google.com/dl/android/maven2/androidx/core/core/1.17.0/core-1.17.0.pom'. Received status code 502 from server: Bad Gateway",
    "   > Could not HEAD 'https://repo.maven.apache.org/maven2/x/y/1/y-1.pom'. Received status code 429 from server: Too Many Requests",
    'Exception in thread "main" java.io.IOException: Server returned HTTP response code: 503 for URL: https://services.gradle.org/distributions/gradle-8.14.3-all.zip',
    '> Could not GET \'https://dl.google.com/x.pom\'.\n   > Read timed out',
    'Caused by: java.net.SocketTimeoutException: Connect timed out',
    'java.net.UnknownHostException: dl.google.com',
    'javax.net.ssl.SSLHandshakeException: Remote host terminated the handshake',
  ]) assert.ok(await retried(GRADLE.patterns, line), line);
});

test('the Gradle build does not retry a compile error, a missing dependency or a lint failure', async () => {
  for (const line of [
    'MainActivity.java:12: error: cannot find symbol\nFAILURE: Build failed with an exception.',
    "> Could not find androidx.core:core:9.9.9.\n  Searched in the following locations:\n    - https://dl.google.com/dl/android/maven2/androidx/core/core/9.9.9/core-9.9.9.pom",
    "   > Could not GET 'https://dl.google.com/x.pom'. Received status code 404 from server: Not Found",
    'Lint found errors in the project; aborting build.',
  ]) assert.ok(!(await retried(GRADLE.patterns, line)), line);
});

test('the Android SDK install retries a failed or incomplete download, not an unknown package', async () => {
  for (const line of [
    'Warning: An error occurred while preparing SDK package Google APIs Intel x86_64 Atom System Image: Connection reset.',
    'Warning: Failed to download package system-images;android-35;google_apis;x86_64',
    'ci-android: Failed to download: system-images/android-35/google_apis/x86_64/system.img missing under /usr/local/lib/android/sdk after sdkmanager',
    'java.net.SocketTimeoutException: Read timed out',
  ]) assert.ok(await retried(ANDROID_SDK.patterns, line), line);
  for (const line of [
    'Warning: Failed to find package \'system-images;android-99;google_apis;x86_64\'',
    'ci-android.sh: line 12: ANDROID_HOME: ANDROID_HOME is not set',
  ]) assert.ok(!(await retried(ANDROID_SDK.patterns, line)), line);
});

test('the Swift package resolution retries network errors, not a bad manifest or a missing version', async () => {
  for (const line of [
    "xcodebuild: error: Could not resolve package dependencies:\n  Failed to clone repository https://github.com/ionic-team/capacitor-swift-pm.git:\n    fatal: unable to access 'https://github.com/ionic-team/capacitor-swift-pm.git/': Could not resolve host: github.com",
    'error: RPC failed; curl 92 HTTP/2 stream 5 was not closed cleanly: CANCEL (err 8)',
    'fatal: unable to access \'https://github.com/x/y.git/\': Failed to connect to github.com port 443 after 75000 ms: Couldn\'t connect to server',
    "fatal: unable to access 'https://github.com/x/y.git/': The requested URL returned error: 502",
    'The network connection was lost.',
  ]) assert.ok(await retried(SPM.patterns, line), line);
  for (const line of [
    "xcodebuild: error: Could not resolve package dependencies:\n  Dependencies could not be resolved because no versions of 'capacitor-swift-pm' match the requirement 9.0.0..<10.0.0",
    "xcodebuild: error: Could not resolve package dependencies:\n  invalid manifest\n  Package.swift:12:5: error: extra argument",
    "fatal: unable to access 'https://github.com/x/y.git/': The requested URL returned error: 404",
  ]) assert.ok(!(await retried(SPM.patterns, line)), line);
});
