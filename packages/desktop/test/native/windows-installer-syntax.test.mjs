// Syntax/packaging proof with the actual pinned builder header and plugins.
// This deliberately does not claim Windows execution acceptance.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, readFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { compileWindowsNSIS, withMaintenanceTimingFixture, withInitialInstallDiagnostics, initialInstallDiagnostic } from '../support/windows-nsis.mjs';
function preservesOriginalInstructions(original, transformed) {
 const functions = source => new Map([...source.replaceAll('\r\n', '\n').matchAll(/^Function ([^\n]+)\n([\s\S]*?)^FunctionEnd/gm)].map(match => [match[1], match[2].trimEnd().split('\n')]));
 const before = functions(original), after = functions(transformed);
 for (const [name, oldLines] of before) {
  let lines = after.get(name); assert.ok(lines, 'original function retained: ' + name);
  if (name === 'Fail') lines = lines.slice(5); // Fixed diagnostic write at entry.
  if (name === '.onInit') lines = lines.slice(1); // Initial diagnostic status.
  assert.equal(lines.length, oldLines.length, 'original instruction positions retained: ' + name);
  for (let index = 0; index < oldLines.length; index++) {
   const line = oldLines[index];
   if (/^\s+Call (?:\$\{Prefix\})?Fail$/.test(line)) assert.match(lines[index], /^\s+Call (?:\$\{Prefix\})?FixtureRefusal\d+$/);
   else if (/^\s+ExecWait .*--installer-preflight=initial.* \$0$/.test(line)) assert.match(lines[index], /^\s+Call FixtureInitialPreflight\d+$/);
   else assert.equal(lines[index], line, name + ': original instruction unchanged');
  }
 }
 const sections = source => new Map([...source.replaceAll('\r\n', '\n').matchAll(/^Section ([^\n]+)\n([\s\S]*?)^SectionEnd/gm)].map(match => [match[1], match[2]]));
 for (const [name, section] of sections(original)) assert.equal(sections(transformed).get(name), section, 'original section instructions unchanged: ' + name);
}
test('initial diagnostic wrappers preserve relative branch positions and refuse inline insertion regressions', async () => {
 const original = await readFile(new URL('../../build/windows-installer.nsi', import.meta.url), 'utf8'), { source } = withInitialInstallDiagnostics(original);
 preservesOriginalInstructions(original, source);
 // Reproduce the two dangerous prior insertions across original +N branches.
 assert.throws(() => preservesOriginalInstructions(original, source.replace(/(\n\s+Call FixtureRefusal\d+)/, '\n  StrCpy $FixtureFailureSite 1$1')), /instruction positions/);
 assert.throws(() => preservesOriginalInstructions(original, source.replace(/(\n\s+Call FixtureInitialPreflight\d+)/, '$1\n  StrCpy $FixtureNativeExit $0')), /instruction positions/);
});
test('private initial installer diagnostics expose only static refusal phases and numeric guard statuses', async () => {
 const source = await readFile(new URL('../../build/windows-installer.nsi', import.meta.url), 'utf8');
 const { sites } = withInitialInstallDiagnostics(source);
 assert.ok(sites.some(site => site.startsWith('CheckPath:')));
 for (const nativeExit of ['not-run', '0', '1', '10']) {
  assert.deepEqual(initialInstallDiagnostic({ format: 1, site: 1, nativeExit }, sites), { format: 1, phase: sites[0], nativeExit: nativeExit === 'not-run' ? null : Number(nativeExit) });
 }
 for (const invalid of [{ format: 1, site: 0, nativeExit: '1' }, { format: 1, site: sites.length + 1, nativeExit: '1' }, { format: 1, site: 1, nativeExit: 'private/path' },
  { format: 1, site: 1, nativeExit: '4294967296' }, { format: 1, site: 1, nativeExit: '0', privateKey: 'never log raw data' }]) assert.throws(() => initialInstallDiagnostic(invalid, sites), /^Error: Invalid private initial install diagnostic$/);
 assert.throws(() => withInitialInstallDiagnostics(source.replaceAll('--installer-preflight=initial', '--changed-boundary')), /boundary changed/);
});
test('custom Windows installer compiles with the actual pinned NSIS builder header and plugins', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-nsis-compile-'));
 try {
  const archive = join(root, 'payload.7z');
  await writeFile(archive, 'compile-only fixture; not executed');
  const bootstrap = join(root, 'Dilla.exe'); await writeFile(bootstrap, 'compile-only embedded guard; not executed');
  await writeFile(archive + '.bootstrap.nsh', `!define DILLA_BOOTSTRAP_EXE "${bootstrap}"\n!define DILLA_MANIFEST_SHA512 "${Buffer.alloc(64, 0xaa).toString('base64')}"\n!define DILLA_PROGRAM_ID "v-${'a'.repeat(32)}"\n!define DILLA_MANIFEST_NAME "${'a'.repeat(128)}.json"\n`);
  const file = await compileWindowsNSIS({ directory: root, archive, transform: source => withInitialInstallDiagnostics(withMaintenanceTimingFixture(source)).source });
  const bytes = await readFile(file);
  assert.equal(bytes.subarray(0, 2).toString(), 'MZ');
  assert.ok(bytes.length > 10_000);
 } finally { await rm(root, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 }); }
});
