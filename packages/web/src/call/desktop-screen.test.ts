import { afterEach, expect, it, vi } from 'vitest';
import { acquireDesktopScreen } from './desktop-screen.ts';
import { browserDeps } from './session.ts';

function fixture() {
  const listeners = new Set<(event: { type: string; id: string }) => void>();
  const stopped = vi.fn(), aborted = vi.fn(async () => {}), release = vi.fn();
  const frames: { data: Uint8Array; init: VideoFrameBufferInit; close: ReturnType<typeof vi.fn> }[] = [];
  const written: unknown[] = [];
  let write: (frame: unknown) => Promise<void> = frame => { written.push(frame); return Promise.resolve(); };
  const track = Object.assign(new EventTarget(), { kind: 'video', stop: stopped, getSettings: () => ({}) }) as unknown as MediaStreamTrack;
  vi.stubGlobal('MediaStreamTrackGenerator', class { constructor() { return Object.assign(track, { writable: { getWriter: () => ({ write: (frame: unknown) => write(frame), abort: aborted, releaseLock: release }) } }); } });
  vi.stubGlobal('VideoFrame', class { constructor(data: Uint8Array, init: VideoFrameBufferInit) { const f = { data, init, close: vi.fn() }; frames.push(f); return f; } });
  vi.stubGlobal('MediaStream', class { constructor(private tracks: MediaStreamTrack[]) {} getTracks() { return this.tracks; } getVideoTracks() { return this.tracks; } getAudioTracks() { return []; } });
  const send = vi.fn(async () => {});
  const request = vi.fn<() => Promise<{ id: string; displaySurface: 'window' } | null>>(() => Promise.resolve({ id: 'selected', displaySurface: 'window' }));
  vi.stubGlobal('dillaDesktop', { requestScreenCapture: request, send, onEvent: (fn: (event: { type: string; id: string }) => void) => { listeners.add(fn); return () => listeners.delete(fn); } });
  const port = { onmessage: null as ((event: MessageEvent) => void) | null, onmessageerror: null as (() => void) | null, postMessage: vi.fn(), start: vi.fn(), close: vi.fn() };
  const announce = (id = 'selected', source: MessageEventSource | null = window) => window.dispatchEvent(new MessageEvent('message', { source, data: { type: 'dilla-screen-capture-port', id }, ports: [port as unknown as MessagePort] }));
  const frame = (extra: Record<string, unknown> = {}) => port.onmessage?.(new MessageEvent('message', { data: { type: 'frame', id: 'selected', sequence: 0, width: 16, height: 16, timestamp: 1, data: new ArrayBuffer(1024), ...extra } }));
  const ended = () => { for (const fn of listeners) fn({ type: 'screen-capture-ended', id: 'selected' }); };
  const open = async () => { const opening = acquireDesktopScreen({ video: true, audio: true }); announce(); await Promise.resolve(); frame(); return opening; };
  return { listeners, stopped, aborted, release, frames, written, track, send, request, port, announce, frame, ended, open, write: (fn: typeof write) => { write = fn; } };
}
afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); });

it('returns only the selected source video after writing its first frame, including early port delivery', async () => {
  const f = fixture(), stream = await f.open();
  expect(stream.getVideoTracks()).toEqual([f.track]); expect(stream.getAudioTracks()).toEqual([]);
  expect(f.request).toHaveBeenCalledWith({ width: 1920, height: 1080, frameRate: 30 });
  expect(f.frames[0]?.init).toEqual({ format: 'RGBA', codedWidth: 16, codedHeight: 16, timestamp: 1 });
  expect(f.frames[0]?.close).toHaveBeenCalledOnce();
  expect(f.port.postMessage).toHaveBeenCalledWith({ type: 'ack', id: 'selected', sequence: 0 });
  expect(f.track.getSettings()).toMatchObject({ displaySurface: 'window', width: 16, height: 16, frameRate: 30 });
  f.track.stop();
});

it('holds ACK and acquisition until the generated track accepts a frame and bounds outstanding work to two', async () => {
  const f = fixture(); let accept!: () => void;
  f.write(() => new Promise<void>(resolve => { accept = resolve; }));
  const opening = acquireDesktopScreen(); let ready = false; void opening.then(() => { ready = true; }, () => {});
  f.announce(); await Promise.resolve(); f.frame(); await Promise.resolve();
  expect(f.port.postMessage).not.toHaveBeenCalled(); expect(ready).toBe(false);
  f.frame({ sequence: 1, timestamp: 2 });
  expect(f.frames).toHaveLength(1);
  f.frame({ sequence: 2, timestamp: 3 });
  await expect(opening).rejects.toMatchObject({ name: 'NotReadableError' });
  expect(f.stopped).toHaveBeenCalledOnce(); expect(f.port.close).toHaveBeenCalledOnce();
  accept(); await Promise.resolve(); await Promise.resolve();
  expect(f.port.postMessage).not.toHaveBeenCalled(); expect(f.frames[0]?.close).toHaveBeenCalledOnce();
});

it.each([
  { width: 3841 }, { height: 2161 }, { width: 15 }, { height: 1.5 },
  { data: new ArrayBuffer(1023) }, { data: new Uint8Array(1024) },
  { sequence: -1 }, { sequence: 0x100000000 }, { sequence: 0.5 },
  { timestamp: NaN }, { timestamp: -1 }, { id: 'other' }, { type: 'surprise' },
])('fails closed before constructing a frame for invalid broker input %#', async invalid => {
  const f = fixture(), opening = acquireDesktopScreen(); f.announce(); await Promise.resolve(); f.frame(invalid);
  await expect(opening).rejects.toMatchObject({ name: 'NotReadableError' });
  expect(f.frames).toHaveLength(0); expect(f.listeners.size).toBe(0); expect(f.port.close).toHaveBeenCalledOnce();
});

it('ends a published track on duplicate frames without acknowledging them', async () => {
  const f = fixture(); await f.open(); const ended = vi.fn(); f.track.addEventListener('ended', ended);
  f.frame(); await Promise.resolve();
  expect(ended).toHaveBeenCalledOnce(); expect(f.frames).toHaveLength(1); expect(f.port.postMessage).toHaveBeenCalledTimes(1);
});

it('ignores foreign window ports and closes unmatched own-window ports', async () => {
  vi.useFakeTimers(); const f = fixture(), opening = acquireDesktopScreen();
  const rejected = expect(opening).rejects.toMatchObject({ name: 'NotReadableError' });
  f.announce('selected', null); await Promise.resolve(); f.announce('other');
  await vi.advanceTimersByTimeAsync(6000);
  await rejected;
  expect(f.frames).toHaveLength(0); expect(f.port.close).toHaveBeenCalled();
});

it('cleans native capture and listeners if its source ends before the request returns', async () => {
  const f = fixture(); f.request.mockImplementation(() => { f.ended(); return Promise.resolve({ id: 'selected', displaySurface: 'window' }); });
  await expect(acquireDesktopScreen()).rejects.toMatchObject({ name: 'AbortError' });
  expect(f.listeners.size).toBe(0); expect(f.send).toHaveBeenCalledWith({ type: 'stop-screen-capture', id: 'selected' });
});

it('maps cancellation to NotAllowedError and closes early ports', async () => {
  const f = fixture(); f.request.mockResolvedValue(null);
  const opening = acquireDesktopScreen(); f.announce();
  await expect(opening).rejects.toMatchObject({ name: 'NotAllowedError' });
  expect(f.listeners.size).toBe(0); expect(f.port.close).toHaveBeenCalledOnce(); expect(f.send).not.toHaveBeenCalled();
});

it('releases a published source once on stop, native ending, and navigation', async () => {
  const f = fixture(); await f.open(); const ended = vi.fn(); f.track.addEventListener('ended', ended);
  f.ended(); f.track.stop(); window.dispatchEvent(new Event('pagehide'));
  expect(ended).toHaveBeenCalledOnce(); expect(f.stopped).toHaveBeenCalledOnce(); expect(f.aborted).toHaveBeenCalledOnce();
  expect(f.listeners.size).toBe(0); expect(f.port.close).toHaveBeenCalledOnce();
  expect(f.send).toHaveBeenCalledTimes(1);
});

it('stops locally without an ended event and rejects further configuration', async () => {
  const f = fixture(); await f.open(); const ended = vi.fn(); f.track.addEventListener('ended', ended); f.track.stop();
  expect(ended).not.toHaveBeenCalled(); await expect(f.track.applyConstraints({ width: 32 })).rejects.toMatchObject({ name: 'InvalidStateError' });
});

it('normalizes capture and reconfiguration constraints without forwarding source ids or audio', async () => {
  const f = fixture(), opening = acquireDesktopScreen({ video: { width: { ideal: 9000 }, height: { max: 8000 }, frameRate: { ideal: 120 } }, audio: true });
  f.announce(); await Promise.resolve(); f.frame(); await opening;
  expect(f.request).toHaveBeenCalledWith({ width: 3840, height: 2160, frameRate: 60 });
  await f.track.applyConstraints({ width: { max: 1280 }, height: { ideal: 720 }, frameRate: 15, deviceId: 'hostile' });
  expect(f.send).toHaveBeenCalledWith({ type: 'configure-screen-capture', id: 'selected', options: { width: 1280, height: 720, frameRate: 15 } });
  f.track.stop();
});

it.each([NaN, Infinity, -1, 0])('rejects non-finite or nonpositive constraints before native selection: %s', async width => {
  const f = fixture(); await expect(acquireDesktopScreen({ video: { width } })).rejects.toMatchObject({ name: 'TypeError' }); expect(f.request).not.toHaveBeenCalled();
});

it('waits for a human native selection beyond two minutes and then accepts its first frame', async () => {
  vi.useFakeTimers(); const f = fixture(); let selected!: (v: { id: string; displaySurface: 'window' }) => void;
  f.request.mockImplementation(() => new Promise(resolve => { selected = resolve; }));
  const opening = acquireDesktopScreen(); let settled = false;
  void opening.then(() => { settled = true; }, () => { settled = true; });
  await vi.advanceTimersByTimeAsync(121000);
  expect(settled).toBe(false); expect(f.send).not.toHaveBeenCalled();
  f.announce(); selected({ id: 'selected', displaySurface: 'window' }); await Promise.resolve(); f.frame();
  expect((await opening).getVideoTracks()).toEqual([f.track]);
  f.track.stop();
});

it('cancels locally on navigation while native selection is pending and cleans a late successful reply', async () => {
  const f = fixture(); let selected!: (v: { id: string; displaySurface: 'window' }) => void;
  f.request.mockImplementation(() => new Promise(resolve => { selected = resolve; }));
  const opening = acquireDesktopScreen(); const rejected = expect(opening).rejects.toMatchObject({ name: 'AbortError' });
  window.dispatchEvent(new Event('pagehide')); await rejected;
  selected({ id: 'selected', displaySurface: 'window' }); await Promise.resolve(); await Promise.resolve();
  expect(f.listeners.size).toBe(0); expect(f.send).toHaveBeenCalledWith({ type: 'stop-screen-capture', id: 'selected' });
});

it('never falls back to direct desktop capture when the bridge lacks the capability', async () => {
  const direct = vi.fn(() => Promise.resolve({} as MediaStream)); vi.stubGlobal('navigator', { mediaDevices: { getDisplayMedia: direct }, userAgent: '' }); vi.stubGlobal('dillaDesktop', {});
  await expect(browserDeps({} as never).media.getDisplayMedia!({ video: true })).rejects.toMatchObject({ name: 'NotSupportedError' }); expect(direct).not.toHaveBeenCalled();
});

it('preserves browser capture when the desktop bridge is absent', async () => {
  const stream = {} as MediaStream, direct = vi.fn(() => Promise.resolve(stream)); vi.stubGlobal('navigator', { mediaDevices: { getDisplayMedia: direct }, userAgent: '' }); vi.stubGlobal('dillaDesktop', undefined);
  expect(await browserDeps({} as never).media.getDisplayMedia!({ video: true })).toBe(stream);
});

it('writes queued frames serially and acknowledges each only after its own write settles', async () => {
  const f = fixture(); const accepts: (() => void)[] = [];
  f.write(() => new Promise<void>(resolve => { accepts.push(resolve); }));
  const opening = acquireDesktopScreen(); f.announce(); await Promise.resolve(); f.frame(); f.frame({ sequence: 1, timestamp: 2 });
  expect(f.frames).toHaveLength(1); accepts[0](); await opening;
  expect(f.port.postMessage.mock.calls).toEqual([[{ type: 'ack', id: 'selected', sequence: 0 }]]);
  expect(f.frames).toHaveLength(2); accepts[1](); await Promise.resolve(); await Promise.resolve();
  expect(f.port.postMessage.mock.calls).toEqual([[{ type: 'ack', id: 'selected', sequence: 0 }], [{ type: 'ack', id: 'selected', sequence: 1 }]]);
  expect(f.frames.every(frame => frame.close.mock.calls.length === 1)).toBe(true); f.track.stop();
});

it.each(['ended', 'messageerror', 'pagehide'])('ends and releases an established track on %s', async reason => {
  const f = fixture(); await f.open(); const ended = vi.fn(); f.track.addEventListener('ended', ended);
  if (reason === 'ended') f.frame({ type: 'ended', reason: 'stopped' });
  else if (reason === 'messageerror') f.port.onmessageerror?.();
  else window.dispatchEvent(new Event('pagehide'));
  expect(ended).toHaveBeenCalledOnce(); expect(f.stopped).toHaveBeenCalledOnce(); expect(f.listeners.size).toBe(0); expect(f.port.close).toHaveBeenCalledOnce();
});

it('closes each frame and releases native capture when writing the generated video fails', async () => {
  const f = fixture(); f.write(() => Promise.reject(new Error('sink failed')));
  await expect(f.open()).rejects.toMatchObject({ name: 'NotReadableError' });
  expect(f.frames[0]?.close).toHaveBeenCalledOnce(); expect(f.port.postMessage).not.toHaveBeenCalled(); expect(f.stopped).toHaveBeenCalledOnce();
});

it('times out a lost live port instead of keeping a silently frozen video track', async () => {
  vi.useFakeTimers(); const f = fixture(); await f.open(); const ended = vi.fn(); f.track.addEventListener('ended', ended);
  await vi.advanceTimersByTimeAsync(10000);
  expect(ended).toHaveBeenCalledOnce(); expect(f.stopped).toHaveBeenCalledOnce(); expect(f.listeners.size).toBe(0);
});

it('rejects timestamps going backwards before allocating another frame', async () => {
  const f = fixture(); await f.open(); f.frame({ sequence: 1, timestamp: 0 });
  expect(f.frames).toHaveLength(1); expect(f.stopped).toHaveBeenCalledOnce();
});

it('keeps a new selected port alive when replacing an existing screen share', async () => {
  const first = fixture(); await first.open();
  const replacement = fixture(); const stream = await replacement.open();
  expect(stream.getVideoTracks()).toEqual([replacement.track]); expect(replacement.port.close).not.toHaveBeenCalled();
  first.track.stop(); replacement.track.stop();
});

it('releases an early transferred port once if starting the DOM port fails', async () => {
  const f = fixture(); f.port.start.mockImplementation(() => { throw new Error('closed port'); });
  const opening = acquireDesktopScreen(); f.announce();
  await expect(opening).rejects.toMatchObject({ name: 'NotReadableError' });
  expect(f.port.close).toHaveBeenCalledOnce(); expect(f.listeners.size).toBe(0); expect(f.stopped).toHaveBeenCalledOnce();
});

it('releases a selected capture even if its returned surface is malformed', async () => {
  const f = fixture(); f.request.mockResolvedValue({ id: 'selected', displaySurface: 'browser' } as never);
  await expect(acquireDesktopScreen()).rejects.toMatchObject({ name: 'NotReadableError' });
  expect(f.send).toHaveBeenCalledWith({ type: 'stop-screen-capture', id: 'selected' }); expect(f.listeners.size).toBe(0);
});
