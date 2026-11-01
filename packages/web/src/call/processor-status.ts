import type { ProcessorStatus } from '@dilla/media';
let owner: symbol | null = null;
let status: ProcessorStatus | null = null;
const listeners = new Set<() => void>();
export const getProcessorStatus = (): ProcessorStatus | null => status;
export const subscribeProcessorStatus = (fn: () => void): (() => void) => { listeners.add(fn); return () => { listeners.delete(fn); }; };
export function reportProcessorStatus(id: symbol, value: ProcessorStatus | null): void {
  if (value === null && owner !== id) return;
  owner = value === null ? null : id; status = value;
  for (const listener of listeners) listener();
}

/** Fix round 1 (M1): the call's input stage state (`active`, `unsupported`, `failed`), or null with no call, so Settings
 *  says when the input volume and the gate are not applied. The gate threshold in force when it failed decides whether the
 *  microphone passes unchanged or sends nothing; Settings reads the current setting for that. */
export type StageStatus = 'active' | 'unsupported' | 'failed';
let stageOwner: symbol | null = null;
let stageStatus: StageStatus | null = null;
const stageListeners = new Set<() => void>();
export const getStageStatus = (): StageStatus | null => stageStatus;
export const subscribeStageStatus = (fn: () => void): (() => void) => { stageListeners.add(fn); return () => { stageListeners.delete(fn); }; };
export function reportStageStatus(id: symbol, value: StageStatus | null): void {
  if (value === null && stageOwner !== id) return;
  stageOwner = value === null ? null : id; stageStatus = value;
  for (const listener of stageListeners) listener();
}
