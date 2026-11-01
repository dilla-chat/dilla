import { createServer, createConnection, type Socket } from 'node:net';
import { createHmac, timingSafeEqual, randomBytes } from 'node:crypto';
import { readUpdateJournal } from './update-journal.ts';
import type { InstalledUpdatePlan } from './update-plan.ts';
const HEX = /^[a-f0-9]{64}$/;
interface Message { kind: 'hello' | 'activate' | 'accepted'; nonce: string; version: string; pid: number; challenge: string; mac: string }
export interface StartupActivation {
 expect(pid: number): void;
 activate(pid: number): Promise<void>;
 close(): Promise<void>;
}
function address(plan: InstalledUpdatePlan): string {
 if (!HEX.test(plan.health.nonce) || !HEX.test(plan.activationSecret) || plan.activationSecret === plan.health.nonce) throw new Error('Invalid private activation ticket');
 if (process.platform === 'linux') return '\0dilla-update-' + plan.health.nonce;
 if (process.platform === 'win32') return '\\\\.\\pipe\\dilla-update-' + plan.health.nonce;
 throw new Error('Installed activation is unavailable on this platform');
}
function signed(plan: InstalledUpdatePlan, kind: Message['kind'], pid: number, challenge: string): Message {
 const value = [kind, plan.health.nonce, plan.health.version, pid, challenge];
 return { kind, nonce: plan.health.nonce, version: plan.health.version, pid, challenge,
  mac: createHmac('sha256', Buffer.from(plan.activationSecret, 'hex')).update(JSON.stringify(value)).digest('hex') };
}
function authenticate(plan: InstalledUpdatePlan, value: unknown, kind: Message['kind'], pid: number, challenge?: string): Message {
 const message = value as Message;
 if (!message || typeof message !== 'object' || Object.keys(message).length !== 6 || message.kind !== kind || message.nonce !== plan.health.nonce
  || message.version !== plan.health.version || message.pid !== pid || typeof message.challenge !== 'string' || !HEX.test(message.challenge) || (challenge !== undefined && challenge !== message.challenge)
  || typeof message.mac !== 'string' || !HEX.test(message.mac)) throw new Error('Invalid startup activation message');
 const expected = signed(plan, kind, pid, message.challenge).mac;
 if (!timingSafeEqual(Buffer.from(message.mac, 'hex'), Buffer.from(expected, 'hex'))) throw new Error('Invalid startup activation authentication');
 return message;
}
function frames(socket: Socket, received: (message: unknown) => void): void {
 let pending = '';
 socket.setEncoding('utf8');
 socket.on('data', chunk => {
  pending += chunk;
  if (Buffer.byteLength(pending) > 1024) { socket.destroy(new Error('Startup activation message is too large')); return; }
  for (;;) {
   const newline = pending.indexOf('\n'); if (newline < 0) return;
   const line = pending.slice(0, newline); pending = pending.slice(newline + 1);
   try { received(JSON.parse(line)); } catch (error) { socket.destroy(error instanceof Error ? error : new Error('Invalid startup activation message')); return; }
  }
 });
}
/** No renderer capability or paths cross this local, content-free channel. The
 * private plan secret is independent of account/worker encryption keys and is
 * never put in CLI, environment, UI, health reports or event payloads. */
export async function createStartupActivation(plan: InstalledUpdatePlan, timeoutMs = 10_000): Promise<StartupActivation> {
 const endpoint = address(plan), sockets = new Set<Socket>(), hellos = new Map<Socket, Message>();
 let expected = 0, committed = false, closed = false, resolveAccepted: (() => void) | undefined;
 const send = (socket: Socket, hello: Message) => { socket.write(JSON.stringify(signed(plan, 'activate', expected, hello.challenge)) + '\n'); };
 const server = createServer(socket => {
  if (closed || sockets.size >= 8) { socket.destroy(); return; }
  sockets.add(socket); socket.setTimeout(timeoutMs, () => socket.destroy());
  socket.on('error', () => {});
  socket.on('close', () => { sockets.delete(socket); hellos.delete(socket); });
  frames(socket, value => {
   if (!expected) throw new Error('Startup activation is not registered');
   if (!hellos.has(socket)) {
    const hello = authenticate(plan, value, 'hello', expected);
    // Authentication has finished. Local readiness has its own bounded health
    // deadline (and the receiver's overall 90s deadline); the short ACK budget
    // starts only after durable commit, never while WASM/OPFS/UI is still readying.
    socket.setTimeout(0); hellos.set(socket, hello); if (committed) send(socket, hello);
   } else {
    authenticate(plan, value, 'accepted', expected, hellos.get(socket)!.challenge);
    if (!committed) throw new Error('Startup activation has not committed');
    resolveAccepted?.();
   }
  });
 });
 await new Promise<void>((done, reject) => { server.once('error', reject); server.listen(endpoint, () => { server.removeListener('error', reject); done(); }); });
 server.on('error', () => { for (const socket of sockets) socket.destroy(); });
 return {
  expect: pid => { if (!Number.isSafeInteger(pid) || pid <= 1 || expected) throw new Error('Invalid registered activation process'); expected = pid; },
  activate: async pid => {
   if (closed || pid !== expected || !expected || committed) throw new Error('Invalid startup activation commit');
   committed = true;
   const accepted = new Promise<void>((done, reject) => {
    resolveAccepted = done;
    const timer = setTimeout(() => { resolveAccepted = undefined; reject(new Error('Startup activation acknowledgment timed out')); }, timeoutMs);
    resolveAccepted = () => { clearTimeout(timer); done(); };
   });
   for (const [socket, hello] of hellos) send(socket, hello);
   await accepted;
  },
  close: async () => {
   if (closed) return; closed = true;
   for (const socket of sockets) socket.destroy();
   await new Promise<void>(done => server.close(() => done()));
  },
 };
}
/** A complete rename may be visible before the writer's directory fsync. Only
 * its authenticated post-fsync message releases the hidden/disabled new UI. */
export async function receiveStartupActivation(plan: InstalledUpdatePlan, pid: number, timeoutMs = 90_000): Promise<void> {
 const endpoint = address(plan), deadline = performance.now() + timeoutMs;
 if (!Number.isSafeInteger(pid) || pid <= 1) throw new Error('Invalid startup activation process');
 for (;;) {
  try {
   await new Promise<void>((done, reject) => {
    const challenge = randomBytes(32).toString('hex'), socket = createConnection(endpoint);
    let settled = false;
    const finish = (error?: Error) => { if (settled) return; settled = true; socket.destroy(); if (error) reject(error); else done(); };
    socket.setTimeout(Math.max(1, deadline - performance.now()), () => finish(new Error('Startup activation timed out')));
    socket.on('error', error => finish(error));
    socket.on('close', () => { if (!settled) finish(new Error('Startup activation connection closed')); });
    socket.once('connect', () => socket.write(JSON.stringify(signed(plan, 'hello', pid, challenge)) + '\n'));
    frames(socket, value => {
     authenticate(plan, value, 'activate', pid, challenge);
     void (async () => {
      const journal = await readUpdateJournal(plan);
      if (journal.phase !== 'complete' || journal.childPid !== pid || journal.version !== plan.health.version) throw new Error('Invalid durable startup activation');
      socket.write(JSON.stringify(signed(plan, 'accepted', pid, challenge)) + '\n', error => finish(error ?? undefined));
     })().catch(error => finish(error instanceof Error ? error : new Error('Invalid startup activation')));
    });
   });
   return;
  } catch (error) {
   const code = (error as NodeJS.ErrnoException).code;
   if (!['ENOENT', 'ECONNREFUSED', 'EPIPE'].includes(code ?? '') || performance.now() >= deadline) throw error;
   await new Promise(done => setTimeout(done, 25));
  }
 }
}
