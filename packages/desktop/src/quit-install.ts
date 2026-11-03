interface QuitEvent { preventDefault(): void }
interface Application {
 once(name: 'will-quit', listener: (event: QuitEvent) => void): unknown;
 removeListener(name: 'will-quit', listener: (event: QuitEvent) => void): unknown;
}

/** Install only after every renderer accepted unloading. The native updater
 * requests the final quit itself; Dilla never replaces files or relaunches. */
export class NativeQuitInstaller {
 private pending: ((event: QuitEvent) => void) | null = null;
 private installing = false;
 private readonly app: Application;
 private readonly onFailure: (error: unknown) => void;
 constructor(app: Application, onFailure: (error: unknown) => void) { this.app = app; this.onFailure = onFailure; }
 arm(install: () => Promise<void>): void {
  if (this.pending || this.installing) throw new Error('Native installation already armed');
  this.pending = event => {
   event.preventDefault(); this.pending = null; this.installing = true;
   void install().catch(error => this.failed(error));
  };
  this.app.once('will-quit', this.pending);
 }
 cancel(): void {
  if (this.pending) { this.app.removeListener('will-quit', this.pending); this.pending = null; }
 }
 failed(error: unknown): void {
  this.cancel(); this.installing = false; this.onFailure(error);
 }
}
