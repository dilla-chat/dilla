/** The browser and system named in a user agent, as the words a person would use. */
export interface DeviceNameParts { browser: string; os: string }

// Order matters: Edge, Opera and Samsung Internet also say Chrome, and Chrome also says Safari.
const BROWSERS: readonly (readonly [RegExp, string])[] = [
  [/\bEdg(?:e|A|iOS)?\//, 'Edge'], [/\bOPR\//, 'Opera'], [/\bSamsungBrowser\//, 'Samsung Internet'],
  [/\b(?:Firefox|FxiOS)\//, 'Firefox'], [/(?:\b|Headless)(?:Chrome|CriOS)\//, 'Chrome'], [/\bSafari\//, 'Safari'],
];
// iPhone and iPad say "like Mac OS X", Android says "Linux", and ChromeOS says "X11": the narrow ones go first.
const SYSTEMS: readonly (readonly [RegExp, string])[] = [
  [/\b(?:iPhone|iPad|iPod)\b/, 'iOS'], [/\bAndroid\b/, 'Android'], [/\bCrOS\b/, 'ChromeOS'],
  [/\bWindows\b/, 'Windows'], [/\bMac OS X\b|\bMacintosh\b/, 'macOS'], [/\bLinux\b|\bX11\b/, 'Linux'],
];

/**
 * A default name for the device this browser runs on, read from its user agent ("Firefox on Linux"). Null when either
 * part is not recognised: a wrong guess would be worse than a blank field. It is only a suggestion the person edits.
 */
export function suggestDeviceName(userAgent: string): DeviceNameParts | null {
  const browser = BROWSERS.find(([re]) => re.test(userAgent))?.[1];
  const os = SYSTEMS.find(([re]) => re.test(userAgent))?.[1];
  return browser === undefined || os === undefined ? null : { browser, os };
}
