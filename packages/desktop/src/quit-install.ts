interface QuitEvent { preventDefault(): void }
interface Application {
 once(name: 'will-quit', listener: (event: QuitEvent) => void): unknown;
 removeListener(name: 'will-quit', listener: (event: QuitEvent) => void): unknown;
 exit(code: number): void;
}

/** Renderer unloads can still veto a requested quit. Replacing the AppImage or
 * launching NSIS is authorized only after every renderer has accepted closing. */
export class NativeQuitInstaller {
 private pending: ((event: QuitEvent) => void) | null = null;
 private installing = false;
 private readonly app: Application;
 private readonly failed: (error: unknown) => void;
 constructor(app: Application, failed: (error: unknown) => void) { this.app = app; this.failed = failed; }
 arm(commit: () => Promise<void>): void {
  if (this.pending || this.installing) throw new Error('Native installation already armed');
  this.pending = event => {
   event.preventDefault(); this.pending = null; this.installing = true;
   void this.finish(commit);
  };
  this.app.once('will-quit', this.pending);
 }
 cancel(): void {
  if (this.pending) { this.app.removeListener('will-quit', this.pending); this.pending = null; }
 }
 private async finish(commit: () => Promise<void>): Promise<void> {
  try { await commit(); this.app.exit(0); }
  catch (error) { this.installing = false; this.failed(error); }
 }
}
