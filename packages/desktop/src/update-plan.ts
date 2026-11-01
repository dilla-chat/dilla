import { updateFS, syncUpdateFile, syncUpdateDirectory, canonicalUpdateUserData } from './update-fs.ts';
const { constants, createReadStream } = updateFS;
const { access, chmod, copyFile, lstat, mkdir, open, readdir, realpath, rm } = updateFS.promises;
import { createHash } from 'node:crypto';
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from 'node:path';
import { createStartupHealth, type StartupHealth } from './update-health.ts';
import { verifyRelease, type Artifact, type Release } from './update.ts';
import { verifyArtifactFile } from './appimage.ts';

export interface UpdateTrust {
 publicKeys: readonly string[]; platform: 'linux' | 'win32'; arch: string; version: string; channel: 'stable' | 'beta'; now?: number;
}
export interface PrepareUpdate {
 userData: string; installed: string; downloaded: string; metadata: Uint8Array; signature: string; parentPid: number;
}
export interface InstalledUpdatePlan {
 format: 1; platform: 'linux' | 'win32'; userData: string; installed: string; parentPid: number; oldVersion: string;
 artifactFile: string; watchdog: string; backup: string; oldFingerprint: string; metadata: string; signature: string; health: StartupHealth; release: Release;
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
  else if (info.isFile()) { await copyFile(from, to, constants.COPYFILE_EXCL); await syncUpdateFile(to); }
  else throw new Error('Unsupported working installation entry');
 }
 await syncUpdateDirectory(destination);
}
function target(release: Release, trust: UpdateTrust): Artifact {
 return release.artifacts.find(artifact => artifact.platform === trust.platform && artifact.arch === trust.arch)!;
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
 const release = authenticate(options.metadata, options.signature, trust), artifact = target(release, trust);
 const userData = await canonicalUpdateUserData(resolve(options.userData)), installed = await realpath(options.installed);
 separate(userData, installed, trust.platform);
 if (!Number.isSafeInteger(options.parentPid) || options.parentPid <= 1) throw new Error('Invalid update parent process');
 await access(dirname(installed), constants.W_OK);
 await verifyArtifactFile(options.downloaded, artifact);
 const oldFingerprint = await fingerprint(installed, trust.platform);
 const health = await createStartupHealth(userData, release.version);
 try {
  const artifactFile = join(health.directory, trust.platform === 'linux' ? 'update.AppImage' : 'update.exe');
  await copyFile(options.downloaded, artifactFile, constants.COPYFILE_EXCL); await verifyArtifactFile(artifactFile, artifact); await syncUpdateFile(artifactFile);
  const backup = join(health.directory, trust.platform === 'linux' ? 'previous.AppImage' : 'previous-app');
  let watchdog: string;
  if (trust.platform === 'linux') {
   await copyFile(installed, backup, constants.COPYFILE_EXCL); await chmod(backup, 0o700); await syncUpdateFile(backup); watchdog = backup;
  } else {
   await copyProgram(dirname(installed), backup);
   watchdog = join(backup, 'dilla-update-watchdog.exe');
   await copyFile(join(backup, basename(installed)), watchdog, constants.COPYFILE_EXCL);
   await syncUpdateFile(watchdog); await syncUpdateDirectory(backup);
  }
  const backedExecutable = trust.platform === 'linux' ? backup : join(backup, basename(installed));
  if (await fingerprint(backedExecutable, trust.platform, true) !== oldFingerprint) throw new Error('Working installation changed while preparing its backup');
  const plan: StoredPlan = { format: 1, platform: trust.platform, userData, installed, parentPid: options.parentPid, oldVersion: trust.version,
   artifactFile, watchdog, backup, oldFingerprint, metadata: Buffer.from(options.metadata).toString('base64'), signature: options.signature, health };
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
export async function readInstalledUpdate(userData: string, nonce: string, trust: UpdateTrust & { executable: string }): Promise<InstalledUpdatePlan> {
 if (!isAbsolute(userData) || !NONCE.test(nonce)) throw new Error('Invalid update plan paths');
 userData = await canonicalUpdateUserData(userData);
 const directory = join(userData, 'updates', `startup-${nonce}`), info = await lstat(directory);
 if (!info.isDirectory() || info.isSymbolicLink() || (process.platform !== 'win32' && ((info.mode & 0o077) !== 0 || info.uid !== process.getuid!()))
  || await realpath(directory) !== join(await realpath(userData), 'updates', `startup-${nonce}`)) throw new Error('Invalid private update plan directory');
 const plan = await readPrivatePlan(join(directory, 'plan.json'));
 const backup = join(directory, trust.platform === 'linux' ? 'previous.AppImage' : 'previous-app');
 const watchdog = trust.platform === 'linux' ? backup : join(backup, 'dilla-update-watchdog.exe');
 if (plan.format !== 1 || plan.platform !== trust.platform || plan.oldVersion !== trust.version || plan.userData !== resolve(userData)
  || plan.backup !== backup || plan.watchdog !== watchdog || plan.artifactFile !== join(directory, trust.platform === 'linux' ? 'update.AppImage' : 'update.exe')
  || plan.health?.directory !== directory || plan.health.file !== join(directory, 'healthy.json') || plan.health.nonce !== nonce
  || !Number.isSafeInteger(plan.parentPid) || plan.parentPid <= 1 || typeof plan.installed !== 'string' || !isAbsolute(plan.installed)
  || typeof plan.metadata !== 'string' || plan.metadata.length > 1_400_000 || !/^[A-Za-z0-9+/]+={0,2}$/.test(plan.metadata)) throw new Error('Invalid update plan paths');
 if (trust.executable !== watchdog || await realpath(watchdog) !== watchdog) throw new Error('Unexpected update helper');
 separate(userData, plan.installed, trust.platform);
 const release = authenticate(Buffer.from(plan.metadata, 'base64'), plan.signature, trust);
 if (plan.health.version !== release.version) throw new Error('Invalid update health version');
 const staged = await lstat(plan.artifactFile);
 if (!staged.isFile() || staged.isSymbolicLink() || await realpath(plan.artifactFile) !== plan.artifactFile) throw new Error('Invalid staged update artifact');
 await verifyArtifactFile(plan.artifactFile, target(release, trust));
 if (await realpath(plan.installed) !== plan.installed || await fingerprint(plan.installed, trust.platform) !== plan.oldFingerprint) throw new Error('The working installation changed after update preparation');
 const old = trust.platform === 'linux' ? watchdog : join(backup, basename(plan.installed));
 if (await fingerprint(old, trust.platform, true) !== plan.oldFingerprint || (trust.platform === 'win32' && await hashFile(watchdog) !== await hashFile(old))) throw new Error('The update helper differs from the working installation');
 return { ...plan, release };
}
