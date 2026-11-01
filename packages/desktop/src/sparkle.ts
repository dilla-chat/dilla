import { createRequire } from 'node:module';
import { join } from 'node:path';
import type { Artifact, Release } from './update.ts';

interface Bridge { prepare(version: string, artifactURL: string): Promise<string>; commit(): void; cancel(): void }
export interface QuitEvents { once(name: 'will-quit', callback: () => void): unknown; removeListener(name: 'will-quit', callback: () => void): unknown }

/** Native installation is released after renderer unloads, so a cancelled quit
 * cannot authorize Sparkle to terminate an active/recovering call or a draft. */
export class SparkleInstaller {
  private commit: (() => void) | null = null;
  private readonly native: Bridge;
  private readonly app: QuitEvents;
  constructor(native: Bridge, app: QuitEvents) { this.native = native; this.app = app; }
  async prepare(release: Release, artifact: Artifact): Promise<void> {
    const version = await this.native.prepare(release.version, artifact.url);
    if (version !== release.version) { this.native.cancel(); throw new Error('Sparkle prepared an unexpected version'); }
  }
  install(): void {
    if (this.commit) throw new Error('Sparkle installation already armed');
    this.commit = () => { this.commit = null; this.native.commit(); };
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
