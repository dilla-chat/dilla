import { describe, expect, it } from 'vitest';
import {
  DEFAULT_SHARE_OPTIONS, SHARE_FPS, SHARE_RESOLUTIONS, shareBitrateBps, shareEncoding, shareEstimate, shareSupport, shareVideoConstraints,
  supportedOptions, type ShareOptions,
} from './share-options.ts';

const o = (over: Partial<ShareOptions> = {}): ShareOptions => ({ ...DEFAULT_SHARE_OPTIONS, ...over });

describe('share options: the capture constraints', () => {
  it('the defaults are the prototype picker’s: the screen, 1080p, 30 fps, with sound', () => {
    expect(DEFAULT_SHARE_OPTIONS).toEqual({ surface: 'monitor', resolution: '1080p', fps: 30, systemAudio: true });
  });

  it.each([
    ['720p', 1280, 720], ['1080p', 1920, 1080], ['1440p', 2560, 1440],
  ] as const)('%s asks for at most %i × %i (the surface keeps its aspect ratio)', (resolution, width, height) => {
    expect(shareVideoConstraints(o({ resolution }))).toEqual({
      displaySurface: 'monitor', width: { max: width }, height: { max: height }, frameRate: { ideal: 30, max: 30 },
    });
  });

  it('source asks for no frame size', () => {
    expect(shareVideoConstraints(o({ resolution: 'source' }))).toEqual({ displaySurface: 'monitor', frameRate: { ideal: 30, max: 30 } });
  });

  it.each(SHARE_FPS)('%i fps is the ideal and the max frame rate', (fps) => {
    expect(shareVideoConstraints(o({ fps })).frameRate).toEqual({ ideal: fps, max: fps });
  });

  it.each(['monitor', 'window', 'browser'] as const)('the %s source is the displaySurface hint', (surface) => {
    expect(shareVideoConstraints(o({ surface })).displaySurface).toBe(surface);
  });

  it('a running capture is reshaped without the surface hint (applyConstraints has none)', () => {
    expect(shareVideoConstraints(o({ resolution: '720p', fps: 15 }), false)).toEqual({ width: { max: 1280 }, height: { max: 720 }, frameRate: { ideal: 15, max: 15 } });
  });
});

describe('share options: the encoding and the estimate (one table)', () => {
  it('the target is the prototype’s table at 30 fps, ×0.6 at 15 and ×1.6 at 60', () => {
    expect(shareBitrateBps(o({ resolution: '720p' }), null)).toBe(2_500_000);
    expect(shareBitrateBps(o({ resolution: '1080p' }), null)).toBe(4_500_000);
    expect(shareBitrateBps(o({ resolution: '1440p' }), null)).toBe(8_000_000);
    expect(shareBitrateBps(o({ resolution: 'source' }), null)).toBe(12_000_000);
    expect(shareBitrateBps(o({ resolution: '1080p', fps: 15 }), null)).toBe(2_700_000);
    expect(shareBitrateBps(o({ resolution: '1080p', fps: 60 }), null)).toBe(7_200_000);
  });

  it('the instance cap bounds the encoding, and the estimate says what is encoded', () => {
    const capped = o({ resolution: '1440p', fps: 60 });
    expect(shareEncoding(capped, 2_500_000)).toEqual({ maxBitrate: 2_500_000, maxFramerate: 60 });
    expect(shareEstimate(capped, 2_500_000)).toEqual({ mbps: '2.5', heavy: false });
  });

  it('the estimate is the encoding’s bitrate in Mb/s, one decimal; above 9000 kbit/s it is heavy', () => {
    for (const resolution of SHARE_RESOLUTIONS) {
      for (const fps of SHARE_FPS) {
        const bps = shareEncoding(o({ resolution, fps }), null).maxBitrate;
        const e = shareEstimate(o({ resolution, fps }), null);
        expect(e.mbps).toBe((bps / 1e6).toFixed(1));
        expect(e.heavy).toBe(bps > 9_000_000);
      }
    }
    expect(shareEstimate(o(), null)).toEqual({ mbps: '4.5', heavy: false });
    expect(shareEstimate(o({ resolution: 'source' }), null)).toEqual({ mbps: '12.0', heavy: true });
  });
});

describe('share options: what this browser can act on', () => {
  const chromeLinux = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';
  const chromeWin = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';
  const firefox = 'Mozilla/5.0 (X11; Linux x86_64; rv:144.0) Gecko/20100101 Firefox/144.0';
  const safari = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15';

  it('Chromium on Windows: every source, sound from each', () => {
    expect(shareSupport({ ua: chromeWin })).toEqual({ surface: true, audio: { monitor: true, window: true, browser: true }, tabOnly: false });
  });
  it('Chromium on Linux: every source, sound only from a tab', () => {
    expect(shareSupport({ ua: chromeLinux, uaPlatform: 'Linux', brands: ['Chromium', 'Google Chrome'] }))
      .toEqual({ surface: true, audio: { monitor: false, window: false, browser: true }, tabOnly: true });
  });
  it.each([['Firefox', firefox], ['Safari', safari]])('%s: no source choice and no sound', (_n, ua) => {
    expect(shareSupport({ ua })).toEqual({ surface: false, audio: { monitor: false, window: false, browser: false }, tabOnly: false });
  });

  it('what a browser cannot do is not asked for: no source → the screen, no sound → off', () => {
    const none = shareSupport({ ua: firefox });
    expect(supportedOptions(o({ surface: 'browser', systemAudio: true }), none)).toEqual(o({ surface: 'monitor', systemAudio: false }));
    const linux = shareSupport({ ua: chromeLinux });
    expect(supportedOptions(o({ surface: 'window', systemAudio: true }), linux)).toMatchObject({ surface: 'window', systemAudio: false });
    expect(supportedOptions(o({ surface: 'browser', systemAudio: true }), linux)).toMatchObject({ surface: 'browser', systemAudio: true });
  });
});
