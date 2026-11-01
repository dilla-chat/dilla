import { createServer, createConnection, type Socket } from 'node:net';
import { createHmac, randomBytes, timingSafeEqual } from 'node:crypto';
import type { InstalledUpdatePlan } from './update-plan.ts';
type Plan = Pick<InstalledUpdatePlan, 'health' | 'oldVersion' | 'parentPid' | 'activationSecret'>;
interface ArmMessage { kind: 'challenge' | 'arm' | 'armed'; nonce: string; version: string; parentPid: number; helperPid: number; challenge: string; mac: string }
const HEX = /^[a-f0-9]{64}$/;
function endpoint(plan: Plan): string {
 if (!HEX.test(plan.health.nonce) || !HEX.test(plan.activationSecret) || plan.health.nonce === plan.activationSecret) throw new Error('Invalid private update arming ticket');
 if (process.platform === 'linux') return '\0dilla-update-arm-' + plan.health.nonce;
 if (process.platform === 'win32') return '\\\\.\\pipe\\dilla-update-arm-' + plan.health.nonce;
 throw new Error('Native update arming is unavailable');
}
function signed(plan: Plan, helperPid: number, kind: ArmMessage['kind'], challenge: string): ArmMessage {
 if (!Number.isSafeInteger(helperPid) || helperPid <= 1 || !Number.isSafeInteger(plan.parentPid) || plan.parentPid <= 1 || helperPid === plan.parentPid) throw new Error('Invalid update arming process');
 const fields = ['dilla-update-arm-v1', kind, plan.health.nonce, plan.oldVersion, plan.parentPid, helperPid, challenge];
 return { kind, nonce: plan.health.nonce, version: plan.oldVersion, parentPid: plan.parentPid, helperPid, challenge,
  mac: createHmac('sha256', Buffer.from(plan.activationSecret, 'hex')).update(JSON.stringify(fields)).digest('hex') };
}
function authenticate(plan: Plan, helperPid: number, value: unknown, kind: ArmMessage['kind'], challenge?: string): ArmMessage {
 const message = value as ArmMessage;
 if (!message || typeof message !== 'object' || Object.keys(message).length !== 7 || message.kind !== kind || message.nonce !== plan.health.nonce || message.version !== plan.oldVersion
  || message.parentPid !== plan.parentPid || message.helperPid !== helperPid || typeof message.challenge !== 'string' || !HEX.test(message.challenge) || (challenge !== undefined && message.challenge !== challenge)
  || typeof message.mac !== 'string' || !HEX.test(message.mac)) throw new Error('Invalid update arming message');
 const expected = signed(plan, helperPid, kind, message.challenge).mac;
 if (!timingSafeEqual(Buffer.from(message.mac, 'hex'), Buffer.from(expected, 'hex'))) throw new Error('Invalid update arming authentication');
 return message;
}
function frames(socket: Socket, maximum: number, receive: (value: unknown) => void): void {
 let text = '', received = 0; socket.setEncoding('utf8');
 socket.on('data', chunk => {
  text += chunk;
  if (Buffer.byteLength(text) > 2048) { socket.destroy(new Error('Invalid update arming frame')); return; }
  for (;;) {
   const newline = text.indexOf('\n'); if (newline < 0) { if (Buffer.byteLength(text) > 1024) socket.destroy(new Error('Invalid update arming frame')); return; }
   const line = text.slice(0, newline); text = text.slice(newline + 1);
   try { if (++received > maximum || Buffer.byteLength(line) > 1024) throw new Error('Extra update arming data'); receive(JSON.parse(line)); }
   catch (error) { socket.destroy(error instanceof Error ? error : new Error('Invalid update arming frame')); return; }
  }
 });
}
export interface UpdateArming { wait(): Promise<boolean>; close(): Promise<void> }
/** Readiness, a visible active pointer and parent death are insufficient. Only
 * the authorized parent's authenticated post-fsync message arms this helper. */
export async function createUpdateArming(plan: Plan, helperPid: number, verifyPublication: () => Promise<void>, parentAlive: () => boolean, timeoutMs = 30_000): Promise<UpdateArming> {
 const address = endpoint(plan), sockets = new Set<Socket>();
 let closed = false, settled = false, armed = false, armedSocket: Socket | undefined, resolve!: (value: boolean) => void;
 const waiting = new Promise<boolean>(done => { resolve = done; });
 const finish = (value: boolean) => { if (settled) return; settled = true; resolve(value); };
 const server = createServer(socket => {
  if (closed || settled || sockets.size >= 8) { socket.destroy(); return; }
  sockets.add(socket); socket.setTimeout(Math.min(timeoutMs, 10_000), () => socket.destroy()); socket.on('error', () => {});
  socket.on('close', () => { sockets.delete(socket); if (armed && armedSocket === socket) finish(true); });
  const challenge = randomBytes(32).toString('hex');
  socket.write(JSON.stringify(signed(plan, helperPid, 'challenge', challenge)) + '\n');
  frames(socket, 1, value => {
   void (async () => {
    const message = authenticate(plan, helperPid, value, 'arm', challenge);
    await verifyPublication();
    if (closed || settled || armed) throw new Error('Update arming is no longer available');
    armed = true; armedSocket = socket;
    socket.end(JSON.stringify(signed(plan, helperPid, 'armed', message.challenge)) + '\n');
   })().catch(error => socket.destroy(error instanceof Error ? error : new Error('Invalid update arming')));
  });
 });
 await new Promise<void>((done, reject) => { server.once('error', reject); server.listen(address, () => { server.removeListener('error', reject); done(); }); });
 server.on('error', () => finish(false));
 const deadline = performance.now() + timeoutMs;
 const timer = setInterval(() => { if (settled || armed) return; try { if (!parentAlive() || performance.now() >= deadline) finish(false); } catch { finish(false); } }, 25);
 return {
  wait: () => waiting,
  close: async () => {
   if (closed) return; closed = true; clearInterval(timer); finish(false);
   for (const socket of sockets) socket.destroy();
   await new Promise<void>(done => server.close(() => done()));
  },
 };
}
/** Caller sends only after activeInstalledUpdate's directory flush returned.
 * On ANY uncertainty it must drain the exact helper before reopening old UI. */
export async function sendUpdateArm(plan: Plan, helperPid: number, timeoutMs = 10_000): Promise<void> {
 const address = endpoint(plan);
 let challenge: string | undefined;
 await new Promise<void>((done, reject) => {
  const socket = createConnection(address); let settled = false;
  const finish = (error?: Error) => { if (settled) return; settled = true; if (error) { socket.destroy(); reject(error); } else { socket.end(); done(); } };
  socket.setTimeout(timeoutMs, () => finish(new Error('Update arm acknowledgment timed out')));
  socket.on('error', error => finish(error)); socket.on('close', () => { if (!settled) finish(new Error('Update arm acknowledgment was lost')); });
  frames(socket, 2, value => {
   if (challenge === undefined) { challenge = authenticate(plan, helperPid, value, 'challenge').challenge; socket.write(JSON.stringify(signed(plan, helperPid, 'arm', challenge)) + '\n'); }
   else { authenticate(plan, helperPid, value, 'armed', challenge); finish(); }
  });
 });
}
