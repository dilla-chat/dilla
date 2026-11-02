import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, rm, readFile, symlink } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { launchSandboxPreparation, launchSandboxCheck, SandboxPreparationAmbiguous, SandboxPreparationGate } from '../src/linux-sandbox-prepare.ts';

// The fixed shell launcher execs the active Node binary without changing PID or
// inherited descriptors. Quote both paths because tool caches can contain spaces.
async function writeCandidate(path: string, source: string, node = process.execPath): Promise<void> {
 const quote = (value: string) => "'" + value.replaceAll("'", "'\\''") + "'";
 const program = path + '.cjs';
 await writeFile(program, source);
 await writeFile(path, `#!/bin/sh\nexec ${quote(node)} ${quote(program)} "$@"\n`, { mode: 0o700 });
}

test('preparation accepts only a complete nonce-bound report from its own child', { skip: process.platform !== 'linux' ? 'requires Linux executable scripts and inherited IPC' : false }, async () => {
 const dir = await mkdtemp(join(tmpdir(), 'dilla-prepare-test-'));
 try {
  const script = join(dir, 'candidate');
  const node = join(dir, "node path's executable"); await symlink(process.execPath, node);
  await writeCandidate(script, `const fs=require('node:fs'); const n=process.argv[2].split('=')[1]; const s=fs.readFileSync(3); if(s.length!==64)process.exit(1); const kind=process.argv[2].includes('-check-')?'dilla-sandbox-checked':'dilla-sandbox-prepared'; fs.writeFileSync(4,JSON.stringify({format:1,kind,nonce:n,generation:7,compatibilityEpoch:1})+'\\n');\n`, node);
  await launchSandboxPreparation(script, Buffer.alloc(64), { generation: 7, compatibilityEpoch: 1 });
  await launchSandboxCheck(script, Buffer.alloc(64), { generation: 7, compatibilityEpoch: 1 });
  await assert.rejects(launchSandboxPreparation(script, Buffer.alloc(64), { generation: 8, compatibilityEpoch: 1 }));
 } finally { await rm(dir, { recursive: true, force: true }); }
});

test('signal death with a surviving detached child leaves preparation outcome ambiguous', { skip: process.platform !== 'linux' ? 'requires Linux signals and executable scripts' : false }, async () => {
 const dir = await mkdtemp(join(tmpdir(), 'dilla-prepare-signal-')), pidFile = join(dir, 'descendant'); let pid: number | undefined;
 try {
  const script = join(dir, 'candidate');
  await writeCandidate(script, `const fs=require('node:fs'),{spawn}=require('node:child_process');fs.readFileSync(3);const child=spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{detached:true,stdio:'ignore'});child.unref();fs.writeFileSync(${JSON.stringify(pidFile)},String(child.pid));process.kill(process.pid,'SIGKILL');\n`);
  const gate = new SandboxPreparationGate(); let attempts = 0;
  await assert.rejects(gate.run(() => { attempts++; return launchSandboxPreparation(script, Buffer.alloc(64), { generation: 7, compatibilityEpoch: 1 }); }), SandboxPreparationAmbiguous);
  pid = Number(await readFile(pidFile, 'utf8')); assert.ok(pid > 1); assert.doesNotThrow(() => process.kill(pid!, 0));
  await assert.rejects(gate.run(async () => { attempts++; }), /may still be running/);
  assert.equal(attempts, 1);
 } finally { if (!pid) { try { pid = Number(await readFile(pidFile, 'utf8')); } catch {} } if (pid && pid > 1) { try { process.kill(pid, 'SIGKILL'); } catch {} } await rm(dir, { recursive: true, force: true }); }
});

test('lost, partial, malformed and excessive reports after spawn are ambiguous only for preparation', { skip: process.platform !== 'linux' ? 'requires Linux executable scripts and inherited IPC' : false }, async () => {
 const dir = await mkdtemp(join(tmpdir(), 'dilla-prepare-reports-'));
 try {
  for (const [name, content] of [['lost', ''], ['partial', '{'], ['malformed', '{}\n'], ['overflow', 'x'.repeat(513)]]) {
   const script = join(dir, name);
   await writeCandidate(script, `const fs=require('node:fs');fs.readFileSync(3);fs.writeFileSync(4,${JSON.stringify(content)});\n`);
   await assert.rejects(launchSandboxPreparation(script, Buffer.alloc(64), { generation: 7, compatibilityEpoch: 1 }), SandboxPreparationAmbiguous, name);
   await assert.rejects(launchSandboxCheck(script, Buffer.alloc(64), { generation: 7, compatibilityEpoch: 1 }), error => !(error instanceof SandboxPreparationAmbiguous), name);
  }
 } finally { await rm(dir, { recursive: true, force: true }); }
});

test('overflow termination retains ambiguity while a detached descendant remains alive', { skip: process.platform !== 'linux' ? 'requires Linux signals and executable scripts' : false }, async () => {
 const dir = await mkdtemp(join(tmpdir(), 'dilla-prepare-overflow-')), pidFile = join(dir, 'descendant'); let pid: number | undefined;
 try {
  const script = join(dir, 'candidate');
  await writeCandidate(script, `const fs=require('node:fs'),{spawn}=require('node:child_process');fs.readFileSync(3);const child=spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{detached:true,stdio:'ignore'});child.unref();fs.writeFileSync(${JSON.stringify(pidFile)},String(child.pid));fs.writeFileSync(4,'x'.repeat(513));setInterval(()=>{},1000);\n`);
  const gate = new SandboxPreparationGate(); let attempts = 0;
  await assert.rejects(gate.run(() => { attempts++; return launchSandboxPreparation(script, Buffer.alloc(64), { generation: 7, compatibilityEpoch: 1 }); }), SandboxPreparationAmbiguous);
  pid = Number(await readFile(pidFile, 'utf8')); assert.ok(pid > 1); assert.doesNotThrow(() => process.kill(pid!, 0));
  await assert.rejects(gate.run(async () => { attempts++; }), /may still be running/); assert.equal(attempts, 1);
 } finally { if (!pid) { try { pid = Number(await readFile(pidFile, 'utf8')); } catch {} } if (pid && pid > 1) { try { process.kill(pid, 'SIGKILL'); } catch {} } await rm(dir, { recursive: true, force: true }); }
});

test('updater preparation gate latches an ambiguous outcome and never invokes a second authorization', async () => {
 const gate = new SandboxPreparationGate(); let attempts = 0;
 await assert.rejects(gate.run(async () => { attempts++; throw new SandboxPreparationAmbiguous('unknown outcome'); }), SandboxPreparationAmbiguous);
 await assert.rejects(gate.run(async () => { attempts++; }), /may still be running/);
 assert.equal(attempts, 1);
});

test('failure to spawn a candidate remains retryable because no child ran', async () => {
 const gate = new SandboxPreparationGate();
 await assert.rejects(gate.run(() => launchSandboxPreparation('/definitely/missing/dilla-candidate', Buffer.alloc(64), { generation: 7, compatibilityEpoch: 1 })), error => !(error instanceof SandboxPreparationAmbiguous));
 let ran = false; await gate.run(async () => { ran = true; }); assert.equal(ran, true);
});
