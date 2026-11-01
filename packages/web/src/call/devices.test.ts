/* eslint-disable @typescript-eslint/require-await -- the doubles answer as the async functions they stand for do */
import { describe, expect, it, vi } from 'vitest';
import { acquireMic, acquireCamera, acquireScreen, shareAudioAdvice } from './devices.ts';

const vtrack = (surface: string) => ({ kind: 'video', stop: vi.fn(), getSettings: () => ({ displaySurface: surface }) }) as unknown as MediaStreamTrack;
const atrack = (restrict?: boolean) => ({ kind: 'audio', stop: vi.fn(), getSettings: () => (restrict === undefined ? {} : { restrictOwnAudio: restrict }) }) as unknown as MediaStreamTrack;
const track = (kind: string) => ({ kind, stop: vi.fn(), id: `${kind}-1` }) as unknown as MediaStreamTrack;
const stream = (...tracks: MediaStreamTrack[]) => ({
  getTracks: () => tracks,
  getAudioTracks: () => tracks.filter((t) => t.kind === 'audio'),
  getVideoTracks: () => tracks.filter((t) => t.kind === 'video'),
}) as unknown as MediaStream;
const named = (name: string) => Object.assign(new Error(name), { name });
const mic = { kind: 'audioinput', deviceId: 'default', groupId: '', label: '' } as MediaDeviceInfo;

function media(gum: () => Promise<MediaStream>, devices: MediaDeviceInfo[] = [mic]) {
  return { getUserMedia: vi.fn(gum), enumerateDevices: vi.fn(async () => devices) };
}

describe('the microphone (flow 06: listen-only states)', () => {
  it('returns the track, asking for the chosen input with the browser AEC/NS/AGC on', async () => {
    const t = track('audio');
    const m = media(async () => stream(t));
    expect(await acquireMic(m, 'abc')).toEqual({ track: t });
    expect(m.getUserMedia).toHaveBeenCalledWith({ audio: { deviceId: { exact: 'abc' }, echoCancellation: true, noiseSuppression: true, autoGainControl: true } });
  });

  it('the system default asks for no device id', async () => {
    const m = media(async () => stream(track('audio')));
    await acquireMic(m, '');
    expect(m.getUserMedia).toHaveBeenCalledWith({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true } });
  });

  it('NotAllowedError is "denied"', async () => {
    expect(await acquireMic(media(async () => { throw named('NotAllowedError'); }), '')).toEqual({ mic: 'denied' });
  });

  it('NotFoundError, or no audioinput listed, is "missing" (and getUserMedia is not asked then)', async () => {
    expect(await acquireMic(media(async () => { throw named('NotFoundError'); }), '')).toEqual({ mic: 'missing' });
    const none = media(async () => stream(track('audio')), []);
    expect(await acquireMic(none, '')).toEqual({ mic: 'missing' });
    expect(none.getUserMedia).not.toHaveBeenCalled();
  });

  it('a chosen input that is gone falls back to the system default once', async () => {
    const t = track('audio');
    let first = true;
    const m = media(async () => { if (first) { first = false; throw named('OverconstrainedError'); } return stream(t); });
    expect(await acquireMic(m, 'gone')).toEqual({ track: t });
    expect(m.getUserMedia).toHaveBeenCalledTimes(2);
  });

  it('a stream without an audio track stops what it got and is "missing"', async () => {
    const v = track('video');
    expect(await acquireMic(media(async () => stream(v)), '')).toEqual({ mic: 'missing' });
    expect((v as unknown as { stop: ReturnType<typeof vi.fn> }).stop).toHaveBeenCalled();
  });
});

describe('camera and screen', () => {
  it('the camera returns its video track', async () => {
    const v = track('video');
    const m = media(async () => stream(v));
    expect(await acquireCamera(m)).toBe(v);
    expect(m.getUserMedia).toHaveBeenCalledWith({ audio: false, video: true });
  });

  it('the share asks the browser to leave out the call\'s own sound, and for every audio it offers', async () => {
    const getDisplayMedia = vi.fn(async () => stream(vtrack('browser'), atrack(true)));
    await acquireScreen({ getDisplayMedia });
    expect(getDisplayMedia).toHaveBeenCalledWith({
      video: { displaySurface: 'monitor', width: { max: 1920 }, height: { max: 1080 }, frameRate: { ideal: 30, max: 30 } },
      audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 2, restrictOwnAudio: true },
      systemAudio: 'include', windowAudio: 'system', selfBrowserSurface: 'exclude', surfaceSwitching: 'exclude',
    });
  });

  it('task 8c: the picker\'s options are the request: the surface, the frame, the rate, and no sound when it is off', async () => {
    const getDisplayMedia = vi.fn(async () => stream(vtrack('window')));
    await acquireScreen({ getDisplayMedia }, { surface: 'window', resolution: 'source', fps: 60, systemAudio: false });
    expect(getDisplayMedia).toHaveBeenCalledWith({
      video: { displaySurface: 'window', frameRate: { ideal: 60, max: 60 } },
      audio: false,
      systemAudio: 'exclude', windowAudio: 'exclude', selfBrowserSurface: 'exclude', surfaceSwitching: 'exclude',
    });
  });

  it('a tab share with sound is tab audio', async () => {
    const a = atrack();
    expect(await acquireScreen({ getDisplayMedia: async () => stream(vtrack('browser'), a) })).toMatchObject({ audio: a, audioKind: 'tab' });
  });

  it('a restricted screen share with sound is system audio', async () => {
    const a = atrack(true);
    expect(await acquireScreen({ getDisplayMedia: async () => stream(vtrack('monitor'), a) })).toMatchObject({ audio: a, audioKind: 'system' });
  });

  it('a system share is published whatever the browser did with restrictOwnAudio', async () => {
    // D5: no detection for browsers that ignore the constraint (Chrome/Edge < 141); the echo there is a documented limit.
    const a = atrack(); // getSettings() has no restrictOwnAudio
    const v = vtrack('window');
    const r = await acquireScreen({ getDisplayMedia: async () => stream(v, a) });
    expect(r).toEqual({ video: v, audio: a, audioKind: 'system' });
    expect((a as unknown as { stop: ReturnType<typeof vi.fn> }).stop).not.toHaveBeenCalled();
  });

  it('a screen without audio is none', async () => {
    expect(await acquireScreen({ getDisplayMedia: async () => stream(vtrack('monitor')) })).toMatchObject({ audio: null, audioKind: 'none' });
  });
});

// Privacy preference must reach capture, not only the neural processor.
it('Off disables browser suppression at acquisition while retaining echo cancellation', async () => {
  const calls: MediaStreamConstraints[] = [];
  const track = { stop() {} } as MediaStreamTrack;
  const media = { enumerateDevices: async () => [{kind:'audioinput'} as MediaDeviceInfo], getUserMedia: async (c:MediaStreamConstraints) => {calls.push(c); return {getAudioTracks:()=>[track],getTracks:()=>[track]} as MediaStream;} };
  await acquireMic(media, '', false);
  expect(calls[0]).toMatchObject({audio:{noiseSuppression:false,echoCancellation:true,autoGainControl:true}});
});

describe('shareAudioAdvice (#37): what sound a share can carry on this browser', () => {
  const cases: Array<[string, { ua: string; uaPlatform?: string; brands?: string[] }, 'unsupported' | 'tab-only' | 'tab-or-system']> = [
    ['Firefox on Linux', { ua: 'Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0' }, 'unsupported'],
    ['Zen (Firefox-based) on Linux', { ua: 'Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0 Zen/1.0' }, 'unsupported'],
    ['Safari on macOS', { ua: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15' }, 'unsupported'],
    ['Chrome on Linux', { ua: 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36', uaPlatform: 'Linux', brands: ['Chromium', 'Google Chrome'] }, 'tab-only'],
    ['Chrome on Windows', { ua: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36', uaPlatform: 'Windows', brands: ['Chromium', 'Google Chrome'] }, 'tab-or-system'],
    ['Edge on Windows', { ua: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0' }, 'tab-or-system'],
    ['Chrome on macOS', { ua: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36', uaPlatform: 'macOS', brands: ['Chromium', 'Google Chrome'] }, 'tab-or-system'],
    ['Chrome on Chrome OS', { ua: 'Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36', uaPlatform: 'Chrome OS', brands: ['Chromium', 'Google Chrome'] }, 'tab-or-system'],
    ['Chromium OS platform string', { ua: 'Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36', uaPlatform: 'Chromium OS', brands: ['Chromium'] }, 'tab-or-system'],
    ['brands alone name Chromium (no Chrome/ in the UA), on Windows', { ua: 'Mozilla/5.0 (Windows NT 10.0)', brands: ['Chromium'] }, 'tab-or-system'],
    ['Chrome on iOS (CriOS)', { ua: 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/141.0.0.0 Mobile/15E148 Safari/604.1' }, 'unsupported'],
    ['Chrome on Android', { ua: 'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36', uaPlatform: 'Android', brands: ['Chromium', 'Google Chrome'] }, 'tab-only'],
  ];
  it.each(cases)('%s', (_name, env, want) => {
    expect(shareAudioAdvice(env)).toBe(want);
  });
});
