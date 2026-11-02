import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chromium } from '@playwright/test';
import { createHash } from 'node:crypto';
import { readFile, writeFile, rm, mkdir } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { spawn, execFileSync } from 'node:child_process';
import { setTimeout as pause } from 'node:timers/promises';
import { createInstalledPackages, assertInstalledFuses } from '../../../../e2e/web/support/installed-packages.ts';
const digest = bytes => createHash('sha512').update(bytes).digest('base64');
async function until(read, label, timeout = 120000) {
 const end = Date.now() + timeout; let last;
 while (Date.now() < end) {
  try { const value = await read(); if (value) return value; } catch (error) { last = error; }
  await pause(100);
 }
 throw Error(`${label} timed out: ${last ?? 'not ready'}`);
}
async function json(file) { return JSON.parse(await readFile(file, 'utf8')); }
async function owned(report, packages) {
 if (!Number.isSafeInteger(report.pid) || report.pid <= 1 || report.pid === process.pid) return false;
 try {
  if (process.platform === 'linux') {
   const stat = await readFile(`/proc/${report.pid}/stat`, 'utf8'), fields = stat.slice(stat.lastIndexOf(')') + 2).split(' ');
   return report.start === fields[19] && !['Z','X'].includes(fields[0]);
  }
  const actual = execFileSync(join(process.env.SystemRoot, 'System32/WindowsPowerShell/v1.0/powershell.exe'), ['-NoProfile','-NonInteractive','-Command', `(Get-CimInstance Win32_Process -Filter 'ProcessId = ${report.pid}').ExecutablePath`], {encoding:'utf8',windowsHide:true}).trim();
  return actual.toLowerCase() === packages.installed.toLowerCase();
 } catch { return false; }
}

// One normal engine handoff. The separate full account acceptance suite retains
// encrypted history, device-key continuity, draft and selected-file safeguards.
test('installed AppImage/NSIS upgrades and relaunches through electron-updater', { skip: !['linux','win32'].includes(process.platform), timeout: 900000 }, async () => {
 const instance = 'https://desktop-update-offline.invalid/';
 const packages = await createInstalledPackages(instance), browsers = new Map();
 const reportFile = version => join(packages.reports, `update-${version}.json`);
 const reports = async () => (await readFile(join(packages.reports,'processes.jsonl'),'utf8')).trim().split('\n').filter(Boolean).map(JSON.parse);
 const attach = version => until(async () => {
  const port = Number((await readFile(join(packages.profile,'DevToolsActivePort'),'utf8')).split('\n')[0]);
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw Error('Invalid owned renderer port');
  let browser = browsers.get(port);
  if (!browser?.isConnected()) { browser = await chromium.connectOverCDP(`http://127.0.0.1:${port}`,{timeout:3000}); browsers.set(port,browser); }
  const page = browser.contexts()[0].pages().find(page => page.url().startsWith(instance));
  if (!page) return null;
  const info = await page.evaluate(() => window.dillaDesktop.info());
  if (info.version !== version) return null;
  await page.waitForFunction(() => (document.querySelector('#root')?.childElementCount ?? 0) > 0 && document.querySelector('.d-splash__line')?.textContent !== 'Starting dilla',{},{timeout:60000});
  const runtime = (await reports()).reverse().find(report => report.role === 'ordinary' && report.version === version);
  assert.ok(runtime && await owned(runtime,packages),'actual owned installed runtime');
  await assertInstalledFuses(process.platform === 'linux' ? `/proc/${runtime.pid}/exe` : runtime.executable);
  assert.deepEqual(await page.evaluate(() => ({require:typeof window.require,process:typeof window.process})),{require:'undefined',process:'undefined'});
  return {page,pid:runtime.pid,version:info.version};
 },`installed ${version}`);
 try {
  const sentinel = join(packages.profile,'device-sentinel'); await writeFile(sentinel,'existing profile retained');
  await rm(join(packages.profile,'DevToolsActivePort'),{force:true});
  const child=spawn(packages.installed,[],{detached:true,stdio:'ignore',env:{...process.env,...(process.platform==='linux'?{APPIMAGE:packages.installed,APPIMAGE_SILENT_INSTALL:'true'}:{})}});
  await new Promise((done,fail)=>{child.once('spawn',done);child.once('error',fail);});child.unref();
  const old=await attach('1.0.0');
  await packages.setNext();
  await old.page.evaluate(()=>window.dillaDesktop.send({type:'check-update'}));
  await until(async()=>{const report=await json(reportFile('1.0.0'));if(report.state.phase==='error')throw Error(report.state.message);return report.state.phase==='ready';},'verified download');
  const before=process.platform==='linux'?packages.old.artifact:packages.old.executable;
  assert.equal(digest(await readFile(packages.installed)),digest(await readFile(before)));
  await old.page.evaluate(()=>{void window.dillaDesktop.send({type:'restart-update'});});
  const next=await attach('1.0.1');assert.notEqual(next.pid,old.pid);
  const after=process.platform==='linux'?packages.next.artifact:packages.next.executable;
  assert.equal(digest(await readFile(packages.installed)),digest(await readFile(after)));
  const profileRetained = await readFile(sentinel,'utf8') === 'existing profile retained';
  assert.equal(profileRetained,true);
  assert.deepEqual((await json(join(packages.profile,'desktop.json'))).instances,[instance]);
  await until(async()=>!await owned({...(await reports()).find(report=>report.pid===old.pid)},packages),'old runtime exit');
  const output = join(import.meta.dirname,'../../test-results');
  await mkdir(output,{recursive:true});
  await writeFile(join(output,`${process.platform}-installed-upgrade.json`),JSON.stringify({
   format:1,platform:process.platform,arch:process.arch,engine:process.platform==='linux'?'AppImageUpdater':'NsisUpdater',
   from:old.version,to:next.version,profileRetained,
  })+'\n');
 } finally {
  const observed=await reports().catch(()=>[]);
  for(const report of observed.reverse())if(await owned(report,packages)){
   if(process.platform==='win32')execFileSync(join(process.env.SystemRoot,'System32/taskkill.exe'),['/PID',String(report.pid),'/T','/F'],{stdio:'ignore'});
   else process.kill(report.pid,'SIGTERM');
  }
  await until(async()=>!(await Promise.all(observed.map(report=>owned(report,packages)))).some(Boolean),'owned fixture exit',10000);
  for(const browser of browsers.values())await browser.close().catch(()=>{});
  await packages.dispose();
 }
});
