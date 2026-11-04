/** The prototype's fmtWhen: local time with weekday, date and time-zone abbreviation. */
export function localUtilityTime(unixSeconds: number): string | null {
  const at = new Date(unixSeconds * 1000);
  if (!Number.isFinite(at.getTime())) return null;
  return new Intl.DateTimeFormat('en-GB', { weekday: 'short', day: 'numeric', month: 'short',
    hour: '2-digit', minute: '2-digit', timeZoneName: 'short' }).format(at);
}
