import { afterEach, expect, it, vi } from 'vitest';
import { LocalMicrophoneTest, LOOPBACK_MS, type LoopbackSettings } from './microphone-test';

// Signal task 10b: Let's check is a ten-second live loopback through the call's own chain (processor → input stage).
const { made, FakeStage } = vi.hoisted(() => {
  const made: { stage: Array<InstanceType<typeof FakeStage>> } = { stage: [] };
  class FakeStage {
    gains: number[] = []; gates: unknown[] = []; destroyed = false;
    output = { connect: vi.fn() };
    constructor(readonly inner: unknown, readonly opts: { gain: number; gate: unknown; onLevel(l: unknown): void }) { made.stage.push(this); }
    async init() {}
    setGain(g: number) { this.gains.push(g); }
    setGate(g: unknown) { this.gates.push(g); }
    destroy() { this.destroyed = true; return Promise.resolve(); }
  }
  return { made, FakeStage };
});
vi.mock('@dilla/media', () => ({
  DillaMicrophoneProcessor: class { processedTrack = { stop() {} }; async init() {} async destroy() {} },
  DillaInputStage: FakeStage,
  processingConstraints: (p: { echo: boolean; agc: boolean }) => ({ echoCancellation: p.echo, autoGainControl: p.agc }),
}));
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); made.stage.length = 0; });

const SETTINGS: LoopbackSettings = { input: 'mic', output: 'spk', noise: 'off', processing: { echo: false, agc: true }, inputVolume: 150,
  outputVolume: 50, sensitivity: { auto: false, thresholdDb: -40 }, mode: 'vad' };

function audio() {
  let closed = false, stopped = false;
  const gains: Array<{ gain: { value: number; setTargetAtTime: ReturnType<typeof vi.fn> }; connect: ReturnType<typeof vi.fn>; disconnect: ReturnType<typeof vi.fn> }> = [];
  const sinks: string[] = [];
  const stream = { getTracks: () => [{ stop: () => { stopped = true; } }], getAudioTracks: () => [{ addEventListener: () => undefined }] };
  const gum = vi.fn((c: MediaStreamConstraints) => { void c; return Promise.resolve(stream); });
  vi.stubGlobal('AudioContext', class {
    state = 'running'; destination = {}; currentTime = 0;
    resume = async () => {}; close = () => { closed = true; this.state = 'closed'; return Promise.resolve(); };
    setSinkId = (id: string) => { sinks.push(id); return Promise.resolve(); };
    createGain() { const g = { gain: { value: 1, setTargetAtTime: vi.fn() }, connect: vi.fn(), disconnect: vi.fn() }; gains.push(g); return g; }
  });
  vi.stubGlobal('navigator', { mediaDevices: { getUserMedia: gum } });
  return { gum, gains, sinks, closed: () => closed, stopped: () => stopped };
}
const events = () => ({ level: vi.fn(), status: vi.fn(), ended: vi.fn() });

it('captures with the echo/AGC switches, applies volume and gate, and plays only on the chosen output', async () => {
  const a = audio();
  const test = new LocalMicrophoneTest(events());
  await test.start(SETTINGS);
  expect(a.gum.mock.calls[0][0]).toMatchObject({ audio: { deviceId: { exact: 'mic' }, echoCancellation: false, autoGainControl: true } });
  expect(made.stage[0].opts).toMatchObject({ gain: 1.5, gate: { enabled: true, auto: false, thresholdDb: -40 } });
  expect(a.sinks).toEqual(['spk']);
  expect(a.gains[0].gain.value).toBe(0.5);
  expect(made.stage[0].output.connect).toHaveBeenCalledWith(a.gains[0]);
  await test.close();
});

it('changes during the test apply at once', async () => {
  const a = audio();
  const test = new LocalMicrophoneTest(events());
  await test.start(SETTINGS);
  test.update({ inputVolume: 80, sensitivity: { auto: true, thresholdDb: -40 }, mode: 'vad', outputVolume: 200 });
  expect(made.stage[0].gains).toEqual([0.8]);
  expect(made.stage[0].gates).toEqual([{ enabled: true, auto: true, thresholdDb: -40 }]);
  // L3: the loopback never plays above 100 %.
  expect(a.gains[0].gain.setTargetAtTime).toHaveBeenCalledWith(1, 0, 0.01);
  // Under push to talk the gate is off, as in the call.
  test.update({ sensitivity: { auto: true, thresholdDb: -40 }, mode: 'ptt' });
  expect(made.stage[0].gates.at(-1)).toEqual({ enabled: false, auto: true, thresholdDb: -40 });
  await test.close();
});

it('ends by itself after ten seconds and releases everything', async () => {
  vi.useFakeTimers();
  const a = audio();
  const e = events();
  const test = new LocalMicrophoneTest(e);
  await test.start(SETTINGS);
  await vi.advanceTimersByTimeAsync(LOOPBACK_MS - 1);
  expect(e.ended).not.toHaveBeenCalled();
  await vi.advanceTimersByTimeAsync(1);
  expect(e.ended).toHaveBeenCalledTimes(1);
  expect(a.stopped()).toBe(true);
  expect(a.closed()).toBe(true);
  expect(made.stage[0].destroyed).toBe(true);
});

it('closing during a permission prompt releases a late microphone and plays nothing', async () => {
  const a = audio();
  let grant!: (s: unknown) => void;
  a.gum.mockImplementationOnce(() => new Promise(r => { grant = r as (s: unknown) => void; }));
  const test = new LocalMicrophoneTest(events());
  const start = test.start(SETTINGS);
  await test.close();
  grant({ getTracks: () => [{ stop: () => undefined }], getAudioTracks: () => [{}] });
  await start;
  expect(made.stage).toHaveLength(0);
  expect(a.gains).toHaveLength(0);
});

it('bounds a stalled setup', async () => {
  vi.useFakeTimers();
  const a = audio();
  a.gum.mockImplementationOnce(() => new Promise(() => undefined));
  const test = new LocalMicrophoneTest(events());
  const rejected = expect(test.start(SETTINGS)).rejects.toThrow(/timed out/);
  await vi.advanceTimersByTimeAsync(15_000);
  await rejected;
  expect(a.closed()).toBe(true);
});
