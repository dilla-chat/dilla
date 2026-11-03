import { test } from 'node:test';
import assert from 'node:assert/strict';
import { observeAudioLifecycle } from '../validation/audio-lifecycle.ts';

test('validation audio lifecycle preserves stop/end semantics, bounds metadata and restores the original track', () => {
 let stops = 0;
 const track = Object.assign(new EventTarget(), { readyState: 'live', stop() { stops++; this.readyState = 'ended'; } }) as unknown as MediaStreamTrack;
 const original = track.stop;
 const observer = observeAudioLifecycle(track);
 function rejectionCleanup() { track.stop(); }
 rejectionCleanup(); track.dispatchEvent(new Event('ended'));
 assert.equal(stops, 1); assert.equal(track.readyState, 'ended');
 assert.deepEqual(observer.events.map(event => event.type), ['stop', 'ended']);
 assert.equal(observer.events[0].state, 'live'); assert.equal(observer.events[1].state, 'ended');
 assert.ok(observer.events[0].callers?.includes('rejectionCleanup'));
 assert.doesNotMatch(JSON.stringify(observer.events), /https?:|file:|\/home\/|node:/);
 for (let i = 0; i < 30; i++) track.dispatchEvent(new Event('ended'));
 assert.equal(observer.events.length, 16);
 observer.dispose(); assert.equal(track.stop, original);
 track.dispatchEvent(new Event('ended')); track.stop();
 assert.equal(stops, 2); assert.equal(observer.events.length, 16);
});
