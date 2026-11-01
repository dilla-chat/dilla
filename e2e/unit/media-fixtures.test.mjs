import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import * as fixtures from '../media/support/fixtures.ts';

test('mac fake microphone files use the audio sandbox permitted user audio directory', () => {
  // Chromium 156 audio.sb permits this existing audio-data subtree. Its common.sb
  // grants arbitrary temporary files metadata access, but no file-read-data grant.
  assert.equal(fixtures.captureAudioDirectory('/var/folders/test/T/media', 'darwin', '/Users/runner'),
    '/Users/runner/Library/Audio/Plug-Ins/dilla-e2e-media-fixtures');
});

test('other platforms retain the requested fixture directory', () => {
  for (const platform of ['linux', 'win32']) {
    assert.equal(fixtures.captureAudioDirectory('/test/media', platform, '/test/home'), '/test/media');
  }
});

test('fake microphone switches name complete audible PCM files with independent peer tones', t => {
  const dir = mkdtempSync(join(tmpdir(), 'dilla-fixture-test-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const tones = [1000, 600].map(hz => fixtures.writeFixtures(dir, hz));
  assert.notEqual(tones[0].wav, tones[1].wav);
  for (const fixture of tones) {
    const wav = readFileSync(fixture.wav);
    assert.equal(wav.toString('ascii', 0, 4), 'RIFF');
    assert.equal(wav.toString('ascii', 8, 12), 'WAVE');
    assert.equal(wav.readUInt16LE(20), 1); // PCM
    assert.equal(wav.readUInt16LE(22), 1); // mono
    assert.equal(wav.readUInt32LE(24), 48_000);
    assert.equal(wav.readUInt32LE(40), wav.length - 44);
    let squares = 0;
    for (let offset = 44; offset < wav.length; offset += 2) squares += (wav.readInt16LE(offset) / 32768) ** 2;
    assert.ok(Math.sqrt(squares / ((wav.length - 44) / 2)) > 0.3);
    assert.ok(fixtures.chromiumMediaArgs(fixture).includes(`--use-file-for-fake-audio-capture=${fixture.wav}`));
  }
});
