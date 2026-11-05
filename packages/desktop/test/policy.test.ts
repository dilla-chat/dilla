import { test } from 'node:test';
import assert from 'node:assert/strict';
import { canonicalInstance, instanceInput, partitionFor, parseInvite, isAppNavigation, externalLink, validateCommand, trustedFrame, instanceRequestURL } from '../src/policy.ts';

test('instance identity includes scheme, port and the entire canonical URL', () => {
  assert.equal(canonicalInstance('https://EXAMPLE.com:443'), 'https://example.com/');
  assert.equal(canonicalInstance('https://example.com/team'), 'https://example.com/team/');
  assert.equal(partitionFor('https://EXAMPLE.com:443/'), partitionFor('https://example.com/'));
  const keys = new Set(['https://example.com/', 'https://example.com:8443/', 'https://example.com/a/', 'https://example.com/b/'].map(partitionFor));
  assert.equal(keys.size, 4);
  assert.match(partitionFor('https://example.com/'), /^persist:dilla-[a-f0-9]{64}$/);
});

test('instances reject credentials, URL confusion and insecure non-loopback origins', () => {
  for (const url of ['http://example.com', 'file:///etc/passwd', 'https://a@b/', 'https://b/?token=x', 'https://b/#x', ' https://b/', 'https://b/\\evil', 'https://b/\n', 'https://b/%2fother']) {
    assert.throws(() => canonicalInstance(url), url);
  }
  assert.throws(() => canonicalInstance('http://127.0.0.1:8443/'));
  assert.equal(canonicalInstance('http://127.0.0.1:8443/', true), 'http://127.0.0.1:8443/');
  assert.throws(() => canonicalInstance('http://127.0.0.1.evil/', true));
  assert.throws(() => canonicalInstance('https://example.com/assets/'));
});

test('HTTP API and gateway paths reach the entire selected instance base path exactly once', () => {
  assert.equal(instanceRequestURL('https://chat.test/v1/instance', 'https://chat.test/team/'), 'https://chat.test/team/v1/instance');
  assert.equal(instanceRequestURL('wss://chat.test/gateway?ticket=opaque', 'https://chat.test/team/'), 'wss://chat.test/team/gateway?ticket=opaque');
  assert.equal(instanceRequestURL('https://chat.test/v1/messages', 'https://chat.test/'), 'https://chat.test/v1/messages');
  assert.equal(instanceRequestURL('wss://chat.test/team/gateway?ticket=x', 'https://chat.test/team/'), 'wss://chat.test/team/gateway?ticket=x');
  assert.equal(instanceRequestURL('https://other.test/v1/instance', 'https://chat.test/team/'), 'https://other.test/v1/instance');
  assert.equal(instanceRequestURL('https://chat.test/v1/v1/instance', 'https://chat.test/v1/'), 'https://chat.test/v1/v1/instance');
  assert.equal(instanceRequestURL('https://chat.test/v1/instance', 'https://chat.test/v1/'), 'https://chat.test/v1/v1/instance');
  assert.equal(instanceRequestURL('wss://chat.test/gateway/team/gateway', 'https://chat.test/gateway/team/'), 'wss://chat.test/gateway/team/gateway');
});

test('invites carry only a validated HTTPS instance and code, with no credentials', () => {
  assert.deepEqual(parseInvite('dilla://invite/example.com/ABCD-EFGH'), { instance: 'https://example.com/', code: 'ABCD-EFGH' });
  assert.deepEqual(parseInvite('dilla://invite/chat.example:8443/abc_DEF'), { instance: 'https://chat.example:8443/', code: 'abc_DEF' });
  for (const link of ['dilla://invite/a@b/code', 'dilla://invite/b/a/b', 'dilla://invite/b/code?token=secret', 'dilla://invite/b/code#x', 'dilla://invite/b/%2e%2e', 'dilla://invite/b/%2F', 'dilla://auth/b/token', 'dilla://invite/b/', 'https://invite/b/code', 'dilla://invite/b/code\n']) {
    assert.throws(() => parseInvite(link), link);
  }
});

test('navigation admits bundled app routes and refuses API HTML, frames and external documents', () => {
  for (const path of ['/', '/welcome?invite=abc', '/settings/voice', '/c/' + 'a'.repeat(32), '/dm/' + 'b'.repeat(32)]) {
    assert.equal(isAppNavigation('https://chat.test' + path, 'https://chat.test/'), true, path);
  }
  for (const path of ['/v1/blob/evil', '/i/code', '//evil.test', '/assets/main.js', '/unknown', '/welcome?token=secret']) {
    assert.equal(isAppNavigation('https://chat.test' + path, 'https://chat.test/'), false, path);
  }
  assert.equal(isAppNavigation('https://other.test/', 'https://chat.test/'), false);
  assert.equal(trustedFrame({ url: 'https://chat.test/', top: true }, 'https://chat.test/'), true);
  assert.equal(trustedFrame({ url: 'https://chat.test/', top: false }, 'https://chat.test/'), false);
  assert.equal(trustedFrame({ url: 'https://chat.test/v1/evil', top: true }, 'https://chat.test/'), false);
  assert.equal(externalLink('javascript:alert(1)'), null);
  assert.equal(externalLink('file:///etc/passwd'), null);
  assert.equal(externalLink('https://user:password@example.com'), null);
  assert.equal(externalLink('https://example.com/doc'), 'https://example.com/doc');
});

test('the desktop boundary validates every command, rejects extra fields and cannot access files or shell', () => {
  assert.deepEqual(validateCommand({ type: 'badge', count: 3 }), { type: 'badge', count: 3 });
  assert.deepEqual(validateCommand({ type: 'guard', activeCall: true, unsavedDraft: false }), { type: 'guard', activeCall: true, unsavedDraft: false });
  for (const cmd of [null, [], { type: 'exec', command: 'id' }, { type: 'badge', count: -1 }, { type: 'badge', count: 1.1 }, { type: 'badge', count: 1, path: '/etc/passwd' }, { type: 'guard', activeCall: false }, { type: 'notify', id: 'n', title: 'x', body: 'x', url: 'file:///etc/passwd' }, { type: 'notify', id: '../secret', title: 'x', body: 'x' }, { type: 'autostart', enabled: 'true' }]) {
    assert.throws(() => validateCommand(cmd));
  }
});

test('the integrated app menu is a parameterless capability', () => {
  assert.deepEqual(validateCommand({ type: 'window-menu' }), { type: 'window-menu' });
  for (const extra of [{ x: -1 }, { window: 'other-instance' }, { items: [{ role: 'quit' }] }]) {
    assert.throws(() => validateCommand({ type: 'window-menu', ...extra }));
  }
});

test('permission origins accept Electron origin URLs with a trailing slash, while refusing other hosts', async () => {
 const { permissionOriginMatches } = await import('../src/policy.ts');
 assert.equal(permissionOriginMatches('https://chat.test/', 'https://chat.test/team/'), true);
 assert.equal(permissionOriginMatches('https://chat.test', 'https://chat.test/team/'), true);
 for (const value of ['null', 'https://chat.test.evil/', 'https://evil.test/', 'https://user@chat.test/', 'http://chat.test/']) assert.equal(permissionOriginMatches(value, 'https://chat.test/'), false);
});


test('native window appearance accepts only the app themes, never arbitrary transparency or colors', () => {
  for (const theme of ['signal']) assert.deepEqual(validateCommand({ type: 'desktop-theme', theme }), { type: 'desktop-theme', theme });
  for (const value of [{ type: 'desktop-theme', theme: 'light' }, { type: 'desktop-theme', theme: 'high-contrast' }, { type: 'desktop-theme', theme: 'transparent' }, { type: 'desktop-theme', theme: 'light', color: '#00000000' }, { type: 'desktop-theme', theme: { bg: 'red' } }]) assert.throws(() => validateCommand(value));
});

test('native layout admits only fixed modes and bounded authentication height', () => {
 for (const value of [{ type: 'window-mode', mode: 'starting', height: null }, { type: 'window-mode', mode: 'workspace', height: null }, { type: 'window-mode', mode: 'authentication', height: 500 }]) assert.deepEqual(validateCommand(value), value);
 for (const value of [{ type: 'window-mode', mode: 'mobile', height: null }, { type: 'window-mode', mode: 'workspace', height: 200 }, { type: 'window-mode', mode: 'authentication', height: -1 }, { type: 'window-mode', mode: 'authentication', height: Infinity }, { type: 'window-mode', mode: 'authentication', height: 500, width: 1 }]) assert.throws(() => validateCommand(value));
});


test('window controls are a closed capability for the owning window only', () => {
 for (const action of ['close', 'minimize', 'toggle-maximize']) assert.deepEqual(validateCommand({ type: 'window-control', action }), { type: 'window-control', action });
 for (const command of [{ type: 'window-control', action: 'exec' }, { type: 'window-control', action: 'close', windowId: 1 }, { type: 'window-control', action: 'resize', width: 400 }]) assert.throws(() => validateCommand(command));
});

test('server settings remain trusted bundled routes, with only registered sections admitted', async () => {
 const { SETTINGS_REGISTRY } = await import('../../web/src/settings/registry.ts');
 const base = 'https://chat.test/', prefix = base + 'c/' + 'a'.repeat(32) + '/settings';
 assert.equal(isAppNavigation(prefix, base), true);
 for (const section of SETTINGS_REGISTRY.filter(entry => entry.scope === 'server')) {
  const url = `${prefix}/${section.id}`;
  assert.equal(isAppNavigation(url, base), true, section.id);
  assert.equal(trustedFrame({ url, top: true }, base), true, section.id);
  assert.equal(trustedFrame({ url, top: false }, base), false);
 }
 for (const path of ['/unknown', '/invites/v1/evil', '/invites?token=secret']) assert.equal(isAppNavigation(prefix + path, base), false);
});

test('a pasted page link reduces to the server address and an /i/<code> link also yields an invite', () => {
  const id = 'a'.repeat(32);
  for (const page of ['https://h.test/welcome', 'https://h.test/welcome/', `https://h.test/c/${id}/${id}`, `https://h.test/c/${id}`, 'https://h.test/dm', 'https://h.test/settings/devices', 'https://h.test/i/abc_DEF-123']) {
    assert.equal(canonicalInstance(page), 'https://h.test/', page);
  }
  assert.equal(canonicalInstance('http://127.0.0.1:8443/welcome', true), 'http://127.0.0.1:8443/');
  assert.equal(canonicalInstance('https://h.test/team'), 'https://h.test/team/');
  assert.deepEqual(instanceInput('https://h.test/i/abc_DEF-123'), { instance: 'https://h.test/', invite: 'abc_DEF-123' });
  assert.deepEqual(instanceInput('https://h.test/welcome'), { instance: 'https://h.test/', invite: null });
  assert.deepEqual(instanceInput('https://h.test/'), { instance: 'https://h.test/', invite: null });
});

test('removed startup health command is unavailable to renderers', () => {
 for (const command of [{ type: 'boot-ready', usable: true }, { type: 'boot-ready' }, { type: 'boot-ready', usable: 'yes' }, { type: 'boot-ready', usable: true, file: '/tmp/report' }, { type: 'boot-ready', usable: true, nonce: 'a'.repeat(64) }, { type: 'boot-ready', usable: true, pid: 123 }, { type: 'boot-ready', usable: true, version: '1.0.1' }]) assert.throws(() => validateCommand(command));
});
