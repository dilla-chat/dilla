import { BotOperationRefusedError, createBotOperations, type CoreClient } from '@dilla/client-core';

export type EvidenceUpload = 'accepted' | 'pending' | 'rejected' | 'unknown';
export type ManualRemovalPhase = 'confirm' | 'sending' | 'result';
export type RemovalOutcome = 'accepted' | 'unknown' | null;
export interface ManualRemovalDialogState {
  key: string;
  msgId: string;
  seq: string;
  reason: string;
  excerpt: string;
  phase: ManualRemovalPhase;
  proofRequestId: string | null;
  evidenceRequestId: string | null;
  evidenceUpload: EvidenceUpload | null;
  removal: RemovalOutcome;
}

export async function resolveManualRemoval(client: CoreClient, proofRequestId: string): Promise<{
  removal: 'accepted' | 'unknown' | 'rejected'; evidenceUpload: EvidenceUpload;
}> {
  try {
    const result = await client.call({ m: 'resolveManualRemoval', proofRequestId }) as { removal?: unknown; evidenceUpload?: unknown };
    return { removal: result?.removal === 'accepted' || result?.removal === 'rejected' ? result.removal : 'unknown',
      evidenceUpload: result?.evidenceUpload === 'accepted' || result?.evidenceUpload === 'pending' || result?.evidenceUpload === 'rejected'
        ? result.evidenceUpload : 'unknown' };
  } catch { return { removal: 'unknown', evidenceUpload: 'unknown' }; }
}

export function newEvidenceRequestId(): string {
  return [...crypto.getRandomValues(new Uint8Array(16))].map(byte => byte.toString(16).padStart(2, '0')).join('');
}

export async function sendManualRemovalEvidence(client: CoreClient, communityId: string, state: ManualRemovalDialogState): Promise<{
  requestId: string;
  upload: EvidenceUpload;
}> {
  // A fresh envelope request ID avoids conflicting with an earlier prepared body
  // whose expiry is fixed. Server evidence is keyed by proofRequestId and is
  // idempotent for the same reason and excerpt, so a late duplicate is safe.
  const requestId = newEvidenceRequestId();
  if (state.proofRequestId === null) return { requestId, upload: 'unknown' };
  // Bot v2 Task 22: evidence is the sealed app operation 161 (v1 61, revision 0); 'accepted' only when the Bot stored it.
  try {
    const result = await createBotOperations(client, communityId).request({ operation: 61, args: [state.proofRequestId, state.reason, state.excerpt],
      revision: '0', requestId });
    return { requestId, upload: result.status === 0 ? 'accepted' : result.status === 1 || result.status === 2 ? 'rejected' : 'pending' };
  } catch (e) {
    return { requestId, upload: e instanceof BotOperationRefusedError ? 'pending' : 'unknown' };
  }
}
