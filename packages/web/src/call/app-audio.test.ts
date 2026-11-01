import { afterEach, expect, it, vi } from 'vitest';
import { acquireApplicationAudio } from './app-audio.ts';
import type { DesktopEvent } from '../../../desktop/src/contract.ts';

vi.mock('./app-audio-worklet.ts?worker&url', () => ({ default: '/application-audio-worklet.js' }));
afterEach(() => { vi.unstubAllGlobals(); });

function fixture(module: () => Promise<void> = async () => {}) {
 const listeners = new Set<(event: DesktopEvent) => void>();
 const track = Object.assign(new EventTarget(), { stop: vi.fn(), kind: 'audio' }) as unknown as MediaStreamTrack;
 const closed = vi.fn(async () => {}), stopFrames = vi.fn(), send = vi.fn(async () => {}), constructed = vi.fn();
 const addModule = vi.fn(module);
 vi.stubGlobal('AudioContext', class {
  audioWorklet = { addModule };
  constructor() { constructed(); }
  createMediaStreamDestination() { return { stream: { getAudioTracks: () => [track] } }; }
  close = closed;
  resume = async () => {};
 });
 vi.stubGlobal('AudioWorkletNode', class { port = { postMessage() {} }; connect() {} });
 const emit = () => { for (const listener of listeners) listener({ type: 'app-audio-ended', id: 'selected' }); };
 const request = vi.fn(() => Promise.resolve({ id: 'selected', label: 'Selected application' }));
 vi.stubGlobal('dillaDesktop', { requestAppAudio: request, onAppAudio: () => stopFrames,
  onEvent: (listener: (event: DesktopEvent) => void) => { listeners.add(listener); return () => listeners.delete(listener); }, send });
 return { emit, request, track, closed, listeners, stopFrames, constructed, addModule, send };
}

it('rejects a native capture that ends before its request acknowledgement without returning a silent live track', async () => {
 const f = fixture();
 f.request.mockImplementation(() => { f.emit(); return Promise.resolve({ id: 'selected', label: 'Selected application' }); });
 await expect(acquireApplicationAudio()).rejects.toMatchObject({ name: 'AbortError' });
 expect(f.constructed).not.toHaveBeenCalled();
 expect(f.listeners.size).toBe(0);
 expect(f.send).toHaveBeenCalledWith({ type: 'stop-app-audio', id: 'selected' });
});

it('cleans up when the selected application exits while its audio worklet is loading', async () => {
 let finish!: () => void;
 const f = fixture(() => new Promise<void>(done => { finish = done; }));
 const opening = acquireApplicationAudio();
 await vi.waitFor(() => expect(f.addModule).toHaveBeenCalled());
 f.emit(); finish();
 await expect(opening).rejects.toMatchObject({ name: 'AbortError' });
 expect(f.closed).toHaveBeenCalledTimes(1); expect(f.listeners.size).toBe(0);
});

it('ends a published capture and releases each subscription once when the native application exits', async () => {
 const f = fixture(), track = await acquireApplicationAudio(), ended = vi.fn();
 track.addEventListener('ended', ended);
 f.emit(); track.stop();
 expect(ended).toHaveBeenCalledTimes(1); expect(f.closed).toHaveBeenCalledTimes(1);
 expect(f.stopFrames).toHaveBeenCalledTimes(1); expect(f.listeners.size).toBe(0);
});
