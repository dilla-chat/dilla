import { test } from 'node:test';
import assert from 'node:assert/strict';
import childProcess from 'node:child_process';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { once } from 'node:events';
import vm from 'node:vm';
import { installNativeAudioDiagnostics } from './support/audio-diagnostics.ts';

test('native audio diagnostics preserve child output/exit and bound redacted metadata without recording PCM', async () => {
 const directory = await mkdtemp(join(tmpdir(), 'dilla-audio-diagnostics-'));
 const binary = process.execPath;
 await writeFile(join(directory, 'capture'), `process.stderr.write('failed https://');setTimeout(()=>{process.stderr.write('private.test/token /private/actor '+ 'x'.repeat(10000));process.stdout.write(Buffer.alloc(3840, 85));process.exitCode=7;},10);\n`);
 // Electron.evaluate serializes this callback; exercise that same closure boundary.
 vm.runInThisContext('(' + installNativeAudioDiagnostics.toString() + ')')(undefined, { binary });
 try {
  const child = childProcess.spawn(binary, ['capture', '123:456', '789'], { cwd: directory });
  const chunks: Buffer[] = [];
  child.stdout.on('data', chunk => chunks.push(chunk));
  child.stderr.resume();
  const [code, signal] = await once(child, 'close');
  assert.equal(code, 7); assert.equal(signal, null);
  assert.deepEqual(Buffer.concat(chunks), Buffer.alloc(3840, 85), 'observer must not consume or alter capture output');
  const [record] = (globalThis as any).__nativeAudioDiagnostics;
  assert.equal(record.sourcePID, 456); assert.deepEqual(record.stops, []);
  assert.equal(record.pcmBytes, 3840); assert.ok(record.pcmChunks > 0);
  assert.ok(record.firstPcmMs >= record.startedMs);
  assert.equal(record.exit.code, 7); assert.equal(record.exit.signal, null);
  assert.ok(record.stderr.length <= 4096);
  assert.match(record.stderr, /failed \[url\] \[path\]/);
  assert.doesNotMatch(JSON.stringify(record), /private\.test|\/private|UUUU/);
  await writeFile(join(directory, 'capture'), `process.stdout.write('ready');setInterval(()=>{},1000);`);
  const live = childProcess.spawn(binary, ['capture', '123:456', '789'], { cwd: directory });
  await once(live.stdout, 'data');
  const closed = once(live, 'close');
  assert.equal(live.kill(), true);
  const [, killedBy] = await closed;
  assert.equal(killedBy, 'SIGTERM');
  const stopped = (globalThis as any).__nativeAudioDiagnostics[1];
  assert.equal(stopped.stops.length, 1); assert.equal(stopped.stops[0].signal, 'SIGTERM');
  assert.equal(stopped.exit.signal, 'SIGTERM');
  const unrelated = childProcess.spawn(process.execPath, ['-e', 'process.exit(0)']);
  await once(unrelated, 'close');
  assert.equal((globalThis as any).__nativeAudioDiagnostics.length, 2, 'only the selected shipping capture executable is observed');
 } finally {
  (globalThis as any).__stopNativeAudioDiagnostics();
  delete (globalThis as any).__stopNativeAudioDiagnostics;
  delete (globalThis as any).__nativeAudioDiagnostics;
  await rm(directory, { recursive: true, force: true });
 }
});
