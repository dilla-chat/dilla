import assert from 'node:assert/strict';
import test from 'node:test';
import { peerSeed } from '../web/support/peer-seed.ts';

test('peer usernames cannot repeat across a worker replacement or multiple peers', () => {
  const previous = new Set(Array.from({ length: 65_536 }, (_, sequence) => peerSeed(0, sequence) & 0xff_ffff));
  assert.equal(previous.size, 65_536);
  for (let sequence = 0; sequence <= 65_535; sequence++) assert.equal(previous.has(peerSeed(1, sequence) & 0xff_ffff), false);
  assert.equal(peerSeed(254, 65_535), 0xff_ffff);
});

test('namespace boundaries fail before native username masking could wrap', () => {
  for (const worker of [-1, 255, 1.5, NaN]) assert.throws(() => peerSeed(worker, 0), RangeError);
  for (const sequence of [-1, 65_536, 0.5, NaN]) assert.throws(() => peerSeed(0, sequence), RangeError);
});
