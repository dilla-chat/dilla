import { test } from 'node:test';
import assert from 'node:assert/strict';
import { captureOptions, captureId, frameBytes, FrameWindow, videoOnlyPermission } from '../src/screen-capture-contract.ts';

test('capture options accept bounded integers and reject additional capture authority', () => {
  assert.deepEqual(captureOptions({ width: 3840, height: 2160, frameRate: 60 }), { width: 3840, height: 2160, frameRate: 60 });
  for (const input of [null, [], {}, { width: 15, height: 16, frameRate: 1 }, { width: 16, height: 2161, frameRate: 1 },
    { width: 16, height: 16, frameRate: 61 }, { width: 16.5, height: 16, frameRate: 1 },
    { width: 16, height: 16, frameRate: NaN }, { width: 16, height: 16, frameRate: 1, sourceId: 'screen:1:0' }]) {
    assert.throws(() => captureOptions(input), /Invalid/);
  }
  assert.deepEqual(captureOptions({ width: 16, height: 16, frameRate: 1 }), { width: 16, height: 16, frameRate: 1 });
});

test('frame buffer dimensions cannot exceed the bounded tightly packed RGBA contract', () => {
  assert.equal(frameBytes(3840, 2160), 33_177_600);
  assert.equal(frameBytes(16, 16), 1024);
  for (const [w, h] of [[0, 16], [3841, 2160], [3840, 2161], [NaN, 16], [16.5, 16]]) assert.throws(() => frameBytes(w, h), /Invalid/);
  assert.equal(captureId('8c6b13cb-667a-4657-9b1d-bdb16fe2ac32'), true);
  for (const id of [null, '', 'screen:1:0', 'https://evil', 'A'.repeat(200)]) assert.equal(captureId(id), false);
});

test('only the owning immutable main frame can receive a video-only capture grant', () => {
  const details = { isMainFrame: true, requestingUrl: 'file:///broker/screen-capture.html', mediaTypes: ['video'] };
  assert.equal(videoOnlyPermission('display-capture', details, 'file:///broker/screen-capture.html'), true);
  for (const input of [{ ...details, isMainFrame: false }, { ...details, requestingUrl: 'file:///other.html' },
    { ...details, mediaTypes: ['audio'] }, { ...details, mediaTypes: ['video', 'audio'] }, { ...details, mediaTypes: [] },
    { isMainFrame: true, requestingUrl: details.requestingUrl }, null]) {
    assert.equal(videoOnlyPermission('display-capture', input, details.requestingUrl), false);
  }
  assert.equal(videoOnlyPermission('media', details, details.requestingUrl), false);
});

test('frame window bounds memory to two unacknowledged frames and validates exact ordered ACKs', () => {
  const window = new FrameWindow('8c6b13cb-667a-4657-9b1d-bdb16fe2ac32');
  assert.equal(window.reserve(100), 0);
  assert.equal(window.reserve(120), 1);
  assert.equal(window.reserve(130), null);
  assert.equal(window.expired(5099), false);
  assert.equal(window.expired(5100), true);
  assert.equal(window.ack({ type: 'ack', id: 'other', sequence: 0 }), false);
  assert.equal(window.ack({ type: 'ack', id: window.id, sequence: 1 }), false);
  assert.equal(window.ack({ type: 'ack', id: window.id, sequence: 0, options: {} }), false);
  assert.equal(window.ack({ type: 'ack', id: window.id, sequence: 0 }), true);
  assert.equal(window.reserve(200), 2);
  assert.equal(window.ack({ type: 'ack', id: window.id, sequence: 0 }), false);
  assert.equal(window.ack({ type: 'ack', id: window.id, sequence: 1 }), true);
  assert.equal(window.ack({ type: 'ack', id: window.id, sequence: 2 }), true);
  assert.equal(window.expired(10_000), false);
});
