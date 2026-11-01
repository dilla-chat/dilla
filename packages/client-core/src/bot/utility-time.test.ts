import { describe, expect, it } from 'vitest';
import { parseReminderTime, resolveReminderTimeCandidate } from './utility-time.ts';

const now = new Date('2026-01-01T00:00:00.000Z');

describe('parseReminderTime', () => {
  it('returns bounded relative durations and an exact timezone preview', () => {
    expect(parseReminderTime('1m', 'UTC', now)).toEqual({
      kind: 'valid', mode: 'relative', seconds: 60,
      preview: '2026-01-01 00:01:00 (UTC, UTC+00:00)',
    });
    expect(parseReminderTime('1.5m', 'Asia/Kathmandu', now)).toEqual({
      kind: 'valid', mode: 'relative', seconds: 90,
      preview: '2026-01-01 05:46:30 (Asia/Katmandu, UTC+05:45)',
    });
    expect(parseReminderTime('365d', 'UTC', now)).toEqual({
      kind: 'valid', mode: 'relative', seconds: 31_536_000,
      preview: '2027-01-01 00:00:00 (UTC, UTC+00:00)',
    });
  });

  it.each([
    ['00001m', 60],
    ['00001.500m', 90],
    ['0w0d0h1m0s0ms', 60],
    ['1h30m', 5_400],
    ['1m1h1m', 3_720],
    ['0'.repeat(4_096) + '1m', 60],
    ['500ms59s500ms', 60],
    ['00060s', 60],
    ['365d0ms', 31_536_000],
    [' 1m ', 60],
  ])('preserves duration grammar and unit arithmetic for %j', (input, seconds) => {
    expect(parseReminderTime(input, 'UTC', now)).toMatchObject({ kind: 'valid', mode: 'relative', seconds });
  });

  it.each([
    '0'.repeat(4_096) + 'x',
    '0'.repeat(4_096) + '.x',
    '0.' + '0'.repeat(4_096) + 'x',
    '1m' + '0'.repeat(4_096) + 'x',
    '1m 30s', '1m-30s', '1m.30s', '.5m', '1.m', '1.2.3m', '1M', '1m1', '1m30sx',
  ])('rejects incomplete or noncontiguous duration tokens (case %#)', input => {
    expect(parseReminderTime(input, 'UTC', now)).toEqual({ kind: 'invalid', reason: 'invalid-date' });
  });

  it.each(['0m', '59s999ms', '365d1s', '9'.repeat(400) + 'm'])('keeps duration bounds and numeric overflow checks for case %#', input => {
    expect(parseReminderTime(input, 'UTC', now).kind).toBe('invalid');
  });

  it('canonicalizes IANA aliases for previews and rejects fixed-offset zones', () => {
    const alias = parseReminderTime('1m', 'europe/stockholm', now);
    expect(alias.kind).toBe('valid');
    if (alias.kind === 'valid') expect(alias.preview).toContain('(Europe/Stockholm,');
    expect(parseReminderTime('1m', '+01:00', now)).toMatchObject({ kind: 'invalid', reason: 'invalid-timezone' });
  });

  it.each(['59s', '366d', '1.001s', '1ms', ''])('rejects out-of-policy or subsecond duration %j', (input) => {
    expect(parseReminderTime(input, 'UTC', now).kind).toBe('invalid');
  });

  it('accepts exact absolute min and max bounds', () => {
    expect(parseReminderTime('2026-01-01T00:01:00Z', 'UTC', now)).toMatchObject({
      kind: 'valid', mode: 'absolute', unixSeconds: 1_767_225_660,
    });
    expect(parseReminderTime('2027-01-01T00:00:00Z', 'UTC', now)).toMatchObject({
      kind: 'valid', mode: 'absolute', unixSeconds: 1_798_761_600,
    });
  });

  it('converts an ordinary local datetime through the named IANA zone', () => {
    expect(parseReminderTime('2026-01-01 02:30', 'Europe/Stockholm', now)).toEqual({
      kind: 'valid', mode: 'absolute', unixSeconds: 1_767_231_000,
      preview: '2026-01-01 02:30:00 (Europe/Stockholm, UTC+01:00)',
    });
  });

  it('uses the explicit offset to resolve an ISO instant and previews in the selected zone', () => {
    expect(parseReminderTime('2026-01-02T00:00+02:00', 'Europe/Stockholm', now)).toEqual({
      kind: 'valid', mode: 'absolute', unixSeconds: 1_767_304_800,
      preview: '2026-01-01 23:00:00 (Europe/Stockholm, UTC+01:00)',
    });
  });

  it('accepts an explicit Z suffix as an absolute UTC instant', () => {
    expect(parseReminderTime('2026-01-02T00:00Z', 'UTC', now)).toEqual({
      kind: 'valid', mode: 'absolute', unixSeconds: 1_767_312_000,
      preview: '2026-01-02 00:00:00 (UTC, UTC+00:00)',
    });
  });

  it.each([
    ['Europe/Stockholm', '2026-03-29T02:30'],
    ['America/New_York', '2026-03-08T02:30'],
    ['Australia/Lord_Howe', '2026-10-04T02:15'],
  ])('rejects a nonexistent wall time in %s', (timezone, input) => {
    expect(parseReminderTime(input, timezone, now)).toMatchObject({ kind: 'invalid', reason: 'nonexistent-local-time' });
  });

  it.each([
    ['Europe/Stockholm', '2026-10-25T02:30', [1_792_888_200, 1_792_891_800], ['UTC+02:00', 'UTC+01:00']],
    ['America/New_York', '2026-11-01T01:30', [1_793_511_000, 1_793_514_600], ['UTC-04:00', 'UTC-05:00']],
    ['Australia/Lord_Howe', '2026-04-05T01:45', [1_775_313_900, 1_775_315_700], ['UTC+11:00', 'UTC+10:30']],
  ])('returns both explicit overlap choices in %s', (timezone, input, instants, offsets) => {
    expect(parseReminderTime(input, timezone, now)).toMatchObject({
      kind: 'ambiguous',
      candidates: [
        { unixSeconds: instants[0], offsetLabel: offsets[0] },
        { unixSeconds: instants[1], offsetLabel: offsets[1] },
      ],
    });
  });

  it('keeps both overlap choices when one has passed, then bounds the explicit choice', () => {
    const duringFold = new Date('2026-10-25T00:45:00.000Z');
    const result = parseReminderTime('2026-10-25T02:30', 'Europe/Stockholm', duringFold);
    expect(result).toMatchObject({ kind: 'ambiguous', candidates: [{ unixSeconds: 1_792_888_200 }, { unixSeconds: 1_792_891_800 }] });
    expect(resolveReminderTimeCandidate(result, 1_792_888_200, duringFold)).toMatchObject({ kind: 'invalid', reason: 'outside-range' });
    expect(resolveReminderTimeCandidate(result, 1_792_891_800, duringFold)).toMatchObject({
      kind: 'valid', mode: 'absolute', unixSeconds: 1_792_891_800,
      preview: '2026-10-25 02:30:00 (Europe/Stockholm, UTC+01:00)',
    });
    expect(resolveReminderTimeCandidate(result, 1_792_891_801, duringFold).kind).toBe('invalid');
  });

  it('resolves a non-hour base offset without host-local Date parsing', () => {
    expect(parseReminderTime('2026-01-01T12:30', 'Asia/Kathmandu', now)).toEqual({
      kind: 'valid', mode: 'absolute', unixSeconds: 1_767_249_900,
      preview: '2026-01-01 12:30:00 (Asia/Katmandu, UTC+05:45)',
    });
  });

  it.each([
    ['UTC', '2026-02-30T09:30'],
    ['UTC', '2026-01-01T24:00'],
    ['UTC', '2026-01-01T00:00+24:00'],
    ['UTC', '2026-01-01T00:00Z'],
    ['Local', '2026-01-02T00:00'],
    ['Not/A_Zone', '2026-01-02T00:00'],
  ])('rejects invalid date/timezone input (%s, %s)', (timezone, input) => {
    expect(parseReminderTime(input, timezone, now).kind).toBe('invalid');
  });

  it('rejects dates before the minute-to-year window', () => {
    expect(parseReminderTime('2026-01-01T00:00:30Z', 'UTC', now)).toMatchObject({ kind: 'invalid', reason: 'outside-range' });
    expect(parseReminderTime('2027-01-01T00:01:00Z', 'UTC', now)).toMatchObject({ kind: 'invalid', reason: 'outside-range' });
  });
});
