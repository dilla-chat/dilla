import test from 'node:test';
import assert from 'node:assert/strict';
import { join } from 'node:path';
import { playwrightChord, replayStep, shotDir, typedText, typingBodies, wallExpiry } from './shoot.mjs';

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

test('typingBodies makes one typing frame per person, to the viewer, ending holdS after now, with a device of its own', () => {
  const fixture = { viewer: 'ada', world: { typing: [{ person: 'mira', channel: 'midgard/general' }, { person: 'bjorn', channel: 'dm/pair' }] } };
  const ids = {
    'person:ada': '01'.repeat(16), 'person:mira': '02'.repeat(16), 'person:bjorn': '03'.repeat(16),
    'channel:midgard/general': '0a'.repeat(16), 'dm:pair': '0b'.repeat(16),
  };
  const [one, two] = typingBodies(fixture, ids, 1000);
  const device = `${'02'.repeat(15)}${(0x02 ^ 0xd7).toString(16)}`;
  assert.deepEqual(one, { to: ids['person:ada'], channel: ids['channel:midgard/general'], user: ids['person:mira'], device, expires: 1010 });
  assert.equal(two.channel, ids['dm:pair']);
  assert.notEqual(two.device, two.user);
  assert.deepEqual(typingBodies({ viewer: 'ada', world: {} }, ids, 1000), []);
});

test('the offline step cuts the network of the browser context and then settles', async () => {
  const calls = [];
  const page = { evaluate: async () => calls.push('fonts'), waitForTimeout: async (ms) => calls.push(`wait ${ms}`) };
  const context = { setOffline: async (on) => calls.push(`offline ${on}`) };
  await replayStep(page, { do: 'offline' }, {}, context);
  assert.deepEqual(calls, ['offline true', 'fonts', 'wait 500']);
});

test('a gateway ticket of a Bot world tells the wall clock: the expiry moves back by the instance lead, nothing else', () => {
  const ticket = Buffer.from('tk');
  const body = (expires) => {
    const out = Buffer.alloc(2 + ticket.length + 5);
    out[0] = 0x82; out[1] = 0x60 | ticket.length; ticket.copy(out, 2);
    out[2 + ticket.length] = 0x1a; out.writeUInt32BE(expires, 3 + ticket.length);
    return out;
  };
  assert.deepEqual(wallExpiry(body(1_795_218_000), 1000), body(1_795_217_000));
  assert.throws(() => wallExpiry(Buffer.from([0x81, 0x01]), 1000), /not \[text, uint32\]/);
  // The longer text lengths (0x78 one byte, 0x79 two bytes) read the same.
  const long = Buffer.concat([Buffer.from([0x82, 0x78, 30]), Buffer.alloc(30, 0x61), Buffer.from([0x1a, 0, 0, 0x03, 0xe8])]);
  assert.equal(wallExpiry(long, 8).readUInt32BE(long.length - 4), 992);
});
