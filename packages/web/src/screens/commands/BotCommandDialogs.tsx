import { useEffect, useState } from 'react';
import { Button, Dialog } from '@dilla/ui';
import { encode } from '@dilla/client-core';
import { t } from '../../strings/index.ts';

export interface BotAnnouncementValue { channelId: string; text: string; kind: 'one_off' | 'daily' | 'weekly'; timezone: string;
  date: string; hour: number; minute: number; weekday: number }
export interface BotRolePanelValue { channelId: string; roles: { id: string; name: string }[]; emojis: Record<string, string> }
export type BotCommandDialogValue = { announcement: BotAnnouncementValue } | { rolePanel: BotRolePanelValue };
interface Channel { id: string; name: string }
interface Role { id: string; name: string }
export type BotCommandDialogState =
  | { kind: 'announcement'; communityId: string; channels: Channel[]; initial: (BotAnnouncementValue & { id: string; revision: number }) | null; prefill: string; mode: 'create' | 'edit'; isCurrent(): boolean; onSubmit(value: BotCommandDialogValue): void; onCancel(): void }
  | { kind: 'rolepanel'; communityId: string; channels: Channel[]; roles: Role[]; isCurrent(): boolean; onSubmit(value: BotCommandDialogValue): void; onCancel(): void };
export function BotCommandDialogs({ dialog }: { dialog: BotCommandDialogState | null }): React.JSX.Element | null {
  useEffect(() => {
    if (dialog === null) return;
    const check = window.setInterval(() => { if (!dialog.isCurrent()) dialog.onCancel(); }, 100);
    return () => window.clearInterval(check);
  }, [dialog]);
  if (dialog === null) return null;
  return dialog.kind === 'announcement' ? <AnnouncementEditor key={`${dialog.communityId}:${dialog.initial?.id ?? 'new'}`} dialog={dialog} />
    : <RolePanelEditor key={dialog.communityId} dialog={dialog} />;
}

function AnnouncementEditor({ dialog }: { dialog: Extract<BotCommandDialogState, { kind: 'announcement' }> }): React.JSX.Element {
  const zone = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  const initial = dialog.initial;
  const [channelId, setChannelId] = useState(initial?.channelId ?? dialog.channels[0]?.id ?? '');
  const [text, setText] = useState(dialog.prefill.trim() !== '' ? dialog.prefill : initial?.text ?? '');
  const [kind, setKind] = useState<BotAnnouncementValue['kind']>(initial?.kind ?? 'one_off');
  const [timezone, setTimezone] = useState(initial?.timezone ?? zone);
  const defaultDate = new Date(Date.now() + 60_000);
  const localDate = `${defaultDate.getFullYear()}-${String(defaultDate.getMonth() + 1).padStart(2, '0')}-${String(defaultDate.getDate()).padStart(2, '0')}`;
  const [date, setDate] = useState(initial?.date ?? localDate);
  const [hour, setHour] = useState(String(initial?.hour ?? defaultDate.getHours()).padStart(2, '0'));
  const [minute, setMinute] = useState(String(initial?.minute ?? defaultDate.getMinutes()).padStart(2, '0'));
  const [weekday, setWeekday] = useState(String(initial?.weekday ?? new Date().getDay()));
  const [error, setError] = useState('');
  const submit = () => {
    const normalized = text.trim();
    const canonicalZone = canonicalTimezone(timezone);
    const numericHour = Number(hour); const numericMinute = Number(minute); const numericWeekday = Number(weekday);
    if (channelId === '' || normalized === '' || new TextEncoder().encode(normalized).length > 1800 ||
      !Number.isInteger(numericHour) || numericHour < 0 || numericHour > 23 || !Number.isInteger(numericMinute) || numericMinute < 0 || numericMinute > 59 ||
      !Number.isInteger(numericWeekday) || numericWeekday < 0 || numericWeekday > 6 || canonicalZone === null) {
      setError(t('botCommand.dialog.invalid')); return;
    }
    if (kind === 'one_off' && (!/^\d{4}-\d{2}-\d{2}$/u.test(date) || occurrence(date, numericHour, numericMinute, canonicalZone) <= Date.now())) {
      setError(t('botCommand.dialog.futureMinute')); return;
    }
    if (kind === 'one_off' && Math.floor(occurrence(date, numericHour, numericMinute, canonicalZone) / 60_000) <= Math.floor(Date.now() / 60_000)) {
      setError(t('botCommand.dialog.futureMinute')); return;
    }
    dialog.onSubmit({ announcement: { channelId, text: normalized, kind, timezone: canonicalZone ?? '', date: kind === 'one_off' ? date : '', hour: numericHour,
      minute: numericMinute, weekday: numericWeekday } });
  };
  const cancel = () => dialog.onCancel();
  return <Dialog open title={t(dialog.mode === 'create' ? 'botCommand.announcement.create' : 'botCommand.announcement.edit')} closeLabel={t('dialog.close')}
    onClose={cancel} footer={<><Button onClick={cancel}>{t('chperm.cancel')}</Button><Button onClick={submit}>{t('botCommand.save')}</Button></>}>
    <label>{t('botCommand.announcement.destination')}<select value={channelId} onChange={event => setChannelId(event.currentTarget.value)} data-autofocus>
      <option value="">{t('botCommand.announcement.chooseDestination')}</option>{dialog.channels.map(channel => <option key={channel.id} value={channel.id}>{channel.name}</option>)}</select></label>
    <label>{t('botCommand.announcement.message')}<textarea value={text} maxLength={1800} onChange={event => setText(event.currentTarget.value)} /></label>
    <label>{t('botCommand.announcement.recurrence')}<select value={kind} onChange={event => setKind(event.currentTarget.value as BotAnnouncementValue['kind'])}>
      <option value="one_off">{t('botCommand.announcement.once')}</option><option value="daily">{t('botCommand.announcement.daily')}</option><option value="weekly">{t('botCommand.announcement.weekly')}</option></select></label>
    {kind === 'one_off' ? <label>{t('botCommand.announcement.date')}<input type="date" value={date} onChange={event => setDate(event.currentTarget.value)} /></label> : null}
    {kind === 'weekly' ? <label>{t('botCommand.announcement.weekday')}<select value={weekday} onChange={event => setWeekday(event.currentTarget.value)}>
      {[0, 1, 2, 3, 4, 5, 6].map(day => <option key={day} value={day}>{t(`botCommand.weekday.${day}` as never)}</option>)}</select></label> : null}
    <label>{t('botCommand.announcement.hour')}<input type="number" min="0" max="23" value={hour} onChange={event => setHour(event.currentTarget.value)} /></label>
    <label>{t('botCommand.announcement.minute')}<input type="number" min="0" max="59" value={minute} onChange={event => setMinute(event.currentTarget.value)} /></label>
    <label>{t('botCommand.announcement.timezone')}<input value={timezone} onChange={event => setTimezone(event.currentTarget.value)} /></label>
    {error === '' ? null : <p role="alert">{error}</p>}
  </Dialog>;
}

function RolePanelEditor({ dialog }: { dialog: Extract<BotCommandDialogState, { kind: 'rolepanel' }> }): React.JSX.Element {
  const [channelId, setChannelId] = useState(dialog.channels[0]?.id ?? '');
  const [emojis, setEmojis] = useState<Record<string, string>>({});
  const [selected, setSelected] = useState<Record<string, boolean>>({});
  const [error, setError] = useState('');
  const submit = () => {
    const roles = dialog.roles.filter(role => selected[role.id] === true);
    const entries = roles.map(role => emojis[role.id]?.trim() ?? '');
    if (channelId === '' || roles.length === 0 || roles.some(role => new TextEncoder().encode(role.name).length > 100)
      || entries.some(value => value === '' || new TextEncoder().encode(value).length > 32 || /[\u0000-\u001f\u007f]/u.test(value))
      || new Set(entries).size !== entries.length || encode(roles.map((role, index) => [entries[index] ?? '', new Uint8Array(16), role.name])).length > 2200) {
      setError(t('botCommand.rolepanel.invalid')); return;
    }
    dialog.onSubmit({ rolePanel: { channelId, roles, emojis: Object.fromEntries(roles.map((role, index) => [role.id, entries[index] ?? ''])) } });
  };
  const cancel = () => dialog.onCancel();
  return <Dialog open title={t('botCommand.rolepanel.title')} closeLabel={t('dialog.close')} onClose={cancel}
    footer={<><Button onClick={cancel}>{t('chperm.cancel')}</Button><Button onClick={submit}>{t('botCommand.rolepanel.publish')}</Button></>}>
    <label>{t('botCommand.announcement.destination')}<select value={channelId} onChange={event => setChannelId(event.currentTarget.value)} data-autofocus>
      <option value="">{t('botCommand.announcement.chooseDestination')}</option>{dialog.channels.map(channel => <option key={channel.id} value={channel.id}>{channel.name}</option>)}</select></label>
    <fieldset><legend>{t('botCommand.rolepanel.roles')}</legend>{dialog.roles.map(role => <div key={role.id}>
      <label><input type="checkbox" checked={selected[role.id] ?? false} onChange={event => { const checked = event.currentTarget.checked; setSelected(value => ({ ...value, [role.id]: checked })); }} />{role.name}</label>
      <label>{t('botCommand.rolepanel.emojiFor', { role: role.name })}<input value={emojis[role.id] ?? ''} maxLength={16}
        onChange={event => { const emoji = event.currentTarget.value; setEmojis(value => ({ ...value, [role.id]: emoji })); }} /></label>
    </div>)}</fieldset>{error === '' ? null : <p role="alert">{error}</p>}
  </Dialog>;
}

function canonicalTimezone(value: string): string | null { try { return new Intl.DateTimeFormat('en-US', { timeZone: value }).resolvedOptions().timeZone; } catch { return null; } }
function occurrence(date: string, hour: number, minute: number, timezone: string): number {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/u.exec(date);
  if (!match) return 0;
  const target = Date.UTC(Number(match[1]), Number(match[2]) - 1, Number(match[3]), hour, minute);
  let result = target;
  for (let pass = 0; pass < 3; pass++) {
    const parts = new Intl.DateTimeFormat('en-GB', { timeZone: timezone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).formatToParts(result);
    const value = Object.fromEntries(parts.map(part => [part.type, part.value]));
    const seen = Date.UTC(Number(value.year), Number(value.month) - 1, Number(value.day), Number(value.hour), Number(value.minute));
    result += target - seen;
  }
  const parts = new Intl.DateTimeFormat('en-GB', { timeZone: timezone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).formatToParts(result);
  const value = Object.fromEntries(parts.map(part => [part.type, part.value]));
  return Number(value.year) === Number(match[1]) && Number(value.month) === Number(match[2]) && Number(value.day) === Number(match[3])
    && Number(value.hour) === hour && Number(value.minute) === minute ? result : 0;
}
