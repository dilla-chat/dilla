// Syntax/packaging proof with the actual pinned builder header and plugins.
// This deliberately does not claim Windows execution acceptance.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, readFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { compileWindowsNSIS, withMaintenanceTimingFixture } from '../support/windows-nsis.mjs';
test('custom Windows installer compiles with the actual pinned NSIS builder header and plugins', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-nsis-compile-'));
 try {
  const archive = join(root, 'payload.7z');
  await writeFile(archive, 'compile-only fixture; not executed');
  const bootstrap = join(root, 'Dilla.exe'); await writeFile(bootstrap, 'compile-only embedded guard; not executed');
  await writeFile(archive + '.bootstrap.nsh', `!define DILLA_BOOTSTRAP_EXE "${bootstrap}"\n!define DILLA_MANIFEST_SHA512 "${Buffer.alloc(64, 0xaa).toString('base64')}"\n!define DILLA_PROGRAM_ID "v-${'a'.repeat(32)}"\n!define DILLA_MANIFEST_NAME "${'a'.repeat(128)}.json"\n`);
  const file = await compileWindowsNSIS({ directory: root, archive, transform: withMaintenanceTimingFixture });
  const bytes = await readFile(file);
  assert.equal(bytes.subarray(0, 2).toString(), 'MZ');
  assert.ok(bytes.length > 10_000);
 } finally { await rm(root, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 }); }
});
