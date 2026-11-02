import { test, expect, _electron, type ElectronApplication } from '@playwright/test';
import { mkdtemp, mkdir, readFile, writeFile, rm } from 'node:fs/promises';
import { spawn, execFile, type ChildProcess } from 'node:child_process';
import { promisify } from 'node:util';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
const exec = promisify(execFile);
const pause = (ms: number) => new Promise(done => setTimeout(done, ms));
let desktop: ElectronApplication, selected: ChildProcess | ElectronApplication, other: ChildProcess | ElectronApplication;
let root: string, env: Record<string, string>, selectedPIDs: number[];
const services: ChildProcess[] = [];
const macActors: { pid: number; path: string }[] = [];
let diagnostic = '';
function start(exe: string, args: string[]): ChildProcess {
 const child = spawn(exe, args, { env, stdio: ['ignore', 'pipe', 'pipe'] });
 child.stderr?.on('data', chunk => { diagnostic += String(chunk); }); services.push(child); return child;
}
function wave(hz: number): Buffer {
 const frames = 48000 * 90, bytes = frames * 4, data = Buffer.alloc(44 + bytes);
 data.write('RIFF'); data.writeUInt32LE(bytes + 36, 4); data.write('WAVEfmt ', 8); data.writeUInt32LE(16, 16); data.writeUInt16LE(1, 20); data.writeUInt16LE(2, 22); data.writeUInt32LE(48000, 24); data.writeUInt32LE(192000, 28); data.writeUInt16LE(4, 32); data.writeUInt16LE(16, 34); data.write('data', 36); data.writeUInt32LE(bytes, 40);
 for (let frame = 0; frame < frames; frame++) { const value = Math.round(Math.sin(2 * Math.PI * hz * frame / 48000) * 12000); data.writeInt16LE(value, 44 + frame * 4); data.writeInt16LE(value, 46 + frame * 4); }
 return data;
}
test.beforeEach(async () => {
 test.setTimeout(90000);
 diagnostic = ''; root = await mkdtemp(join(tmpdir(), 'dilla-native-audio-e2e-'));
 env = Object.fromEntries(Object.entries(process.env).filter((row): row is [string, string] => row[1] !== undefined));
 if (process.platform === 'linux') {
  Object.assign(env, { XDG_RUNTIME_DIR: root, PIPEWIRE_RUNTIME_DIR: root, XDG_STATE_HOME: root, XDG_CACHE_HOME: root });
  start('pipewire', []);
  for (let i = 0; i < 50; i++) { try { await exec('pw-cli', ['info', '0'], { env, timeout: 500 }); break; } catch { if (i === 49) throw new Error(diagnostic); await pause(100); } }
  start('wireplumber', []); await pause(500);
  for (const [name, frequency] of [['selected', 1000], ['other', 880], ['call', 440]] as const) await writeFile(join(root, name + '.wav'), wave(frequency));
  selected = start('pw-play', ['--target=0', '--properties={ application.name="Dilla Selected Acceptance" }', join(root, 'selected.wav')]);
  other = start('pw-play', ['--target=0', '--properties={ application.name="Dilla Other Acceptance" }', join(root, 'other.wav')]);
  selectedPIDs = [(selected as ChildProcess).pid!];
 } else if (process.platform === 'darwin') {
  const executable = join(root, 'TonePlayer');
  await exec('swiftc', ['-O', resolve('test/support/TonePlayer.swift'), '-o', executable], { env, timeout: 60000 });
  for (const [name, frequency] of [['selected', 1000], ['other', 880]] as const) {
   const bundle = join(root, name + '.app'), contents = join(bundle, 'Contents');
   await mkdir(join(contents, 'MacOS'), { recursive: true });
   await exec('cp', [executable, join(contents, 'MacOS/TonePlayer')], { env });
   await writeFile(join(contents, 'Info.plist'), `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>CFBundleExecutable</key><string>TonePlayer</string><key>CFBundleIdentifier</key><string>dev.dilla.acceptance.${name}</string><key>CFBundleName</key><string>Dilla ${name} audio acceptance</string><key>CFBundlePackageType</key><string>APPL</string></dict></plist>`);
   await exec('codesign', ['--force', '--sign', '-', bundle], { env, timeout: 10000 });
   const file = join(root, name + '.wav'); await writeFile(file, wave(frequency));
   // Launch as independent applications through LaunchServices. Directly
   // spawning both executables can group their audio under the test runner's
   // process responsibility, unlike two apps a person opens from the Dock.
   const readyFile = join(root, name + '-ready.json');
   const actor = start('/usr/bin/open', ['-n', '-W', bundle, '--args', file, readyFile]);
   let ready: { pid: number; bundle: string; time: number } | undefined;
   for (let attempt = 0; attempt < 150; attempt++) {
    try { ready = JSON.parse(await readFile(readyFile, 'utf8')); break; } catch { await pause(100); }
   }
   expect(ready?.bundle).toBe(`dev.dilla.acceptance.${name}`);
   expect(ready?.time).toBeGreaterThan(0.5);
   expect(Number.isSafeInteger(ready?.pid) && ready!.pid > 1).toBe(true);
   macActors.push({ pid: ready!.pid, path: bundle });
   console.log('NATIVE_TONE_READY', JSON.stringify(ready));
   if (name === 'selected') { selected = actor; selectedPIDs = [ready!.pid]; } else other = actor;
  }
 } else {
  env.DILLA_AUDIO_DIAGNOSTICS = '1';
  // Independent real playback applications, without fake media devices or PCM injection.
  const actor = join(root, 'tone.cjs');
  await writeFile(actor, `const {app,BrowserWindow}=require('electron');let w;app.enableSandbox();app.setPath('userData',process.argv.find(a=>a.startsWith('--actor-profile=')).slice(16));app.commandLine.appendSwitch('autoplay-policy','no-user-gesture-required');app.commandLine.appendSwitch('enable-logging','file');app.commandLine.appendSwitch('log-file',process.argv.find(a=>a.startsWith('--audio-log=')).slice(12));app.commandLine.appendSwitch('vmodule','audio*=2,core_audio*=2');app.whenReady().then(()=>{w=new BrowserWindow({webPreferences:{sandbox:true,contextIsolation:true,nodeIntegration:false,backgroundThrottling:false}});w.loadURL('data:text/html,<title>Application audio acceptance</title><script>const c=new AudioContext({sampleRate:48000}),o=c.createOscillator(),g=c.createGain();o.frequency.value='+Number(process.argv.find(a=>a.startsWith('--tone=')).slice(7))+';g.gain.value=0.35;o.connect(g).connect(c.destination);o.start();c.resume();window.audioAcceptance={context:c,oscillator:o};</script>')})`);
  selected = await _electron.launch({ chromiumSandbox: true, args: [actor, '--tone=1000', '--actor-profile=' + join(root, 'selected-profile'), '--audio-log=' + join(root, 'selected-audio.log')], env });
  other = await _electron.launch({ chromiumSandbox: true, args: [actor, '--tone=880', '--actor-profile=' + join(root, 'other-profile'), '--audio-log=' + join(root, 'other-audio.log')], env });
  for (const [application, frequency] of [[selected, 1000], [other, 880]] as const) {
   const page = await (application as ElectronApplication).firstWindow();
   await page.waitForFunction(frequency => { const a = (window as any).audioAcceptance; return a?.context.state === 'running' && a.context.currentTime > 0.5 && a.oscillator.frequency.value === frequency; }, frequency);
   console.log('NATIVE_AUDIO_PLAYBACK', JSON.stringify(await page.evaluate(() => { const a = (window as any).audioAcceptance; return { frequency: a.oscillator.frequency.value, state: a.context.state, time: a.context.currentTime, output: a.context.getOutputTimestamp() }; })));
  }
  // Wait for real playback before sampling process IDs: Chromium's audio service
  // is created lazily, and WASAPI attributes its session to that utility process.
  selectedPIDs = await (selected as ElectronApplication).evaluate(({ app }) => [process.pid, ...app.getAppMetrics().map(metric => metric.pid)]);
 }
 desktop = await _electron.launch({ chromiumSandbox: true, args: [resolve('.'), `--profile=${join(root, 'profile')}`, '--instance=http://127.0.0.1:1/', '--allow-loopback', '--allow-loopback-in-peer-connection', '--media-validation'], env });
 const page = await desktop.firstWindow(); await page.waitForFunction(() => 'validation' in window);
 await desktop.evaluate(({ BrowserWindow }) => {
  const session = BrowserWindow.getAllWindows()[0].webContents.session;
  session.setPermissionCheckHandler((_contents, permission) => ['media', 'local-network', 'loopback-network'].includes(permission));
  session.setPermissionRequestHandler((_contents, _permission, callback) => callback(true));
 });
 if (process.platform === 'linux') {
  await desktop.evaluate(async (_electron, file) => { const { spawn } = process.getBuiltinModule('node:child_process'); (globalThis as any).callAudioFixture = spawn('pw-play', ['--target=0', '--properties={ application.name="Dilla Own Call Acceptance" }', file], { stdio: 'ignore' }); }, join(root, 'call.wav'));
 } else {
  await page.evaluate(async () => { const context = new AudioContext({ sampleRate: 48000 }), oscillator = context.createOscillator(), gain = context.createGain(); oscillator.frequency.value = 440; gain.gain.value = 0.35; oscillator.connect(gain).connect(context.destination); oscillator.start(); await context.resume(); (globalThis as any).callAudioFixture = context; });
 }
 const binary = resolve('native-bin', process.platform === 'win32' ? 'dilla-audio.exe' : 'dilla-audio');
 let sources: { id: string; label: string }[] = [];
 for (let i = 0; i < 20; i++) {
  const pid = desktop.process().pid!;
  const result = await exec(binary, ['list', String(pid)], { env, timeout: 12000 });
  sources = JSON.parse(result.stdout);
  if (i === 0 || i === 19) diagnostic += result.stderr;
  if (sources.some(source => selectedPIDs.includes(Number(source.id.split(':')[1])))) break;
  await pause(250);
 }
 if (process.platform === 'win32') {
  for (const name of ['selected', 'other']) {
   console.log('NATIVE_AUDIO_DEVICE_LOG', name, await readFile(join(root, name + '-audio.log'), 'utf8').then(log => log.slice(-24000), error => String(error)));
  }
  console.log('NATIVE_AUDIO_PROCESS_METRICS', JSON.stringify(await (selected as ElectronApplication).evaluate(({ app }) => app.getAppMetrics().map(({ pid, type, name, serviceName }) => ({ pid, type, name, serviceName })))));
 }
 console.log('NATIVE_AUDIO_ENVIRONMENT', JSON.stringify({ platform: process.platform, sources, selectedPIDs, diagnostic }));
 const source = sources.find(source => selectedPIDs.includes(Number(source.id.split(':')[1])));
 expect(source, 'a real selected playback process must be discoverable by the shipping native backend').toBeDefined();
 expect(sources.some(source => source.label === 'Dilla Own Call Acceptance')).toBe(false);
 await desktop.evaluate(({ dialog }, { binary, sourceID }) => {
  const original = dialog.showMessageBox.bind(dialog);
  dialog.showMessageBox = (async (...args: any[]) => {
   const options = args.at(-1);
   if (options.message !== 'Choose the application others will hear') return (original as any)(...args);
   const { execFileSync } = process.getBuiltinModule('node:child_process');
   const current = JSON.parse(execFileSync(binary, ['list', String(process.pid)], { encoding: 'utf8', timeout: 10000 }));
   const index = current.findIndex((row: {id: string}) => row.id === sourceID);
   if (index < 0) throw new Error('Selected playback application disappeared');
   return { response: index + 1, checkboxChecked: false };
  }) as typeof dialog.showMessageBox;
 }, { binary, sourceID: source!.id });
});
test.afterEach(async () => {
 await desktop?.evaluate(async () => { (globalThis as any).callAudioFixture?.kill?.(); }).catch(() => {});
 await desktop?.close().catch(() => {});
 for (const actor of [selected, other]) if (actor && 'close' in actor) await (actor as ElectronApplication).close().catch(() => {});
 for (const actor of macActors.splice(0)) {
  // Do not signal an unrelated process if a failed actor's PID was reused.
  const command = await exec('/bin/ps', ['-p', String(actor.pid), '-o', 'command=']).then(result => result.stdout, () => '');
  if (command.includes(actor.path + '/Contents/MacOS/TonePlayer')) await exec('/bin/kill', ['-TERM', String(actor.pid)]).catch(() => {});
 }
 for (const child of services.splice(0).reverse()) { if (child.exitCode === null && child.signalCode === null) { child.kill(); await new Promise<void>(done => { child.once('close', () => done()); setTimeout(done, 2000); }); } }
 if (root) await rm(root, { recursive: true, force: true });
});
test('selected application reaches an encrypted listener while other applications and Dilla call audio stay excluded', async () => {
 const page = await desktop.firstWindow();
 const result = await page.evaluate(() => (window as any).validation.audio());
 console.log('NATIVE_AUDIO_ENCRYPTED_LISTENER', JSON.stringify(result));
 expect(result.transport).toEqual(['connected', 'connected']);
 expect(result.peaks.selected).toBeGreaterThan(-35);
 expect(result.peaks.call).toBeLessThan(-45);
 expect(result.peaks.other).toBeLessThan(-45);
 expect(result.receiver?.decrypted['7']).toBeGreaterThan(50);
 expect(result.sender?.encrypted['7']['3']).toBeGreaterThan(50);
 expect(result.sender?.passedThrough).toBe(0);
 expect(result.errors).toEqual([]);
});
test('native application audio without an authenticated source slot cannot fall back to plaintext', async () => {
 const page = await desktop.firstWindow();
 const result = await page.evaluate(async () => {
  // Exercise a pending publish that outlives the harness's former two-second
  // unpublish timer. Capture must reach the blocker after negotiation completes.
  const createOffer = RTCPeerConnection.prototype.createOffer;
  RTCPeerConnection.prototype.createOffer = new Proxy(createOffer, {
   async apply(target, connection, args) {
    await new Promise(done => setTimeout(done, 2500));
    return Reflect.apply(target, connection, args);
   },
  });
  try { return await (window as any).validation.audio({ source: 'unknown', receiver: false, ms: 4000 }); }
  finally { RTCPeerConnection.prototype.createOffer = createOffer; }
 });
 console.log('NATIVE_AUDIO_UNAUTHENTICATED_SENDER', JSON.stringify(result));
 expect(result.transport).toEqual(['connected', 'connected']);
 expect(result.inputPeaks.selected, 'real native audio must reach the sender before rejection is measured').toBeGreaterThan(-35);
 expect(result.senderTrackEnded).toBe(false);
 expect(result.errors).toEqual(['a1a1: E_BAD_OPTIONS: track source unknown has no slot']);
 expect(result.unpublished).toEqual([result.trackID]);
 expect(result.bytesSent).toBe(0);
 expect(result.packetsSent).toBe(0);
 expect(result.peaks.selected).toBeLessThan(-80);
 expect(result.sender?.dropped.blocked).toBeGreaterThan(10);
 expect(result.sender?.droppedByTrack[result.trackID]).toBeGreaterThan(10);
 expect(result.sender?.passedThrough).toBe(0);
});
