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
  if (typeof shell === 'string' && shell !== '') return shell;
  return typeof navigator === 'undefined' || !navigator.language ? 'en-GB' : navigator.language;
}

const formats = new Map<string, Intl.DateTimeFormat>();
function format(kind: 'time' | 'day' | 'dayTime'): Intl.DateTimeFormat {
  // Message times are always 24-hour HH:MM, as the design draws them; only dates follow the region.
  const locale = kind === 'time' ? 'en-GB' : dateLocale();
  const key = `${kind}|${locale}`;
  let f = formats.get(key);
  if (f === undefined) {
    const options: Record<typeof kind, Intl.DateTimeFormatOptions> = {
      time: { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' },
      day: { year: 'numeric', month: 'short', day: 'numeric' },
      dayTime: { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' },
    };
    try { f = new Intl.DateTimeFormat(locale, options[kind]); } catch { f = new Intl.DateTimeFormat('en-GB', options[kind]); }
    formats.set(key, f);
  }
  return f;
}

export function formatTime(ts: number): string { return format('time').format(new Date(ts * 1000)); }
export function formatDay(ts: number): string { return format('day').format(new Date(ts * 1000)); }
export function formatDayTime(ts: number): string { return format('dayTime').format(new Date(ts * 1000)); }
