import { createHash, createPublicKey } from 'node:crypto';
import { cp, mkdir, mkdtemp, open, readFile, readdir, rename, rm, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import { execFileSync } from 'node:child_process';
import { createWindowsProgramManifest, verifyWindowsProgramManifest, verifyWindowsProgramTree, windowsProgramID } from '../src/windows-manifest.ts';
import { describeCoffResource } from './windows-coff-diagnostic.mjs';
const desktop = resolve(import.meta.dirname, '..'), repository = resolve(desktop, '../..');
const sha512 = raw => createHash('sha512').update(raw).digest('base64');
function windowsEnvironment() {
 const env = { ...process.env };
 if (env.VCINSTALLDIR) return env;
 const vswhere = join(env['ProgramFiles(x86)'] ?? 'C:/Program Files (x86)', 'Microsoft Visual Studio/Installer/vswhere.exe');
 const installation = execFileSync(vswhere, ['-latest', '-products', '*', '-requires', 'Microsoft.VisualStudio.Component.VC.Tools.x86.x64', '-property', 'installationPath'], { encoding: 'utf8' }).trim();
 if (!installation) throw new Error('Visual Studio SDK is required to brand the Windows launcher');
 const variables = execFileSync(env.ComSpec ?? 'cmd.exe', ['/d', '/s', '/c', `call "${join(installation, 'VC/Auxiliary/Build/vcvars64.bat')}" >nul && set`], { windowsVerbatimArguments: true, encoding: 'utf8' });
 for (const line of variables.split(/\r?\n/)) { const split = line.indexOf('='); if (split > 0) env[line.slice(0, split)] = line.slice(split + 1); }
 return env;
}
async function compileNativeLauncher({ file, config, iconFile }) {
 if (process.platform !== 'win32' || !iconFile || config.arch !== 'x64') throw new Error('Build the branded Windows launcher on native Windows x64');
 const root = await mkdtemp(join(tmpdir(), 'dilla-launcher-build-'));
 try {
  const packageDirectory = join(root, 'packages/desktop/native/windows-launcher');
  await mkdir(packageDirectory, { recursive: true });
  await cp(join(repository, 'go.mod'), join(root, 'go.mod')); await cp(join(repository, 'go.sum'), join(root, 'go.sum'));
  await cp(join(repository, 'internal/desktoplaunch'), join(root, 'internal/desktoplaunch'), { recursive: true, filter: path => !path.endsWith('_test.go') });
  await cp(join(desktop, 'native/windows-launcher'), packageDirectory, { recursive: true, filter: path => !path.endsWith('_test.go') && !path.endsWith('.syso') });
  await cp(iconFile, join(packageDirectory, 'icon.ico'));
  const version = config.initialVersion.split('-')[0].split('.').map(Number);
  if (version.length !== 3 || version.some(part => !Number.isInteger(part) || part < 0 || part > 65535)) throw new Error('Windows VERSIONINFO cannot represent this version');
  const numeric = version.join(',') + ',0';
  const resource = `#include <windows.h>\n1 ICON "icon.ico"\n1 RT_MANIFEST "launcher.manifest"\n1 VERSIONINFO\nFILEVERSION ${numeric}\nPRODUCTVERSION ${numeric}\nFILEFLAGSMASK 0x3fL\nFILEFLAGS 0\nFILEOS VOS_NT_WINDOWS32\nFILETYPE VFT_APP\nFILESUBTYPE 0\nBEGIN\n BLOCK "StringFileInfo" BEGIN BLOCK "040904b0" BEGIN\n VALUE "FileDescription", "${config.productName}\\0"\n VALUE "ProductName", "${config.productName}\\0"\n VALUE "InternalName", "Dilla\\0"\n VALUE "OriginalFilename", "Dilla.exe\\0"\n VALUE "FileVersion", "${config.initialVersion}\\0"\n VALUE "ProductVersion", "${config.initialVersion}\\0"\n VALUE "LegalCopyright", "Dilla contributors\\0"\n END END\n BLOCK "VarFileInfo" BEGIN VALUE "Translation", 0x409, 1200 END\nEND\n`;
  await writeFile(join(packageDirectory, 'branding.rc'), resource);
  await writeFile(join(packageDirectory, 'launcher.manifest'), '<?xml version="1.0" encoding="UTF-8" standalone="yes"?><assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0"><trustInfo xmlns="urn:schemas-microsoft-com:asm.v3"><security><requestedPrivileges><requestedExecutionLevel level="asInvoker" uiAccess="false"/></requestedPrivileges></security></trustInfo><application xmlns="urn:schemas-microsoft-com:asm.v3"><windowsSettings><longPathAware xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">true</longPathAware></windowsSettings></application></assembly>');
  const env = windowsEnvironment();
  execFileSync('rc.exe', ['/nologo', '/fo', 'branding.res', 'branding.rc'], { cwd: packageDirectory, env, stdio: 'inherit' });
  execFileSync('cvtres.exe', ['/NOLOGO', '/MACHINE:X64', '/OUT:branding.syso', 'branding.res'], { cwd: packageDirectory, env, stdio: 'inherit' });
  const encoded = Buffer.from(JSON.stringify(config)).toString('base64');
  try {
   execFileSync('go', ['build', '-trimpath', '-ldflags', `-H=windowsgui -s -w -X main.bootstrapConfig=${encoded}`, '-o', file, './packages/desktop/native/windows-launcher'], { cwd: root, env, stdio: 'inherit' });
  } catch (error) {
   // Preserve the original link failure even if diagnostic collection fails.
   try {
    const object = await open(join(packageDirectory, 'branding.syso'), 'r');
    try {
     const size = (await object.stat()).size;
     if (size > 16 * 1024 * 1024) console.error('Windows launcher COFF diagnostic: object exceeds size limit');
     else {
      const bytes = Buffer.alloc(size), { bytesRead } = await object.read(bytes, 0, size, 0);
      console.error('Windows launcher COFF diagnostic:', JSON.stringify(describeCoffResource(bytes.subarray(0, bytesRead))));
     }
    } finally { await object.close(); }
   } catch { console.error('Windows launcher COFF diagnostic: unavailable'); }
   throw error;
  }
 } finally { await rm(root, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 }); }
}
/** Run ONLY after Electron's ASAR integrity, fuses, PE edits and signing finish.
 * The stable launcher sits outside the manifest and never hashes itself. */
export async function finalizeWindowsProgram({ appOutDir, outDir, version, arch, productName, appId, publicKeys, iconFile, compileLauncher = compileNativeLauncher, signLauncher = () => Promise.resolve() }) {
 if (arch !== 'x64' || !/^[A-Za-z][A-Za-z0-9._ -]{0,63}$/.test(productName) || productName.trim() !== productName || productName.endsWith('.')
  || !/^[A-Za-z][A-Za-z0-9.-]{0,127}$/.test(appId) || appId.endsWith('.') || !Array.isArray(publicKeys) || !publicKeys.length || publicKeys.length > 8
  || publicKeys.some(key => { try { return typeof key !== 'string' || !/^-----BEGIN PUBLIC KEY-----\r?\n[A-Za-z0-9+/=\r\n]+-----END PUBLIC KEY-----\s*$/.test(key) || createPublicKey(key).asymmetricKeyType !== 'ed25519'; } catch { return true; } })) throw new Error('Invalid compiled Windows bootstrap identity/trust');
 const channel = version.includes('-beta.') ? 'beta' : 'stable', context = { version, channel, arch };
 const raw = await createWindowsProgramManifest(appOutDir, context), digest = sha512(raw), manifest = verifyWindowsProgramManifest(raw, digest, context);
 if (!manifest.files.some(file => file.path === 'dilla-desktop.exe' && file.size > 0) || !manifest.files.some(file => file.path === 'resources/app.asar' && file.size > 0)) throw new Error('Missing final Electron executable or ASAR');
 const entries = await readdir(appOutDir), programDirectory = join(appOutDir, 'versions', windowsProgramID(digest));
 await mkdir(programDirectory, { recursive: true });
 for (const entry of entries) await rename(join(appOutDir, entry), join(programDirectory, entry));
 await mkdir(join(appOutDir, 'manifests'));
 const manifestFile = join(appOutDir, 'manifests', Buffer.from(digest, 'base64').toString('hex') + '.json');
 await writeFile(manifestFile, raw, { flag: 'wx' });
 const config = { format: 1, appId, productName, channel, arch, initialVersion: version, initialManifestSha512: digest, publicKeys };
 const launcher = join(appOutDir, 'Dilla.exe'); await compileLauncher({ file: launcher, config, iconFile }); await signLauncher(launcher);
 await writeFile(join(appOutDir, 'initial-slot.json'), JSON.stringify({ format: 1, kind: 'initial', generation: 0, version, manifestSha512: digest }) + '\n', { flag: 'wx' });
 await mkdir(outDir, { recursive: true });
 const publishedManifest = join(outDir, `dilla-${version}-win-${arch}.manifest.json`); await writeFile(publishedManifest, raw);
 const state = { appOutDir, programDirectory, manifestFile, publishedManifest, manifest, digest, config, launcherDigest: sha512(await readFile(launcher)), initialSlotDigest: sha512(await readFile(join(appOutDir, 'initial-slot.json'))) };
 await assertWindowsProgramUnchanged(state); return state;
}
/** Called again immediately before the NSIS payload is archived. A late fuse,
 * resource, signing or other file mutation must fail the build. */
export async function assertWindowsProgramUnchanged(state) {
 if (sha512(await readFile(join(state.appOutDir, 'Dilla.exe'))) !== state.launcherDigest || sha512(await readFile(join(state.appOutDir, 'initial-slot.json'))) !== state.initialSlotDigest) throw new Error('Windows bootstrap launcher or initial slot changed before archive');
 const raw = await readFile(state.manifestFile);
 verifyWindowsProgramManifest(raw, state.digest, state.manifest);
 await verifyWindowsProgramTree(state.programDirectory, state.manifest);
 if (!Buffer.from(raw).equals(await readFile(state.publishedManifest))) throw new Error('Published Windows manifest changed before archive');
}
