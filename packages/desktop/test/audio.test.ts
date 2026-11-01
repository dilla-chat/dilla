import { test } from 'node:test';
import assert from 'node:assert/strict';
import { PCMFrames, parseAudioSources } from '../src/native-audio.ts';

test('audio transport frames arbitrary pipe chunks into bounded stereo 48kHz packets and rejects invalid floats', () => {
  const frames: Float32Array[] = [];
  const decoder = new PCMFrames(frame => frames.push(frame));
  const pcm = Buffer.alloc(480 * 2 * 4 * 2);
  for (let i = 0; i < pcm.length; i += 4) pcm.writeFloatLE(0.25, i);
  decoder.push(pcm.subarray(0, 13)); decoder.push(pcm.subarray(13));
  assert.equal(frames.length, 2); assert.equal(frames[0].length, 960); assert.equal(frames[0][0], 0.25);
  const bad = Buffer.alloc(3840); bad.writeFloatLE(NaN, 0);
  assert.throws(() => decoder.push(bad), /PCM/);
  assert.throws(() => decoder.push(Buffer.alloc(1024 * 1024 + 1)), /PCM/);
});

test('native source listings admit only bounded opaque identifiers and labels', () => {
  assert.deepEqual(parseAudioSources('[{"id":"123:456","label":"Game"}]'), [{ id: '123:456', label: 'Game' }]);
  for (const text of ['[] garbage', '[{"id":"--exec","label":"bad"}]', '[{"id":"1:2","label":"x","command":"sh"}]', '[{"id":"1:2","label":"x"},{"id":"1:2","label":"y"}]']) assert.throws(() => parseAudioSources(text));
});
