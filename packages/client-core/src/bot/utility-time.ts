const MIN_REMINDER_SECONDS = 60;
const MAX_REMINDER_SECONDS = 365 * 24 * 60 * 60;
const MAX_UNIX_SECONDS = 253_402_300_799;
const LOCAL_DATE_TIME = /^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2})(?::(\d{2})(?:\.(\d{1,9}))?)?(Z|[+-]\d{2}:\d{2})?$/;

export type ReminderTimeCandidate = {
  unixSeconds: number;
  offsetLabel: string;
  preview: string;
};

export type ReminderTimeResult =
  | { kind: 'valid'; mode: 'relative'; seconds: number; preview: string }
  | { kind: 'valid'; mode: 'absolute'; unixSeconds: number; preview: string }
  | { kind: 'ambiguous'; candidates: [ReminderTimeCandidate, ReminderTimeCandidate] }
  | { kind: 'invalid'; reason: string };

type LocalParts = { year: number; month: number; day: number; hour: number; minute: number; second: number };

/** Parse a reminder duration or datetime without consulting the host's local timezone. */
export function parseReminderTime(input: string, timezone: string, now: Date): ReminderTimeResult {
  if (!(now instanceof Date) || !Number.isFinite(now.getTime())) return invalid('invalid-now');
  const zone = normalizeReminderTimezone(timezone);
  if (zone === null) return invalid('invalid-timezone');
  const formatter = makeFormatter(zone);
  if (!formatter) return invalid('invalid-timezone');

  const trimmed = input.trim();
  const durationSeconds = parseDurationSeconds(trimmed);
  if (durationSeconds !== null) {
    if (durationSeconds < MIN_REMINDER_SECONDS || durationSeconds > MAX_REMINDER_SECONDS) return invalid('outside-range');
    const dueSeconds = Math.floor(now.getTime() / 1000) + durationSeconds;
    if (dueSeconds < 1 || dueSeconds > MAX_UNIX_SECONDS) return invalid('outside-range');
    return { kind: 'valid', mode: 'relative', seconds: durationSeconds, preview: formatPreview(dueSeconds, zone, formatter) };
  }

  const parsed = parseDateTime(trimmed);
  if (!parsed) return invalid(trimmed === '' ? 'invalid-format' : 'invalid-date');
  const { parts, offset } = parsed;
  const baseMillis = localAsUtcMillis(parts);
  if (!Number.isFinite(baseMillis)) return invalid('invalid-date');

  if (offset !== null) {
    const unixSeconds = (baseMillis - offset * 60_000) / 1000;
    if (!Number.isInteger(unixSeconds) || unixSeconds < 1 || unixSeconds > MAX_UNIX_SECONDS) return invalid('outside-range');
    const range = checkFutureWindow(unixSeconds, now);
    if (range) return invalid(range);
    return { kind: 'valid', mode: 'absolute', unixSeconds, preview: formatPreview(unixSeconds, zone, formatter) };
  }

  const matches = findLocalCandidates(parts, baseMillis, timezone, formatter);
  if (matches.length === 0) return invalid('nonexistent-local-time');
  if (matches.length > 1) {
    const [first, second] = matches.slice(0, 2);
    return {
      kind: 'ambiguous',
      candidates: [candidate(first, zone, formatter), candidate(second, zone, formatter)],
    };
  }
  const unixSeconds = matches[0];
  if (unixSeconds < 1 || unixSeconds > MAX_UNIX_SECONDS) return invalid('outside-range');
  const range = checkFutureWindow(unixSeconds, now);
  if (range) return invalid(range);
  return { kind: 'valid', mode: 'absolute', unixSeconds, preview: formatPreview(unixSeconds, zone, formatter) };
}

/** Return an Intl canonical IANA name that Go's time.LoadLocation can accept. */
export function normalizeReminderTimezone(timezone: string): string | null {
  if (typeof timezone !== 'string' || timezone.length < 1 || timezone.length > 100 || timezone === 'Local'
    || /^[+-]\d{2}(?::?\d{2})?$/u.test(timezone)) return null;
  try {
    return new Intl.DateTimeFormat('en', { timeZone: timezone }).resolvedOptions().timeZone;
  } catch {
    return null;
  }
}

function invalid(reason: string): ReminderTimeResult { return { kind: 'invalid', reason }; }

function parseDurationSeconds(value: string): number | null {
  if (!value) return null;
  const scale: Record<string, number> = { ms: 1, s: 1_000, m: 60_000, h: 3_600_000, d: 86_400_000, w: 604_800_000 };
  let millis = 0;
  let index = 0;
  // Consume each token at the current position; never retry an unterminated
  // digit run at every suffix, which makes an unanchored regex quadratic.
  while (index < value.length) {
    const start = index;
    while (isDurationDigit(value, index)) index++;
    if (index === start) return null;
    if (value[index] === '.') {
      const fractionStart = ++index;
      while (isDurationDigit(value, index)) index++;
      if (index === fractionStart) return null;
    }
    const amount = Number(value.slice(start, index));
    if (!Number.isFinite(amount)) return null;
    let unit = value[index++];
    if (unit === 'm' && value[index] === 's') { unit = 'ms'; index++; }
    if (scale[unit] === undefined) return null;
    millis += amount * scale[unit];
    if (!Number.isFinite(millis)) return null;
  }
  if (!Number.isInteger(millis / 1000)) return null;
  return millis / 1000;
}

function isDurationDigit(value: string, index: number): boolean {
  const code = value.charCodeAt(index);
  return code >= 48 && code <= 57;
}

function parseDateTime(value: string): { parts: LocalParts; offset: number | null } | null {
  const match = LOCAL_DATE_TIME.exec(value);
  if (!match) return null;
  const [, yearText, monthText, dayText, hourText, minuteText, secondText, fraction, offsetText] = match;
  const parts: LocalParts = {
    year: Number(yearText), month: Number(monthText), day: Number(dayText), hour: Number(hourText),
    minute: Number(minuteText), second: Number(secondText ?? '0'),
  };
  if (!validLocalParts(parts) || (fraction !== undefined && /[1-9]/.test(fraction))) return null;
  if (offsetText === undefined) return { parts, offset: null };
  if (offsetText === 'Z') return { parts, offset: 0 };
  const hours = Number(offsetText.slice(1, 3));
  const minutes = Number(offsetText.slice(4, 6));
  if (hours > 23 || minutes > 59) return null;
  const sign = offsetText[0] === '+' ? 1 : -1;
  return { parts, offset: sign * (hours * 60 + minutes) };
}

function validLocalParts(parts: LocalParts): boolean {
  if (parts.year < 1 || parts.year > 9999 || parts.month < 1 || parts.month > 12
    || parts.hour > 23 || parts.minute > 59 || parts.second > 59) return false;
  const date = new Date(0);
  date.setUTCFullYear(parts.year, parts.month - 1, parts.day);
  date.setUTCHours(parts.hour, parts.minute, parts.second, 0);
  return parts.day >= 1 && date.getUTCFullYear() === parts.year && date.getUTCMonth() === parts.month - 1 && date.getUTCDate() === parts.day;
}

function localAsUtcMillis(parts: LocalParts): number {
  const date = new Date(0);
  date.setUTCFullYear(parts.year, parts.month - 1, parts.day);
  date.setUTCHours(parts.hour, parts.minute, parts.second, 0);
  return date.getTime();
}

function makeFormatter(timezone: string): Intl.DateTimeFormat | null {
  const canonical = normalizeReminderTimezone(timezone);
  if (canonical === null) return null;
  try {
    return new Intl.DateTimeFormat('en-GB-u-ca-gregory-nu-latn', {
      timeZone: canonical,
      year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit',
      hourCycle: 'h23',
    });
  } catch {
    return null;
  }
}

function partsAt(unixSeconds: number, formatter: Intl.DateTimeFormat): LocalParts | null {
  const values = new Map(formatter.formatToParts(unixSeconds * 1000).map((part) => [part.type, part.value]));
  const parts: LocalParts = {
    year: Number(values.get('year')), month: Number(values.get('month')), day: Number(values.get('day')),
    hour: Number(values.get('hour')), minute: Number(values.get('minute')), second: Number(values.get('second')),
  };
  return Object.values(parts).every(Number.isFinite) ? parts : null;
}

function sameParts(left: LocalParts | null, right: LocalParts): boolean {
  return left !== null && left.year === right.year && left.month === right.month && left.day === right.day
    && left.hour === right.hour && left.minute === right.minute && left.second === right.second;
}

function offsetAt(unixSeconds: number, formatter: Intl.DateTimeFormat): number | null {
  const parts = partsAt(unixSeconds, formatter);
  if (!parts) return null;
  const offsetMinutes = (localAsUtcMillis(parts) - unixSeconds * 1000) / 60_000;
  return Number.isInteger(offsetMinutes) ? offsetMinutes : null;
}

function findLocalCandidates(parts: LocalParts, baseMillis: number, timezone: string, formatter: Intl.DateTimeFormat): number[] {
  const offsets = new Set<number>();
  // Sampling every 30 minutes across four days captures ordinary transitions and
  // half-hour DST changes such as Lord Howe, without depending on host TZ data.
  for (let deltaMinutes = -2_880; deltaMinutes <= 2_880; deltaMinutes += 30) {
    const instant = Math.floor((baseMillis + deltaMinutes * 60_000) / 1000);
    const offset = offsetAt(instant, formatter);
    if (offset !== null) offsets.add(offset);
  }
  const matches = new Set<number>();
  for (const offset of offsets) {
    const candidate = (baseMillis - offset * 60_000) / 1000;
    if (!Number.isInteger(candidate)) continue;
    if (sameParts(partsAt(candidate, formatter), parts)) matches.add(candidate);
  }
  return [...matches].sort((a, b) => a - b);
}

function checkFutureWindow(unixSeconds: number, now: Date): string | null {
  const secondsFromNow = unixSeconds - now.getTime() / 1000;
  return secondsFromNow < MIN_REMINDER_SECONDS || secondsFromNow > MAX_REMINDER_SECONDS ? 'outside-range' : null;
}

function offsetLabel(offsetMinutes: number): string {
  const sign = offsetMinutes < 0 ? '-' : '+';
  const absolute = Math.abs(offsetMinutes);
  return `UTC${sign}${String(Math.floor(absolute / 60)).padStart(2, '0')}:${String(absolute % 60).padStart(2, '0')}`;
}

function formatPreview(unixSeconds: number, timezone: string, formatter: Intl.DateTimeFormat): string {
  const parts = partsAt(unixSeconds, formatter)!;
  const offset = offsetAt(unixSeconds, formatter)!;
  return `${String(parts.year).padStart(4, '0')}-${String(parts.month).padStart(2, '0')}-${String(parts.day).padStart(2, '0')} `
    + `${String(parts.hour).padStart(2, '0')}:${String(parts.minute).padStart(2, '0')}:${String(parts.second).padStart(2, '0')} `
    + `(${timezone}, ${offsetLabel(offset)})`;
}

function candidate(unixSeconds: number, timezone: string, formatter: Intl.DateTimeFormat): ReminderTimeCandidate {
  const offset = offsetAt(unixSeconds, formatter)!;
  return { unixSeconds, offsetLabel: offsetLabel(offset), preview: formatPreview(unixSeconds, timezone, formatter) };
}

/** Require an explicit fold choice, then validate that chosen instant against the current scheduling window. */
export function resolveReminderTimeCandidate(result: ReminderTimeResult, unixSeconds: number, now: Date): ReminderTimeResult {
  if (!(now instanceof Date) || !Number.isFinite(now.getTime())) return invalid('invalid-now');
  if (result.kind !== 'ambiguous') return invalid('not-ambiguous');
  const chosen = result.candidates.find(candidate => candidate.unixSeconds === unixSeconds);
  if (!chosen) return invalid('invalid-selection');
  const range = checkFutureWindow(unixSeconds, now);
  if (range) return invalid(range);
  if (unixSeconds < 1 || unixSeconds > MAX_UNIX_SECONDS) return invalid('outside-range');
  return { kind: 'valid', mode: 'absolute', unixSeconds, preview: chosen.preview };
}
