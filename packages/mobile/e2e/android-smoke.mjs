// Android emulator smoke test of the real app (debug APK) against two local dillad test hosts over HTTPS.
// Driven through adb and the WebView's DevTools socket (Playwright's Android driver over CDP). Exits 0 on success.
//
//   node packages/mobile/e2e/android-smoke.mjs [--skip-build] [--keep-emulator] [--avd=dilla35]
//
// Needs: target/dilla-testhost (go build -o target/dilla-testhost ./cmd/dilla-testhost), target/release/dilla-testkit
// (cargo build -p dilla-testkit --release --locked), internal/mlswasi/testdata/dilla_core_wasi.wasm, openssl,
// ANDROID_HOME (default ~/Android/Sdk), JAVA_HOME with JDK 21 (default ~/.local/jdk/jdk-21*), and an AVD on a
// google_apis image (adb root works there) with Chrome.
//
// The run: two hosts (A, B) behind HTTPS proxies whose certificate a throwaway CA signs for a.dilla.test and
// b.dilla.test. The emulator resolves no such names; its global HTTP proxy (settings global http_proxy) points at a
// local CONNECT proxy that maps exactly those two names, on the two test ports, to this machine (see HOSTS below).
// The CA is put into the emulator's user CA store, which only the debug build trusts
// (android/app/src/debug/res/xml/network_security_config.xml). Then, in the app: add instance A in the picker, sign up,
// join a native peer's server and see the peer's message; add instance B and sign up there; back on A, send the app to
// the background for 120 s while the peer posts, resume and see it; force-stop, relaunch, and find the same account
// still signed in and both instances in the picker; then 125 in-app loads of the picker and the client, each answering
// its first bridge call (the Capacitor reply-proxy race). Along the way: no CSP violation, and desktop-auth
// answers native HTTP (no Origin) while the WebView fetch is refused.
import { execFileSync, spawn } from 'node:child_process';
import { closeSync, existsSync, mkdtempSync, openSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { createInterface } from 'node:readline';
import { createHmac, randomInt } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { _android as android, expect } from '@playwright/test';
import { t } from '../../web/src/strings/index.ts';
import { createNativeHttpsProxy } from '../../../e2e/web/support/native-https-proxy.mjs';

const root = fileURLToPath(new URL('..', import.meta.url));
const repo = resolve(root, '../..');
const args = new Set(process.argv.slice(2));
const avd = [...args].find(a => a.startsWith('--avd='))?.slice(6) ?? 'dilla35';
const sdk = process.env.ANDROID_HOME ?? join(homedir(), 'Android/Sdk');
const jdkRoot = join(homedir(), '.local/jdk');
const javaHome = process.env.JAVA_HOME ?? (existsSync(jdkRoot) ? join(jdkRoot, readdirSync(jdkRoot).filter(d => d.startsWith('jdk-21')).sort().at(-1) ?? '') : undefined);
const env = { ...process.env, ANDROID_HOME: sdk, ...(javaHome ? { JAVA_HOME: javaHome } : {}) };
const ADB = join(sdk, 'platform-tools/adb');
const APP = 'chat.dilla.app';
// Screenshots, logcat and the emulator's own log. CI points it at a non-hidden path in the workspace
// (DILLA_ANDROID_E2E_OUT, .github/workflows/mobile.yml) so its failure-evidence upload finds the files.
const OUT = resolve(process.env.DILLA_ANDROID_E2E_OUT ?? join(root, 'e2e/.out'));
const DEVICE_WAIT_MS = 180_000;
const BOOT_WAIT_MS = 300_000;
const WAIT = 60_000;
const BACKGROUND_S = 120;
// The app accepts only instances with a DNS name (no IP literal, no localhost), and the run must not depend on outside
// DNS. Name resolution is done by our own CONNECT proxy, which the emulator uses as its global HTTP proxy: the WebView,
// Chrome's Custom Tab and CapacitorHttp (Java) all send `CONNECT a.dilla.test:18443` to it, unresolved, and it opens
// 127.0.0.1:18443 here. A WebView --host-resolver-rules flag would not reach CapacitorHttp, and dnsmasq behind the
// emulator's -dns-server would need port 53 on this machine (a privileged port).
const HOST_A = 'a.dilla.test', HOST_B = 'b.dilla.test';
const CONNECT_PORT = 18480;
/** Every tunnel the emulator opened through the CONNECT proxy, for the resume measurement (host clock). */
const tunnels = [];
/** CONNECT targets the proxy refused (Android's own background traffic), and the test hosts, for a failed run's diagnostics. */
const refused = [];
const hosts = [];
const work = mkdtempSync(join(tmpdir(), 'dilla-android-smoke-'));
const cleanups = [];
const started = Date.now();

function log(line) { console.log(`[${((Date.now() - started) / 1000).toFixed(1).padStart(6)}s] ${line}`); }
const adb = (...a) => execFileSync(ADB, a, { encoding: 'utf8', env }).trim();
const sleep = ms => new Promise(r => setTimeout(r, ms));
const copy = (key, vars) => t(key, vars);
const esc = s => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

// ---- test hosts and the native peer --------------------------------------------------------------------------------

async function startHost(name, hostName, port, control, proxyPort, tls) {
  const data = join(work, `host-${name}`);
  const host = spawn(join(repo, 'target/dilla-testhost'), ['-listen', `127.0.0.1:${port}`, '-control', `127.0.0.1:${control}`,
    '-core', join(repo, 'internal/mlswasi/testdata/dilla_core_wasi.wasm'), '-log-level', 'warn', '-data-dir', data, '-production-acl'],
  { stdio: ['ignore', 'pipe', 'pipe'] });
  cleanups.push(() => host.kill('SIGTERM'));
  const invite = await new Promise((ok, fail) => {
    let seen = '';
    const timer = setTimeout(() => fail(new Error(`host ${name} printed no invite:\n${seen}`)), WAIT);
    const on = chunk => { seen += chunk; const m = /DILLA_TESTKIT_INVITE=(\S+)/.exec(seen); if (m) { clearTimeout(timer); ok(m[1]); } };
    host.stdout.on('data', on); host.stderr.on('data', on);
    host.once('exit', code => { clearTimeout(timer); fail(new Error(`host ${name} exited ${code}:\n${seen}`)); });
  });
  const proxy = await createNativeHttpsProxy({ key: readFileSync(tls.key), cert: readFileSync(tls.cert), upstreamPort: port, port: proxyPort });
  cleanups.push(() => proxy.close());
  // What reached /desktop-auth/, with its Origin header: the native-HTTP check below reads it.
  const desktopAuth = [];
  // The resume measurement: every request and gateway upgrade, with its socket's close (host clock).
  const traffic = [];
  proxy.on('request', request => { traffic.push({ at: Date.now(), what: `${request.method} ${request.url.replace(/[A-Za-z0-9_-]{43}/, '<id>').split('?')[0]}` }); });
  proxy.on('upgrade', (request, socket) => {
    const entry = { at: Date.now(), what: `UPGRADE ${request.url.split('?')[0]}` };
    traffic.push(entry);
    socket.on('close', () => traffic.push({ at: Date.now(), what: `CLOSED ${request.url.split('?')[0]} (opened +${Date.now() - entry.at} ms before)` }));
  });
  proxy.on('request', request => { if (request.url.startsWith('/desktop-auth/')) desktopAuth.push({ method: request.method, path: request.url, origin: request.headers.origin ?? null, agent: request.headers['user-agent'] ?? '' }); });
  return { name, desktopAuth, traffic, invite, inviteCode: invite.split(',')[0], ds: `http://127.0.0.1:${port}`, control: `http://127.0.0.1:${control}`, url: `https://${hostName}:${proxyPort}/` };
}

/** dilla-testkit web-driver: the native second account (e2e/web/support/driver.ts speaks the same JSON lines). */
async function startPeer(host) {
  const child = spawn(join(repo, 'target/release/dilla-testkit'), ['web-driver', '--ds', host.ds, '--seed', String(randomInt(1, 0xff_ffff))],
    { env: { ...process.env, DILLA_TESTKIT_INVITE: host.invite, DILLA_TESTKIT_CONTROL: host.control }, stdio: ['pipe', 'pipe', 'pipe'] });
  cleanups.push(() => child.kill('SIGTERM'));
  child.stderr.on('data', d => process.stderr.write(d));
  let next = 1; const pending = new Map();
  createInterface({ input: child.stdout }).on('line', line => {
    const answer = JSON.parse(line); const p = pending.get(answer.id); if (!p) return; pending.delete(answer.id);
    if (answer.ok) p.ok(answer); else p.fail(new Error(`web-driver: ${answer.error}`));
  });
  const request = (op, a = {}) => new Promise((ok, fail) => { const id = next++; pending.set(id, { ok, fail }); child.stdin.write(`${JSON.stringify({ id, op, ...a })}\n`); });
  return { request, community: `android-${randomInt(0, 0xffff).toString(16)}`, channel: 'general' };
}

// ---- TOTP against the host's fake clock (as e2e/web/support/totp.ts) -----------------------------------------------

function totp(secret, counter) {
  const B32 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'; let bits = 0, value = 0; const out = [];
  for (const ch of secret.replace(/[\s=-]/g, '').toUpperCase()) { value = (value << 5) | B32.indexOf(ch); bits += 5; if (bits >= 8) { out.push((value >>> (bits - 8)) & 0xff); bits -= 8; } }
  const msg = Buffer.alloc(8); msg.writeBigUInt64BE(BigInt(counter));
  const mac = createHmac('sha1', Buffer.from(out)).update(msg).digest(); const off = mac[mac.length - 1] & 0x0f;
  return ((((mac[off] & 0x7f) << 24) | (mac[off + 1] << 16) | (mac[off + 2] << 8) | mac[off + 3]) % 1e6).toString().padStart(6, '0');
}
async function nextCode(host, account) {
  for (;;) {
    const now = (await (await fetch(`${host.control}/debug/clock`)).json()).now;
    const c = Math.floor(now / 30), n = Math.max(c, account.lastCounter + 1);
    if (n <= c + 1) { account.lastCounter = n; return totp(account.totpSecret, n); }
    await fetch(`${host.control}/debug/clock`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ seconds: 30 }) });
  }
}

/** The CONNECT proxy: only HOST_A/HOST_B on the two HTTPS proxy ports, to 127.0.0.1; anything else is refused. */
async function startConnectProxy() {
  const { createServer } = await import('node:http');
  const { connect: tcp } = await import('node:net');
  const allowed = new Map([[`${HOST_A}:18443`, 18443], [`${HOST_B}:18444`, 18444]]);
  const server = createServer((_request, response) => { response.writeHead(403).end(); });
  server.on('connect', (request, client, head) => {
    const port = allowed.get(request.url ?? '');
    // Refused tunnels (Android's own background traffic, e.g. connectivity checks, also goes through the global proxy)
    // can be reset by the device; without a listener that ECONNRESET is an unhandled 'error' and kills the run.
    if (port === undefined) { refused.push({ target: request.url, at: Date.now() }); client.on('error', () => {}); client.end('HTTP/1.1 403 Forbidden\r\n\r\n'); return; }
    const entry = { target: request.url, opened: Date.now(), closed: null };
    tunnels.push(entry);
    const upstream = tcp(port, '127.0.0.1', () => { client.write('HTTP/1.1 200 Connection Established\r\n\r\n'); if (head.length) upstream.write(head); upstream.pipe(client).pipe(upstream); });
    const close = () => { entry.closed ??= Date.now(); upstream.destroy(); client.destroy(); };
    upstream.on('error', close); client.on('error', close); upstream.on('close', close); client.on('close', close);
  });
  server.on('clientError', (_error, socket) => { socket.destroy(); });
  await new Promise((ok, fail) => { server.once('error', fail); server.listen(CONNECT_PORT, '127.0.0.1', ok); });
  cleanups.push(() => new Promise(ok => { server.closeAllConnections?.(); server.close(() => ok()); }));
}

// ---- the emulator, the CA and the app ------------------------------------------------------------------------------

async function ensureEmulator() {
  if (adb('devices').split('\n').slice(1).some(l => /\tdevice$/.test(l))) { log('emulator already running'); return false; }
  log(`starting emulator ${avd}`);
  // The emulator's own output goes to OUT/emulator.log: the evidence when it never comes up (no KVM, no GPU mode).
  const emuLog = join(OUT, 'emulator.log');
  const fd = openSync(emuLog, 'a');
  const emu = spawn(join(sdk, 'emulator/emulator'), ['-avd', avd, '-no-window', '-no-snapshot-save', '-no-audio', '-no-boot-anim'], { stdio: ['ignore', fd, fd], detached: true, env });
  closeSync(fd);
  let exited = null;
  emu.once('exit', (code, signal) => { exited = signal ?? `exit ${code}`; });
  emu.unref();
  try {
    execFileSync(ADB, ['wait-for-device'], { env, timeout: DEVICE_WAIT_MS, stdio: 'ignore' });
  } catch (error) {
    throw new Error(`emulator ${avd}: no device after ${DEVICE_WAIT_MS / 1000} s${exited ? ` (emulator ${exited})` : ''}; see ${emuLog}`, { cause: error });
  }
  const deadline = Date.now() + BOOT_WAIT_MS;
  for (;;) {
    let booted = '';
    try { booted = adb('shell', 'getprop', 'sys.boot_completed'); } catch { /* offline while it boots */ }
    if (booted === '1') break;
    if (exited) throw new Error(`emulator ${avd} stopped while booting (${exited}); see ${emuLog}`);
    if (Date.now() > deadline) throw new Error(`emulator ${avd} did not finish booting in ${BOOT_WAIT_MS / 1000} s; see ${emuLog}`);
    await sleep(2000);
  }
  log('emulator booted');
  return true;
}

function makeTls() {
  const dir = join(work, 'tls');
  execFileSync('mkdir', ['-p', dir]);
  const run = a => execFileSync('openssl', a, { stdio: 'ignore' });
  const ca = join(dir, 'ca.pem'), caKey = join(dir, 'ca-key.pem'), key = join(dir, 'server-key.pem'), cert = join(dir, 'server.pem');
  run(['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', caKey, '-out', ca, '-days', '1', '-subj', '/CN=Dilla disposable Android e2e CA', '-addext', 'basicConstraints=critical,CA:TRUE', '-addext', 'keyUsage=critical,keyCertSign,cRLSign']);
  run(['req', '-new', '-newkey', 'rsa:2048', '-nodes', '-keyout', key, '-out', join(dir, 'server.csr'), '-subj', `/CN=${HOST_A}`]);
  writeFileSync(join(dir, 'server.ext'), `subjectAltName=DNS:${HOST_A},DNS:${HOST_B}\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n`);
  run(['x509', '-req', '-in', join(dir, 'server.csr'), '-CA', ca, '-CAkey', caKey, '-CAcreateserial', '-out', cert, '-days', '1', '-extfile', join(dir, 'server.ext')]);
  return { ca, key, cert };
}

/** A user-added CA, as Settings → Encryption → Install a certificate would store it (needs adb root: google_apis image). */
function installUserCa(ca) {
  const hash = execFileSync('openssl', ['x509', '-subject_hash_old', '-noout', '-in', ca], { encoding: 'utf8' }).trim();
  const dir = '/data/misc/user/0/cacerts-added';
  adb('root'); adb('wait-for-device');
  adb('push', ca, `/data/local/tmp/${hash}.0`);
  adb('shell', `mkdir -p ${dir} && cp /data/local/tmp/${hash}.0 ${dir}/${hash}.0 && chown -R system:system ${dir} && chmod 755 ${dir} && chmod 644 ${dir}/${hash}.0 && restorecon -R ${dir}`);
  cleanups.push(() => { try { adb('shell', `rm -f ${dir}/${hash}.0 /data/local/tmp/${hash}.0`); } catch { /* the emulator may be gone */ } });
  log(`dev CA installed as user CA ${hash}.0`);
  // Chrome's first-run screen would cover the Custom Tab the sign-in opens. A debuggable emulator image honours
  // this command-line file (ro.debuggable=1).
  adb('shell', 'echo "_ --disable-fre --no-default-browser-check --no-first-run" > /data/local/tmp/chrome-command-line && am force-stop com.android.chrome');
  cleanups.push(() => { try { adb('shell', 'rm -f /data/local/tmp/chrome-command-line'); } catch { /* gone */ } });
  // All HTTP(S) from the device goes through the CONNECT proxy, which resolves the two test names (10.0.2.2 is this
  // machine as the emulator sees it).
  adb('shell', 'settings', 'put', 'global', 'http_proxy', `10.0.2.2:${CONNECT_PORT}`);
  cleanups.push(() => { try { adb('shell', 'settings', 'put', 'global', 'http_proxy', ':0'); } catch { /* gone */ } });
}

function buildAndInstall() {
  const npm = process.env.npm_execpath;
  if (!args.has('--skip-build')) {
    log('building the web client, the bundle and the debug APK');
    if (npm) execFileSync(process.execPath, [npm, 'run', 'build', '-w', '@dilla/mobile'], { cwd: repo, stdio: 'inherit', env });
    else execFileSync('npm', ['run', 'build', '-w', '@dilla/mobile'], { cwd: repo, stdio: 'inherit', env });
    execFileSync('npx', ['cap', 'sync', 'android'], { cwd: root, stdio: 'inherit', env });
    execFileSync('./gradlew', ['assembleDebug', '-q'], { cwd: join(root, 'android'), stdio: 'inherit', env });
  }
  const apk = join(root, 'android/app/build/outputs/apk/debug/app-debug.apk');
  execFileSync(process.execPath, [join(root, 'scripts/check-apk-network-config.mjs'), apk, '--debug'], { stdio: 'inherit', env });
  try { adb('uninstall', APP); } catch { /* not installed */ }
  adb('install', '-r', '-g', apk);
  log('debug APK installed (fresh, runtime permissions granted)');
}

function launch() { adb('shell', 'monkey', '-p', APP, '-c', 'android.intent.category.LAUNCHER', '1'); }

/** The app's WebView through its DevTools socket (webview_devtools_remote_<pid>). Playwright's Android driver attaches to
 *  that socket over adb; plain chromium.connectOverCDP cannot, since a WebView refuses Browser.setDownloadBehavior. */
async function connect(urlPattern) {
  let pid = '';
  for (let i = 0; i < 60 && !pid; i++) { try { pid = adb('shell', 'pidof', APP); } catch { await sleep(500); } }
  if (!pid) throw new Error('the app did not start');
  const [device] = await android.devices();
  if (!device) throw new Error('adb lists no device');
  cleanups.push(() => device.close().catch(() => {}));
  // By the process's own socket: after a force-stop the package's old socket must not be picked up.
  const webview = await device.webView({ socketName: `webview_devtools_remote_${pid}` }, { timeout: WAIT });
  const page = await webview.page();
  // Every action and wait is bounded: Playwright's Android pages default to no timeout, so a stalled WebView hung a
  // fill/click until the job's 20-minute limit and the diagnostics were never written (run 42187112370).
  page.setDefaultTimeout(WAIT);
  page.setDefaultNavigationTimeout(WAIT);
  for (let i = 0; i < 120 && !urlPattern.test(page.url()); i++) await sleep(500);
  if (!urlPattern.test(page.url())) throw new Error(`the WebView is at ${page.url()}, expected ${urlPattern}`);
  watch(page);
  return { page, pid };
}

const violations = [];
/** The WebView's console errors and warnings and its failed requests (last 200 each), for a failed run's diagnostics. */
const consoleLines = [], failedRequests = [];
const keep = (list, item) => { list.push(item); if (list.length > 200) list.shift(); };
function watch(page) {
  page.on('console', message => {
    const text = message.text();
    if (/Content Security Policy|Refused to (load|execute|apply|connect|create)/i.test(text)) violations.push(text);
    if (message.type() === 'error' || message.type() === 'warning') keep(consoleLines, `${new Date().toISOString()} ${message.type()} ${text.slice(0, 500)}`);
  });
  page.on('requestfailed', request => { keep(failedRequests, `${new Date().toISOString()} ${request.method()} ${request.url().split('?')[0]} ${request.failure()?.errorText ?? ''}`); });
}

/** Why a step stalled: everything the proxy, the hosts and the WebView saw, written to OUT and summarised in the log. */
function writeDiagnostics() {
  const rel = at => `${((at - started) / 1000).toFixed(1)}s`;
  const lines = [
    `# tunnels (${tunnels.length})`, ...tunnels.map(t => `${rel(t.opened)} ${t.target} ${t.closed === null ? 'open' : `closed ${rel(t.closed)}`}`),
    `# refused CONNECTs (${refused.length})`, ...refused.map(r => `${rel(r.at)} ${r.target}`),
    ...hosts.flatMap(h => [`# instance ${h.name} requests (${h.traffic.length})`, ...h.traffic.map(t => `${rel(t.at)} ${t.what}`)]),
    `# WebView console errors and warnings (${consoleLines.length})`, ...consoleLines,
    `# WebView failed requests (${failedRequests.length})`, ...failedRequests,
  ];
  writeFileSync(join(OUT, 'diagnostics.txt'), `${lines.join('\n')}\n`);
  console.error(`diagnostics: ${tunnels.length} tunnels (last at ${tunnels.length ? rel(tunnels.at(-1).opened) : '-'}), ${refused.length} refused, `
    + hosts.map(h => `${h.name} ${h.traffic.length} requests (last ${h.traffic.length ? `${rel(h.traffic.at(-1).at)} ${h.traffic.at(-1).what}` : '-'})`).join(', ')
    + `; last failed requests:\n  ${failedRequests.slice(-8).join('\n  ') || '-'}\nlast console errors:\n  ${consoleLines.slice(-8).join('\n  ') || '-'}`);
}

// ---- UI steps (the selectors of e2e/web/support/app.ts) -------------------------------------------------------------

const button = (page, key, vars) => page.getByRole('button', { name: copy(key, vars), exact: true });
const tabs = page => page.getByRole('navigation', { name: copy('nav.tabs.label'), exact: true });
const rail = page => page.getByRole('navigation', { name: copy('shell.rail.label'), exact: true });
const named = (scope, name) => scope.getByRole('button', { name: new RegExp(`^${esc(name)}( ${esc(copy('slow.badge'))})?(, |$)`) });
const composer = (page, channel) => page.getByRole('textbox', { name: copy('shell.composer.label', { channel }), exact: true });
const row = (page, channel, text) => page.getByRole('log', { name: copy('shell.log.label', { channel }), exact: true }).locator('.d-message-row').filter({ hasText: text });

async function expectShell(page) {
  await expect(page.locator('.d-app-shell')).toBeVisible({ timeout: WAIT });
  await expect(tabs(page)).toBeVisible({ timeout: WAIT });
  await expect(page.locator('.d-onboarding-frame')).toHaveCount(0);
}

async function pickInstance(page, url) {
  await expect(page.getByRole('heading', { level: 1 })).toBeVisible({ timeout: WAIT });
  await page.getByLabel('Instance address').fill(url);
  // The picker probes the instance (10 s) before it navigates. On a cold CI emulator the first probe can time out; a
  // person taps Connect again, and so does the test, at most three times, saying so in the log.
  for (let attempt = 1; ; attempt++) {
    await page.getByRole('button', { name: 'Connect', exact: true }).click();
    const outcome = await Promise.race([
      page.waitForURL(/^https:\/\/localhost\/$/, { timeout: WAIT }).then(() => 'navigated', () => 'timeout'),
      expect(page.locator('#error')).not.toBeEmpty({ timeout: WAIT }).then(() => 'error', () => 'timeout'),
    ]);
    if (outcome === 'navigated') return;
    if (outcome === 'timeout') throw new Error(`the picker neither navigated nor showed an error for ${url} in ${WAIT / 1000} s`);
    const message = await page.locator('#error').textContent().catch(() => '');
    log(`picker: connect to ${url} failed (attempt ${attempt}): ${message}`);
    if (attempt === 3) throw new Error(`the picker could not connect to ${url} in 3 attempts: ${message}`);
  }
}

async function signUp(page, host) {
  const account = { username: `a${randomInt(0, 0xffff_ffff).toString(16)}`, password: 'e2e-password-1234', totpSecret: null, lastCounter: -1 };
  // Review M1: the picker's navigation to / boots the client; a goto while that load is still running raced it
  // (the page detached on a cold emulator). Wait until the client shows its onboarding, then go to the invite.
  await expect(page.locator('.d-onboarding-frame').getByRole('heading', { level: 1 })).toBeVisible({ timeout: WAIT });
  const welcome = `https://localhost/welcome?invite=${encodeURIComponent(host.inviteCode)}`;
  const connectTitle = page.getByRole('heading', { name: copy('onboarding.connect.title') });
  await page.goto(welcome);
  // A cold CI emulator has left this load blank (run 42170416320); record the page's state, then load it once more.
  if (!await connectTitle.isVisible() && !await connectTitle.waitFor({ timeout: WAIT / 2 }).then(() => true, () => false)) {
    const state = await page.evaluate(() => `${location.href} readyState=${document.readyState} body=${document.body?.innerHTML.length ?? 0} chars`).catch(e => `unreadable (${e})`);
    log(`signup on ${host.name}: the invite page is blank after ${WAIT / 2000} s (${state}); loading it again`);
    await page.goto(welcome);
  }
  await expect(connectTitle).toBeVisible({ timeout: WAIT });
  await button(page, 'onboarding.next').click();
  await page.getByLabel(copy('onboarding.identity.username'), { exact: true }).fill(account.username);
  await page.getByLabel(copy('onboarding.identity.password'), { exact: true }).fill(account.password);
  await button(page, 'onboarding.identity.submit').click();
  await expect(page.locator('.d-recovery-key li').first()).toBeVisible({ timeout: WAIT });
  await page.getByRole('checkbox', { name: copy('onboarding.keys.acknowledge'), exact: true }).check();
  await button(page, 'onboarding.keys.submit').click();
  await page.getByRole('radio', { name: new RegExp(esc(copy('factor.choose.app'))) }).check();
  await button(page, 'factor.choose.next').click();
  const key = page.getByLabel(copy('factor.totp.key'), { exact: true });
  await expect(key).toHaveValue(/^[A-Z2-7]{16,}$/, { timeout: WAIT });
  account.totpSecret = await key.inputValue();
  await page.getByLabel(copy('factor.totp.code'), { exact: true }).fill(await nextCode(host, account));
  const show = button(page, 'onboarding.done.codesShow');
  const codes = page.getByRole('list', { name: copy('factor.codes.label'), exact: true });
  await expect(codes.or(show)).toBeVisible({ timeout: WAIT });
  if (await show.isVisible()) await show.click();
  await page.getByRole('checkbox', { name: copy('onboarding.done.codesAck'), exact: true }).check();
  await button(page, 'onboarding.done.next').click();
  await expectShell(page);
  return account;
}

async function joinServer(page, peer, setup) {
  if (!(await rail(page).isVisible())) await tabs(page).getByRole('link', { name: copy('home.tab'), exact: true }).click();
  await rail(page).getByRole('button', { name: copy('layout.rail.add'), exact: true }).click();
  await page.getByRole('dialog', { name: copy('layout.add.title'), exact: true }).getByRole('button', { name: copy('layout.add.join'), exact: true }).click();
  const dialog = page.getByRole('dialog', { name: copy('join.title'), exact: true });
  await dialog.getByLabel(copy('join.invite'), { exact: true }).fill(setup.invite_code);
  await dialog.getByRole('button', { name: copy('join.submit'), exact: true }).click();
  await expect(dialog).toBeHidden({ timeout: WAIT });
  await expect(named(rail(page), peer.community)).toBeVisible({ timeout: WAIT });
}

async function openChannel(page, peer) {
  if (!(await rail(page).isVisible())) await tabs(page).getByRole('link', { name: copy('home.tab'), exact: true }).click();
  await named(rail(page), peer.community).click();
  await named(page.getByRole('navigation', { name: copy('shell.channels.label'), exact: true }), peer.channel).click();
  await expect(composer(page, peer.channel)).toBeEnabled({ timeout: WAIT });
}

async function waitForDevices(peer, count) {
  for (let i = 0; i < 120; i++) {
    await peer.request('sync');
    if ((await peer.request('members')).devices.length === count) return;
    await sleep(500);
  }
  throw new Error(`the peer's group never reached ${count} devices`);
}

async function switchInstance(page) {
  await page.evaluate(() => { void globalThis.dillaMobile.send({ type: 'switch-instance' }); });
  await page.waitForURL(/\/picker\.html$/, { timeout: WAIT });
}

// ---- the run -------------------------------------------------------------------------------------------------------

async function main() {
  const ownEmulator = await ensureEmulator();
  if (ownEmulator && !args.has('--keep-emulator')) cleanups.unshift(() => { try { adb('emu', 'kill'); log('emulator stopped'); } catch { /* gone */ } });
  const tls = makeTls();
  await startConnectProxy();
  const a = await startHost('A', HOST_A, 18463, 18464, 18443, tls);
  const b = await startHost('B', HOST_B, 18465, 18466, 18444, tls);
  hosts.push(a, b);
  log(`hosts up: A ${a.url}, B ${b.url}`);
  installUserCa(tls.ca);
  buildAndInstall();

  // 1. A fresh install opens the picker; add instance A.
  launch();
  let { page } = await connect(/^https:\/\/localhost\//);
  await page.waitForURL(/\/picker\.html$/, { timeout: WAIT });
  log('step 1: picker shown on first launch');
  await pickInstance(page, `https://${HOST_A}:18443`);
  const stored = await page.evaluate(async () => (await globalThis.dillaMobile.info()).instance);
  if (stored !== a.url) throw new Error(`current instance is ${stored}, expected ${a.url}`);
  log(`step 1: instance A added and current (${stored})`);

  // 2. Sign up on A; the native peer (the second account) owns a server the phone joins.
  const peerA = await startPeer(a);
  const setup = await peerA.request('setup', { community: peerA.community, channel: peerA.channel });
  await peerA.request('register');
  const accountA = await signUp(page, a);
  log(`step 2: signed up on A as ${accountA.username}`);
  await joinServer(page, peerA, setup);
  await openChannel(page, peerA);
  await waitForDevices(peerA, 2);

  // 3. The second account sends; the phone shows it.
  const first = `from the peer ${randomInt(0, 0xffffff).toString(16)}`;
  await peerA.request('send', { body: first });
  await expect(row(page, peerA.channel, first)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  log(`step 3: peer message arrived on the device: "${first}"`);
  const reply = `from the phone ${randomInt(0, 0xffffff).toString(16)}`;
  await composer(page, peerA.channel).fill(reply);
  await composer(page, peerA.channel).press('Enter');
  await expect(row(page, peerA.channel, reply)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  for (let i = 0; i < 60; i++) { const s = await peerA.request('sync'); if (s.received.some(m => m.body === reply)) break; if (i === 59) throw new Error('the peer never got the phone\'s reply'); await sleep(500); }
  log(`step 3: phone reply reached the peer: "${reply}"`);

  // The second instance: picker, add B, sign up there.
  await switchInstance(page);
  await pickInstance(page, `https://${HOST_B}:18444`);
  const accountB = await signUp(page, b);
  log(`step 2: signed up on B as ${accountB.username}`);
  await switchInstance(page);
  await expect(page.locator('#instances button')).toHaveCount(2);
  await page.locator('#instances button', { hasText: a.url }).click();
  await page.waitForURL(/^https:\/\/localhost\/$/, { timeout: WAIT });
  await expectShell(page);
  await openChannel(page, peerA);
  await expect(row(page, peerA.channel, first)).toHaveCount(1, { timeout: WAIT });
  log('back on A: history intact');

  // Desktop-auth through the bridge (ruling 2): the ceremony requests go out over native HTTP with no Origin, the
  // system browser (Custom Tabs) opens /desktop-auth/<id>, and the verifier never appears in a URL. The WebView's own
  // fetch to the same route is refused (no CORS there, and the server rejects a foreign Origin).
  const webviewFetch = await page.evaluate(async base => {
    try { return String((await fetch(`${base}desktop-auth/request`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' })).status); }
    catch (e) { return `refused (${e})`; }
  }, a.url);
  a.desktopAuth.length = 0;
  await page.evaluate(() => { void globalThis.dillaMobile.browserAuth({ kind: 'get', options: '{"publicKey":{"challenge":"AAAAAAAAAAAAAAAAAAAAAA"}}' }).catch(() => {}); });
  for (let i = 0; i < 60 && !(a.desktopAuth.some(r => r.method === 'GET' && /^\/desktop-auth\/[A-Za-z0-9_-]{43}$/.test(r.path)) && a.desktopAuth.some(r => r.path.endsWith('/consume'))); i++) await sleep(500);
  const top = adb('shell', 'dumpsys', 'activity', 'activities').split('\n').find(l => /topResumedActivity|mResumedActivity/.test(l))?.trim() ?? '';
  const posts = a.desktopAuth.filter(r => r.method === 'POST');
  const opened = a.desktopAuth.find(r => r.method === 'GET' && /^\/desktop-auth\/[A-Za-z0-9_-]{43}$/.test(r.path));
  log(`desktop-auth: WebView fetch -> ${webviewFetch}`);
  for (const r of a.desktopAuth.slice(0, 6)) log(`desktop-auth: ${r.method} ${r.path.replace(/[A-Za-z0-9_-]{43}/, '<id>')} origin=${r.origin} agent=${r.agent.slice(0, 40)}`);
  log(`desktop-auth: top activity ${top}`);
  if (/^2/.test(webviewFetch)) throw new Error(`the WebView fetch reached /desktop-auth/: ${webviewFetch}`);
  if (!posts.some(r => r.path === '/desktop-auth/request') || !posts.some(r => r.path.endsWith('/consume'))) throw new Error('no native desktop-auth request/consume');
  if (posts.some(r => r.origin !== null)) throw new Error(`a desktop-auth POST carried Origin: ${JSON.stringify(posts)}`);
  if (!opened) throw new Error('the system browser never opened /desktop-auth/<id>');
  // Back to the app (the Custom Tab stays open behind it until the abandoned ceremony expires).
  const appOnTop = () => adb('shell', 'dumpsys', 'activity', 'activities').split('\n').some(l => /topResumedActivity/.test(l) && l.includes(APP));
  launch();
  for (let i = 0; i < 40 && !appOnTop(); i++) await sleep(500);
  if (!appOnTop()) throw new Error('the app did not come back in front of the Custom Tab');
  await sleep(1000);

  // 4. Background for 120 s; the peer posts meanwhile; resume and see it. Measured (review round 2, item 3): in the worker,
  // each gateway socket's attempt, open, error and close (WebSocket.prototype.addEventListener is wrapped: the gateway
  // attaches its listeners right after `new WebSocket`); at the proxies, every tunnel, request and upgrade; and whether
  // the page navigated or a new worker started while in the background. Device times are relative to the resume event.
  const events = await page.evaluateHandle(() => { const seen = []; globalThis.dillaMobile.onEvent(e => seen.push({ type: e.type, at: Date.now() })); return seen; });
  const worker = page.workers().find(w => /\/assets\/entry-/.test(w.url()));
  if (!worker) throw new Error(`no core worker among ${page.workers().map(w => w.url()).join(', ')}`);
  await worker.evaluate(() => {
    const log = globalThis.__dillaGatewayLog = [];
    const add = WebSocket.prototype.addEventListener;
    WebSocket.prototype.addEventListener = function (type, listener, options) {
      if (!this.__dillaSeen) {
        this.__dillaSeen = true;
        log.push({ at: Date.now(), e: 'attempt', url: this.url.split('?')[0] });
        add.call(this, 'open', () => log.push({ at: Date.now(), e: 'open' }));
        add.call(this, 'error', () => log.push({ at: Date.now(), e: 'error' }));
        add.call(this, 'close', event => log.push({ at: Date.now(), e: 'close', code: event.code, clean: event.wasClean, reason: event.reason }));
      }
      return add.call(this, type, listener, options);
    };
  });
  let navigations = 0, workersStarted = 0;
  page.on('framenavigated', frame => { if (frame === page.mainFrame()) navigations++; });
  page.on('worker', () => { workersStarted++; });
  const hostHome = Date.now();
  const tunnelsBefore = tunnels.length, trafficBefore = a.traffic.length;
  adb('shell', 'input', 'keyevent', 'KEYCODE_HOME');
  log(`step 4: app in the background for ${BACKGROUND_S} s`);
  await sleep(BACKGROUND_S / 2 * 1000);
  const meanwhile = `while backgrounded ${randomInt(0, 0xffffff).toString(16)}`;
  await peerA.request('send', { body: meanwhile });
  log(`step 4: peer sent "${meanwhile}" at t+${BACKGROUND_S / 2} s`);
  await sleep(BACKGROUND_S / 2 * 1000);
  const hostResume = Date.now();
  launch();
  await expect(row(page, peerA.channel, meanwhile)).toHaveAttribute('data-state', 'ok', { timeout: 120_000 });
  const shownAt = await page.evaluate(() => Date.now());
  const seen = await events.jsonValue();
  const resumeAt = seen.find(e => e.type === 'resume')?.at;
  log(`step 4: resumed; the message is shown; bridge events ${JSON.stringify(seen.map(e => e.type))}`);
  if (!seen.some(e => e.type === 'pause') || resumeAt === undefined) throw new Error(`pause/resume events missing: ${JSON.stringify(seen)}`);
  const gatewayLog = await worker.evaluate(() => globalThis.__dillaGatewayLog);
  const rel = at => `${((at - resumeAt) / 1000).toFixed(1)} s`;
  log(`step 4: RESUME->MESSAGE ${((shownAt - resumeAt) / 1000).toFixed(1)} s (device clock); page navigations ${navigations}, new workers ${workersStarted} (0/0: the same page and core worker continued)`);
  for (const entry of gatewayLog) log(`step 4: worker gateway ${entry.e}${entry.code === undefined ? '' : ` code=${entry.code} clean=${entry.clean}${entry.reason ? ` reason=${entry.reason}` : ''}`}${entry.url ? ` ${entry.url}` : ''} at ${rel(entry.at)}`);
  const hostRel = at => `${((at - hostResume) / 1000).toFixed(1)} s`;
  const newTunnels = tunnels.slice(tunnelsBefore);
  log(`step 4: proxy: ${newTunnels.length} tunnels opened from HOME to the message, first at ${newTunnels.length ? hostRel(newTunnels[0].opened) : '-'} (host clock, rel. to resume; HOME at ${hostRel(hostHome)})`);
  const traffic = a.traffic.slice(trafficBefore);
  for (const t of traffic.filter(t => /gateway|CLOSED|UPGRADE/.test(t.what))) log(`step 4: instance A ${t.what} at ${hostRel(t.at)} (host clock)`);
  log(`step 4: instance A: ${traffic.filter(t => !/gateway|CLOSED|UPGRADE/.test(t.what)).length} other requests (sync after resume, ceremony polls)`);

  // 5. Force-stop and relaunch: the same account on A, and both instances in the picker.
  adb('shell', 'am', 'force-stop', APP);
  log('step 5: force-stopped');
  launch();
  ({ page } = await connect(/^https:\/\/localhost\/(?!picker\.html)/));
  await expectShell(page);
  await openChannel(page, peerA);
  await expect(row(page, peerA.channel, reply)).toHaveCount(1, { timeout: WAIT });
  await expect(row(page, peerA.channel, meanwhile)).toHaveCount(1, { timeout: WAIT });
  const after = `after relaunch ${randomInt(0, 0xffffff).toString(16)}`;
  await peerA.request('send', { body: after });
  await expect(row(page, peerA.channel, after)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  log('step 5: relaunched into the shell on A, history and live delivery intact (same account, no sign-in)');
  await switchInstance(page);
  // The picker renders its saved instances after load (an async Preferences read): wait for them before reading the list
  // (run 42200767452 read it empty).
  await expect(page.locator('#instances button')).toHaveCount(2, { timeout: WAIT });
  const listed = await page.locator('#instances button').allTextContents();
  if (JSON.stringify(listed) !== JSON.stringify([a.url, b.url])) throw new Error(`picker lists ${JSON.stringify(listed)}`);
  log(`step 5: picker lists both instances ${JSON.stringify(listed)}`);
  await page.screenshot({ path: join(OUT, 'picker.png') }).catch(() => {});

  // 6. Navigation stress: in-app loads of the picker and the client, back and forth. Stock Capacitor Android 8.6.0 lost
  // a new page's first plugin answer on 1-6 % of them on a local emulator (the reply-proxy race,
  // patches/@capacitor+android+*.patch): the picker never listed its instances and the client stayed blank. Picker loads
  // are cheap, so each round trip reloads the picker four times before the client, 125 loads in about 15 s;
  // at 2 % per load a stock build fails this step 92 % of the time. A lost answer never arrives, so a bound well above
  // a normal load tells them apart; html[data-mobile-platform] is set only once the client's bridge info() answered.
  const stressRounds = 25, pickerLoads = 4, stressWait = WAIT / 4;
  const stalled = [];
  let loads = 0;
  for (let i = 0; i < stressRounds; i++) {
    for (let j = 0; j < pickerLoads; j++, loads++) {
      await page.goto(`https://localhost/picker.html?stress=${i}.${j}`);
      if (!await expect(page.locator('#instances button')).toHaveCount(2, { timeout: stressWait }).then(() => true, () => false)) stalled.push(`picker ${i}.${j}`);
    }
    await page.goto('https://localhost/');
    loads++;
    if (!await page.locator('html[data-mobile-platform]').waitFor({ state: 'attached', timeout: stressWait }).then(() => true, () => false)) stalled.push(`client ${i}`);
    // Each stall costs a full stressWait: stop at five so a broken build fails inside the step's time limit and still logs.
    if (stalled.length >= 5) break;
  }
  log(`step 6: navigation stress: ${loads - stalled.length}/${loads} in-app loads answered their first bridge call${stalled.length ? `; stalled: ${stalled.join(', ')}` : ''}`);
  if (stalled.length) throw new Error(`navigation stress: ${stalled.length}/${loads} loads never got their first bridge answer (${stalled.join(', ')})`);

  if (violations.length) throw new Error(`CSP violations:\n${violations.join('\n')}`);
  log('no CSP violations in the console');
}

let failed = false;
try { execFileSync('mkdir', ['-p', OUT]); await main(); log('PASS'); }
catch (error) {
  failed = true; console.error(error);
  try { writeDiagnostics(); } catch (e) { console.error(`diagnostics: ${e}`); }
  // Diagnostics for a failed run, before the emulator goes away.
  try { writeFileSync(join(OUT, 'logcat.txt'), execFileSync(ADB, ['logcat', '-d', '-t', '3000'], { env, maxBuffer: 64 << 20 })); } catch { /* best effort */ }
  try { writeFileSync(join(OUT, 'failure.png'), execFileSync(ADB, ['exec-out', 'screencap', '-p'], { env, maxBuffer: 64 << 20 })); } catch { /* best effort */ }
}
finally {
  for (const cleanup of cleanups.reverse()) { try { await cleanup(); } catch { /* best effort */ } }
  rmSync(work, { recursive: true, force: true });
}
process.exit(failed ? 1 : 0);
