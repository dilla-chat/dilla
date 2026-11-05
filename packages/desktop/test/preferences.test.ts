import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Preferences, permissionTokens } from '../src/preferences.ts';
import { desktopEntry, rebindLinuxAutostart } from '../src/autostart.ts';

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

test('saved addresses with an app page path load as the server address, duplicates collapsing', async () => {
  const root = await mkdtemp(join(tmpdir(), 'dilla-prefs-path-'));
  try {
    await writeFile(join(root, 'desktop.json'), JSON.stringify({ version: 1, instances: ['https://h.test/welcome', 'https://h.test/', 'https://other.test/team/'], autostart: false }));
    const prefs = new Preferences(root);
    await prefs.load();
    assert.deepEqual(prefs.instances, ['https://h.test/', 'https://other.test/team/']);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('remembers only Allow answers for the fixed permission set, per instance, and forgets them on removal', async () => {
  const root = await mkdtemp(join(tmpdir(), 'dilla-grants-'));
  try {
    const prefs = new Preferences(root);
    await prefs.load();
    await prefs.addInstance('https://a.example/');
    await prefs.addInstance('https://b.example/');
    await prefs.grant('https://a.example/', 'audio');
    await prefs.grant('https://a.example/', 'audio');
    await prefs.grant('https://a.example/', 'notifications');
    await assert.rejects(prefs.grant('https://a.example/', 'local-network'), /permission/i);
    await assert.rejects(prefs.grant('https://a.example/', 'geolocation'), /permission/i);
    await assert.rejects(prefs.grant('https://c.example/', 'audio'), /instance/i);
    const restored = new Preferences(root);
    await restored.load();
    assert.deepEqual(restored.grants('https://a.example/'), ['audio', 'notifications']);
    assert.deepEqual(restored.grants('https://b.example/'), []);
    await restored.removeInstance('https://a.example/');
    const again = new Preferences(root);
    await again.load();
    assert.deepEqual(again.instances, ['https://b.example/']);
    assert.deepEqual(again.grants('https://a.example/'), []);
    assert.equal('grants' in JSON.parse(await readFile(join(root, 'desktop.json'), 'utf8')), false);
    for (const grants of [{ 'https://b.example/': ['local-network'] }, { 'https://z.example/': ['audio'] }, { 'https://b.example/': 'audio' }, []]) {
      await writeFile(join(root, 'desktop.json'), JSON.stringify({ version: 1, instances: ['https://b.example/'], autostart: false, grants }));
      const lenient = new Preferences(root);
      await lenient.load();
      assert.deepEqual(lenient.instances, ['https://b.example/']);
      assert.deepEqual(lenient.grants('https://b.example/'), []);
    }
    await writeFile(join(root, 'desktop.json'), JSON.stringify({ version: 1, instances: ['javascript:x'], autostart: false, grants: {} }));
    await assert.rejects(new Preferences(root).load(), /instance|HTTPS/i);
    assert.deepEqual(permissionTokens('media', ['audio']), ['audio']);
    assert.deepEqual(permissionTokens('media', []), ['audio', 'video']);
    assert.deepEqual(permissionTokens('notifications', ['audio']), ['notifications']);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('Linux autostart quotes the exact executable and never interpolates shell syntax', () => {
  assert.match(desktopEntry('/opt/Dilla App/dilla'), /Exec="\/opt\/Dilla App\/dilla" --hidden/);
  assert.throws(() => desktopEntry('/opt/dilla\nExec=evil'));
  assert.throws(() => desktopEntry('relative/dilla'));
  assert.ok(!desktopEntry('/opt/$weird`name/dilla').includes('"/opt/$weird`name/dilla"'));
});

test('stock AppImage rename rebinds autostart synchronously with the same safe Exec quoting', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-autostart-rebind-'));
 try {
  const path = join(root, 'Dilla $value 1.0.1.AppImage');
  rebindLinuxAutostart(root, path);
  assert.equal(await readFile(join(root, 'autostart/dilla-desktop.desktop'), 'utf8'), desktopEntry(path));
  assert.throws(() => rebindLinuxAutostart(root, '/opt/update\nExec=evil'));
  assert.equal(await readFile(join(root, 'autostart/dilla-desktop.desktop'), 'utf8'), desktopEntry(path));
 } finally { await rm(root, { recursive: true, force: true }); }
});
