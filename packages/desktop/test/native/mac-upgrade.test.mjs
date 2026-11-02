import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { spawn, spawnSync, execFileSync } from 'node:child_process';
import { createHash, createPrivateKey, sign } from 'node:crypto';
import { mkdtemp, readFile, writeFile, mkdir, rm, rename, open } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { resolve, join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { build } from 'esbuild';
import { createReleaseKeys } from '../../scripts/release-keys.mjs';
import { appcast, signedAppcast } from '../../scripts/release-feed.mjs';
import { bundleDigest, serveArchive } from '../support/mac-installed-upgrade.ts';

const desktop = resolve(import.meta.dirname, '../..'), repo = resolve(desktop, '../..');
const origin = 'http://127.0.0.1:18849', sentinel = 'preserve-device-storage';
const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');
const versionOf = app => execFileSync('/usr/libexec/PlistBuddy', ['-c', 'Print :CFBundleVersion', join(app, 'Contents/Info.plist')], { encoding: 'utf8' }).trim();
const verifyApp = app => execFileSync('codesign', ['--verify', '--deep', '--strict', app], { stdio: 'inherit' });

// Only the fixture ASAR reads these commands. Production SparkleInstaller and
// native bridge logic still perform preparation, cancelled quit and will-quit
// installation; no runtime URL/key override is added to the shipping app.
const fixtureMain = String.raw`
const { app } = require('electron');
const fs = require('node:fs');
const path = require('node:path');
const cfg = require('./settings.json');
const { loadSparkle } = require('./sparkle.cjs');
app.setPath('userData', cfg.profile);
let timer, lastId, busy = false;
function publish(id, stage, extra = {}) {
 const file = path.join(cfg.reports, id + '.json');
 fs.writeFileSync(file + '.tmp', JSON.stringify({ stage, version: app.getVersion(), pid: process.pid, sentinel: fs.readFileSync(path.join(cfg.profile, 'sentinel'), 'utf8'), ...extra }));
 fs.renameSync(file + '.tmp', file);
}
app.on('will-quit', () => {
 clearInterval(timer);
 if (app.getVersion() === '1.0.0') publish('will-quit', 'will-quit', { commandId: lastId });
});
app.whenReady().then(() => {
 if (app.getVersion() === '1.0.1') { publish('upgraded', 'upgraded'); app.quit(); return; }
 const installer = loadSparkle(process.resourcesPath, app);
 publish('startup', 'running');
 timer = setInterval(async () => {
  if (busy) return;
  let command;
  try { command = JSON.parse(fs.readFileSync(cfg.command, 'utf8')); }
  catch (error) { if (error.code === 'ENOENT') return; throw error; }
  if (command.id === lastId) return;
  lastId = command.id; busy = true;
  try {
   if (command.action === 'prepare') {
    await installer.prepare({ version: '1.0.1' }, { url: cfg.url, sparkleSignature: cfg.signature });
    publish(command.id, 'ready');
   } else if (command.action === 'ping') publish(command.id, 'running');
   else if (command.action === 'cancel-quit') {
    installer.install();
    app.once('before-quit', event => { event.preventDefault(); installer.cancelledQuit(); });
    app.quit();
    publish(command.id, 'quit-cancelled');
   } else if (command.action === 'quit') { installer.install(); app.quit(); }
   else throw new Error('Unknown fixture command');
  } catch (error) { publish(command.id, 'rejected', { error: String(error) }); }
  finally { busy = false; }
 }, 50);
}).catch(error => { publish('fatal', 'fatal', { error: String(error) }); app.exit(1); });
`;

test('installed ad-hoc macOS Sparkle rejects feed/archive/download faults, safely retries each and commits only on quit', { skip: process.platform !== 'darwin', timeout: 900000 }, async t => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-sparkle-installed-'));
 const profile = join(root, 'profile'), reports = join(root, 'reports'), command = join(root, 'command.json');
 const logs = join(root, 'process.log'), log = await open(logs, 'a');
 const native = join(desktop, 'native-bin/dilla-sparkle.node');
 let originalDigest, child, upgradedPid, server, launchError, sequence = 0, phase = 'setup', fault = 'none';
 let requests = { feed: 0, archive: 0 };
 const outcomes = {};
 const enter = label => { phase = label; t.diagnostic(label); };
 const waitReport = async (id, allowExit = false, failureId) => {
  const deadline = Date.now() + 180000;
  while (Date.now() < deadline) {
   try { return JSON.parse(await readFile(join(reports, id + '.json'), 'utf8')); }
   catch (error) { if (error.code !== 'ENOENT') throw error; }
   for (const terminal of ['fatal', ...(failureId ? [failureId] : [])]) {
    let failure;
    try { failure = JSON.parse(await readFile(join(reports, terminal + '.json'), 'utf8')); }
    catch (error) { if (error.code !== 'ENOENT') throw error; }
    if (failure) throw new Error(`${phase}: fixture failed: ${JSON.stringify(failure)}`);
   }
   if (launchError) throw launchError;
   if (!allowExit && child && (child.exitCode !== null || child.signalCode !== null)) throw new Error(`${phase}: installed fixture exited before ${id}; code ${child.exitCode}, signal ${child.signalCode}`);
   await delay(100);
  }
  throw new Error(`${phase}: no ${id} report within 180 seconds`);
 };
 const send = async (action, expectReply = true) => {
  const id = String(++sequence);
  await writeFile(command + '.tmp', JSON.stringify({ id, action })); await rename(command + '.tmp', command);
  return expectReply ? waitReport(id) : id;
 };
 try {
  originalDigest = sha256(await readFile(native));
  await createReleaseKeys(join(root, 'keys'));
  const trust = JSON.parse(await readFile(join(root, 'keys/release-trust.json'), 'utf8'));
  const seed = Buffer.from((await readFile(join(root, 'keys/sparkle-private.key'), 'utf8')).trim(), 'base64');
  const key = createPrivateKey({ format: 'jwk', key: { kty: 'OKP', crv: 'Ed25519', d: seed.toString('base64url'), x: Buffer.from(trust.sparklePublicKey, 'base64').toString('base64url') } }); seed.fill(0);
  execFileSync(process.execPath, [join(desktop, 'scripts/build-sparkle.mjs'), '--acceptance'], { stdio: 'inherit' });
  assert.equal(sha256(await readFile(native)), originalDigest, 'acceptance build overwrote production bridge');
  const pack = async (version, signature = '') => {
   const project = join(root, 'project-' + version), output = join(root, 'out-' + version);
   await mkdir(project, { recursive: true });
   await writeFile(join(project, 'package.json'), JSON.stringify({ name: 'dilla-update-acceptance', version, main: 'main.cjs', description: 'Dilla installed Sparkle acceptance fixture', author: 'Dilla', license: 'AGPL-3.0-or-later' }));
   await writeFile(join(project, 'main.cjs'), fixtureMain);
   await build({ entryPoints: [join(desktop, 'src/sparkle.ts')], outfile: join(project, 'sparkle.cjs'), bundle: true, platform: 'node', target: 'node24', format: 'cjs' });
   await writeFile(join(project, 'settings.json'), JSON.stringify({ profile, reports, command, url: origin + '/DillaUpdateAcceptance.zip', signature }));
   const config = { appId: 'dev.dilla.desktop.update-acceptance', productName: 'DillaUpdateAcceptance', executableName: 'DillaUpdateAcceptance', electronVersion: '45.2.1', electronDist: join(repo, 'node_modules/electron/dist'), directories: { output }, files: ['main.cjs', 'sparkle.cjs', 'settings.json', 'package.json'], asar: true, extraResources: [{ from: join(desktop, 'acceptance-bin/dilla-sparkle.node'), to: 'native/dilla-sparkle.node' }], mac: { target: 'dir', identity: '-', notarize: false, extraFiles: [{ from: join(desktop, 'native-bin/Sparkle.framework'), to: 'Frameworks/Sparkle.framework' }], extendInfo: { CFBundleVersion: version, SUFeedURL: origin + '/appcast.xml', SUPublicEDKey: trust.sparklePublicKey, SURequireSignedFeed: true, SUVerifyUpdateBeforeExtraction: true, SUEnableAutomaticChecks: false, SUAutomaticallyUpdate: false, SUEnableInstallerLauncherService: true, SUEnableDownloaderService: false, NSAppTransportSecurity: { NSAllowsLocalNetworking: true } } } };
   await writeFile(join(project, 'builder.json'), JSON.stringify(config));
   execFileSync(process.execPath, [join(repo, 'node_modules/electron-builder/cli.js'), '--projectDir', project, '--config', join(project, 'builder.json'), '--mac', '--' + process.arch, '--dir', '--publish', 'never'], { cwd: repo, stdio: 'inherit', env: { ...process.env, CSC_FOR_PULL_REQUEST: 'true' } });
   const app = join(output, process.arch === 'arm64' ? 'mac-arm64' : 'mac', 'DillaUpdateAcceptance.app');
   verifyApp(app);
   const signatureInfo = spawnSync('codesign', ['-d', '-v', app], { encoding: 'utf8' });
   assert.equal(signatureInfo.status, 0); assert.match(signatureInfo.stderr, /Signature=adhoc/);
   return app;
  };
  enter('package the two reusable ad-hoc fixtures');
  const next = await pack('1.0.1');
  const archive = join(root, 'DillaUpdateAcceptance.zip');
  execFileSync('ditto', ['-c', '-k', '--sequesterRsrc', '--keepParent', next, archive]);
  const archiveBytes = await readFile(archive), signature = sign(null, archiveBytes, key).toString('base64');
  const previous = await pack('1.0.0', signature);
  const installed = join(root, 'Applications/DillaUpdateAcceptance.app'); await mkdir(join(root, 'Applications'));
  await mkdir(profile); await writeFile(join(profile, 'sentinel'), sentinel);
  const release = { version: '1.0.1', channel: 'stable', publishedAt: new Date().toISOString() };
  const artifact = { url: origin + '/DillaUpdateAcceptance.zip', size: archiveBytes.length };
  const feed = signedAppcast(appcast(release, artifact, signature), key);
  server = createServer((req, res) => {
   if (req.url === '/appcast.xml') {
    requests.feed++;
    const content = fault === 'feed' ? Buffer.from(feed.toString().replace('Dilla desktop stable', 'Dilla desktop forged')) : feed;
    res.writeHead(200, { 'Content-Type': 'application/xml', 'Content-Length': content.length, 'Cache-Control': 'no-store' }); res.end(content);
   } else if (req.url === '/DillaUpdateAcceptance.zip') { requests.archive++; serveArchive(res, archiveBytes, fault === 'feed' ? 'none' : fault); }
   else { res.writeHead(404); res.end(); }
  });
  await new Promise((done, reject) => { server.once('error', reject); server.listen(18849, '127.0.0.1', done); });
  // A completed installation consumes Sparkle's prepared update. Restoring the
  // same old fixture for the next scenario prevents a cached valid preparation
  // from hiding a later archive/download fault, without packaging again.
  for (const kind of ['feed', 'archive', 'download']) {
   enter(kind + ': install and launch the working old fixture');
   await rm(installed, { recursive: true, force: true }); execFileSync('ditto', [previous, installed]);
   await rm(command, { force: true }); await rm(reports, { recursive: true, force: true }); await mkdir(reports);
   const oldDigest = await bundleDigest(installed);
   launchError = undefined; fault = kind; requests = { feed: 0, archive: 0 };
   child = spawn(join(installed, 'Contents/MacOS/DillaUpdateAcceptance'), [], { stdio: ['ignore', log.fd, log.fd] });
   child.once('error', error => { launchError = error; });
   const startup = await waitReport('startup'); assert.equal(startup.stage, 'running'); assert.equal(startup.version, '1.0.0'); assert.equal(startup.pid, child.pid);
   const assertOldWorking = async () => {
    const running = await send('ping');
    assert.equal(running.stage, 'running'); assert.equal(running.version, '1.0.0'); assert.equal(running.pid, startup.pid); assert.equal(running.sentinel, sentinel);
    assert.equal(child.exitCode, null); assert.equal(child.signalCode, null);
    assert.equal(versionOf(installed), '1.0.0'); assert.equal(await bundleDigest(installed), oldDigest, 'failed or uncommitted update changed the installed app');
    assert.equal(await readFile(join(profile, 'sentinel'), 'utf8'), sentinel);
    await assert.rejects(readFile(join(reports, 'will-quit.json')), { code: 'ENOENT' });
   };
   enter(kind + ': reject the injected fault without changing the live installation');
   const rejected = await send('prepare'); assert.equal(rejected.stage, 'rejected', JSON.stringify(rejected)); assert.ok(rejected.error);
   assert.ok(requests.feed > 0, 'Sparkle did not fetch the signed feed');
   if (kind === 'feed') assert.equal(requests.archive, 0, 'untrusted feed reached archive download');
   else assert.ok(requests.archive > 0, 'archive fault was not exercised');
   const failedRequests = { ...requests };
   outcomes[kind] = { rejection: rejected, failedRequests, oldBundleSha256: oldDigest };
   await assertOldWorking(); verifyApp(installed);
   enter(kind + ': explicitly retry in the same installed process with valid signed bytes');
   fault = 'none';
   const ready = await send('prepare'); assert.equal(ready.stage, 'ready', JSON.stringify(ready)); assert.equal(ready.pid, startup.pid);
   outcomes[kind].retry = ready;
   assert.ok(requests.archive > failedRequests.archive, 'retry did not fetch the valid archive');
   await assertOldWorking();
   if (kind === 'feed') {
    enter('feed: a cancelled Electron quit disarms the native installation');
    const cancelled = await send('cancel-quit'); assert.equal(cancelled.stage, 'quit-cancelled', JSON.stringify(cancelled));
    outcomes[kind].cancelledQuit = cancelled;
    await assertOldWorking();
    const preparedAgain = await send('prepare'); assert.equal(preparedAgain.stage, 'ready', JSON.stringify(preparedAgain));
    outcomes[kind].preparedAgain = preparedAgain;
    await assertOldWorking();
   }
   enter(kind + ': commit through the real will-quit bridge and relaunch the upgraded app');
   const quitId = await send('quit', false);
   const upgraded = await waitReport('upgraded', true, quitId), quit = await waitReport('will-quit', true, quitId);
   assert.equal(quit.commandId, quitId); assert.equal(quit.pid, startup.pid); assert.equal(quit.version, '1.0.0');
   assert.equal(upgraded.stage, 'upgraded'); assert.equal(upgraded.version, '1.0.1'); assert.equal(upgraded.sentinel, sentinel); assert.notEqual(upgraded.pid, startup.pid);
   assert.ok(Number.isSafeInteger(upgraded.pid) && upgraded.pid > 0); upgradedPid = upgraded.pid;
   assert.equal(versionOf(installed), '1.0.1'); assert.notEqual(await bundleDigest(installed), oldDigest);
   verifyApp(installed);
   // Both fixture versions quit themselves; wait before restoring their bundle.
   const deadline = Date.now() + 10000;
   while (Date.now() < deadline && (child.exitCode === null && child.signalCode === null || processAlive(upgraded.pid))) await delay(100);
   assert.ok(child.exitCode !== null || child.signalCode !== null, 'old app stayed alive after commit');
   assert.equal(processAlive(upgraded.pid), false, 'upgraded fixture did not exit');
   upgradedPid = undefined;
   Object.assign(outcomes[kind], { quit, upgrade: upgraded, preservedProfileSentinel: true });
  }
  assert.equal(sha256(await readFile(native)), originalDigest, 'production bridge changed during acceptance');
  await mkdir(join(desktop, 'test-results'), { recursive: true });
  await writeFile(join(desktop, 'test-results/mac-installed-upgrade.json'), JSON.stringify({ platform: process.platform, arch: process.arch, scenarios: outcomes, from: '1.0.0', to: '1.0.1', signing: 'ad-hoc', productionBridgeUnchanged: true, cancelledQuitVerified: true, profileEvidence: 'plain sentinel only' }, null, 2));
 } catch (error) {
  await mkdir(join(desktop, 'test-results'), { recursive: true });
  await writeFile(join(desktop, 'test-results/mac-installed-upgrade-error.json'), JSON.stringify({ platform: process.platform, arch: process.arch, phase, error: String(error), requests, outcomes, log: (await readFile(logs, 'utf8')).slice(-12000) }, null, 2));
  throw error;
 } finally {
  child?.kill(); if (upgradedPid && processAlive(upgradedPid)) process.kill(upgradedPid, 'SIGTERM');
  if (server) { server.closeAllConnections(); await new Promise(done => server.close(done)); }
  await log.close(); await rm(root, { recursive: true, force: true, maxRetries: 20, retryDelay: 250 });
 }
});

function processAlive(pid) {
 try { process.kill(pid, 0); return true; }
 catch (error) { if (error.code === 'ESRCH') return false; throw error; }
}
