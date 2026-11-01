import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Preferences } from '../src/preferences.ts';
import { desktopEntry } from '../src/autostart.ts';

test('persists only authorized canonical instances and startup choice, without losing earlier instances', async () => {
  const root = await mkdtemp(join(tmpdir(), 'dilla-preferences-'));
  try {
    const prefs = new Preferences(root);
    await prefs.load();
    await Promise.all([prefs.addInstance('https://EXAMPLE.com/'), prefs.addInstance('https://example.com:8443/')]);
    await prefs.setAutostart(true);
    const restored = new Preferences(root);
    await restored.load();
    assert.deepEqual(restored.instances, ['https://example.com/', 'https://example.com:8443/']);
    assert.equal(restored.autostart, true);
    const saved = JSON.parse(await readFile(join(root, 'desktop.json'), 'utf8'));
    assert.deepEqual(Object.keys(saved).sort(), ['autostart', 'instances', 'version']);
    await writeFile(join(root, 'desktop.json'), JSON.stringify({ version: 1, instances: ['javascript:alert(1)'], autostart: false }));
    await assert.rejects(new Preferences(root).load(), /instance|HTTPS/i);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('Linux autostart quotes the exact executable and never interpolates shell syntax', () => {
  assert.match(desktopEntry('/opt/Dilla App/dilla'), /Exec="\/opt\/Dilla App\/dilla" --hidden/);
  assert.throws(() => desktopEntry('/opt/dilla\nExec=evil'));
  assert.throws(() => desktopEntry('relative/dilla'));
  assert.ok(!desktopEntry('/opt/$weird`name/dilla').includes('"/opt/$weird`name/dilla"'));
});
