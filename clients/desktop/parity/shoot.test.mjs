import test from 'node:test';
import assert from 'node:assert/strict';
import { join } from 'node:path';
import { playwrightChord, shotDir, typedText } from './shoot.mjs';

test('shotDir without --out is the committed references', () => {
  assert.equal(shotDir('/p', 'linux', 'shell/channel-view', null), join('/p', 'references', 'linux', 'shell'));
});

test('shotDir with --out never points into the committed references', () => {
  const dir = shotDir('/p', 'linux', 'shell/channel-view', '/scratch/run');
  assert.equal(dir, join('/scratch/run', 'shots', 'shell'));
  assert.ok(!dir.startsWith('/p'));
});

test('playwrightChord replays every key of the chord grammar, function keys included', () => {
  assert.equal(playwrightChord('shift-f10'), 'Shift+F10');
  assert.equal(playwrightChord('f5'), 'F5');
  assert.equal(playwrightChord('ctrl-shift-m'), 'Control+Shift+m');
  assert.equal(playwrightChord('escape'), 'Escape');
  assert.equal(playwrightChord('f'), 'f');
});

test('typedText fills the invite and the authenticator code from the world, and types anything else as written', async () => {
  const page = { locator: (sel) => ({ first: () => ({ inputValue: async () => `secret of ${sel}` }) }) };
  const ctx = { state: { invite: 'INV1' }, page, codeFor: async (secret) => `code for ${secret}` };
  assert.equal(await typedText('{invite}', ctx), 'INV1');
  assert.equal(await typedText('{totp:#key}', ctx), 'code for secret of #key');
  assert.equal(await typedText('ada', ctx), 'ada');
  assert.equal(await typedText('{inviter}', ctx), '{inviter}');
});
