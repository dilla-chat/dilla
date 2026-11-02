import { updateFS, syncUpdateFile, syncUpdateDirectory, canonicalUpdateUserData } from './update-fs.ts';
const { constants, createReadStream } = updateFS;
const { access, chmod, lstat, mkdir, open, readdir, realpath, rename, rm } = updateFS.promises;
import { createHash, randomBytes } from 'node:crypto';
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from 'node:path';
import { createStartupHealth, type StartupHealth } from './update-health.ts';
import { verifyRelease, newerVersion, type Artifact, type Release } from './update.ts';
import { verifyArtifactFile } from './appimage.ts';
import { readUpdateJournal } from './update-journal.ts';
import { activeInstalledUpdate } from './update-state.ts';
import { verifyWindowsRelease } from './windows-release.ts';
import { verifyWindowsInstallationSnapshot, verifyWindowsProgram, readWindowsSlots, type WindowsInstallation } from './windows-installation.ts';
import { sameWindowsSlot } from './windows-slots.ts';
import { verifyWindowsProgramManifest } from './windows-manifest.ts';
import type { UninstallSnapshot } from './update-registry.ts';
import sandboxConfiguration from '../linux-sandbox-component.json' with { type: 'json' };
import { copySandboxSidecars, readSandboxSidecars, type SandboxSidecarTrust } from './linux-sandbox-cache.ts';

export interface UpdateTrust {
 publicKeys: readonly string[]; platform: 'linux' | 'win32'; arch: string; version: string; channel: 'stable' | 'beta'; now?: number;
}
export interface PrepareUpdate {
 userData: string; installed: string; downloaded: string; metadata: Uint8Array; signature: string; parentPid: number; uninstall?: UninstallSnapshot; windows?: WindowsInstallation; programManifest?: Uint8Array; sandboxDirectory?: string;
}
export interface InstalledUpdatePlan {
 format: 1; platform: 'linux' | 'win32'; arch: string; userData: string; installed: string; parentPid: number; oldVersion: string; activationSecret: string; verifiedAt: number;
 artifactFile: string; watchdog: string; backup: string; oldFingerprint: string; metadata: string; signature: string; health: StartupHealth; uninstall?: UninstallSnapshot; windows?: WindowsInstallation; release: Release;
}
type StoredPlan = Omit<InstalledUpdatePlan, 'release'>;
const NONCE = /^[a-f0-9]{64}$/;
function inside(parent: string, path: string): boolean {
 const part = relative(resolve(parent), resolve(path));
 return part === '' || (!isAbsolute(part) && part !== '..' && !part.startsWith('..' + sep));
}
function separate(userData: string, installed: string, platform: 'linux' | 'win32'): void {
 const program = platform === 'win32' ? dirname(installed) : installed;
 if (!isAbsolute(userData) || !isAbsolute(installed) || inside(program, userData) || inside(userData, program)) throw new Error('The working installation and device data must not overlap');
}
async function hashFile(path: string): Promise<string> {
 const info = await lstat(path);
 if (!info.isFile() || info.isSymbolicLink() || info.size > 4 * 1024 ** 3) throw new Error('Invalid working installation file');
 const hash = createHash('sha512'); for await (const bytes of createReadStream(path)) hash.update(bytes);
 return hash.digest('base64');
}
async function fingerprint(path: string, platform: 'linux' | 'win32', backup = false): Promise<string> {
 if (platform === 'linux') return hashFile(path);
 const hash = createHash('sha512'), root = dirname(path);
 async function visit(directory: string): Promise<void> {
  const info = await lstat(directory);
  if (!info.isDirectory() || info.isSymbolicLink()) throw new Error('Invalid working installation directory');
  for (const name of (await readdir(directory)).sort()) {
   const file = join(directory, name);
   // The extra executable is checked independently against the original exe.
   if (backup && file === join(root, 'dilla-update-watchdog.exe')) continue;
   const entry = await lstat(file), nameInTree = relative(root, file).split(sep).join('/');
   if (entry.isSymbolicLink()) throw new Error('Update backups cannot contain filesystem links');
   if (entry.isDirectory()) { hash.update(JSON.stringify(['directory', nameInTree])); await visit(file); }
   else if (entry.isFile()) hash.update(JSON.stringify(['file', nameInTree, await hashFile(file)]));
   else throw new Error('Unsupported working installation entry');
  }
 }
 await visit(root);
 return hash.digest('base64');
}
async function copyProgram(source: string, destination: string): Promise<void> {
 await mkdir(destination, { mode: 0o700 });
 for (const entry of await readdir(source, { withFileTypes: true })) {
  const from = join(source, entry.name), to = join(destination, entry.name), info = await lstat(from);
  if (info.isSymbolicLink()) throw new Error('Update backups cannot contain filesystem links');
  if (info.isDirectory()) await copyProgram(from, to);
  else if (info.isFile()) { await updateFS.promises.copyFile(from, to, constants.COPYFILE_EXCL); await syncUpdateFile(to); }
  else throw new Error('Unsupported working installation entry');
 }
 await syncUpdateDirectory(destination);
}
function target(release: Release, trust: UpdateTrust): Artifact {
 return release.artifacts.find(artifact => artifact.platform === trust.platform && artifact.arch === trust.arch)!;
}
function sandboxTrust(release: Release, trust: UpdateTrust): SandboxSidecarTrust {
 const artifact = target(release, trust);
 if (!artifact.linuxSandbox || !release.assets) throw new Error('Missing signed Linux sandbox metadata');
 return { version: release.version, descriptor: artifact.linuxSandbox, assets: release.assets, keys: sandboxConfiguration.publicKeys, now: trust.now ?? Date.now() };
}
function authenticate(metadata: Uint8Array, signature: string, trust: UpdateTrust): Release {
 return verifyRelease(metadata, signature, { publicKeys: trust.publicKeys, channel: trust.channel, platform: trust.platform, arch: trust.arch, currentVersion: trust.version, now: trust.now ?? Date.now() });
}
async function storePlan(file: string, plan: StoredPlan): Promise<void> {
 const handle = await open(file, 'wx', 0o600);
 try { await handle.writeFile(JSON.stringify(plan)); await handle.sync(); } finally { await handle.close(); }
}

/** Preparation changes only a fresh private update directory. A copy, permission
 * or disk-space failure leaves the installed app and all device stores intact. */
export async function prepareInstalledUpdate(options: PrepareUpdate, trust: UpdateTrust): Promise<InstalledUpdatePlan> {
 const release = options.windows ? verifyWindowsRelease(options.metadata, options.signature, {publicKeys:trust.publicKeys,channel:trust.channel,platform:trust.platform,arch:trust.arch,currentVersion:trust.version,now:trust.now??Date.now()}) : authenticate(options.metadata, options.signature, trust), artifact = target(release, trust);
 const userData = await canonicalUpdateUserData(resolve(options.userData)), installed = await realpath(options.installed);
 separate(userData, installed, trust.platform);
 if (!Number.isSafeInteger(options.parentPid) || options.parentPid <= 1) throw new Error('Invalid update parent process');
 await access(dirname(installed), constants.W_OK);
 await verifyArtifactFile(options.downloaded, artifact);
 if(options.windows){if(trust.platform!=='win32'||!options.programManifest||!artifact.programManifest)throw new Error('Missing authenticated Windows program layout');await verifyWindowsInstallationSnapshot(options.windows,userData,installed,trust);verifyWindowsProgramManifest(options.programManifest,artifact.programManifest.sha512,{version:release.version,channel:trust.channel,arch:'x64'});if(options.programManifest.byteLength!==artifact.programManifest.size)throw new Error('Windows manifest size differs');}
 const oldFingerprint = await fingerprint(installed, trust.platform);
 const health = await createStartupHealth(userData, release.version);
 try {
  const artifactFile = join(health.directory, trust.platform === 'linux' ? 'update.AppImage' : 'update.exe');
  if (trust.platform === 'linux') {
   if (!options.sandboxDirectory) throw new Error('Missing authenticated sandbox cache');
   await copySandboxSidecars(options.sandboxDirectory, join(health.directory, 'sandbox'), sandboxTrust(release, trust));
  }
  await updateFS.promises.copyFile(options.downloaded, artifactFile, constants.COPYFILE_EXCL); await verifyArtifactFile(artifactFile, artifact); await syncUpdateFile(artifactFile);
  const backup = join(health.directory, trust.platform === 'linux' ? 'previous.AppImage' : 'previous-app');
  let watchdog: string;
  if (trust.platform === 'linux') {
   await updateFS.promises.copyFile(installed, backup, constants.COPYFILE_EXCL); await chmod(backup, 0o700); await syncUpdateFile(backup); watchdog = backup;
  } else {
   await copyProgram(dirname(installed), backup);
   const watchdogRoot = join(health.directory, 'watchdog-app');
   await copyProgram(backup, watchdogRoot);
   watchdog = join(watchdogRoot, 'dilla-update-watchdog.exe');
   await updateFS.promises.copyFile(join(watchdogRoot, basename(installed)), watchdog, constants.COPYFILE_EXCL);
   await syncUpdateFile(watchdog); await syncUpdateDirectory(watchdogRoot);
  }
  const backedExecutable = trust.platform === 'linux' ? backup : join(backup, basename(installed));
  if (await fingerprint(backedExecutable, trust.platform, true) !== oldFingerprint) throw new Error('Working installation changed while preparing its backup');
  const plan: StoredPlan = { format: 1, platform: trust.platform, arch: trust.arch, userData, installed, parentPid: options.parentPid, oldVersion: trust.version, activationSecret: randomBytes(32).toString('hex'), verifiedAt: trust.now ?? Date.now(),
   artifactFile, watchdog, backup, oldFingerprint, metadata: Buffer.from(options.metadata).toString('base64'), signature: options.signature, health, ...(options.uninstall ? { uninstall: options.uninstall } : {}), ...(options.windows ? {windows:options.windows}: {}) };
  if(options.windows){
   for(const[name,bytes]of [['release.json',options.metadata],['release.json.sig',options.signature+'\n'],['old-slot.json',JSON.stringify(options.windows.oldSlot)],['program-manifest.json',options.programManifest!]] as const){const handle=await open(join(health.directory,name),'wx',0o600);try{await handle.writeFile(bytes);await handle.sync();}finally{await handle.close();}}
  }
  await storePlan(join(health.directory, 'plan.json'), plan);
  // Flush bottom-up, including any newly created profile ancestors, before
  // advertising readiness. None of these writes touches the installed program.
  for (let directory = health.directory;; directory = dirname(directory)) {
   await syncUpdateDirectory(directory);
   if (dirname(directory) === directory) break;
  }
  return { ...plan, release };
 } catch (error) { await rm(health.directory, { recursive: true, force: true }); throw error; }
}

async function readPrivatePlan(file: string): Promise<StoredPlan> {
 const link = await lstat(file);
 if (!link.isFile() || link.isSymbolicLink()) throw new Error('Invalid private update plan');
 const handle = await open(file, constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0));
 try {
  const info = await handle.stat();
  if (!info.isFile() || info.size > 2 * 1024 ** 2 || (process.platform !== 'win32' && ((info.mode & 0o077) !== 0 || info.uid !== process.getuid!()))) throw new Error('Invalid private update plan');
  const buffer = Buffer.alloc(2 * 1024 ** 2 + 1), { bytesRead } = await handle.read(buffer, 0, buffer.length, 0);
  if (bytesRead > 2 * 1024 ** 2) throw new Error('Invalid private update plan');
  return JSON.parse(buffer.subarray(0, bytesRead).toString()) as StoredPlan;
 } finally { await handle.close(); }
}

/** The watchdog derives its only plan path from the app's own userData and a
 * nonce. Embedded trust verifies the original release again before installation. */
async function readPlan(userData: string, nonce: string, trust: UpdateTrust & { executable: string }, recovery: boolean, restored = false, complete = false, abandonedPreparation = false): Promise<InstalledUpdatePlan> {
 if (!isAbsolute(userData) || !NONCE.test(nonce)) throw new Error('Invalid update plan paths');
 userData = await canonicalUpdateUserData(userData);
 const directory = join(userData, 'updates', `startup-${nonce}`), info = await lstat(directory);
 if (!info.isDirectory() || info.isSymbolicLink() || (process.platform !== 'win32' && ((info.mode & 0o077) !== 0 || info.uid !== process.getuid!()))
  || await realpath(directory) !== join(await realpath(userData), 'updates', `startup-${nonce}`)) throw new Error('Invalid private update plan directory');
 const plan = await readPrivatePlan(join(directory, 'plan.json'));
 const backup = join(directory, trust.platform === 'linux' ? 'previous.AppImage' : 'previous-app');
 const watchdog = trust.platform === 'linux' ? backup : join(directory, 'watchdog-app/dilla-update-watchdog.exe');
 if (plan.format !== 1 || plan.platform !== trust.platform || plan.arch !== trust.arch || plan.oldVersion !== trust.version || plan.userData !== resolve(userData)
  || plan.backup !== backup || plan.watchdog !== watchdog || plan.artifactFile !== join(directory, trust.platform === 'linux' ? 'update.AppImage' : 'update.exe')
  || plan.health?.directory !== directory || plan.health.file !== join(directory, 'healthy.json') || plan.health.nonce !== nonce
  || typeof plan.activationSecret !== 'string' || !NONCE.test(plan.activationSecret) || plan.activationSecret === nonce
  || !Number.isSafeInteger(plan.verifiedAt) || plan.verifiedAt < 1 || plan.verifiedAt > (trust.now ?? Date.now()) + 300_000
  || !Number.isSafeInteger(plan.parentPid) || plan.parentPid <= 1 || typeof plan.installed !== 'string' || !isAbsolute(plan.installed)
  || typeof plan.metadata !== 'string' || plan.metadata.length > 1_400_000 || !/^[A-Za-z0-9+/]+={0,2}$/.test(plan.metadata)) throw new Error('Invalid update plan paths');
 if (trust.executable !== watchdog || await realpath(watchdog) !== watchdog) throw new Error('Unexpected update helper');
 separate(userData, plan.installed, trust.platform);
 // Recovery only restores the previously verified old program. Reverify the
 // signature at its original acceptance time so an expired download cannot
 // prevent rollback months later; initial installation always checks now.
 const releaseTrust=recovery?{...trust,now:plan.verifiedAt}:trust;
 const release = plan.windows ? verifyWindowsRelease(Buffer.from(plan.metadata,'base64'),plan.signature,{publicKeys:trust.publicKeys,channel:trust.channel,platform:trust.platform,arch:trust.arch,currentVersion:trust.version,now:releaseTrust.now??Date.now()}) : authenticate(Buffer.from(plan.metadata, 'base64'), plan.signature, releaseTrust);
 if (trust.platform === 'linux') await readSandboxSidecars(join(directory, 'sandbox'), sandboxTrust(release, releaseTrust));
 if(plan.windows){
  if(trust.platform!=='win32')throw new Error('Invalid Windows plan platform');
  await verifyWindowsInstallationSnapshot(plan.windows,userData,plan.installed,trust);
  const raw=await updateFS.promises.readFile(join(directory,'release.json')),signature=(await updateFS.promises.readFile(join(directory,'release.json.sig'),'utf8')).trim();
  if(!raw.equals(Buffer.from(plan.metadata,'base64'))||signature!==plan.signature||!Buffer.from(await updateFS.promises.readFile(join(directory,'old-slot.json'))).equals(Buffer.from(JSON.stringify(plan.windows.oldSlot))))throw new Error('Windows fixed provenance differs from accepted plan');
  const artifact=target(release,trust);if(!artifact.programManifest)throw new Error('Missing signed Windows manifest');
  const manifest=await updateFS.promises.readFile(join(directory,'program-manifest.json'));if(manifest.byteLength!==artifact.programManifest.size)throw new Error('Invalid staged Windows manifest size');
  verifyWindowsProgramManifest(manifest,artifact.programManifest.sha512,{version:release.version,channel:trust.channel,arch:'x64'});
 }
 if (plan.health.version !== release.version) throw new Error('Invalid update health version');
 const staged = await lstat(plan.artifactFile);
 if (!staged.isFile() || staged.isSymbolicLink() || await realpath(plan.artifactFile) !== plan.artifactFile) throw new Error('Invalid staged update artifact');
 await verifyArtifactFile(plan.artifactFile, target(release, trust));
 if (!recovery) {
  if (await realpath(plan.installed) !== plan.installed || await fingerprint(plan.installed, trust.platform) !== plan.oldFingerprint) throw new Error('The working installation changed after update preparation');
 } else {
  if (!(complete ? ['complete'] : restored ? ['prepared', 'installing', 'launching', 'recovering', 'failed', 'complete'] : abandonedPreparation ? ['prepared', 'installing', 'launching', 'recovering'] : ['installing', 'launching', 'recovering']).includes((await readUpdateJournal(plan)).phase)) throw new Error('Invalid update recovery phase');
  if (await canonicalUpdateUserData(dirname(plan.installed)) !== dirname(plan.installed)) throw new Error('Invalid recovery installation path');
  try { const entry = await lstat(plan.installed); if (entry.isSymbolicLink() || !entry.isFile()) throw new Error('Invalid recovery installation path'); }
  catch (error) { if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error; }
 }
 const old = trust.platform === 'linux' ? watchdog : join(dirname(watchdog), basename(plan.installed));
 if (await fingerprint(old, trust.platform, true) !== plan.oldFingerprint || (trust.platform === 'win32' && await hashFile(watchdog) !== await hashFile(old))) throw new Error('The update helper differs from the working installation');
 if (trust.platform === 'win32') {
  try { if (await fingerprint(join(backup, basename(plan.installed)), 'win32') !== plan.oldFingerprint) throw new Error('The recovery backup differs from the working installation'); }
  catch (error) {
   // Once the independent backup has been renamed into place, a crash before
   // the final journal flush is recoverable only if the whole installed tree
   // is already exactly the verified old program. The helper is still intact.
   if (!recovery || (error as NodeJS.ErrnoException).code !== 'ENOENT' || !await isOriginalInstallation({ ...plan, release })) throw error;
  }
 }
 return { ...plan, release };
}

export async function isOriginalInstallation(plan: InstalledUpdatePlan): Promise<boolean> {
 try { return await realpath(plan.installed) === plan.installed && await fingerprint(plan.installed, plan.platform) === plan.oldFingerprint; }
 catch { return false; }
}

/** The rollback tree was prepared independently from the executing helper.
 * Rename whole trees rather than needing another program-sized allocation
 * after a disk-full installer. New-only files stay in the nonce-bound quarantine. */
export async function restoreWindowsProgram(plan: InstalledUpdatePlan): Promise<void> {
 if (plan.platform !== 'win32') throw new Error('Invalid Windows recovery platform');
 if (await isOriginalInstallation(plan)) return;
 if(plan.windows)throw new Error('The retained Windows program changed; preserve both programs and reinstall Dilla');
 separate(plan.userData, plan.installed, plan.platform);
 const root = dirname(plan.installed), parent = dirname(root);
 if (root === parent || await realpath(parent) !== parent) throw new Error('Invalid Windows recovery root');
 if (await fingerprint(join(plan.backup, basename(plan.installed)), 'win32') !== plan.oldFingerprint) throw new Error('Invalid Windows recovery backup');
 const failed = root + '.failed-' + plan.health.nonce;
 let moved = false;
 try {
  const info = await lstat(root);
  if (!info.isDirectory() || info.isSymbolicLink() || await realpath(root) !== root) throw new Error('Invalid Windows recovery root');
  try { await lstat(failed); throw new Error('Windows recovery quarantine already exists'); }
  catch (error) { if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error; }
  await rename(root, failed); moved = true; await syncUpdateDirectory(parent);
 } catch (error) { if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error; }
 try { await rename(plan.backup, root); await syncUpdateDirectory(parent); await syncUpdateDirectory(plan.health.directory); }
 catch (error) {
  if (moved) { try { await rename(failed, root); } catch { /* Both independent old copies remain. */ } }
  throw error;
 }
}

export async function readInstalledUpdate(userData: string, nonce: string, trust: UpdateTrust & { executable: string }): Promise<InstalledUpdatePlan> {
 return readPlan(userData, nonce, trust, false);
}
/** Recovery may encounter changed program bytes only in a committed mutation
 * phase. It still authenticates the original descriptor, artifact and backup. */
export async function readRecoverableInstalledUpdate(userData: string, nonce: string, trust: UpdateTrust & { executable: string }, abandonedPreparation = false): Promise<InstalledUpdatePlan> {
 return readPlan(userData, nonce, trust, true, false, false, abandonedPreparation);
}

export async function restoreOriginalAppImage(plan: InstalledUpdatePlan): Promise<void> {
 if (plan.platform !== 'linux' || await hashFile(plan.backup) !== plan.oldFingerprint) throw new Error('Invalid AppImage recovery backup');
 if (await isOriginalInstallation(plan)) return;
 if (await realpath(dirname(plan.installed)) !== dirname(plan.installed)) throw new Error('Invalid AppImage recovery path');
 try { const info = await lstat(plan.installed); if (info.isSymbolicLink() || !info.isFile()) throw new Error('Invalid AppImage recovery path'); }
 catch (error) { if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error; }
 // replaceAppImage already reserved this old file on the destination volume.
 // Prefer its authenticated rename to a new full copy when that volume is full;
 // the executing independent helper backup is never renamed out from under it.
 const previous = plan.installed + '.previous';
 try {
  const info = await lstat(previous);
  if (info.isFile() && !info.isSymbolicLink() && await realpath(previous) === previous && await hashFile(previous) === plan.oldFingerprint) {
   await rename(previous, plan.installed); await syncUpdateDirectory(dirname(plan.installed)); return;
  }
 } catch (error) { if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error; }
 const stage = plan.installed + '.restore-' + plan.health.nonce;
 try {
  await updateFS.promises.copyFile(plan.backup, stage, constants.COPYFILE_EXCL); await chmod(stage, 0o700);
  if (await hashFile(stage) !== plan.oldFingerprint) throw new Error('Invalid staged AppImage recovery');
  await syncUpdateFile(stage); await rename(stage, plan.installed); await syncUpdateDirectory(dirname(plan.installed));
 } finally { await rm(stage, { force: true }); }
}

/** A normally restarted app may be the new version. It can hand recovery to
 * the independently verified old copy only for its own exact installation. */
async function readRestartPlan(userData: string, nonce: string, trust: UpdateTrust & { installed: string }, mode: 'prepared' | 'interrupted' | 'complete'): Promise<InstalledUpdatePlan> {
 if (!NONCE.test(nonce)) throw new Error('Invalid update restart nonce');
 const root = await canonicalUpdateUserData(userData), directory = join(root, 'updates', `startup-${nonce}`);
 const stored = await readPrivatePlan(join(directory, 'plan.json'));
 if (stored.installed !== trust.installed && (!stored.windows || updatedWindowsExecutable({...stored,release:JSON.parse(Buffer.from(stored.metadata,'base64').toString()) as Release})!==trust.installed)) throw new Error('Update recovery belongs to another installation');
 // A newer unrelated old version cannot select this app as its recovery host.
 if (typeof stored.oldVersion !== 'string' || newerVersion(stored.oldVersion, trust.version)) throw new Error('Invalid recovery installation version');
 const watchdog = trust.platform === 'linux' ? join(directory, 'previous.AppImage') : join(directory, 'watchdog-app/dilla-update-watchdog.exe');
 if (mode === 'complete' && stored.oldVersion !== trust.version && stored.health?.version !== trust.version) throw new Error('Invalid completed installation version');
 const plan = await readPlan(root, nonce, { ...trust, version: stored.oldVersion, executable: watchdog }, true, mode === 'prepared', mode === 'complete');
 if (mode === 'prepared' && !await isOriginalInstallation(plan)) throw new Error('The prepared working installation changed');
 return plan;
}
export async function readUpdateForRestart(userData: string, nonce: string, trust: UpdateTrust & { installed: string }): Promise<InstalledUpdatePlan> {
 return readRestartPlan(userData, nonce, trust, 'interrupted');
}
/** Observe a prepared helper without authorizing any mutation/recovery. */
export async function readPreparedUpdateForRestart(userData: string, nonce: string, trust: UpdateTrust & { installed: string }): Promise<InstalledUpdatePlan> {
 return readRestartPlan(userData, nonce, trust, 'prepared');
}
/** Observation only: no rollback reader accepts this completed phase. */
export async function readCompletedUpdateForRestart(userData: string, nonce: string, trust: UpdateTrust & { installed: string }): Promise<InstalledUpdatePlan> {
 return readRestartPlan(userData, nonce, trust, 'complete');
}

/** An owned rollback launch can survive a stale journal when disk quota prevents
 * recording failure. This marker grants no program/path authority: the exact
 * active transaction, signature/artifact, independent helper and entire restored
 * old installation/version must all verify before ordinary startup is allowed. */
export async function readRestoredInstalledUpdate(userData: string, nonce: string, trust: UpdateTrust & { installed: string }): Promise<InstalledUpdatePlan> {
 const pending = await activeInstalledUpdate(userData);
 if (!pending || pending.nonce !== nonce || !NONCE.test(nonce)) throw new Error('Invalid restored update transaction');
 const root = await canonicalUpdateUserData(userData), directory = join(root, 'updates', `startup-${nonce}`);
 const stored = await readPrivatePlan(join(directory, 'plan.json'));
 if (stored.oldVersion !== trust.version) throw new Error('Invalid restored update version');
 if (stored.installed !== trust.installed) throw new Error('Invalid restored update installation');
 const watchdog = trust.platform === 'linux' ? join(directory, 'previous.AppImage') : join(directory, 'watchdog-app/dilla-update-watchdog.exe');
 const plan = await readPlan(root, nonce, { ...trust, executable: watchdog }, true, true);
 if (!await isOriginalInstallation(plan)) throw new Error('The working installation has not been restored');
 if(plan.windows){
  const slotTrust={publicKeys:trust.publicKeys,channel:trust.channel,arch:trust.arch,initialVersion:plan.windows.initialVersion,initialManifestSha512:plan.windows.initialManifestSha512},provenance=join(root,'updates');
  const selected=await readWindowsSlots(plan.windows.root,slotTrust,provenance);if(!sameWindowsSlot(selected,plan.windows.oldSlot))throw new Error('The restored Windows slot is no longer selected');
 }
 return plan;
}

export function updatedWindowsExecutable(plan:InstalledUpdatePlan):string{
 const digest=plan.release?.artifacts.find(file=>file.platform==='win32'&&file.arch===plan.arch)?.programManifest?.sha512;
 if(!plan.windows||!digest)throw new Error('Missing Windows active program authority');
 return join(plan.windows.root,'versions','v-'+Buffer.from(digest,'base64').toString('hex').slice(0,32),'dilla-desktop.exe');
}
export async function verifyUpdatedWindowsProgram(plan:InstalledUpdatePlan):Promise<string>{
 const artifact=plan.release.artifacts.find(file=>file.platform==='win32'&&file.arch===plan.arch)!;if(!plan.windows||!artifact.programManifest)throw new Error('Missing Windows program manifest');
 return verifyWindowsProgram(plan.windows.root,{format:1,kind:'release',generation:plan.windows.oldSlot.generation+1,version:plan.health.version,manifestSha512:artifact.programManifest.sha512,nonce:plan.health.nonce,acceptedAt:plan.verifiedAt},{channel:plan.release.channel,arch:plan.arch});
}
