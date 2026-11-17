import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import yaml from 'js-yaml';
import { LINKED_LIBRARIES, neededLibraries, unknownLibraries } from './desktop-gpui-linux-deps.mjs';
import { DEPENDS } from './desktop-gpui-aur.mjs';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const nfpm = yaml.load(readFileSync(join(ROOT, 'clients/desktop/packaging/linux/nfpm.yaml'), 'utf8'));

test('every linked library has its package in the deb, the rpm and the AUR recipe', () => {
  for (const [soname, pkg] of Object.entries(LINKED_LIBRARIES)) {
    assert.ok(nfpm.overrides.deb.depends.includes(pkg.deb), `deb depends lacks ${pkg.deb} (for ${soname})`);
    assert.ok(nfpm.overrides.rpm.depends.includes(pkg.rpm), `rpm depends lacks ${pkg.rpm} (for ${soname})`);
    assert.ok(DEPENDS.includes(pkg.aur), `AUR DEPENDS lacks ${pkg.aur} (for ${soname})`);
  }
});

test('alpha.1 smoke regression: GLib (libgio) is a dependency everywhere', () => {
  for (const soname of ['libgio-2.0.so.0', 'libgobject-2.0.so.0', 'libglib-2.0.so.0']) {
    assert.ok(Object.hasOwn(LINKED_LIBRARIES, soname), soname);
  }
  assert.ok(nfpm.overrides.deb.depends.includes('libglib2.0-0t64 | libglib2.0-0'));
});

test('readelf output parses to its sonames, and an unknown one is reported', () => {
  const out = [
    ' 0x0000000000000001 (NEEDED)             Shared library: [libxcb.so.1]',
    ' 0x0000000000000001 (NEEDED)             Shared library: [libgio-2.0.so.0]',
    ' 0x0000000000000001 (NEEDED)             Shared library: [libfoo.so.3]',
    ' 0x000000000000000c (INIT)               0x1000',
  ].join('\n');
  assert.deepEqual(neededLibraries(out), ['libxcb.so.1', 'libgio-2.0.so.0', 'libfoo.so.3']);
  assert.deepEqual(unknownLibraries(neededLibraries(out)), ['libfoo.so.3']);
});
