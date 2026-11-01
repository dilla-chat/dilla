interface HandoffDeps { isAlive(pid: number): boolean; now(): number; sleep(): Promise<void> }
const defaults: HandoffDeps = {
  isAlive: pid => { try { process.kill(pid, 0); return true; } catch (error) { if ((error as NodeJS.ErrnoException).code === 'ESRCH') return false; throw error; } },
  now: Date.now,
  sleep: () => new Promise(resolve => setTimeout(resolve, 100)),
};

/** A new AppImage must not compete for the single-instance lock before the old process actually exits. */
export async function waitForPreviousProcess(value: string | undefined, deps: HandoffDeps = defaults): Promise<void> {
  if (value === undefined) return;
  if (!/^[1-9][0-9]{0,9}$/.test(value) || Number(value) === process.pid) throw new Error('Invalid update process handoff');
  const deadline = deps.now() + 10_000;
  while (deps.isAlive(Number(value))) {
    if (deps.now() >= deadline) throw new Error('The previous Dilla process is still running; restart after saving your drafts');
    await deps.sleep();
  }
}
