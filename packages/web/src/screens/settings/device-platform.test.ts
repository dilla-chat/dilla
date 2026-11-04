import { describe, expect, it } from 'vitest';
import type { DesktopBridge } from '../../../../desktop/src/contract.ts';
import { devicePlatformLabel } from './device-platform.ts';

const FIREFOX = 'Mozilla/5.0 (X11; Linux x86_64; rv:139.0) Gecko/20100101 Firefox/139.0';
const ELECTRON = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Dilla/0.1.0-beta.2 Chrome/140.0.0.0 Electron/38.0.0 Safari/537.36';
const bridge = (platform?: string) => ({ ...(platform === undefined ? {} : { platform }), info: () => Promise.reject(new Error('unused')),
  send: () => Promise.resolve(), onEvent: () => () => {} }) as DesktopBridge;

describe('the device platform label (display only, never a trust signal)', () => {
  it('names the desktop app\'s OS from the shell bridge, whatever the user agent says', () => {
    expect(devicePlatformLabel(bridge('darwin'), ELECTRON)).toBe('Desktop · macOS');
    expect(devicePlatformLabel(bridge('win32'), ELECTRON)).toBe('Desktop · Windows');
    expect(devicePlatformLabel(bridge('linux'), FIREFOX)).toBe('Desktop · Linux');
    expect(devicePlatformLabel(bridge('aix'), ELECTRON)).toBe('Desktop');
    expect(devicePlatformLabel(bridge(), ELECTRON)).toBe('Desktop');
  });

  it('names the browser from its user agent, and says only Browser when it is unknown', () => {
    expect(devicePlatformLabel(undefined, FIREFOX)).toBe('Browser · Firefox');
    expect(devicePlatformLabel(undefined, 'something else')).toBe('Browser');
    // Without the shell's bridge an Electron-looking user agent is still a browser: the user agent claims nothing.
    expect(devicePlatformLabel(undefined, ELECTRON)).toBe('Browser · Chrome');
  });
});
