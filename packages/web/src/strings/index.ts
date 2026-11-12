import { en } from './en.ts';
export { en } from './en.ts';
export type StringKey = keyof typeof en;

export function t(key: StringKey, vars?: Record<string, string | number>): string {
  return en[key].replace(/\{([a-zA-Z]+)\}/g, (_, name: string) => {
    if (vars?.[name] === undefined) throw new Error(`strings: ${key} needs {${name}}`);
    return String(vars[name]);
  });
}

/**
 * The locale dates follow: the system region. Electron's own Intl reads the UI language (en-US), not the macOS region, so the
 * desktop shell hands the main process's system locale to the preload (`dillaDesktop.locale`); the web uses `navigator.language`.
 */
export function dateLocale(): string {
  const shell = (globalThis as { dillaDesktop?: { locale?: unknown } }).dillaDesktop?.locale;
  if (typeof shell === 'string' && shell !== '') {
    try { return Intl.getCanonicalLocales(shell)[0] ?? 'en-GB'; } catch { /* fall through to the page's own language */ }
  }
  return typeof navigator === 'undefined' || !navigator.language ? 'en-GB' : navigator.language;
}

const formats = new Map<string, Intl.DateTimeFormat>();
function format(kind: 'time' | 'day' | 'dayTime' | 'clock' | 'stamp'): Intl.DateTimeFormat {
  // Message times are always 24-hour HH:MM, as the design draws them; only dates follow the region.
  const locale = kind === 'time' || kind === 'clock' ? 'en-GB' : dateLocale();
  const key = `${kind}|${locale}`;
  let f = formats.get(key);
  if (f === undefined) {
    const options: Record<typeof kind, Intl.DateTimeFormatOptions> = {
      time: { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' },
      day: { year: 'numeric', month: 'short', day: 'numeric' },
      dayTime: { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' },
      clock: { hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' },
      stamp: { dateStyle: 'medium', timeStyle: 'short', hourCycle: 'h23' },
    };
    try { f = new Intl.DateTimeFormat(locale, options[kind]); } catch { f = new Intl.DateTimeFormat('en-GB', options[kind]); }
    formats.set(key, f);
  }
  return f;
}

export function formatTime(ts: number): string { return format('time').format(new Date(ts * 1000)); }
export function formatDay(ts: number): string { return format('day').format(new Date(ts * 1000)); }
export function formatDayTime(ts: number): string { return format('dayTime').format(new Date(ts * 1000)); }
/** Diagnostics clock: 24-hour HH:MM:SS from milliseconds. */
export function formatClock(ms: number): string { return format('clock').format(new Date(ms)); }
/** A medium date with a 24-hour short time, in the system region (the moderation log). */
export function formatStamp(ts: number): string { return format('stamp').format(new Date(ts * 1000)); }
