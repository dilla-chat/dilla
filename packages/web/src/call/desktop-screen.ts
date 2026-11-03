import { desktopBridge } from '../desktop.ts';
import type { ScreenCaptureOptions, ScreenCaptureSource } from '../../../desktop/src/contract.ts';

type GeneratedTrack = MediaStreamTrack & { writable: WritableStream<VideoFrame> };
type GeneratorConstructor = new (options: { kind: 'video' }) => GeneratedTrack;
type CaptureFrame = { type: 'frame'; id: string; sequence: number; width: number; height: number; timestamp: number; data: ArrayBuffer };

/** Only numeric capture limits cross the bridge; source selection stays in main. */
function captureOptions(constraints?: MediaTrackConstraints, fallback: ScreenCaptureOptions = { width: 1920, height: 1080, frameRate: 30 }): ScreenCaptureOptions {
  const limit = (value: ConstrainDouble | undefined, initial: number, min: number, max: number): number => {
    if (value === undefined) return initial;
    let number: number | undefined;
    if (typeof value === 'number') number = value;
    else if (value && typeof value === 'object') {
      // Validate every supplied numeric constraint, including a non-selected bound.
      for (const key of ['min', 'max', 'ideal', 'exact'] as const) {
        if (value[key] !== undefined && (typeof value[key] !== 'number' || !Number.isFinite(value[key]) || value[key] <= 0)) throw new TypeError('Invalid screen capture constraint');
      }
      number = value.max ?? value.ideal ?? value.exact ?? value.min;
    } else throw new TypeError('Invalid screen capture constraint');
    if (number === undefined) return initial;
    if (!Number.isFinite(number) || number <= 0) throw new TypeError('Invalid screen capture constraint');
    return Math.max(min, Math.min(max, Math.floor(number)));
  };
  return { width: limit(constraints?.width, fallback.width, 16, 3840), height: limit(constraints?.height, fallback.height, 16, 2160), frameRate: limit(constraints?.frameRate, fallback.frameRate, 1, 60) };
}

function validFrame(frame: unknown, id: string, sequence: number, timestamp: number): frame is CaptureFrame {
  if (!frame || typeof frame !== 'object') return false;
  const f = frame as CaptureFrame;
  return f.type === 'frame' && f.id === id && Number.isInteger(f.sequence) && f.sequence >= 0 && f.sequence <= 0xffffffff && f.sequence > sequence
    && Number.isInteger(f.width) && f.width >= 16 && f.width <= 3840 && Number.isInteger(f.height) && f.height >= 16 && f.height <= 2160
    && Number.isFinite(f.timestamp) && f.timestamp >= 0 && f.timestamp >= timestamp
    && f.data instanceof ArrayBuffer && f.data.byteLength === f.width * f.height * 4;
}

/** Receive bounded RGBA frames from the selected isolated broker. Never captures in this renderer. */
export async function acquireDesktopScreen(options?: DisplayMediaStreamOptions): Promise<MediaStream> {
  const bridge = desktopBridge();
  const Generator = (globalThis as unknown as { MediaStreamTrackGenerator?: GeneratorConstructor }).MediaStreamTrackGenerator;
  if (!bridge?.requestScreenCapture || !Generator || typeof VideoFrame !== 'function') throw new DOMException('Desktop screen capture unavailable', 'NotSupportedError');
  if (options?.video === false) throw new TypeError('Screen capture requires video');
  let configured = captureOptions(typeof options?.video === 'object' ? options.video : undefined);
  let source: ScreenCaptureSource | null = null, track: GeneratedTrack | undefined;
  let writer: WritableStreamDefaultWriter<VideoFrame> | undefined, port: MessagePort | undefined;
  let closed = false, ready = false, sequence = -1, timestamp = -1, processing = false;
  let stopEvents = () => {};
  const earlyPorts = new Map<string, MessagePort>(), earlyEnded = new Set<string>(), queue: CaptureFrame[] = [];
  let resolve!: (stream: MediaStream) => void, reject!: (error: unknown) => void;
  const result = new Promise<MediaStream>((yes, no) => { resolve = yes; reject = no; });
  const unreadable = () => new DOMException('Desktop screen capture failed', 'NotReadableError');
  let timer: ReturnType<typeof setTimeout> | undefined;
  const armTimeout = (ms: number) => { clearTimeout(timer); timer = setTimeout(() => stop(unreadable(), true), ms); };
  const closePort = (p: MessagePort) => { p.onmessage = null; p.onmessageerror = null; p.close(); };
  const stopSource = (selected: ScreenCaptureSource) => { void bridge.send({ type: 'stop-screen-capture', id: selected.id }).catch(() => {}); };
  const stop = (error: unknown = new DOMException('Desktop screen capture ended', 'AbortError'), ended = false) => {
    if (closed) return;
    closed = true; clearTimeout(timer); stopEvents(); window.removeEventListener('message', receivePort); window.removeEventListener('pagehide', navigate);
    if (port) closePort(port);
    for (const pending of earlyPorts.values()) closePort(pending);
    earlyPorts.clear(); earlyEnded.clear(); queue.length = 0;
    if (source) stopSource(source);
    if (writer) { const activeWriter = writer; void activeWriter.abort().catch(() => {}).finally(() => { try { activeWriter.releaseLock(); } catch { /* A pending write owns the lock until abort settles. */ } }); }
    if (track) { stopTrack?.(); if (ended && ready) track.dispatchEvent(new Event('ended')); }
    if (!ready) reject(error);
  };
  let stopTrack: (() => void) | undefined;
  const navigate = () => stop(new DOMException('Page navigated', 'AbortError'), true);
  const drain = async () => {
    if (processing || closed) return;
    processing = true;
    try {
      while (queue.length && !closed) {
        const f = queue.shift()!;
        let frame: VideoFrame | undefined;
        try {
          frame = new VideoFrame(new Uint8Array(f.data), { format: 'RGBA', codedWidth: f.width, codedHeight: f.height, timestamp: f.timestamp });
          await writer!.write(frame);
        } finally { frame?.close(); }
        if (closed) break;
        dimensions = { width: f.width, height: f.height };
        port!.postMessage({ type: 'ack', id: source!.id, sequence: f.sequence });
        armTimeout(10000);
        if (!ready) { const stream = new MediaStream([track!]); ready = true; resolve(stream); }
      }
    } catch { stop(unreadable(), true); }
    finally { processing = false; }
  };
  let dimensions: { width?: number; height?: number } = {};
  const attachPort = (p: MessagePort) => {
    if (port) { closePort(p); stop(unreadable(), true); return; }
    port = p;
    // A running share must not claim or close the next share's transferred port.
    window.removeEventListener('message', receivePort);
    p.onmessageerror = () => stop(unreadable(), true);
    p.onmessage = event => {
      if (closed) return;
      const data: unknown = event.data;
      if (data && typeof data === 'object' && (data as { type?: unknown }).type === 'ended') {
        const end = data as { id?: unknown; reason?: unknown };
        stop(end.id === source!.id && (end.reason === 'stopped' || end.reason === 'failed') ? new DOMException('Desktop source ended', 'AbortError') : unreadable(), true); return;
      }
      // One write plus one queued frame: the sender must await the oldest ACK.
      if (!validFrame(data, source!.id, sequence, timestamp) || queue.length + Number(processing) >= 2) { stop(unreadable(), true); return; }
      sequence = data.sequence; timestamp = data.timestamp; queue.push(data); void drain();
    };
    p.start();
  };
  function receivePort(event: MessageEvent) {
    const data: unknown = event.data;
    if (event.source !== window || !data || typeof data !== 'object' || (data as { type?: unknown }).type !== 'dilla-screen-capture-port') return;
    const id: unknown = (data as { id?: unknown }).id, received = event.ports;
    if (typeof id !== 'string' || id.length === 0 || id.length > 256 || received.length !== 1) { for (const p of received) closePort(p); return; }
    const p = received[0];
    if (closed || (source && source.id !== id)) { closePort(p); return; }
    if (source) { attachPort(p); return; }
    if (earlyPorts.has(id) || earlyPorts.size >= 8) { closePort(p); return; }
    earlyPorts.set(id, p);
  }
  try {
    // The capture page can deliver a port or end before IPC resolves.
    window.addEventListener('message', receivePort); window.addEventListener('pagehide', navigate);
    stopEvents = bridge.onEvent(event => {
      if (event.type !== 'screen-capture-ended') return;
      if (!source) { if (earlyEnded.size < 16) earlyEnded.add(event.id); return; }
      if (event.id === source.id) stop(new DOMException('Desktop source ended', 'AbortError'), true);
    });
    // Human selection has no deadline; main bounds each owner to one pending picker.
    void bridge.requestScreenCapture(configured).then(selected => {
      if (closed) { if (selected) stopSource(selected); return; }
      if (!selected) { stop(new DOMException('Screen selection cancelled', 'NotAllowedError')); return; }
      if (typeof selected.id !== 'string' || !selected.id || selected.id.length > 256) { stop(unreadable()); return; }
      source = selected;
      if (!['monitor', 'window'].includes(selected.displaySurface)) { stop(unreadable()); return; }
      if (earlyEnded.has(selected.id)) { stop(new DOMException('Desktop source ended', 'AbortError')); return; }
      earlyEnded.clear();
      try {
        track = new Generator({ kind: 'video' }); writer = track.writable.getWriter(); stopTrack = track.stop.bind(track);
        track.stop = () => stop();
        track.addEventListener('ended', () => { if (!closed) stop(new DOMException('Generated track ended', 'AbortError')); }, { once: true });
        const settings = track.getSettings.bind(track);
        track.getSettings = () => ({ ...settings(), ...dimensions, displaySurface: source!.displaySurface, frameRate: configured.frameRate });
        track.applyConstraints = async constraints => {
          if (closed) throw new DOMException('Screen capture has stopped', 'InvalidStateError');
          const next = captureOptions(constraints, configured);
          try { await bridge.send({ type: 'configure-screen-capture', id: source!.id, options: next }); }
          catch (error) { stop(unreadable(), true); throw error; }
          if (closed) throw new DOMException('Screen capture has stopped', 'InvalidStateError');
          configured = next;
        };
        armTimeout(5000);
        const selectedPort = earlyPorts.get(selected.id);
        earlyPorts.delete(selected.id);
        for (const p of earlyPorts.values()) closePort(p);
        earlyPorts.clear();
        if (selectedPort) attachPort(selectedPort);
      } catch { stop(unreadable(), true); }
    }, () => stop(unreadable()));
  } catch { stop(unreadable()); }
  return result;
}
