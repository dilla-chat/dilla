import { desktopBridge } from '../../desktop.ts';
import { t } from '../../strings/index.ts';
import { browserName } from './device-name.ts';

const DESKTOP_OS: Readonly<Record<string, string>> = { darwin: 'macOS', win32: 'Windows', linux: 'Linux' };

/**
 * Where this device runs, for its own row in Settings → Devices: "Desktop · macOS" in the desktop app (the shell's preload
 * sets the bridge's fixed `platform`), "Browser · Firefox" in a browser (its user agent), or "Desktop"/"Browser" when the
 * rest is unknown.
 *
 * This is a display label, never a trust signal. The trust signal is the custody tier in the device's signed credential,
 * which the core sets (tier 1, browser custody, for the web client and the desktop app alike) and which nothing here reads
 * or changes. Instance-served page script could fake a bridge in a browser and make this label say "Desktop"; that buys
 * nothing, because the label is shown only on the device's own row, is never sent to the server or other members, and
 * grants nothing.
 */
export function devicePlatformLabel(bridge = desktopBridge(), userAgent = globalThis.navigator?.userAgent ?? ''): string {
  if (bridge !== undefined) {
    const os = typeof bridge.platform === 'string' ? DESKTOP_OS[bridge.platform] : undefined;
    return os === undefined ? t('devices.platform.desktopUnknown') : t('devices.platform.desktop', { os });
  }
  const browser = browserName(userAgent);
  return browser === null ? t('devices.platform.browserUnknown') : t('devices.platform.browser', { browser });
}
