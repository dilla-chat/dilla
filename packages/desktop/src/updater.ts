import { NativeQuitInstaller } from './quit-install.ts';
import { loadSparkle, type SparkleInstaller } from './sparkle.ts';
import { app } from 'electron';
import { AppImageUpdater, NsisUpdater, type AppUpdater } from 'electron-updater';
import { Provider, type ProviderRuntimeOptions } from 'electron-updater/out/providers/Provider.js';
import type { UpdateInfo, ResolvedUpdateFileInfo } from 'electron-updater/out/types.js';
import { rm } from 'node:fs/promises';
import { spawn } from 'node:child_process';
import type { CustomPublishOptions } from 'builder-util-runtime';
import { RELEASE_PUBLIC_KEYS } from './update-keys.ts';
import { newerVersion, verifyRelease, UpdateCoordinator, type Artifact, type Release, type UpdateState } from './update.ts';
import { replaceAppImage, verifyArtifactFile } from './appimage.ts';

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
  // Do not instantiate Electron's macOS updater: unsigned builds MUST use the Sparkle bridge.
  let coordinator: UpdateCoordinator;
  const installer = new NativeQuitInstaller(app, error => { coordinator.installationFailed(error); failedQuit(); });
  coordinator = new UpdateCoordinator({
    check: async () => {
      if (!app.isPackaged) throw new Error('Automatic updates need an installed acceptance build');
      if (RELEASE_PUBLIC_KEYS.length === 0) throw new Error('Launch blocked: no desktop release signing key is embedded');
      if (process.platform === 'linux' && !process.env.APPIMAGE) throw new Error('Updates require the installed AppImage');
      const [bytes, signature] = await Promise.all([boundedFetch(feed, 1_048_576), boundedFetch(feed + '.sig', 128)]);
      const release = verifyRelease(bytes, Buffer.from(signature).toString('utf8').trim(), {
        publicKeys: RELEASE_PUBLIC_KEYS, channel, platform: process.platform, arch: process.arch,
        currentVersion: '0.0.0', now: Date.now(),
      });
      if (!newerVersion(release.version, app.getVersion())) return null;
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
      if (process.platform === 'darwin' && sparkle && target) { await sparkle.prepare(release, target); return; }
      if (!native || !target) throw new Error('No verified update');
      const files = await native.downloadUpdate();
      if (files.length !== 1) throw new Error('Unexpected update artifacts');
      try { await verifyArtifactFile(files[0], target); downloadedPath = files[0]; }
      catch (error) { await rm(files[0], { force: true }); downloadedPath = null; throw error; }
    },
    cancelledQuit: () => { sparkle?.cancelledQuit(); installer.cancel(); },
    install: async () => {
      if (process.platform === 'darwin' && sparkle) { sparkle.install(); return; }
      if (!native || !target || !downloadedPath) throw new Error('No verified native update');
      try { await verifyArtifactFile(downloadedPath, target); }
      catch (error) { await rm(downloadedPath, { force: true }); downloadedPath = null; throw error; }
      const launch = (path: string, args: string[]): Promise<void> => new Promise((resolve, reject) => {
        const child = spawn(path, args, { detached: true, stdio: 'ignore', env: { ...process.env, APPIMAGE_SILENT_INSTALL: 'true', DILLA_UPDATE_PREVIOUS_PID: String(process.pid) } });
        child.once('error', reject); child.once('spawn', () => { child.unref(); resolve(); });
      });
      const file = downloadedPath, artifact = target;
      installer.arm(async () => {
       // Recheck after unload as well: no installer sees an unauthenticated cache file.
       await verifyArtifactFile(file, artifact);
       if (process.platform === 'linux') {
        await replaceAppImage(file, process.env.APPIMAGE!, artifact, path => launch(path, []));
       } else {
        // The authenticated, per-user NSIS installer. Never fall back to elevation or shell.openPath.
        await launch(file, ['/S', '--updated', '--force-run']);
       }
      });
    },
  }, changed);
  return coordinator;
}
