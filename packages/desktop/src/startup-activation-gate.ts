/** Main owns the commit decision; this gate carries no account data or paths. */
export class StartupActivationGate {
 private activated: boolean;
 private pending = new Map<object, () => void>();
 constructor(activated: boolean) { this.activated = activated; }
 wait(owner: object, signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.reject(new Error('Startup wait cancelled'));
  if (this.activated) return Promise.resolve();
  if (this.pending.has(owner)) return Promise.reject(new Error('This owner is already waiting for startup'));
  return new Promise((resolve, reject) => {
   const cleanup = () => { this.pending.delete(owner); signal.removeEventListener('abort', cancel); };
   const cancel = () => { cleanup(); reject(new Error('Startup wait cancelled')); };
   this.pending.set(owner, () => { cleanup(); resolve(); });
   signal.addEventListener('abort', cancel, { once: true });
  });
 }
 open(): void {
  this.activated = true;
  for (const release of this.pending.values()) release();
 }
}
