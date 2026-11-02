import { LINUX_INSTALL_KIND, assertLinuxSelfUpdate } from './linux-build.ts';
import { NativeQuitInstaller, UnstoppedUpdateHelper } from './quit-install.ts';
import { loadSparkle, type SparkleInstaller } from './sparkle.ts';
import { app } from 'electron';
import { AppImageUpdater, NsisUpdater, type AppUpdater } from 'electron-updater';
import { Provider, type ProviderRuntimeOptions } from 'electron-updater/out/providers/Provider.js';
import type { UpdateInfo, ResolvedUpdateFileInfo } from 'electron-updater/out/types.js';
import { rm } from 'node:fs/promises';
import { currentWindowsInstallation } from './windows-installation.ts';
import { verifyWindowsRelease } from './windows-release.ts';
import { verifyWindowsProgramManifest } from './windows-manifest.ts';
import { prepareInstalledUpdate } from './update-plan.ts';
import { launchUpdateWatchdog } from './update-runtime.ts';
import { snapshotUninstall } from './update-registry.ts';
import { activateInstalledUpdate } from './update-state.ts';
import { writeUpdateJournal } from './update-journal.ts';
import type { CustomPublishOptions } from 'builder-util-runtime';
import { RELEASE_PUBLIC_KEYS } from './update-keys.ts';
import { newerVersion, verifyRelease, UpdateCoordinator, type Artifact, type Release, type UpdateState } from './update.ts';
import { verifyArtifactFile } from './appimage.ts';

const channel = app.getVersion().includes('-beta.') ? 'beta' : 'stable';
const feed = `https://github.com/jonasthim/dilla/releases/download/desktop-${channel}/desktop-${channel}.json`;

async function boundedFetch(url: string, maxBytes: number): Promise<Uint8Array> {
  const response = await fetch(url, { signal: AbortSignal.timeout(30_000), cache: 'no-store' });
  if (!response.ok || !response.body) throw new Error(`Update download failed (${response.status}); try again`);
  const chunks: Uint8Array[] = [];
  let size = 0;
  const reader = response.body.getReader();
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > maxBytes) throw new Error('Update metadata is too large');
      chunks.push(value);
    }
  } finally { await reader.cancel(); }
  return Buffer.concat(chunks);
}

/** The OS updater sees only a descriptor already authenticated by the pinned release key. */
class PreparedProvider extends Provider<UpdateInfo> {
  private artifact: Artifact;
  private release: Release;
  constructor(options: CustomPublishOptions, _updater: AppUpdater, runtime: ProviderRuntimeOptions) {
    super({ ...runtime, isUseMultipleRangeRequest: false }); this.artifact = options.artifact as Artifact; this.release = options.release as Release;
  }
  async getLatestVersion(): Promise<UpdateInfo> {
    const file = { url: this.artifact.url, sha512: this.artifact.sha512, size: this.artifact.size };
    return { version: this.release.version, releaseDate: this.release.publishedAt, files: [file], path: file.url, sha512: file.sha512 };
  }
  resolveFiles(): ResolvedUpdateFileInfo[] {
    return [{ url: new URL(this.artifact.url), info: { url: this.artifact.url, sha512: this.artifact.sha512, size: this.artifact.size } }];
  }
}

export function createUpdater(changed: (state: UpdateState) => void, failedQuit: () => void): UpdateCoordinator {
  let native: AppImageUpdater | NsisUpdater | null = null;
  let sparkle: SparkleInstaller | null = null;
  let target: Artifact | null = null;
  let downloadedPath: string | null = null;
  let programManifest: Uint8Array | undefined;
  let descriptor: { metadata: Uint8Array; signature: string } | null = null;
  // Do not instantiate Electron's macOS updater: unsigned builds MUST use the Sparkle bridge.
  let coordinator: UpdateCoordinator;
  const installer = new NativeQuitInstaller(app, error => { coordinator.installationFailed(error); failedQuit(); });
  const packageManaged = process.platform === 'linux' && LINUX_INSTALL_KIND === 'system-package';
  coordinator = new UpdateCoordinator({
    ...(packageManaged ? { ownership: 'package-manager' as const } : {}),
    check: async () => {
      if (!app.isPackaged) throw new Error('Automatic updates need an installed acceptance build');
      if (RELEASE_PUBLIC_KEYS.length === 0) throw new Error('Launch blocked: no desktop release signing key is embedded');
      if (process.platform === 'linux' && !packageManaged) {
        assertLinuxSelfUpdate();
        if (!process.env.APPIMAGE) throw new Error('Updates require the installed AppImage');
      }
      const [bytes, signature] = await Promise.all([boundedFetch(feed, 1_048_576), boundedFetch(feed + '.sig', 128)]);
      const release = (process.platform === 'win32' ? verifyWindowsRelease : verifyRelease)(bytes, Buffer.from(signature).toString('utf8').trim(), {
        publicKeys: RELEASE_PUBLIC_KEYS, channel, platform: process.platform, arch: process.arch,
        currentVersion: '0.0.0', now: Date.now(),
      });
      if (!newerVersion(release.version, app.getVersion())) return null;
      if (packageManaged) return release;
      descriptor = { metadata: bytes, signature: Buffer.from(signature).toString('utf8').trim() };
      target = release.artifacts.find(a => a.platform === process.platform && a.arch === process.arch)!;
      if (process.platform === 'darwin') { sparkle ??= loadSparkle(process.resourcesPath, app); return release; }
      // A new native object on every retry also discards electron-updater's in-memory cached download descriptor.
      native = process.platform === 'win32' ? new NsisUpdater() : process.platform === 'linux' ? new AppImageUpdater() : null;
      if (!native) throw new Error('No native updater for this platform');
      native.autoDownload = false; native.autoInstallOnAppQuit = false; native.allowDowngrade = false;
      native.allowPrerelease = channel === 'beta'; native.disableDifferentialDownload = true;
      native.on('error', () => {}); // Rejected check/download promises carry the error to the coordinator.
      native.setFeedURL({ provider: 'custom', updateProvider: PreparedProvider, release, artifact: target });
      const checked = await native.checkForUpdates();
      if (!checked?.isUpdateAvailable) return null;
      return release;
    },
    download: async release => {
      if (process.platform === 'linux') assertLinuxSelfUpdate();
      if (process.platform === 'darwin' && sparkle && target) { await sparkle.prepare(release, target); return; }
      if (!native || !target) throw new Error('No verified update');
      programManifest = undefined;
      if(process.platform==='win32'){if(!target.programManifest)throw new Error('Missing authenticated Windows manifest');programManifest=await boundedFetch(target.programManifest.url,1048576);if(programManifest.byteLength!==target.programManifest.size)throw new Error('Windows manifest size differs');verifyWindowsProgramManifest(programManifest,target.programManifest.sha512,{version:release.version,channel,arch:'x64'});}
      const files = await native.downloadUpdate();
      if (files.length !== 1) throw new Error('Unexpected update artifacts');
      try { await verifyArtifactFile(files[0], target); downloadedPath = files[0]; }
      catch (error) { await rm(files[0], { force: true }); downloadedPath = null; throw error; }
    },
    cancelledQuit: () => { sparkle?.cancelledQuit(); installer.cancel(); },
    install: async () => {
      if (process.platform === 'linux') assertLinuxSelfUpdate();
      if (process.platform === 'darwin' && sparkle) { sparkle.install(); return; }
      if (!native || !target || !downloadedPath) throw new Error('No verified native update');
      try { await verifyArtifactFile(downloadedPath, target); }
      catch (error) { await rm(downloadedPath, { force: true }); downloadedPath = null; throw error; }
      if (!descriptor || !['linux', 'win32'].includes(process.platform)) throw new Error('No authenticated update descriptor');
      const file = downloadedPath, artifact = target;
      installer.arm(async () => {
       // Recheck after unload as well: no installer sees an unauthenticated cache file.
       await verifyArtifactFile(file, artifact);
       const platform = process.platform as 'linux' | 'win32';
       const installed = platform === 'linux' ? process.env.APPIMAGE! : process.execPath;
       const windows=platform==='win32'?await currentWindowsInstallation(app.getPath('userData'),installed,{publicKeys:RELEASE_PUBLIC_KEYS,version:app.getVersion(),channel,arch:process.arch,productName:app.getName()}):undefined;
       const uninstall = windows ? await snapshotUninstall(windows.root) : undefined;
       const plan = await prepareInstalledUpdate({ userData: app.getPath('userData'), installed, downloaded: file, parentPid: process.pid,
        metadata: descriptor!.metadata, signature: descriptor!.signature, ...(uninstall ? { uninstall } : {}), ...(windows ? {windows,programManifest}: {}) },
        { publicKeys: RELEASE_PUBLIC_KEYS, platform, arch: process.arch, version: app.getVersion(), channel });
       const watchdog = await launchUpdateWatchdog(plan);
       try { await activateInstalledUpdate(plan); await watchdog.arm(); }
       catch (error) {
        try { await watchdog.cancel(); } catch { throw new UnstoppedUpdateHelper('The update helper could not be stopped safely'); }
        try { await writeUpdateJournal(plan, { phase: 'failed', failure: 'interrupted' }); } catch { /* The verified working app remains usable; the retained preparation still requires retry. */ }
        throw error;
       }
      });
    },
  }, changed);
  return coordinator;
}
