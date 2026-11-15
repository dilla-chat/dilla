import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import test, { after } from 'node:test';
import { fileURLToPath } from 'node:url';

const script = join(dirname(fileURLToPath(import.meta.url)), 'check-desktop-release-binary.sh');
const dir = mkdtempSync(join(tmpdir(), 'dilla-release-binary-'));
after(() => rmSync(dir, { recursive: true, force: true }));

// A fixture "binary": ELF-ish bytes with NULs around the text, as a marker sits in rodata or the symbol table.
function fixture(name, text) {
  const path = join(dir, name);
  writeFileSync(path, Buffer.concat([Buffer.from([0x7f, 0x45, 0x4c, 0x46, 0, 0, 0]), Buffer.from(text), Buffer.alloc(64)]));
  return path;
}
const run = (...args) => spawnSync('bash', [script, ...args], { encoding: 'utf8' });

test('a binary without the test-only markers passes', () => {
  const result = run(fixture('clean', 'dilla_desktop::screens::shell\0--version\0--instance=\0automationless'));
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /carries no test-only code/);
});

for (const marker of ['automation::run', '_ZN13dilla_desktop10automation3run17h0123456789abcdefE', 'gpui::test_support::', 'forget_from_forced', '--automation',
  'dilla_desktop::automation::HarnessView', 'clients/desktop/dilla-desktop/src/automation.rs', 'dilla-automation-interface/1', 'DILLA_AUTOMATION_HIDE_TEXT']) {
  test(`a binary carrying ${marker} fails with exit 1`, () => {
    const result = run(fixture(`dirty-${marker.replace(/\W/g, '_')}`, `prefix\0${marker}\0suffix`));
    assert.equal(result.status, 1, result.stdout);
    assert.match(result.stderr, /test-only feature is compiled in/);
  });
}

test('a missing binary, an unreadable one or bad usage is a tool error (exit 2)', () => {
  assert.equal(run(join(dir, 'absent')).status, 2);
  assert.equal(run().status, 2);
  assert.equal(run(fixture('a', 'x'), fixture('b', 'y')).status, 2);
  if (process.getuid?.() !== 0) {
    const locked = fixture('locked', 'x');
    chmodSync(locked, 0o000);
    assert.equal(run(locked).status, 2);
  }
});
