import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { spawn, spawnSync, execFileSync } from 'node:child_process';
import { createHash, createPrivateKey, sign } from 'node:crypto';
import { mkdtemp, readFile, writeFile, mkdir, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { resolve, join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { createReleaseKeys } from '../../scripts/release-keys.mjs';
import { appcast, signedAppcast } from '../../scripts/release-feed.mjs';

const desktop = resolve(import.meta.dirname, '../..'), repo = resolve(desktop, '../..');
const origin = 'http://127.0.0.1:18849';
test('an installed ad-hoc macOS app rejects a tampered signed feed and upgrades through Sparkle without commercial signing', { skip: process.platform !== 'darwin', timeout: 480000 }, async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-sparkle-installed-'));
 const report = join(root, 'report.json'), profile = join(root, 'profile');
 let child, server;
 const native = join(desktop, 'native-bin/dilla-sparkle.node');
 const originalDigest = createHash('sha256').update(await readFile(native)).digest('hex');
 try {
  await createReleaseKeys(join(root, 'keys'));
  const trust = JSON.parse(await readFile(join(root, 'keys/release-trust.json'), 'utf8'));
  const seed = Buffer.from((await readFile(join(root, 'keys/sparkle-private.key'), 'utf8')).trim(), 'base64');
  const key = createPrivateKey({ format: 'jwk', key: { kty: 'OKP', crv: 'Ed25519', d: seed.toString('base64url'), x: Buffer.from(trust.sparklePublicKey, 'base64').toString('base64url') } }); seed.fill(0);
  execFileSync(process.execPath, [join(desktop, 'scripts/build-sparkle.mjs'), '--acceptance'], { stdio: 'inherit' });
  assert.equal(createHash('sha256').update(await readFile(native)).digest('hex'), originalDigest, 'acceptance build overwrote production bridge');
  const main = `const {app}=require('electron');const fs=require('node:fs');const path=require('node:path');const cfg=require('./settings.json');app.setPath('userData',cfg.profile);app.whenReady().then(async()=>{if(app.getVersion()==='1.0.1'){fs.writeFileSync(cfg.report,JSON.stringify({stage:'upgraded',sentinel:fs.readFileSync(path.join(cfg.profile,'sentinel'),'utf8')}));app.quit();return;}try{const bridge=require(path.join(process.resourcesPath,'native/dilla-sparkle.node'));await bridge.prepare('1.0.1',cfg.url,cfg.signature);fs.writeFileSync(cfg.report,JSON.stringify({stage:'ready'}));app.once('will-quit',()=>bridge.commit());app.quit();}catch(error){fs.writeFileSync(cfg.report,JSON.stringify({stage:'rejected',error:String(error)}));app.quit();}});`;
  const pack = async (version, signature = '') => {
   const project = join(root, 'project-' + version), output = join(root, 'out-' + version);
   await mkdir(project, { recursive: true });
   await writeFile(join(project, 'package.json'), JSON.stringify({ name: 'dilla-update-acceptance', version, main: 'main.cjs', description: 'Dilla installed Sparkle acceptance fixture', author: 'Dilla', license: 'AGPL-3.0-or-later' }));
   await writeFile(join(project, 'main.cjs'), main);
   await writeFile(join(project, 'settings.json'), JSON.stringify({ profile, report, url: origin + '/DillaUpdateAcceptance.zip', signature }));
   const config = { appId: 'dev.dilla.desktop.update-acceptance', productName: 'DillaUpdateAcceptance', executableName: 'DillaUpdateAcceptance', electronVersion: '45.2.1', electronDist: join(repo, 'node_modules/electron/dist'), directories: { output }, files: ['main.cjs', 'settings.json', 'package.json'], asar: true, extraResources: [{ from: join(desktop, 'acceptance-bin/dilla-sparkle.node'), to: 'native/dilla-sparkle.node' }], mac: { target: 'dir', identity: '-', notarize: false, extraFiles: [{ from: join(desktop, 'native-bin/Sparkle.framework'), to: 'Frameworks/Sparkle.framework' }], extendInfo: { CFBundleVersion: version, SUFeedURL: origin + '/appcast.xml', SUPublicEDKey: trust.sparklePublicKey, SURequireSignedFeed: true, SUVerifyUpdateBeforeExtraction: true, SUEnableAutomaticChecks: false, SUAutomaticallyUpdate: false, SUEnableInstallerLauncherService: true, SUEnableDownloaderService: false, NSAppTransportSecurity: { NSAllowsLocalNetworking: true } } } };
   await writeFile(join(project, 'builder.json'), JSON.stringify(config));
   execFileSync(process.execPath, [join(repo, 'node_modules/electron-builder/cli.js'), '--projectDir', project, '--config', join(project, 'builder.json'), '--mac', '--' + process.arch, '--dir', '--publish', 'never'], { cwd: repo, stdio: 'inherit', env: { ...process.env, CSC_FOR_PULL_REQUEST: 'true' } });
   const app = join(output, process.arch === 'arm64' ? 'mac-arm64' : 'mac', 'DillaUpdateAcceptance.app');
   execFileSync('codesign', ['--verify', '--deep', '--strict', app], { stdio: 'inherit' });
   const signatureInfo = spawnSync('codesign', ['-d', '-v', app], { encoding: 'utf8' });
   assert.equal(signatureInfo.status, 0); assert.match(signatureInfo.stderr, /Signature=adhoc/);
   return app;
  };
  const next = await pack('1.0.1');
  const archive = join(root, 'DillaUpdateAcceptance.zip');
  execFileSync('ditto', ['-c', '-k', '--sequesterRsrc', '--keepParent', next, archive]);
  const archiveBytes = await readFile(archive), signature = sign(null, archiveBytes, key).toString('base64');
  const previous = await pack('1.0.0', signature);
  const installed = join(root, 'Applications/DillaUpdateAcceptance.app'); await mkdir(join(root, 'Applications'));
  execFileSync('ditto', [previous, installed]); await mkdir(profile, { recursive: true }); await writeFile(join(profile, 'sentinel'), 'preserve-device-storage');
  const release = { version: '1.0.1', channel: 'stable', publishedAt: new Date().toISOString() };
  const artifact = { url: origin + '/DillaUpdateAcceptance.zip', size: archiveBytes.length };
  const feed = signedAppcast(appcast(release, artifact, signature), key);
  let tampered = true;
  server = createServer((req, res) => {
   if (req.url === '/appcast.xml') {
    const content = tampered ? Buffer.from(feed.toString().replace('Dilla desktop stable', 'Dilla desktop forged')) : feed;
    res.writeHead(200, { 'Content-Type': 'application/xml', 'Content-Length': content.length }); res.end(content);
   } else if (req.url === '/DillaUpdateAcceptance.zip') { res.writeHead(200, { 'Content-Length': archiveBytes.length }); res.end(archiveBytes); }
   else { res.writeHead(404); res.end(); }
  });
  await new Promise((done, reject) => { server.once('error', reject); server.listen(18849, '127.0.0.1', done); });
  const launch = async () => {
   await rm(report, { force: true });
   child = spawn(join(installed, 'Contents/MacOS/DillaUpdateAcceptance'), [], { stdio: 'inherit' });
   let launchError; child.once('error', error => { launchError = error; });
   let handedOff = false;
   const deadline = Date.now() + 180000;
   while (Date.now() < deadline) {
    try { const value = JSON.parse(await readFile(report, 'utf8')); if (value.stage !== 'ready') return value; handedOff = true; } catch {}
    if (launchError) throw launchError;
    if (child.exitCode !== null && !handedOff) throw new Error('Installed fixture exited before completing upgrade; code ' + child.exitCode);
    await delay(200);
   }
   throw new Error('Installed Sparkle upgrade did not finish within 180 seconds');
  };
  const rejected = await launch(); assert.equal(rejected.stage, 'rejected', JSON.stringify(rejected));
  await delay(1000);
  assert.equal(execFileSync('/usr/libexec/PlistBuddy', ['-c', 'Print :CFBundleVersion', join(installed, 'Contents/Info.plist')], { encoding: 'utf8' }).trim(), '1.0.0');
  tampered = false;
  const upgraded = await launch(); assert.deepEqual(upgraded, { stage: 'upgraded', sentinel: 'preserve-device-storage' });
  assert.equal(execFileSync('/usr/libexec/PlistBuddy', ['-c', 'Print :CFBundleVersion', join(installed, 'Contents/Info.plist')], { encoding: 'utf8' }).trim(), '1.0.1');
  execFileSync('codesign', ['--verify', '--deep', '--strict', installed], { stdio: 'inherit' });
  await mkdir(join(desktop, 'test-results'), { recursive: true });
  await writeFile(join(desktop, 'test-results/mac-installed-upgrade.json'), JSON.stringify({ platform: process.platform, arch: process.arch, tamperedFeed: rejected, upgrade: upgraded, from: '1.0.0', to: '1.0.1', signing: 'ad-hoc', productionBridgeUnchanged: true }, null, 2));
 } catch (error) {
  await mkdir(join(desktop, 'test-results'), { recursive: true });
  await writeFile(join(desktop, 'test-results/mac-installed-upgrade-error.json'), JSON.stringify({ platform: process.platform, arch: process.arch, error: String(error) }, null, 2));
  throw error;
 } finally {
  child?.kill(); if (server) await new Promise(done => server.close(done));
  await rm(root, { recursive: true, force: true, maxRetries: 20, retryDelay: 250 });
 }
});
