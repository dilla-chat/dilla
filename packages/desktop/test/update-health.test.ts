import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, writeFile, rm, symlink, mkdir, realpath } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { createStartupHealth, reportStartupHealth, waitForStartupHealth } from '../src/update-health.ts';

async function fixture(run: (root: string) => Promise<void>) {
 // macOS's temporary directory has a /var -> /private/var ancestor alias.
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-startup-health-')));
 try { await run(root); } finally { await rm(root, { recursive: true, force: true }); }
}

test('a new process reports its exact version and private startup ticket atomically', async () => fixture(async root => {
 const ticket = await createStartupHealth(root, '1.0.1');
 assert.match(ticket.nonce, /^[a-f0-9]{64}$/);
 await reportStartupHealth(root, '1.0.1', process.pid, ticket.file, ticket.nonce);
 assert.deepEqual(JSON.parse(await readFile(ticket.file, 'utf8')), { nonce: ticket.nonce, version: '1.0.1', pid: process.pid });
 const child = { pid: process.pid, exitCode: null, signalCode: null };
 let now = 0;
 await waitForStartupHealth(ticket, child, { now: () => now, sleep: async () => { now += 100; } });
 assert.ok(now >= 1000, 'startup must remain healthy for a grace interval');
}));

test('health reports never write outside the private update directory or through a substituted link', async () => fixture(async root => {
 const ticket = await createStartupHealth(root, '1.0.1');
 await assert.rejects(reportStartupHealth(root, '1.0.1', process.pid, join(root, 'arbitrary.json'), ticket.nonce), /health path/);
 if (process.platform !== 'win32') {
  await rm(ticket.directory, { recursive: true });
  const elsewhere = join(root, 'elsewhere'); await mkdir(elsewhere);
  await symlink(elsewhere, ticket.directory);
  await assert.rejects(reportStartupHealth(root, '1.0.1', process.pid, ticket.file, ticket.nonce), /directory/);
 }
}));

test('wrong nonce, version, process, oversized reports and an exited child cannot approve startup', async () => fixture(async root => {
 const ticket = await createStartupHealth(root, '1.0.1');
 const child = { pid: process.pid, exitCode: null as number | null, signalCode: null };
 const valid = { nonce: ticket.nonce, version: '1.0.1', pid: process.pid };
 for (const report of [{ ...valid, nonce: '0'.repeat(64) }, { ...valid, version: '1.0.2' }, { ...valid, pid: process.pid + 1 }, 'x'.repeat(5000)]) {
  await writeFile(ticket.file, typeof report === 'string' ? report : JSON.stringify(report), { mode: 0o600 });
  await assert.rejects(waitForStartupHealth(ticket, child), /health report/);
 }
 await writeFile(ticket.file, JSON.stringify(valid), { mode: 0o600 }); child.exitCode = 1;
 await assert.rejects(waitForStartupHealth(ticket, child), /exited/);
}));

test('a missing acknowledgement or a crash during the grace interval fails startup', async () => fixture(async root => {
 const ticket = await createStartupHealth(root, '1.0.1');
 const child = { pid: process.pid, exitCode: null as number | null, signalCode: null };
 let now = 0;
 await assert.rejects(waitForStartupHealth(ticket, child, { timeoutMs: 500, now: () => now, sleep: async () => { now += 100; } }), /timed out/);
 await reportStartupHealth(root, '1.0.1', process.pid, ticket.file, ticket.nonce);
 now = 0;
 await assert.rejects(waitForStartupHealth(ticket, child, { now: () => now, sleep: async () => { now += 100; if (now >= 500) child.exitCode = 1; } }), /exited/);
}));


test('startup polling refuses an acknowledgement through a replaced ticket directory', async () => fixture(async root => {
 const ticket = await createStartupHealth(root, '1.0.1');
 const child = { pid: process.pid, exitCode: null, signalCode: null };
 const elsewhere = join(root, 'elsewhere'); await mkdir(elsewhere, { mode: 0o700 });
 await writeFile(join(elsewhere, 'healthy.json'), JSON.stringify({ nonce: ticket.nonce, version: ticket.version, pid: child.pid }), { mode: 0o600 });
 let now = 0;
 await assert.rejects(waitForStartupHealth(ticket, child, { now: () => now, sleep: async () => {
  now += 100;
  if (now === 100) {
   await rm(ticket.directory, { recursive: true });
   await symlink(elsewhere, ticket.directory, process.platform === 'win32' ? 'junction' : 'dir');
  }
 } }), /directory/);
}));


test('health tickets use a canonical profile while reports accept the app profile alias', async () => fixture(async root => {
 const profiles = join(root, 'profiles'); await mkdir(profiles);
 const alias = join(root, 'profile-link');
 await symlink(profiles, alias, process.platform === 'win32' ? 'junction' : 'dir');
 const ticket = await createStartupHealth(join(alias, 'Dilla'), '1.0.1');
 assert.equal(ticket.directory, join(profiles, 'Dilla', 'updates', `startup-${ticket.nonce}`));
 await reportStartupHealth(join(alias, 'Dilla'), ticket.version, process.pid, ticket.file, ticket.nonce);
 let now = 0;
 await waitForStartupHealth(ticket, { pid: process.pid, exitCode: null, signalCode: null }, { now: () => now, sleep: async () => { now += 100; } });
}));
