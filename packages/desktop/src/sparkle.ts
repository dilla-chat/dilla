import { createRequire } from 'node:module';
import { join } from 'node:path';
import type { Artifact, Release } from './update.ts';

interface Bridge { prepare(version: string, artifactURL: string, signature: string): Promise<string>; commit(): void; cancel(): void }
export interface QuitEvent { preventDefault(): void }
export interface QuitEvents {
  once(name: 'will-quit', callback: (event: QuitEvent) => void): unknown;
  removeListener(name: 'will-quit', callback: (event: QuitEvent) => void): unknown;
  quit(): void;
}
/** How long the committed host stays alive for Sparkle's installer to terminate it before it quits on its own. A
 * quit on its own still installs the prepared update on termination, but the installer then relaunches nothing. */
export const SPARKLE_TERMINATION_GRACE_MS = 30_000;

/** Native installation is released after renderer unloads, so a cancelled quit
 * cannot authorize Sparkle to terminate an active/recovering call or a draft. */
export class SparkleInstaller {
  private commit: ((event: QuitEvent) => void) | null = null;
  private readonly native: Bridge;
  private readonly app: QuitEvents;
  private readonly grace: number;
  constructor(native: Bridge, app: QuitEvents, grace = SPARKLE_TERMINATION_GRACE_MS) { this.native = native; this.app = app; this.grace = grace; }
  async prepare(release: Release, artifact: Artifact): Promise<void> {
    if (!artifact.sparkleSignature) throw new Error('Missing authenticated Sparkle signature');
    const version = await this.native.prepare(release.version, artifact.url, artifact.sparkleSignature);
    if (version !== release.version) { this.native.cancel(); throw new Error('Sparkle prepared an unexpected version'); }
  }
  install(): void {
    if (this.commit) throw new Error('Sparkle installation already armed');
    this.commit = event => {
      this.commit = null;
      // The install reply sends Sparkle's installer its "resume to stage 2 and relaunch" request asynchronously; the
      // installer then terminates this process itself and relaunches the update once it has exited. A process that
      // exits on its own right after the reply races that request: the installer sees the host gone, finishes the
      // prepared installation on termination and, never having been asked to, does not relaunch (run 633 on Intel,
      // run 638 on Apple Silicon). So this quit is withheld and Sparkle's own termination request ends the process.
      event.preventDefault();
      this.native.commit();
      setTimeout(() => this.app.quit(), this.grace).unref?.();
    };
    this.app.once('will-quit', this.commit);
  }
  cancelledQuit(): void {
    if (this.commit) { this.app.removeListener('will-quit', this.commit); this.commit = null; }
    this.native.cancel();
  }
}
export function loadSparkle(resources: string, app: QuitEvents): SparkleInstaller {
  const require = createRequire(join(resources, 'app.asar/package.json'));
  return new SparkleInstaller(require(join(resources, 'native/dilla-sparkle.node')) as Bridge, app);
}
