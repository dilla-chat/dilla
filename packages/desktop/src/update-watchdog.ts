import { WindowsCommitUncertain } from './windows-slots.ts';
import { replaceAppImage, verifyArtifactFile } from './appimage.ts';
import { waitForStartupHealth, type StartupHealth } from './update-health.ts';
import { writeUpdateJournal, readUpdateJournal, type UpdateFailure, type UpdateJournal } from './update-journal.ts';
import { isOriginalInstallation, restoreWindowsProgram, restoreOriginalAppImage, type InstalledUpdatePlan } from './update-plan.ts';
export interface UpdateChild { pid?: number; groupPid?: number; exitCode: number | null; signalCode: string | null }
export interface WatchdogProcesses {
 waitParent(pid: number): Promise<void>;
 launch(file: string, health?: StartupHealth): Promise<UpdateChild>;
 stop(child: UpdateChild): Promise<void>;
 activate?(child: UpdateChild): Promise<void>;
 installWindows(file: string): Promise<void>;
 commitWindows?():Promise<void>;
 windowsTarget?():Promise<string>;
 stopInstaller?(journal?: UpdateJournal): Promise<void>;
 restoreWindowsMetadata(): Promise<void>;
 recoverLaunches?(): Promise<UpdateChild[]>;
 recoverChild?(pid: number): Promise<UpdateChild | null>;
}
export interface UpdateOutcome { phase: 'complete' | 'failed'; durable: boolean }
async function recordRecovery(plan: InstalledUpdatePlan, state: Parameters<typeof writeUpdateJournal>[1]): Promise<boolean> {
 try { await writeUpdateJournal(plan, state); return true; } catch { return false; }
}
type HealthClock = Parameters<typeof waitForStartupHealth>[2];
/** Only the helper owns the replacement process. Its exact PID must acknowledge
 * the signed version and survive the health grace before installation succeeds. */
export async function runInstalledUpdate(plan: InstalledUpdatePlan, processes: WatchdogProcesses, clock?: HealthClock): Promise<UpdateOutcome> {
 const artifact = plan.release.artifacts.find(file => file.platform === plan.platform && file.arch === plan.arch);
 if (!artifact) throw new Error('No authenticated update artifact for this target');
 await verifyArtifactFile(plan.artifactFile, artifact);
 await processes.waitParent(plan.parentPid);
 let child: UpdateChild | undefined;
 let failure: UpdateFailure = plan.platform === 'win32' ? 'installer' : 'startup';
 try {
  // If the first durable phase cannot be written after parent exit, keep the
  // original untouched and reopen it through the same recovery path.
  await writeUpdateJournal(plan, { phase: 'installing' });
  const launch = async (file: string) => {
   failure = 'startup';
   await writeUpdateJournal(plan, { phase: 'launching' });
   child = await processes.launch(file, plan.health);
   await writeUpdateJournal(plan, { phase: 'launching', childPid: child.pid });
   await waitForStartupHealth(plan.health, child, clock);
   // Activation is durable before a user can interact with the new app. A
   // status-write failure still aborts the disabled child and restores old.
   await writeUpdateJournal(plan, { phase: 'complete', childPid: child.pid });
  };
  if (plan.platform === 'win32') {
   await processes.installWindows(plan.artifactFile);
   await launch(processes.windowsTarget ? await processes.windowsTarget() : plan.installed);
   await processes.commitWindows?.();
  } else await replaceAppImage(plan.artifactFile, plan.installed, artifact, launch, async () => {
   await recordRecovery(plan, { phase: 'recovering', childPid: child?.pid, failure: 'startup' });
   if (child) await processes.stop(child);
  });
 } catch (error) {
  if(error instanceof WindowsCommitUncertain){if(child)await processes.stop(child);return{phase:'complete',durable:false};}
  // AppImage replacement has already restored the original before resolving
  // its failure. If stopping the failed child/rollback failed, do not launch a
  // competing process or pretend restoration succeeded.
  if (plan.platform === 'win32') {
   // This check is independent of writable journal/PID fields. It drains the
   // private job and any authenticated actor before program files are moved.
   await processes.stopInstaller?.();
   await recordRecovery(plan, { phase: 'recovering', childPid: child?.pid, failure });
   if (child) await processes.stop(child);
   await restoreWindowsProgram(plan);
   try { await processes.restoreWindowsMetadata(); } catch { failure = 'metadata'; }
  }
  if (!await isOriginalInstallation(plan)) throw error;
  const durable = await recordRecovery(plan, { phase: 'failed', failure });
  await processes.launch(plan.installed);
  return { phase: 'failed', durable };
 }
 // This is past the durable commit point. An acknowledgment can be lost after
 // the new app enabled user entry, so it must never trigger a rollback.
 try { if (child) await processes.activate?.(child); } catch { /* Ordinary restart can open the verified complete version. */ }
 return { phase: 'complete', durable: true };
}

/** Interrupted plans recover to the old version; they never silently retry an
 * installer. An owned still-running child is stopped before touching its files. */
export async function recoverInstalledUpdate(plan: InstalledUpdatePlan, processes: WatchdogProcesses): Promise<UpdateOutcome> {
 const journal = await readUpdateJournal(plan);
 if (!['prepared', 'installing', 'launching', 'recovering'].includes(journal.phase)) throw new Error('Invalid update recovery phase');
 await processes.waitParent(plan.parentPid);
 if (plan.platform === 'win32') await processes.stopInstaller?.(journal);
 const launched = await processes.recoverLaunches?.() ?? [];
 for (const child of launched) await processes.stop(child);
 if (journal.childPid && !launched.some(child => child.pid === journal.childPid)) {
  if (!processes.recoverChild) throw new Error('Cannot identify the interrupted update process');
  const child = await processes.recoverChild(journal.childPid);
  if (child) await processes.stop(child);
 }
 await recordRecovery(plan, { phase: 'recovering', failure: 'interrupted' });
 let failure: UpdateFailure = 'interrupted';
 if (plan.platform === 'linux') await restoreOriginalAppImage(plan);
 else { await restoreWindowsProgram(plan); try { await processes.restoreWindowsMetadata(); } catch { failure = 'metadata'; } }
 if (!await isOriginalInstallation(plan)) throw new Error('The original installation could not be restored');
 const durable = await recordRecovery(plan, { phase: 'failed', failure });
 await processes.launch(plan.installed);
 return { phase: 'failed', durable };
}
