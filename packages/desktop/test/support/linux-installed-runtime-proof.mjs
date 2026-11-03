import assert from 'node:assert/strict';
import { open, readFile, realpath } from 'node:fs/promises';
import { constants } from 'node:fs';
import { dirname, basename, resolve, isAbsolute } from 'node:path';
import { pathToFileURL } from 'node:url';
import { kernelFacts } from './linux-sandbox-foundation.mjs';
const validPid = pid => Number.isSafeInteger(pid) && pid > 1;
const requireFact = condition => assert.ok(condition, 'Invalid installed AppImage sandbox observation');
async function identity(pid) {
 requireFact(validPid(pid));
 const [text,status] = await Promise.all([readFile(`/proc/${pid}/stat`, 'utf8'),readFile(`/proc/${pid}/status`, 'utf8')]);
 const fields = text.slice(text.lastIndexOf(')') + 2).split(' '), uid = /^Uid:\s+(.*)$/m.exec(status)?.[1].trim().split(/\s+/).map(Number);
 requireFact(uid?.length === 4 && uid.every(value => value === process.getuid()) && process.getuid() > 0);
 requireFact(['R','S','D','T','t','I'].includes(fields[0]) && /^\d+$/.test(fields[19]));
 return fields[19];
}
export async function readRuntimeReport(file) {
 const handle = await open(file, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
 try {
  const info = await handle.stat(); requireFact(info.isFile() && info.size > 0 && info.size <= 16384);
  const bytes = Buffer.alloc(16385), {bytesRead} = await handle.read(bytes, 0, bytes.length, 0);
  requireFact(bytesRead <= 16384);
  return JSON.parse(bytes.subarray(0, bytesRead).toString('utf8'));
 } finally { await handle.close(); }
}
function verifyObservation(r) {
 requireFact(r?.format === 1 && validPid(r.mainPid) && validPid(r.rendererPid) && r.mainPid !== r.rendererPid
  && typeof r.start === 'string' && /^\d+$/.test(r.start) && Number.isSafeInteger(r.uid) && r.uid > 0
  && r.sandbox === true && r.contextIsolation === true && r.nodeIntegration === false
  && typeof r.executable === 'string' && isAbsolute(r.executable) && resolve(r.executable) === r.executable && basename(r.executable) === 'dilla-desktop'
  && r.mount?.point === dirname(r.executable) && /^\.mount_[^/]+$/.test(basename(r.mount.point))
  && r.mount.type === 'fuse.Dilla.AppImage');
 const f = r.facts;
 requireFact(f && [f.mainUid,f.rendererUid].every(uids => Array.isArray(uids) && uids.length === 4 && uids.every(uid => uid === r.uid))
  && f.rendererExecutable === r.executable && typeof f.rendererCommand === 'string'
  && /(?:^|\s)--type=renderer(?:\s|$)/.test(f.rendererCommand)
  && !/(?:^|\s)--(?:no-sandbox|disable-setuid-sandbox)(?:[=\s]|$)/.test(f.rendererCommand)
  && f.seccomp === 2 && f.noNewPrivs === 1 && f.capEff === 0 && f.descendant === true);
}
/** Only the owned fixture main supplies Electron's renderer PID/preferences.
 * Kernel observation remains outside the app; the renderer cannot self-certify. */
export async function observeInstalledRuntime(report, expected) {
 requireFact(report?.format === 1 && validPid(report.mainPid) && validPid(report.rendererPid)
  && report.mainPid !== report.rendererPid && report.mainPid === expected?.pid
  && report.sandbox === true && report.contextIsolation === true && report.nodeIntegration === false
  && report.version === expected.version && typeof expected.start === 'string' && /^\d+$/.test(expected.start)
  && process.platform === 'linux' && process.getuid() > 0);
 requireFact(await identity(report.mainPid) === expected.start);
 const rendererStart = await identity(report.rendererPid);
 const executable = await realpath(`/proc/${report.mainPid}/exe`);
 requireFact(executable === report.executable);
 const facts = await kernelFacts(report.mainPid, report.rendererPid);
 const mountinfo = await readFile(`/proc/${report.mainPid}/mountinfo`, 'utf8');
 const mounts = mountinfo.trim().split('\n').map(line => {
  const [before,after] = line.split(' - ');
  return {point:before.split(' ')[4].replace(/\\([0-7]{3})/g, (_,octal) => String.fromCharCode(parseInt(octal,8))),type:after?.split(' ')[0]};
 });
 const mount = mounts.find(m => m.point === dirname(executable));
 requireFact(await identity(report.mainPid) === expected.start && await identity(report.rendererPid) === rendererStart
  && await realpath(`/proc/${report.mainPid}/exe`) === executable);
 const result = {...report,start:expected.start,uid:process.getuid(),mount,facts};
 verifyObservation(result);
 return result;
}
export function verifyInstalledRuntimeProof(report) {
 requireFact(report?.format === 1 && report.platform === 'linux' && report.arch === 'x64' && report.engine === 'AppImageUpdater'
  && report.from === '1.0.0' && report.to === '1.0.1' && report.profileRetained === true
  && Array.isArray(report.sandbox) && report.sandbox.length === 2);
 for (const [index,version] of ['1.0.0','1.0.1'].entries()) {
  requireFact(report.sandbox[index]?.version === version); verifyObservation(report.sandbox[index]);
 }
 requireFact(report.sandbox[0].mainPid !== report.sandbox[1].mainPid);
 return true;
}
if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
 requireFact(process.argv.length === 3);
 verifyInstalledRuntimeProof(await readRuntimeReport(process.argv[2]));
 console.log('Initial and upgraded installed AppImages: live kernel sandbox observations verified');
}
