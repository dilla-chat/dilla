import { LINUX_INSTALL_KIND, assertLinuxSelfUpdate } from './linux-build.ts';
import { NativeQuitInstaller } from './quit-install.ts';
import { loadSparkle, type SparkleInstaller } from './sparkle.ts';
import { app } from 'electron';
import { AppImageUpdater, NsisUpdater, type AppUpdater } from 'electron-updater';
import { Provider, type ProviderRuntimeOptions } from 'electron-updater/out/providers/Provider.js';
import type { UpdateInfo, ResolvedUpdateFileInfo } from 'electron-updater/out/types.js';
import { rm } from 'node:fs/promises';
import type { CustomPublishOptions } from 'builder-util-runtime';
import { RELEASE_PUBLIC_KEYS } from './update-keys.ts';
import { newerVersion, verifyRelease, UpdateCoordinator, type Artifact, type Release, type UpdateState } from './update.ts';
import { verifyArtifactFile } from './appimage.ts';
import sandboxConfiguration from '../linux-sandbox-component.json' with { type: 'json' };
import { readSandboxSidecars, saveSandboxSidecars, saveManualSignature, fetchSandboxAsset, type SandboxSidecars } from './linux-sandbox-cache.ts';
import { launchSandboxPreparation, launchSandboxCheck, SandboxPreparationGate } from './linux-sandbox-prepare.ts';
import { homedir } from 'node:os';
import { join } from 'node:path';

const channel = app.getVersion().includes('-beta.') ? 'beta' : 'stable';
// The channel feed is served from GitHub Pages (scripts/pages-feed.mjs); versioned artifacts stay in their desktop-v releases.
const feed = `https://dilla-chat.github.io/dilla/${channel}/desktop-${channel}.json`;

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

export function createUpdater(changed: (state: UpdateState) => void, failedQuit: () => void, appImagePathChanged: (path: string) => void = () => {}): UpdateCoordinator {
  let native: AppImageUpdater | NsisUpdater | null = null;
  let sparkle: SparkleInstaller | null = null;
  let target: Artifact | null = null;
  let downloadedPath: string | null = null;
  let sandbox: SandboxSidecars | null = null;
  let sandboxDirectory: string | null = null;
  const sandboxPreparationGate = new SandboxPreparationGate();
  const verifySandbox = async () => {
    if (!sandbox || !sandboxDirectory) throw new Error('Missing authenticated sandbox sidecars');
    return readSandboxSidecars(sandboxDirectory, { ...sandbox, now: Date.now() });
  };
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
      const release = verifyRelease(bytes, Buffer.from(signature).toString('utf8').trim(), {
        publicKeys: RELEASE_PUBLIC_KEYS, channel, platform: process.platform, arch: process.arch,
        currentVersion: '0.0.0', now: Date.now(),
      });
      if (!newerVersion(release.version, app.getVersion())) return null;
      if (packageManaged) return release;
      target = release.artifacts.find(a => a.platform === process.platform && a.arch === process.arch)!;
      if (process.platform === 'darwin') { sparkle ??= loadSparkle(process.resourcesPath, app); return release; }
      // A new native object on every retry also discards electron-updater's in-memory cached download descriptor.
      native = process.platform === 'win32' ? new NsisUpdater() : process.platform === 'linux' ? new AppImageUpdater() : null;
      if (!native) throw new Error('No native updater for this platform');
      native.autoDownload = false; native.autoInstallOnAppQuit = false; native.allowDowngrade = false;
      native.allowPrerelease = channel === 'beta'; native.disableDifferentialDownload = true;
      native.on('error', error => {
        if (coordinator.state.phase === 'installing') installer.failed(error);
      }); // Check/download rejections are handled by the coordinator.
      if (process.platform === 'linux') native.on('appimage-filename-updated', appImagePathChanged);
      native.setFeedURL({ provider: 'custom', updateProvider: PreparedProvider, release, artifact: target });
      const checked = await native.checkForUpdates();
      if (!checked?.isUpdateAvailable) return null;
      return release;
    },
    download: async release => {
      if (process.platform === 'linux') assertLinuxSelfUpdate();
      if (process.platform === 'darwin' && sparkle && target) { await sparkle.prepare(release, target); return; }
      if (!native || !target) throw new Error('No verified update');
      sandbox = null;
      sandboxDirectory = null;
      const files = await native.downloadUpdate();
      if (files.length !== 1) throw new Error('Unexpected update artifacts');
      try {
       await verifyArtifactFile(files[0], target);
       if (process.platform === 'linux') {
        if (!target.linuxSandbox || !release.assets) throw new Error('Missing authenticated sandbox descriptor');
        const base = target.linuxSandbox.bundle.url, signal = AbortSignal.timeout(10_000);
        const [capsule, manifest, signature] = await Promise.all([fetchSandboxAsset(base, 48*1024**2, signal), fetchSandboxAsset(base+'.json', 65536, signal), fetchSandboxAsset(base+'.sig', 89, signal)]);
        sandbox = { version: release.version, descriptor: target.linuxSandbox, assets: release.assets, capsule, manifest, signature, keys: sandboxConfiguration.publicKeys, now: Date.now() };
        sandboxDirectory = await saveSandboxSidecars(app.getPath('userData'), sandbox);
        await saveManualSignature(process.env.XDG_CACHE_HOME || join(homedir(), '.cache'), sandbox);
        await verifySandbox();
       }
       downloadedPath = files[0];
      }
      catch (error) { await rm(files[0], { force: true }); downloadedPath = null; throw error; }
    },
    cancelledQuit: () => { sparkle?.cancelledQuit(); installer.cancel(); },
    preparePrerequisites: async () => {
      if (process.platform !== 'linux' || packageManaged) return;
      if (!target || !downloadedPath || !sandbox) throw new Error('Missing verified update');
      await verifyArtifactFile(downloadedPath, target);
      const verified = await verifySandbox();
      await sandboxPreparationGate.run(() => launchSandboxPreparation(downloadedPath!, Buffer.from(verified.signature.toString('utf8').trim(), 'base64'), verified));
      await verifyArtifactFile(downloadedPath, target);
      await verifySandbox();
    },
    install: async relaunch => {
      if (process.platform === 'linux') assertLinuxSelfUpdate();
      if (process.platform === 'darwin' && sparkle) { sparkle.install(); return; }
      if (!native || !target || !downloadedPath) throw new Error('No verified native update');
      try { await verifyArtifactFile(downloadedPath, target); }
      catch (error) { await rm(downloadedPath, { force: true }); downloadedPath = null; throw error; }
      const file = downloadedPath, artifact = target;
      if (process.platform === 'linux') await verifySandbox();
      installer.arm(async () => {
       // Recheck after unload as well: no installer sees an unauthenticated cache file.
       await verifyArtifactFile(file, artifact);
       if (process.platform === 'linux') {
        const checked = await verifySandbox();
        await launchSandboxCheck(file, Buffer.from(checked.signature.toString('utf8').trim(), 'base64'), checked);
        await verifyArtifactFile(file, artifact);
        await verifySandbox();
       }
       // Electron owns the installer and relaunch. Its automatic quit hook is
       // disabled so renderer vetoes are resolved before it can change files.
       delete process.env.APPIMAGE_EXIT_AFTER_INSTALL;
       native!.quitAndInstall(true, relaunch);
      });
    },
  }, changed);
  return coordinator;
}
