import { captureId, captureOptions, frameBytes, FrameWindow } from './screen-capture-contract.ts';
import type { ScreenCaptureOptions } from './contract.ts';

interface CaptureFrame {
  codedWidth: number;
  codedHeight: number;
  visibleRect: { x: number; y: number; width: number; height: number } | null;
  timestamp: number;
  close(): void;
  copyTo(buffer: ArrayBuffer, options: { format: 'RGBA'; rect: { x: number; y: number; width: number; height: number }; layout: { offset: number; stride: number }[] }): Promise<{ offset: number; stride: number }[]>;
}
declare const MediaStreamTrackProcessor: {
  new(options: { track: MediaStreamTrack; maxBufferSize: number }): { readable: ReadableStream<CaptureFrame> };
};
declare global {
  interface Window { dillaCaptureBroker: { status(type: 'started' | 'heartbeat' | 'ended' | 'failed' | 'configured', id: string): void } }
}
interface Capture {
  id: string;
  port: MessagePort;
  sourceId: string;
  options: ScreenCaptureOptions;
  flow: FrameWindow;
  stream: MediaStream | null;
  reader: ReadableStreamDefaultReader<CaptureFrame> | null;
  timer: ReturnType<typeof setInterval> | null;
  stopped: boolean;
  started: boolean;
  configuring: boolean;
  lastFrame: number;
  timestamp: number;
}
let active: Capture | null = null;

function finish(capture: Capture, reason: 'stopped' | 'failed'): void {
  if (capture.stopped) return;
  capture.stopped = true;
  if (capture.timer) clearInterval(capture.timer);
  for (const track of capture.stream?.getTracks() ?? []) track.stop();
  const reader = capture.reader;
  capture.reader = null;
  if (reader) void reader.cancel().then(() => { try { reader.releaseLock(); } catch { /* A pending read may still settle. */ } }).catch(() => {});
  try { capture.port.postMessage({ type: 'ended', id: capture.id, reason }); } catch { /* The consumer may already be gone. */ }
  capture.port.close();
  capture.port.onmessage = null;
  capture.port.onmessageerror = null;
  window.dillaCaptureBroker.status(reason === 'failed' ? 'failed' : 'ended', capture.id);
}
function constraints(options: ScreenCaptureOptions): MediaTrackConstraints {
  return { width: { max: options.width }, height: { max: options.height }, frameRate: { max: options.frameRate } };
}
async function configure(capture: Capture, input: unknown): Promise<void> {
  if (capture.stopped || capture.configuring) { finish(capture, 'failed'); return; }
  try {
    const options = captureOptions(input);
    const track = capture.stream?.getVideoTracks()[0];
    if (!track) throw new Error('Capture track unavailable');
    capture.configuring = true;
    await track.applyConstraints(constraints(options));
    if (capture.stopped) return;
    capture.options = options;
    capture.configuring = false;
    window.dillaCaptureBroker.status('configured', capture.id);
  } catch { finish(capture, 'failed'); }
}
async function run(capture: Capture): Promise<void> {
  try {
    // This source comes exclusively from the native main-process selection, never from a frame-port message.
    const stream = await navigator.mediaDevices.getUserMedia({ audio: false, video: {
      mandatory: { chromeMediaSource: 'desktop', chromeMediaSourceId: capture.sourceId,
        maxWidth: capture.options.width, maxHeight: capture.options.height, maxFrameRate: capture.options.frameRate },
    } as MediaTrackConstraints });
    capture.stream = stream;
    if (capture.stopped) { for (const track of stream.getTracks()) track.stop(); return; }
    const tracks = stream.getVideoTracks();
    if (tracks.length !== 1 || stream.getAudioTracks().length) throw new Error('Unexpected capture tracks');
    const track = tracks[0];
    track.addEventListener('ended', () => finish(capture, capture.started ? 'stopped' : 'failed'), { once: true });
    const reader = new MediaStreamTrackProcessor({ track, maxBufferSize: 1 }).readable.getReader();
    capture.reader = reader;
    while (!capture.stopped) {
      const { done, value: frame } = await reader.read();
      if (done) { finish(capture, capture.started ? 'stopped' : 'failed'); return; }
      try {
        if (capture.stopped) return;
        const rect = frame.visibleRect ?? { x: 0, y: 0, width: frame.codedWidth, height: frame.codedHeight };
        const width = rect.width, height = rect.height;
        const bytes = frameBytes(width, height);
        if (!Number.isFinite(frame.timestamp) || frame.timestamp < 0 || frame.timestamp < capture.timestamp) throw new Error('Invalid frame timestamp');
        capture.lastFrame = performance.now();
        // Reserve before allocation/copy. Dropped frames are still closed by finally.
        const sequence = capture.flow.reserve(performance.now());
        if (sequence === null) continue;
        const data = new ArrayBuffer(bytes);
        const layout = await frame.copyTo(data, { format: 'RGBA', rect, layout: [{ offset: 0, stride: width * 4 }] });
        if (capture.stopped) return;
        if (layout.length !== 1 || layout[0].offset !== 0 || layout[0].stride !== width * 4) throw new Error('Invalid frame layout');
        capture.port.postMessage({ type: 'frame', id: capture.id, sequence, width, height, timestamp: frame.timestamp, data }, [data]);
        capture.timestamp = frame.timestamp;
        if (!capture.started) { capture.started = true; window.dillaCaptureBroker.status('started', capture.id); }
      } finally { frame.close(); }
    }
  } catch { finish(capture, 'failed'); }
}

window.addEventListener('message', event => {
  if (event.source !== window || event.origin !== location.origin) return;
  const value: unknown = event.data;
  if (!value || typeof value !== 'object' || Array.isArray(value)) return;
  const data = value as Record<string, unknown>;
  if (data.type === 'dilla-screen-broker-configure') {
    if (active && data.id === active.id) void configure(active, data.options);
    return;
  }
  if (data.type !== 'dilla-screen-broker-start') return;
  if (active || event.ports.length !== 1 || !captureId(data.id) || typeof data.sourceId !== 'string'
    || data.sourceId.length > 512 || !/^(screen|window):[^\u0000-\u001f]+$/.test(data.sourceId)) {
    for (const port of event.ports) port.close();
    return;
  }
  let options: ScreenCaptureOptions;
  try { options = captureOptions(data.options); } catch { event.ports[0].close(); return; }
  const capture: Capture = { id: data.id, port: event.ports[0], sourceId: data.sourceId, options, flow: new FrameWindow(data.id),
    stream: null, reader: null, timer: null, stopped: false, started: false, configuring: false, lastFrame: performance.now(), timestamp: 0 };
  active = capture;
  capture.port.onmessage = event => {
    if (event.ports?.length) { for (const port of event.ports) port.close(); finish(capture, 'failed'); return; }
    if (!capture.flow.ack(event.data)) finish(capture, 'failed');
  };
  capture.port.onmessageerror = () => finish(capture, 'failed');
  capture.port.addEventListener('close', () => finish(capture, 'failed'), { once: true });
  capture.port.start();
  capture.timer = setInterval(() => {
    const now = performance.now();
    if (capture.flow.expired(now) || now - capture.lastFrame >= 15000) { finish(capture, 'failed'); return; }
    window.dillaCaptureBroker.status('heartbeat', capture.id);
  }, 1000);
  void run(capture);
});
window.addEventListener('pagehide', () => { if (active) finish(active, 'stopped'); }, { once: true });
