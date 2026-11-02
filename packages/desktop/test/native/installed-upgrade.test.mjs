import './windows-installed-upgrade.test.mjs';
import desktopBuilder from '../../electron-builder.cjs';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import * as requireFS from 'node:fs';
import { createServer } from 'node:http';
import { spawn, execFileSync, spawnSync } from 'node:child_process';
import { createHash, generateKeyPairSync, sign, randomBytes } from 'node:crypto';
import { mkdtemp, mkdir, readFile, writeFile, rm, cp, symlink, chmod, open, readdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { resolve, join, dirname } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { build } from 'esbuild';
import { snapshotUninstall } from '../../src/update-registry.ts';
const desktop = resolve(import.meta.dirname, '../..'), repo = resolve(desktop, '../..');
const platform = process.platform;
const digest = bytes => createHash('sha512').update(bytes).digest('base64');
async function until(read, label, timeout = 120000) {
 const end = Date.now() + timeout;
 let last;
 while (Date.now() < end) {
  try { const value = await read(); if (value) return value; } catch (error) { if (error.terminal) throw error; last = error; }
  await delay(100);
 }
 throw new Error(`${label} timed out${last ? ': ' + last : ''}`);
}
async function json(file) { return JSON.parse(await readFile(file, 'utf8')); }
function alive(pid) { try { if (platform === 'linux') { const stat = requireFS.readFileSync(`/proc/${pid}/stat`, 'utf8'); if (['Z', 'X'].includes(stat.slice(stat.lastIndexOf(')') + 2)[0])) return false; } process.kill(pid, 0); return true; } catch (error) { if (error.code === 'ESRCH' || error.code === 'ENOENT') return false; throw error; } }
async function stop(pid) {
 if (!pid || !alive(pid)) return;
 if (platform === 'win32') execFileSync('taskkill', ['/PID', String(pid), '/T', '/F'], { stdio: 'ignore' });
 else {
  process.kill(pid, 'SIGTERM');
  const grace = Date.now() + 2000;
  while (alive(pid) && Date.now() < grace) await delay(50);
  if (alive(pid)) process.kill(pid, 'SIGKILL');
 }
 await until(async () => !alive(pid), 'owned fixture exit', 10000);
}

async function stopOnly(pid) {
 if (!alive(pid)) throw new Error('Expected owned fixture process already exited');
 process.kill(pid, 'SIGKILL');
 await until(async () => !alive(pid), 'owned helper-only exit', 10000);
}

// Compile a separate acceptance application. Fixture keys, fixed loopback
// transport and test orchestration are immutable in these fixture ASARs only;
// production keys, GitHub URL policy, fuses and OS protections are unchanged.
test('installed AppImage/NSIS authenticated upgrade observes real startup health and restores a failed next app', { skip: platform !== 'linux', timeout: 900000 }, async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-installed-update-'));
 const id = randomBytes(6).toString('hex'), product = 'DillaUpdateAcceptance' + id;
 const profile = join(root, 'profile'), reports = join(root, 'reports'), logs = join(root, 'process.log');
 const keys = generateKeyPairSync('ed25519'), key = keys.publicKey.export({ type: 'spki', format: 'pem' }).toString();
 const productionKeyHash = digest(await readFile(join(desktop, 'src/update-keys.ts')));
 const log = await open(logs, 'a');
 let server, current, installerBytes, descriptor, signature, pid, launched;
 let fault = 'none';
 const outcomes = {};
 try {
  await mkdir(profile); await mkdir(reports); await writeFile(join(profile, 'device-sentinel'), 'encrypted-device-data-survives');
  await writeFile(join(profile, 'desktop.json'), JSON.stringify({ version: 1, instances: ['https://desktop-update-offline.invalid/'], autostart: false }));
  server = createServer((req, res) => {
   if (req.url === '/release.json') {
    const bytes = fault === 'metadata' ? Buffer.from(descriptor.toString().replace('"format":1', '"format":2')) : descriptor;
    res.writeHead(200, { 'content-length': bytes.length }); res.end(bytes);
   } else if (req.url === '/release.json.sig') res.end(signature);
   else if (req.url === '/artifact') {
    if (fault === 'download') { res.writeHead(200, { 'content-length': installerBytes.length }); res.write(installerBytes.subarray(0, 1024)); res.destroy(); }
    else {
     const bytes = fault === 'artifact' ? Buffer.from(installerBytes) : installerBytes;
     if (fault === 'artifact') bytes[Math.floor(bytes.length / 2)] ^= 1;
     res.writeHead(200, { 'content-length': bytes.length }); res.end(bytes);
    }
   } else { res.writeHead(404); res.end(); }
  });
  await new Promise((done, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', done); });
  const origin = `http://127.0.0.1:${server.address().port}`;
  const sourceMain = await readFile(join(desktop, 'src/main.ts'), 'utf8'), sourceUpdater = await readFile(join(desktop, 'src/updater.ts'), 'utf8'), sourceRuntime = await readFile(join(desktop, 'src/update-runtime.ts'), 'utf8'), sourceInstaller = await readFile(join(desktop, 'src/update-installer.ts'), 'utf8'), sourceWatchdog = await readFile(join(desktop, 'src/update-watchdog.ts'), 'utf8'), sourceJournal = await readFile(join(desktop, 'src/update-journal.ts'), 'utf8'), sourceState = await readFile(join(desktop, 'src/update-state.ts'), 'utf8');
  const pack = async version => {
   const project = join(root, 'project-' + version), output = join(root, 'out-' + version), dist = join(project, 'dist');
   await mkdir(dist, { recursive: true });
   await cp(join(desktop, 'dist'), dist, { recursive: true });
   // The installed fixture must pair the current bootstrap with its current
   // renderer/preload, including the content-free durable activation wait.
   await rm(join(dist, 'client'), { recursive: true, force: true });
   await cp(join(repo, 'packages/web/dist'), join(dist, 'client'), { recursive: true });
   await build({ entryPoints: [join(desktop, 'src/preload.ts')], outfile: join(dist, 'preload.cjs'), bundle: true,
    platform: 'node', target: 'es2022', format: 'cjs', external: ['electron'] });
   for (const boundary of ['try { await startupGate.wait(win, abort.signal); owner(event); }', 'const forwarded = target === request.url ? request : new Request(target, request);']) {
    assert.equal(sourceMain.split(boundary).length, 2, 'The native activation observation must instrument the actual production boundary');
   }
   await writeFile(join(project, 'package.json'), JSON.stringify({ name: 'dilla-update-acceptance-' + id, version, main: 'dist/main.cjs', description: 'Installed updater acceptance fixture', author: 'Dilla', license: 'AGPL-3.0-or-later' }));
   await build({ entryPoints: [join(desktop, 'src/main.ts')], outfile: join(dist, 'main.cjs'), define: platform === 'linux' ? { __DILLA_LINUX_INSTALL_KIND__: JSON.stringify('appimage') } : {}, bundle: true, platform: 'node', target: 'node24', format: 'cjs', external: ['electron'], plugins: [{ name: 'compiled-fixture-only', setup(plugin) {
    plugin.onLoad({ filter: /\/src\/update-keys\.ts$|\\src\\update-keys\.ts$/ }, () => ({ contents: `export const RELEASE_PUBLIC_KEYS = ${JSON.stringify([key])};`, loader: 'ts' }));
    plugin.onLoad({ filter: /\/src\/updater\.ts$|\\src\\updater\.ts$/ }, () => ({ contents: ('import { BrowserWindow } from "electron";\n' + sourceUpdater).replace(/const feed = `[^`]+`;/, `const feed = ${JSON.stringify(origin + '/release.json')};`).replace('url: new URL(this.artifact.url)', `url: new URL(${JSON.stringify(origin + '/artifact')})`).replace('const watchdog = await launchUpdateWatchdog(plan);', `const watchdog = await launchUpdateWatchdog(plan); if(plan.health.version==='1.0.11'){app.emit('activate');await new Promise(done=>setTimeout(done,100));process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports, 'accepted-quit-windows.json'))},JSON.stringify({pid:process.pid,windows:BrowserWindow.getAllWindows().length}));}`), loader: 'ts', resolveDir: join(desktop, 'src') }));
    plugin.onLoad({ filter: /\/src\/update-runtime\.ts$|\\src\\update-runtime\.ts$/ }, () => ({ contents: sourceRuntime.replaceAll("stdio: 'ignore'", "stdio: 'inherit'").replace('await recoverInstalledUpdate(plan, processes); return;', `if(plan.health.version==='1.0.11'){process.getBuiltinModule('fs').appendFileSync(${JSON.stringify(join(reports,'prepared-recovery-owners.jsonl'))},JSON.stringify({pid:process.pid})+'\\n');await new Promise(done=>setTimeout(done,5000));} await recoverInstalledUpdate(plan, processes); return;`), loader: 'ts', resolveDir: join(desktop, 'src') }));
    plugin.onLoad({ filter: /\/src\/update-watchdog\.ts$|\\src\\update-watchdog\.ts$/ }, () => ({ contents: sourceWatchdog.replace('child = await processes.launch(file, plan.health);', "child = await processes.launch(file, plan.health); if (plan.health.version === '1.0.7') await new Promise(done => setTimeout(done, 10000));"), loader: 'ts', resolveDir: join(desktop, 'src') }));
    plugin.onLoad({ filter: /\/src\/update-journal\.ts$|\\src\\update-journal\.ts$/ }, () => ({ contents: sourceJournal.replace('await rename(temporary, file); await syncUpdateDirectory(plan.health.directory);', `await rename(temporary, file); if (value.phase === 'complete' && plan.health.version === '1.0.9') { await updateFS.promises.writeFile(${JSON.stringify(join(reports, 'complete-visible.json'))}, JSON.stringify({...value,writerPid:process.pid})); await new Promise(done => setTimeout(done, 15000)); if (await updateFS.promises.readFile(${JSON.stringify(join(reports, 'complete-fault'))}, 'utf8') === 'fail') throw Object.assign(new Error('fixture directory fsync ENOSPC'), {code:'ENOSPC'}); } await syncUpdateDirectory(plan.health.directory);`), loader: 'ts', resolveDir: join(desktop, 'src') }));
    plugin.onLoad({ filter: /\/src\/update-state\.ts$|\\src\\update-state\.ts$/ }, () => ({ contents: sourceState.replace('await rename(temporary, file); await syncUpdateDirectory(directory);', `await rename(temporary, file); if(plan.health.version==='1.0.11'){await updateFS.promises.writeFile(${JSON.stringify(join(reports, 'pointer-visible-before-arm.json'))},JSON.stringify({nonce:plan.health.nonce,parentPid:plan.parentPid}));await new Promise(done=>setTimeout(done,10000));throw Object.assign(new Error('fixture active publication fsync ENOSPC'),{code:'ENOSPC'});} await syncUpdateDirectory(directory);`), loader:'ts',resolveDir:join(desktop,'src') }));
    // Exercise the identical kernel timeout/drain path promptly. Only the
    // separately compiled fixture changes its wait duration; shipping code is 5m.
    plugin.onLoad({ filter: /\/src\/update-installer\.ts$|\\src\\update-installer\.ts$/ }, () => ({ contents: sourceInstaller.replace('false,300000)', 'false,4000)'), loader: 'ts', resolveDir: join(desktop, 'src') }));
    plugin.onLoad({ filter: /\/src\/main\.ts$|\\src\\main\.ts$/ }, () => ({ contents: sourceMain
     .replaceAll("app.setName('Dilla');", `app.setName(${JSON.stringify(product)}); app.setPath('userData', ${JSON.stringify(profile)});`)
     .replace('if (!startupLaunch.nonce) return;', `if (!startupLaunch.nonce) return; ${version === '1.0.3' ? 'await new Promise(done => setTimeout(done, 10000));' : ''}`)
     .replace("dialog.showErrorBox('Dilla update failed', previousUpdateFailure);", "if (process.argv.includes('--fixture-user-retry')) await updater.check(); else dialog.showErrorBox('Dilla update failed', previousUpdateFailure);")
     .replace('const updater = createUpdater(state => {', `const updater = createUpdater(state => { process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports, version + '.json'))}, JSON.stringify({ version: app.getVersion(), pid: process.pid, state, inheritedHealth: process.env.DILLA_UPDATE_STARTUP_NONCE ?? null })); if(state.phase==='ready') setTimeout(() => { void requestQuit(true); }, 200);`)
     .replace('try { await startupGate.wait(win, abort.signal); owner(event); }', `process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports, version + '-startup-wait.json'))}, JSON.stringify({pid:process.pid,pending:!startupActivated})); try { await startupGate.wait(win, abort.signal); owner(event); }`)
     .replace('const forwarded = target === request.url ? request : new Request(target, request);', `if (new URL(request.url).pathname.startsWith('/v1/')) { if (!startupActivated) process.getBuiltinModule('fs').appendFileSync(${JSON.stringify(join(reports, version + '-precommit-api.jsonl'))}, JSON.stringify({pid:process.pid})+'\\n'); else if (new URL(request.url).pathname === '/v1/instance') process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports, version + '-activated-discovery.json'))}, JSON.stringify({pid:process.pid})); } const forwarded = target === request.url ? request : new Request(target, request);`)
     .replace('activationPlan = await waitForRegisteredStartup(', `${version === '1.0.7' ? `process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports, version + '-unregistered.json'))}, JSON.stringify({pid:process.pid,nonce:startupLaunch.nonce,windows:BrowserWindow.getAllWindows().length}));` : ''} activationPlan = await waitForRegisteredStartup(`)
     .replace("else if (command.type === 'boot-ready') await startupUsability.status(instance, command.usable);", `else if (command.type === 'boot-ready') { if (command.usable) process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports, version + '-core-ready.json'))}, JSON.stringify({pid:process.pid,elapsedMs:performance.now(),usable:true})); await startupUsability.status(instance, command.usable); }`)
     .replace('await waitForStartupActivation(activationPlan, process.pid);', `for (const win of windows.values()) win.webContents.emit('will-prevent-unload', {preventDefault(){}}); process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports, version + '-pending.json'))}, JSON.stringify(BrowserWindow.getAllWindows().map(win => ({visible:win.isVisible(),enabled:win.isEnabled()})))); await waitForStartupActivation(activationPlan, process.pid);`)
     .replace('activateTray();\n       for (const link', `activateTray(); process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports, version + '-activated.json'))}, JSON.stringify(BrowserWindow.getAllWindows().map(win => ({visible:win.isVisible(),enabled:win.isEnabled()}))));\n       for (const link`)
     .replace('void app.whenReady().then(async () => {', `void app.whenReady().then(async () => { process.getBuiltinModule('fs').appendFileSync(${JSON.stringify(join(reports, version + '-normal-boots.jsonl'))}, JSON.stringify({pid:process.pid,version:app.getVersion()})+'\\n'); ${version === '1.0.2' ? 'app.exit(23); return;' : ''} dialog.showMessageBoxSync = (() => { process.getBuiltinModule('fs').writeFileSync(${JSON.stringify(join(reports, version + '-pending-dialog.json'))}, JSON.stringify({pid:process.pid})); return 0; }) as typeof dialog.showMessageBoxSync;`), loader: 'ts', resolveDir: join(desktop, 'src') }));
   } }] });
   const base = { appId: 'dev.dilla.desktop.update-acceptance.' + id, productName: product, executableName: 'dilla-desktop', toolsets: desktopBuilder.toolsets, electronVersion: '45.2.1', electronDist: join(repo, 'node_modules/electron/dist'), directories: { output }, files: ['dist/**', 'package.json'], asar: true, afterPack: join(desktop, 'scripts/linux-appimage-launcher.cjs'), appImage: { executableArgs: [] }, artifactName: `fixture-${version}.\${ext}`, publish: { provider: 'generic', url: origin }, electronFuses: { runAsNode: false, enableCookieEncryption: true, enableNodeOptionsEnvironmentVariable: false, enableNodeCliInspectArguments: false, enableEmbeddedAsarIntegrityValidation: true, onlyLoadAppFromAsar: true, grantFileProtocolExtraPrivileges: false }, linux: { target: 'AppImage', category: 'Network', icon: join(desktop, 'assets/icon.png') }, win: { target: 'nsis', icon: join(desktop, 'assets/icon.png') }, nsis: { perMachine: false, oneClick: true, allowElevation: false, deleteAppDataOnUninstall: false } };
   if (['1.0.4', '1.0.5', '1.0.6'].includes(version) && platform === 'win32') {
    base.nsis.include = join(project, 'fail-install.nsh');
    await writeFile(base.nsis.include, '!macro customInstall\nFileOpen $0 "$INSTDIR\\partial-fixture.txt" w\nFileWrite $0 "partially installed"\nFileClose $0\n' + (version === '1.0.4' ? 'SetErrorLevel 17\nQuit\n' : 'Sleep 600000\n') + '!macroend\n');
   }
   await writeFile(join(project, 'builder.json'), JSON.stringify(base));
   execFileSync(process.execPath, [join(repo, 'node_modules/electron-builder/cli.js'), '--projectDir', project, '--config', join(project, 'builder.json'), platform === 'linux' ? '--linux' : '--win', '--' + process.arch, '--publish', 'never'], { cwd: repo, stdio: ['ignore', log.fd, log.fd] });
   for (const entry of await readdir(output, { withFileTypes: true })) if (entry.isDirectory()) await rm(join(output, entry.name), { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
   return join(output, `fixture-${version}.${platform === 'linux' ? 'AppImage' : 'exe'}`);
  };
  const next = await pack('1.0.1'), broken = await pack('1.0.2'), delayed = await pack('1.0.3'), failedInstaller = platform === 'win32' ? await pack('1.0.4') : null, hungInstaller = platform === 'win32' ? await pack('1.0.5') : null, interruptedInstaller = platform === 'win32' ? await pack('1.0.6') : null, unregistered = await pack('1.0.7'), race = await pack('1.0.9'), unarmed = await pack('1.0.11'), previous = await pack('1.0.0');
  const installed = platform === 'linux' ? join(root, 'Applications/Dilla.AppImage') : join(process.env.LOCALAPPDATA, 'Programs', product, 'dilla-desktop.exe');
  if (platform === 'linux') { await mkdir(dirname(installed)); await cp(previous, installed); await chmod(installed, 0o700); }
  else { execFileSync(previous, ['/S', '/D=' + dirname(installed)], { stdio: ['ignore', log.fd, log.fd] }); }
  const oldDigest = digest(await readFile(installed));
  const release = async (file, version) => {
   installerBytes = await readFile(file);
   const now = Date.now(); descriptor = Buffer.from(JSON.stringify({ format: 1, version, channel: 'stable', publishedAt: new Date(now - 1000).toISOString(), expiresAt: new Date(now + 3600000).toISOString(), artifacts: [{ platform, arch: process.arch, url: `https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/dilla-${version}${platform === 'linux' ? '.AppImage' : '.exe'}`, size: installerBytes.length, sha512: digest(installerBytes) }] }));
   signature = sign(null, descriptor, keys.privateKey).toString('base64');
  };
  const launch = async (manualRetry = false) => {
   launched = spawn(installed, manualRetry ? ['--fixture-user-retry'] : [], { detached: true, stdio: ['ignore', log.fd, log.fd] }); launched.unref(); pid = launched.pid;
   launched.on('error', error => { outcomes.launchError = String(error); });
   await delay(500);
   if (launched.exitCode !== null || launched.signalCode !== null) throw new Error('Installed fixture failed to launch: ' + (await readFile(logs, 'utf8')).slice(-2000));
  };
  await release(next, '1.0.1');
  for (const kind of ['metadata', 'artifact', 'download']) {
   fault = kind; await rm(join(reports, '1.0.0.json'), { force: true }); await launch();
   const failure = await until(async () => { const report = await json(join(reports, '1.0.0.json')); return report.state.phase === 'error' ? report : null; }, kind + ' rejection');
   assert.equal(failure.inheritedHealth, null); assert.equal(digest(await readFile(installed)), oldDigest);
   assert.equal(await readFile(join(profile, 'device-sentinel'), 'utf8'), 'encrypted-device-data-survives');
   outcomes[kind] = { ...failure.state, mainPid: failure.pid, launcherPid: launched.pid }; await stop(failure.pid); pid = null;
  }
  fault = 'none'; await rm(join(reports, '1.0.0.json'), { force: true }); await launch();
  const completed = await until(async () => {
   const old = await json(join(reports, '1.0.0.json')); if (old.state.phase === 'error') throw Object.assign(new Error('Installed updater failed: ' + old.state.message), { terminal: true });
   const active = await json(join(profile, 'updates/active.json')), directory = join(profile, 'updates', 'startup-' + active.nonce), journal = await json(join(directory, 'journal.json'));
   if (journal.phase !== 'complete') return null;
   return { journal, health: await json(join(directory, 'healthy.json')), report: await json(join(reports, '1.0.1.json')) };
  }, 'authenticated installed upgrade', 180000);
  pid = completed.health.pid;
  assert.equal(completed.health.pid, completed.journal.childPid); assert.equal(completed.health.pid, completed.report.pid); assert.equal(completed.report.version, '1.0.1'); assert.equal(completed.report.inheritedHealth, null); assert.equal(alive(pid), true);
  if (platform === 'linux') assert.equal(digest(await readFile(installed + '.previous')), oldDigest);
  const pendingWindows = await json(join(reports, '1.0.1-pending.json'));
  const activatedWindows = await until(async () => json(join(reports, '1.0.1-activated.json')), 'durable activation UI');
  outcomes.pendingWindows = pendingWindows;
  assert.ok(pendingWindows.length > 0); assert.ok(pendingWindows.every(win => !win.visible && !win.enabled));
  await assert.rejects(readFile(join(reports, '1.0.1-pending-dialog.json')), { code: 'ENOENT' });
  assert.equal(activatedWindows.length, pendingWindows.length); assert.ok(activatedWindows.every(win => win.visible && win.enabled));
  const localCore = await json(join(reports, '1.0.1-core-ready.json')); assert.equal(localCore.pid, completed.health.pid); assert.equal(localCore.usable, true); assert.ok(localCore.elapsedMs > 0);
  const startupWait = await json(join(reports, '1.0.1-startup-wait.json'));
  assert.deepEqual(startupWait, { pid: completed.health.pid, pending: true });
  await assert.rejects(readFile(join(reports, '1.0.1-precommit-api.jsonl')), { code: 'ENOENT' });
  const activatedDiscovery = await until(async () => json(join(reports, '1.0.1-activated-discovery.json')), 'ordinary account startup after durable activation');
  assert.equal(activatedDiscovery.pid, completed.health.pid);
  outcomes.upgrade = { ...completed, pendingWindows, activatedWindows, localCore, startupWait, activatedDiscovery, offlineInstance: 'https://desktop-update-offline.invalid/' };
  const registry = platform === 'win32' ? await snapshotUninstall(dirname(installed)) : null;
  if (registry) assert.equal(registry.displayVersion?.data, '1.0.1');
  await stop(pid); pid = null;
  const workingDigest = digest(await readFile(installed));
  await release(broken, '1.0.2'); await rm(join(reports, '1.0.1.json'), { force: true }); await launch();
  const recovered = await until(async () => {
   const active = await json(join(profile, 'updates/active.json')), journal = await json(join(profile, 'updates', 'startup-' + active.nonce, 'journal.json')), report = await json(join(reports, '1.0.1.json'));
   return journal.phase === 'failed' && report.state.phase === 'error' ? { journal, report } : null;
  }, 'failed installed startup rollback', 180000);
  pid = recovered.report.pid;
  assert.equal(recovered.journal.failure, 'startup'); assert.equal(recovered.report.version, '1.0.1'); assert.equal(digest(await readFile(installed)), workingDigest); assert.equal(alive(pid), true);
  await delay(1500); assert.equal((await json(join(reports, '1.0.1.json'))).state.phase, 'error', 'failed version must require explicit retry');
  assert.equal(await readFile(join(profile, 'device-sentinel'), 'utf8'), 'encrypted-device-data-survives'); outcomes.rollback = recovered;
  if (registry) assert.deepEqual(await snapshotUninstall(dirname(installed)), registry);
  await stop(pid); pid = null;

  // Kill only the actual watchdog during the new child's delayed startup, leave
  // its child alive, and restart the installed program normally. The journal
  // must hand recovery to the authenticated old copy without rerunning NSIS.
  await release(delayed, '1.0.3'); await rm(join(reports, '1.0.1.json'), { force: true }); await launch(true);
  const interrupted = await until(async () => {
   const active = await json(join(profile, 'updates/active.json')), directory = join(profile, 'updates', 'startup-' + active.nonce), journal = await json(join(directory, 'journal.json'));
   return journal.phase === 'launching' && journal.childPid ? { directory, journal, helper: await json(join(directory, 'watchdog-ready.json')) } : null;
  }, 'owned child starts before interrupted recovery');
  await stopOnly(interrupted.helper.pid); pid = interrupted.journal.childPid; assert.equal(alive(pid), true, 'recovery must own stopping the interrupted child');
  await launch();
  const restarted = await until(async () => {
   const journal = await json(join(interrupted.directory, 'journal.json')), report = await json(join(reports, '1.0.1.json'));
   return journal.phase === 'failed' && journal.failure === 'interrupted' && report.state.phase === 'error' ? { journal, report } : null;
  }, 'normal installed restart recovers interrupted upgrade', 180000);
  assert.equal(alive(interrupted.journal.childPid), false, 'production recovery stopped the owned interrupted child');
  pid = restarted.report.pid; assert.equal(alive(pid), true); assert.equal(digest(await readFile(installed)), workingDigest);
  if (registry) assert.deepEqual(await snapshotUninstall(dirname(installed)), registry);
  outcomes.interruptedRestart = restarted; await stop(pid); pid = null;

  // Interrupt before childPid can be written. The actual updated main must
  // remain before renderer/store initialization, and nonce discovery must find
  // and stop it even though no PID is available in the durable journal.
  await release(unregistered, '1.0.7'); await rm(join(reports, '1.0.1.json'), { force: true }); await launch(true);
  const beforeRegistration = await until(async () => {
   const active = await json(join(profile, 'updates/active.json')), directory = join(profile, 'updates', 'startup-' + active.nonce), journal = await json(join(directory, 'journal.json'));
   const child = await json(join(reports, '1.0.7-unregistered.json'));
   return journal.phase === 'launching' && !journal.childPid ? { directory, journal, child, helper: await json(join(directory, 'watchdog-ready.json')) } : null;
  }, 'actual child is blocked before durable registration');
  assert.equal(beforeRegistration.child.nonce, beforeRegistration.journal.nonce); assert.equal(beforeRegistration.child.windows, 0);
  assert.equal(alive(beforeRegistration.child.pid), true); await assert.rejects(readFile(join(reports, '1.0.7.json')), { code: 'ENOENT' });
  await stopOnly(beforeRegistration.helper.pid); assert.equal(alive(beforeRegistration.child.pid), true); await launch();
  const registrationRecovery = await until(async () => {
   const journal = await json(join(beforeRegistration.directory, 'journal.json')), report = await json(join(reports, '1.0.1.json'));
   return journal.phase === 'failed' && journal.failure === 'interrupted' && report.state.phase === 'error' ? { journal, report, unregisteredChildPid: beforeRegistration.child.pid } : null;
  }, 'unregistered child recovery', 180000);
  assert.equal(alive(registrationRecovery.unregisteredChildPid), false);
  pid = registrationRecovery.report.pid; assert.equal(alive(pid), true); assert.equal(digest(await readFile(installed)), workingDigest);
  await assert.rejects(readFile(join(reports, '1.0.7.json')), { code: 'ENOENT' });
  if (registry) assert.deepEqual(await snapshotUninstall(dirname(installed)), registry);
  outcomes.beforeRegistration = registrationRecovery; await stop(pid); pid = null;

  // Parent death after active rename but BEFORE its flush/ARM must leave the
  // old app installed. An unarmed helper can never infer permission from the file.
  await release(unarmed, '1.0.11'); await rm(join(reports, '1.0.1.json'), { force:true }); await launch(true);
  const beforeArm = await until(async()=>json(join(reports,'pointer-visible-before-arm.json')), 'active rename visible before parent flush/arm');
  const acceptedQuit = await json(join(reports,'accepted-quit-windows.json')); outcomes.acceptedQuit = acceptedQuit;
  assert.equal(acceptedQuit.windows,0,'accepted quit cannot reopen windows while preparing its helper');
  const unarmedDirectory=join(profile,'updates','startup-'+beforeArm.nonce), unarmedHelper=await json(join(unarmedDirectory,'watchdog-ready.json'));
  await stopOnly(beforeArm.parentPid); await until(async()=>!alive(unarmedHelper.pid),'unarmed helper exits after parent death',10000);
  assert.equal(digest(await readFile(installed)),workingDigest,'no install is allowed before authenticated post-flush ARM');
  await launch();
  const preparedOwnersFile=join(reports,'prepared-recovery-owners.jsonl');
  const preparedOwner=await until(async()=>{const rows=(await readFile(preparedOwnersFile,'utf8')).trim().split('\n').map(JSON.parse);return rows[0];},'first recovery owns readiness before changing prepared phase');
  assert.equal((await json(join(unarmedDirectory,'journal.json'))).phase,'prepared');
  await rm(join(reports,'1.0.1-normal-boots.jsonl'),{force:true});
  const secondPrepared=spawn(installed,[],{detached:true,stdio:['ignore',log.fd,log.fd]});secondPrepared.unref();
  await until(async()=>secondPrepared.exitCode!==null||secondPrepared.signalCode!==null,'second ordinary observer exits under prepared recovery owner',12000);
  const preparedOwners=(await readFile(preparedOwnersFile,'utf8')).trim().split('\n').map(JSON.parse);
  assert.equal(preparedOwners.length,1,'two serialized ordinary launches must not spawn competing recovery owners');
  assert.equal(alive(preparedOwner.pid),true);assert.equal((await json(join(unarmedDirectory,'watchdog-ready.json'))).pid,preparedOwner.pid);
  await assert.rejects(readFile(join(reports,'1.0.1-normal-boots.jsonl')),{code:'ENOENT'});
  outcomes.preparedRecoveryOwner={...preparedOwner,owners:preparedOwners.length,secondObserverPid:secondPrepared.pid};
  const abandoned=await until(async()=>{const journal=await json(join(unarmedDirectory,'journal.json')),report=await json(join(reports,'1.0.1.json'));return journal.phase==='failed'&&report.state.phase==='error'?{journal,report}:null;},'abandoned preparation recovers without installing',60000);
  pid=abandoned.report.pid;assert.equal(alive(pid),true);assert.equal(digest(await readFile(installed)),workingDigest);outcomes.unarmedParentDeath=abandoned;await stop(pid);pid=null;

  if (failedInstaller) {
   await release(failedInstaller, '1.0.4'); await rm(join(reports, '1.0.1.json'), { force: true }); await launch(true);
   const refused = await until(async () => {
    const active = await json(join(profile, 'updates/active.json')), journal = await json(join(profile, 'updates', 'startup-' + active.nonce, 'journal.json')), report = await json(join(reports, '1.0.1.json'));
    return journal.phase === 'failed' && journal.failure === 'installer' && report.state.phase === 'error' ? { journal, report } : null;
   }, 'actual NSIS partial installation failure recovers', 180000);
   pid = refused.report.pid; assert.equal(alive(pid), true); assert.equal(digest(await readFile(installed)), workingDigest);
   assert.deepEqual(await snapshotUninstall(dirname(installed)), registry);
   await assert.rejects(readFile(join(dirname(installed), 'partial-fixture.txt')), { code: 'ENOENT' });
   await assert.rejects(readFile(join(reports, '1.0.4.json')), { code: 'ENOENT' });
   outcomes.failedInstaller = refused;
  }
  if (hungInstaller && interruptedInstaller) {
   for (const [kind, file, version] of [['installerTimeout', hungInstaller, '1.0.5'], ['installerParentDeath', interruptedInstaller, '1.0.6']]) {
    await stop(pid); pid = null;
    await release(file, version); await rm(join(reports, '1.0.1.json'), { force: true }); await launch(true);
    const installing = await until(async () => {
     const active = await json(join(profile, 'updates/active.json')), directory = join(profile, 'updates', 'startup-' + active.nonce), journal = await json(join(directory, 'journal.json'));
     await readFile(join(dirname(installed), 'partial-fixture.txt'));
     return journal.phase === 'installing' && journal.installerPid && journal.installerActorPid ? { directory, journal, helper: await json(join(directory, 'watchdog-ready.json')) } : null;
    }, 'actual hung NSIS has started');
    assert.equal(alive(installing.journal.installerPid), true);
    if (kind === 'installerParentDeath') {
     // Terminate only the helper, never taskkill /T: its independent actor must
     // observe the held parent handle and stop/drain the native installer job.
     await stopOnly(installing.helper.pid);
     await until(async () => !alive(installing.journal.installerPid), 'parent-death contained NSIS exit', 30000);
     await launch();
    }
    const result = await until(async () => {
     const journal = await json(join(installing.directory, 'journal.json')), report = await json(join(reports, '1.0.1.json'));
     return journal.phase === 'failed' && report.state.phase === 'error' ? { journal, report, actualInstallerPid: installing.journal.installerPid, actualActorPid: installing.journal.installerActorPid } : null;
    }, 'hung NSIS recovery', 180000);
    pid = result.report.pid; assert.equal(alive(pid), true);
    assert.equal(alive(result.actualInstallerPid), false); assert.equal(alive(result.actualActorPid), false);
    assert.equal(result.journal.failure, kind === 'installerTimeout' ? 'installer' : 'interrupted');
    assert.equal(digest(await readFile(installed)), workingDigest);
    assert.deepEqual(await snapshotUninstall(dirname(installed)), registry);
    await assert.rejects(readFile(join(dirname(installed), 'partial-fixture.txt')), { code: 'ENOENT' });
    await assert.rejects(readFile(join(reports, version + '.json')), { code: 'ENOENT' });
    outcomes[kind] = result;
   }
  }
  for (const fault of ['fail', 'success']) {
   await stop(pid); pid = null;
   await writeFile(join(reports, 'complete-fault'), fault); await rm(join(reports, 'complete-visible.json'), { force: true }); await rm(join(reports, '1.0.1.json'), { force: true });
   await release(race, '1.0.9'); await launch(true);
   const visible = await until(async () => json(join(reports, 'complete-visible.json')), 'complete rename visible before directory flush');
   const newProgramDigest = digest(await readFile(installed));
   assert.equal(visible.phase, 'complete'); assert.equal(alive(visible.writerPid), true);
   await stop(visible.childPid);
   await rm(join(reports, '1.0.9-normal-boots.jsonl'), { force: true }); await rm(join(reports, '1.0.9.json'), { force: true });
   const secondary = spawn(installed, [], { detached: true, stdio: ['ignore', log.fd, log.fd] }); secondary.unref(); pid = secondary.pid;
   await until(async () => secondary.exitCode !== null || secondary.signalCode !== null, 'ordinary second launch exits before UI during incomplete owner commit', 12000);
   await assert.rejects(readFile(join(reports, '1.0.9-normal-boots.jsonl')), { code: 'ENOENT' }); pid = null;
   const active = await json(join(profile, 'updates/active.json')), directory = join(profile, 'updates', 'startup-' + active.nonce);
   if (fault === 'fail') {
    const result = await until(async () => {
     const journal = await json(join(directory, 'journal.json')), report = await json(join(reports, '1.0.1.json'));
     return journal.phase === 'failed' && report.state.phase === 'error' ? { journal, report } : null;
    }, 'failed complete flush restores working app', 60000);
    pid = result.report.pid; assert.equal(alive(pid), true); assert.equal(digest(await readFile(installed)), workingDigest);
    if (registry) assert.deepEqual(await snapshotUninstall(dirname(installed)), registry);
    outcomes.completeFlushFailure = result;
   } else {
    await until(async () => !alive(visible.writerPid), 'durably committed watchdog finishes despite dead health child', 60000);
    assert.equal((await json(join(directory, 'journal.json'))).phase, 'complete'); assert.equal(digest(await readFile(installed)), newProgramDigest);
    await launch(); const report = await until(async () => { const value = await json(join(reports, '1.0.9.json')); return value.state.phase === 'idle' ? value : null; }, 'ordinary restart opens committed version');
    pid = report.pid; assert.equal(alive(pid), true); assert.equal(report.version, '1.0.9');
    outcomes.completeFlushSuccess = { journal: await json(join(directory, 'journal.json')), report, registeredChildCrashed: visible.childPid, noRollback: true };
   }
  }
  assert.equal(await readFile(join(profile, 'device-sentinel'), 'utf8'), 'encrypted-device-data-survives');
  assert.equal(digest(await readFile(join(desktop, 'src/update-keys.ts'))), productionKeyHash);
  await mkdir(join(desktop, 'test-results'), { recursive: true }); await writeFile(join(desktop, 'test-results', `${platform}-installed-upgrade.json`), JSON.stringify({ platform, arch: process.arch, ...outcomes, productionTrustUnchanged: true, osProtectionsUnchanged: true, from: '1.0.0', to: '1.0.1', failedNext: '1.0.2' }, null, 2));
 } catch (error) {
  await mkdir(join(desktop, 'test-results'), { recursive: true }); await writeFile(join(desktop, 'test-results', `${platform}-installed-upgrade-error.json`), JSON.stringify({ error: String(error), root, outcomes, log: await readFile(logs, 'utf8') }, null, 2)); throw error;
 } finally {
  for (const version of ['1.0.0', '1.0.1', '1.0.2', '1.0.3', '1.0.4', '1.0.5', '1.0.6', '1.0.7', '1.0.9', '1.0.11']) { try { await stop((await json(join(reports, version + '.json'))).pid); } catch {} }
  await stop(pid).catch(() => {});
  if (launched?.pid) await stop(launched.pid).catch(() => {});
  if (server) await new Promise(done => server.close(done));
  await log.close();
  // Windows only removes the CI-owned fixture via its own NSIS uninstaller;
  // no user installation or broad registry subtree is selected by this test.
  if (platform === 'win32') {
   const uninstaller = join(process.env.LOCALAPPDATA, 'Programs', product, `Uninstall ${product}.exe`);
   spawnSync(uninstaller, ['/S'], { stdio: 'ignore' });
   for (const outcome of [outcomes.rollback, outcomes.interruptedRestart, outcomes.failedInstaller, outcomes.beforeRegistration, outcomes.completeFlushFailure, outcomes.installerTimeout, outcomes.installerParentDeath]) {
    if (outcome?.journal?.nonce) await rm(dirname(uninstaller) + '.failed-' + outcome.journal.nonce, { recursive: true, force: true, maxRetries: 20, retryDelay: 250 });
   }
  }
  await rm(root, { recursive: true, force: true, maxRetries: 20, retryDelay: 250 });
 }
});
