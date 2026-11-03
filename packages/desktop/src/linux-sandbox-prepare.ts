import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';

export class SandboxPreparationAmbiguous extends Error {}

/** Retain uncertainty for this app process; only a new process can retry. */
export class SandboxPreparationGate {
 private ambiguous = false;
 private running = false;
 async run(action: () => Promise<void>): Promise<void> {
  if (this.ambiguous) throw new SandboxPreparationAmbiguous('The previous sandbox operation may still be running. Restart Dilla after it has settled.');
  if (this.running) throw new Error('Sandbox preparation is already running');
  this.running = true;
  try { await action(); }
  catch (error) { if (error instanceof SandboxPreparationAmbiguous) this.ambiguous = true; throw error; }
  finally { this.running = false; }
 }
}

/** Call only after authenticating the candidate and its sidecars in main. */
export async function launchSandboxPreparation(candidate: string, signature: Uint8Array, expected: { generation: number; compatibilityEpoch: 1 }): Promise<void> {
 return runSandboxRole(candidate, signature, expected, 'prepare');
}
export async function launchSandboxCheck(candidate: string, signature: Uint8Array, expected: { generation: number; compatibilityEpoch: 1 }): Promise<void> {
 return runSandboxRole(candidate, signature, expected, 'check');
}
async function runSandboxRole(candidate: string, signature: Uint8Array, expected: { generation: number; compatibilityEpoch: 1 }, role: 'prepare' | 'check'): Promise<void> {
 if (signature.length !== 64) throw Error('Invalid sandbox signature');
 const nonce = randomBytes(32).toString('hex');
 const child = spawn(candidate, [`--dilla-sandbox-${role}-update=${nonce}`], { stdio: ['ignore', 'ignore', 'ignore', 'pipe', 'pipe'], env: Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith('DILLA_UPDATE_'))) });
 const failure = (reason: string): Error => role === 'prepare' && child.pid !== undefined
  ? new SandboxPreparationAmbiguous('Sandbox preparation outcome is unknown; its system operation may still be running. Restart Dilla after it has settled.')
  : new Error(reason);
 const input = child.stdio[3], output = child.stdio[4];
 if (!input || !output || !('write' in input)) { child.kill(); throw failure('Sandbox preparation IPC unavailable'); }
 const chunks: Buffer[] = []; let length = 0;
 const result = await new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve, reject) => {
  let settled = false;
  const timer = setTimeout(() => fail('Sandbox preparation timed out'), role === 'prepare' ? 120_000 : 10_000);
  const fail = (reason: string) => { if (settled) return; settled = true; clearTimeout(timer); if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL'); reject(failure(reason)); };
  child.once('error', error => { if (settled) return; settled = true; clearTimeout(timer); reject(child.pid === undefined ? error : failure('Sandbox preparation process failed')); });
  child.once('close', (code, signal) => { if (settled) return; settled = true; clearTimeout(timer); resolve({ code, signal }); });
  output.on('error', () => fail('Sandbox preparation report was lost'));
  output.on('data', (part: Buffer) => { length += part.length; if (length > 512) fail('Sandbox preparation report was too large'); else chunks.push(part); });
  input.on('error', () => fail('Sandbox preparation input failed'));
  input.end(signature);
 });
 if (result.code !== 0 || result.signal !== null) throw failure('Sandbox preparation was cancelled or failed');
 const raw = Buffer.concat(chunks).toString('utf8');
 const report = JSON.stringify({ format: 1, kind: role === 'prepare' ? 'dilla-sandbox-prepared' : 'dilla-sandbox-checked', nonce, generation: expected.generation, compatibilityEpoch: expected.compatibilityEpoch }) + '\n';
 if (raw !== report) throw failure('Invalid sandbox preparation report');
}
