import { expect, it } from 'vitest';
import { PCMRing } from './pcm-ring.ts';
it('bounded application-audio playback never replays an unbounded backlog and supplies silence on underrun', () => {
  const ring = new PCMRing(960 * 4);
  const frame = new Float32Array(960).fill(.25);
  ring.push(frame);
  const left = new Float32Array(128), right = new Float32Array(128);
  ring.read(left, right);
  expect(left.every(x => x === .25)).toBe(true); expect(right.every(x => x === .25)).toBe(true);
  for (let i = 0; i < 100; i++) ring.push(frame);
  expect(ring.queued).toBeLessThanOrEqual(3840);
  expect(ring.dropped).toBeGreaterThan(0);
  for (let i = 0; i < 20; i++) ring.read(left, right);
  expect(left.every(x => x === 0)).toBe(true);
  expect(ring.underruns).toBeGreaterThan(0);
});
