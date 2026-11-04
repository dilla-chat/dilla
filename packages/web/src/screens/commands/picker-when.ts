// Signal wave B Task 2: `/remind`'s when chip. The prototype's phrases ("in 1 hour", "tonight 20:00", "tomorrow 09:00")
// become the registry's forms (a duration, or an ISO local date and time in the browser's zone, Q10), then the reminder
// parser of client-core decides: valid (with the "Fri 30 Oct, 20:00 CET" preview), ambiguous (a DST fold: two
// candidates), or invalid.
import { parseReminderTime, type ReminderTimeCandidate } from '@dilla/client-core';

export type WhenResult =
  | { kind: 'empty' }
  | { kind: 'invalid'; reason: 'format' | 'range' }
  | { kind: 'valid'; when: string; unixSeconds: number; preview: string }
  | { kind: 'ambiguous'; when: string; candidates: readonly (ReminderTimeCandidate & { label: string })[] };

/** The browser's time zone (Q10), as an IANA name. */
export function browserTimeZone(): string {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'; } catch { return 'UTC'; }
}

/** The prototype's `fmtWhen`: "Fri 30 Oct, 20:00 CET", in the given zone. */
export function formatWhen(unixSeconds: number, timeZone: string): string {
  return new Intl.DateTimeFormat('en-GB', { weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit', timeZoneName: 'short', timeZone })
    .format(new Date(unixSeconds * 1000));
}

const pad = (n: number): string => String(n).padStart(2, '0');

function localNow(now: Date, timeZone: string): { y: number; m: number; d: number; hh: number; mm: number } {
  const parts = new Map(new Intl.DateTimeFormat('en-GB-u-ca-gregory-nu-latn', { timeZone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })
    .formatToParts(now).map(part => [part.type, part.value]));
  return { y: Number(parts.get('year')), m: Number(parts.get('month')), d: Number(parts.get('day')), hh: Number(parts.get('hour')), mm: Number(parts.get('minute')) };
}

/** The registry's form of a phrase, or null when it is none the picker knows. */
export function registryWhen(text: string, timeZone: string, now: Date): string | null {
  const value = text.trim().toLowerCase();
  if (value === '') return null;
  const spoken = /^in\s+(\d+)\s*(minutes?|mins?|m|hours?|hrs?|h|days?|d|weeks?|w)$/u.exec(value);
  if (spoken !== null) return `${spoken[1]}${spoken[2][0]}`;
  const short = /^(?:in\s+)?((?:\d+(?:\.\d+)?(?:ms|s|m|h|d|w))+)$/u.exec(value);
  if (short !== null) return short[1];
  const clock = /^(tonight|today|tomorrow)?\s*(\d{1,2}):(\d{2})$/u.exec(value);
  if (clock !== null) {
    const hour = Number(clock[2]); const minute = Number(clock[3]);
    if (hour > 23 || minute > 59) return null;
    const here = localNow(now, timeZone);
    const later = clock[1] === 'tomorrow' || hour * 60 + minute <= here.hh * 60 + here.mm;
    const day = new Date(Date.UTC(here.y, here.m - 1, here.d + (later ? 1 : 0)));
    return `${String(day.getUTCFullYear()).padStart(4, '0')}-${pad(day.getUTCMonth() + 1)}-${pad(day.getUTCDate())}T${pad(hour)}:${pad(minute)}`;
  }
  const iso = /^(\d{4}-\d{2}-\d{2})[t ](\d{2}:\d{2}(?::\d{2})?)$/u.exec(value);
  if (iso !== null) return `${iso[1]}T${iso[2]}`;
  return null;
}

/** The when chip's state: what the bar previews, what the suggestions offer, what a run sends. */
export function resolveWhen(text: string, timeZone: string, now: Date, choiceLabel: (index: number, candidate: ReminderTimeCandidate, preview: string) => string): WhenResult {
  if (text.trim() === '') return { kind: 'empty' };
  const when = registryWhen(text, timeZone, now);
  if (when === null) return { kind: 'invalid', reason: 'format' };
  const parsed = parseReminderTime(when, timeZone, now);
  if (parsed.kind === 'invalid') return { kind: 'invalid', reason: parsed.reason === 'outside-range' ? 'range' : 'format' };
  if (parsed.kind === 'ambiguous') return { kind: 'ambiguous', when,
    candidates: parsed.candidates.map((candidate, index) => ({ ...candidate, label: choiceLabel(index, candidate, formatWhen(candidate.unixSeconds, timeZone)) })) };
  const unixSeconds = parsed.mode === 'relative' ? Math.floor(now.getTime() / 1000) + parsed.seconds : parsed.unixSeconds;
  return { kind: 'valid', when, unixSeconds, preview: formatWhen(unixSeconds, timeZone) };
}
