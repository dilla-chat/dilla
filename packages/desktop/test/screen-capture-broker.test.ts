import { test } from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const id = '8c6b13cb-667a-4657-9b1d-bdb16fe2ac32';
async function fixture() {
  const compiled = await build({ entryPoints: [fileURLToPath(new URL('../src/screen-capture-broker.ts', import.meta.url))],
    bundle: true, write: false, platform: 'browser', format: 'iife' });
  let message: ((event: any) => void) | undefined;
  let clock = 0;
  const ticks: (() => void)[] = [];
  const status: any[] = [];
  const pending: ((data: any) => void)[] = [];
  let canceled = false, stopped = false, copies = 0, closes = 0;
  const track = { addEventListener: () => {}, stop: () => { stopped = true; }, applyConstraints: async () => {} };
  const reader = { read: () => new Promise(resolve => { pending.push(resolve); }), cancel: async () => { canceled = true; }, releaseLock: () => {} };
  const sent: any[] = [];
  const port: any = { start: () => {}, close: () => { port.closed = true; }, addEventListener: () => {},
    postMessage: (data: any) => sent.push(data), closed: false, onmessage: null, onmessageerror: null };
  const window: any = { addEventListener: (name: string, handler: any) => { if (name === 'message') message = handler; },
    dillaCaptureBroker: { status: (type: string, captureId: string) => status.push({ type, id: captureId }) } };
  vm.runInNewContext(compiled.outputFiles[0].text, {
    window, location: { origin: 'dilla-internal://capture' }, navigator: { mediaDevices: { getUserMedia: async (options: any) => {
      assert.equal(options.audio, false);
      assert.equal(options.video.mandatory.chromeMediaSourceId, 'screen:123:0');
      return { getTracks: () => [track], getVideoTracks: () => [track], getAudioTracks: () => [] };
    } } },
    MediaStreamTrackProcessor: class { readable = { getReader: () => reader }; },
    ArrayBuffer, Uint8Array, performance: { now: () => clock }, setInterval: (fn: () => void) => { ticks.push(fn); return 1; }, clearInterval: () => {},
  });
  message!({ source: window, origin: 'dilla-internal://capture', ports: [port], data: { type: 'dilla-screen-broker-start', id,
    sourceId: 'screen:123:0', options: { width: 1920, height: 1080, frameRate: 30 } } });
  const flush = async () => { for (let i = 0; i < 12; i++) await Promise.resolve(); };
  await flush();
  return { sent, port, status, flush, get stopped() { return stopped; }, get canceled() { return canceled; },
    get copies() { return copies; }, get closes() { return closes; },
    tick: (ms: number) => { clock = ms; for (const fn of ticks) fn(); },
    frame: async (width = 16, height = 16) => {
      const resolve = pending.shift(); assert.ok(resolve);
      resolve({ done: false, value: { displayWidth: width, displayHeight: height, codedWidth: width, codedHeight: height,
        visibleRect: { x: 0, y: 0, width, height }, timestamp: clock * 1000, close: () => { closes++; },
        copyTo: async (_bytes: any, options: any) => { copies++; assert.equal(options.format, 'RGBA'); return [{ offset: 0, stride: width * 4 }]; } } });
      await flush();
    },
  };
}

test('raw frame flow drops before copy at two outstanding frames and resumes only on exact oldest ACK', async () => {
  const f = await fixture();
  await f.frame(); await f.frame(); await f.frame();
  assert.equal(f.sent.filter(data => data.type === 'frame').length, 2);
  assert.equal(f.copies, 2);
  assert.equal(f.closes, 3);
  assert.equal(f.sent[0].data.byteLength, 1024);
  assert.equal(f.sent[0].sequence, 0);
  f.port.onmessage({ data: { type: 'ack', id, sequence: 0 } });
  await f.frame();
  assert.equal(f.sent.filter(data => data.type === 'frame').length, 3);
  assert.equal(f.copies, 3);
  f.port.onmessage({ data: { type: 'ack', id, sequence: 0 } });
  assert.equal(f.stopped, true);
  assert.equal(f.canceled, true);
  assert.equal(f.port.closed, true);
  assert.equal(f.sent.at(-1).type, 'ended');
  assert.equal(f.sent.at(-1).reason, 'failed');
});

test('lost consumer ACK closes port and stops native capture within bounded timeout', async () => {
  const f = await fixture();
  await f.frame();
  f.tick(5000);
  assert.equal(f.stopped, true);
  assert.equal(f.port.closed, true);
  assert.deepEqual(f.status.at(-1), { type: 'failed', id });
});

test('oversized native frame is closed without allocating or transferring pixel memory', async () => {
  const f = await fixture();
  await f.frame(3841, 2160);
  assert.equal(f.copies, 0);
  assert.equal(f.closes, 1);
  assert.equal(f.sent.filter(data => data.type === 'frame').length, 0);
  assert.equal(f.stopped, true);
});

test('native capture stalled without any frame fails rather than retaining tracks indefinitely', async () => {
  const f = await fixture();
  f.tick(15_000);
  assert.equal(f.stopped, true);
  assert.equal(f.port.closed, true);
});
