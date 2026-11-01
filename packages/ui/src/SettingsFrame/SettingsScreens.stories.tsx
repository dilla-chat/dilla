import type { Meta, StoryObj } from '@storybook/react-vite';
import { useEffect, useState, type ReactNode } from 'react';
import { SettingsFrame } from './SettingsFrame.tsx';
import { SettingsNav } from '../SettingsNav/SettingsNav.tsx';
import { SettingsRow } from '../SettingsRow/SettingsRow.tsx';
import { DeviceRow } from '../DeviceRow/DeviceRow.tsx';
import { Segmented } from '../Segmented/Segmented.tsx';
import { Toggle } from '../Toggle/Toggle.tsx';
import { RecoveryKeyField } from '../RecoveryKeyField/RecoveryKeyField.tsx';
import { Dialog } from '../Dialog/Dialog.tsx';
import { Button } from '../Button/Button.tsx';
// The shipped copy of @dilla/web, so the F16 screenshots show exactly what the app says.
import { t } from '../../../web/src/strings/index.ts';

const meta = { title: 'Screens/Settings', parameters: { layout: 'fullscreen' } } satisfies Meta;
export default meta;
type Story = StoryObj<typeof meta>;

const noop = () => {};
const NAV = [
  { id: 'devices', label: t('settings.nav.devices') },
  { id: 'notifications', label: t('settings.nav.notifications') },
  { id: 'appearance', label: t('settings.nav.appearance') },
];
function Frame({ active, children }: { active: string; children: ReactNode }) {
  return (
    <SettingsFrame open title={t('settings.title')} closeLabel={t('settings.close')} onClose={noop}
      nav={<SettingsNav label={t('settings.nav.label')} items={NAV} activeId={active} onSelect={noop} />}>
      {children}
    </SettingsFrame>
  );
}
// Flow 03's order: the list, the count line with refresh under it, then the two actions on this browser under a hairline.
function DevicesSection({ dialog }: { dialog?: 'remove' | 'wrong' | 'unlisted' }) {
  const remove = { label: t('devices.revoke'), onAction: noop, danger: true };
  // A nested dialog opens after the frame, as in the app (a person opens it from inside Settings): opened in the same
  // commit, its showModal would run first (child effects run first) and the frame would cover it in the top layer.
  const [opened, setOpened] = useState(false);
  useEffect(() => { setOpened(true); }, []);
  return (
    <Frame active="devices">
      <section aria-labelledby="s-devices" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--group-gap)' }}>
        <h2 id="s-devices">{t('devices.title')}</h2>
        <p>{t('devices.body')}</p>
        <ul style={{ listStyle: 'none', margin: 0, padding: 0 }}>
          <DeviceRow name="cccccccc" tier="web" tierLabel={t('devices.tier.web')} own ownLabel={t('devices.thisBrowser')} lastSeen={t('devices.lastSeen', { when: '14:02' })} />
          <DeviceRow name="d1d1d1d1" tier="native" tierLabel={t('devices.tier.native')} lastSeen={t('devices.lastSeen', { when: '13:58' })} action={remove} />
          <DeviceRow name="f3f3f3f3" tier="web" tierLabel={t('devices.tier.web')} state={t('devices.unlisted')} lastSeen={t('devices.lastSeen', { when: '13:41' })} action={remove} />
          <DeviceRow name="e2e2e2e2" tier="web" tierLabel={t('devices.tier.web')} state={t('devices.revoked')} lastSeen={t('devices.lastSeen', { when: '3 Oct 2026 09:12' })} />
        </ul>
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--group-gap)' }}>
          <p style={{ margin: 0 }}>{t('devices.cap.other', { n: 3 })}</p>
          <Button size="sm">{t('devices.refresh')}</Button>
        </div>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--group-gap)', borderTop: '1px solid var(--hairline)', paddingTop: 'var(--group-gap)' }}>
          <Button variant="danger">{t('devices.signOut')}</Button>
          <Button>{t('devices.forget')}</Button>
        </div>
      </section>
      {dialog === 'remove' || dialog === 'wrong' ? <KeyDialogStory open={opened} wrong={dialog === 'wrong'} /> : null}
      {dialog === 'unlisted' ? (
        <Dialog open={opened} title={t('devices.removeUnlistedTitle')} onClose={noop} closeLabel={t('dialog.close')}
          footer={<Button variant="danger">{t('devices.confirmRevoke')}</Button>}>
          <p>{t('devices.removeUnlistedBody')}</p>
        </Dialog>
      ) : null}
    </Frame>
  );
}
function KeyDialogStory({ open, wrong }: { open: boolean; wrong: boolean }) {
  const [value, setValue] = useState(wrong ? '7K2N-QX9D-H4TB-R8NW-C3VF-J6PZ-A1GE-Y5KS-M0QT-B7XH-W2DN-F9RC-P4ZA' : '7K2M-QX9D-H4TB');
  const short = !wrong;
  return (
    <Dialog open={open} title={t('devices.revokeTitle')} onClose={noop} closeLabel={t('dialog.close')}
      footer={<Button variant="danger" type="submit" form="s-revoke" {...(short ? { 'aria-disabled': true, 'aria-describedby': 's-revoke-length' } : {})}>{t('devices.confirmRevoke')}</Button>}>
      <form id="s-revoke" noValidate onSubmit={e => e.preventDefault()}>
        <p>{t('devices.revokeBody')}</p>
        <RecoveryKeyField id="s-revoke-key" label={t('devices.keyLabel')} value={value} onChange={setValue}
          hint={t('signin.key.hint', { n: wrong ? 52 : 12 })} error={wrong ? t('devices.error.wrongKey') : undefined} />
        {short ? <p id="s-revoke-length">{t('signin.error.keyLength')}</p> : null}
      </form>
    </Dialog>
  );
}
const CHANNEL_MODES = [
  { value: 'default', label: t('notify.channel.default') }, { value: 'all', label: t('notify.channel.all') },
  { value: 'mentions', label: t('notify.channel.mentions') }, { value: 'nothing', label: t('notify.channel.nothing') },
];
const SHORT_ROWS: readonly (readonly [string, string, string, boolean])[] = [
  ['c3', t('notify.title.channel', { channel: 'general', server: 'Midgard' }), 'default', false],
  ['d4', t('notify.title.channel', { channel: 'random', server: 'Midgard' }), 'mentions', true],
  ['9a', t('notify.title.dm', { sender: 'bob' }), 'all', false],
];
const MODES = ['default', 'all', 'mentions', 'nothing'] as const;
// 38 channels over two servers and two DMs: the list at the size a busy account reaches (F16).
const LONG_ROWS: readonly (readonly [string, string, string, boolean])[] = Array.from({ length: 40 }, (_, i) => [
  `r${i}`,
  i >= 38 ? t('notify.title.dm', { sender: i === 38 ? 'bob' : 'helga' })
    : t('notify.title.channel', { channel: `channel-${i + 1}`, server: i < 24 ? 'Midgard' : 'Valhalla' }),
  MODES[i % 4], i % 7 === 3,
] as const);
function NotificationsSection({ blocked, rows = SHORT_ROWS }: { blocked?: boolean; rows?: typeof SHORT_ROWS }) {
  const [mode, setMode] = useState('dms-mentions');
  return (
    <Frame active="notifications">
      <section aria-labelledby="s-notify" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--group-gap)' }}>
        <h2 id="s-notify">{t('notify.title')}</h2>
        <p>{t('notify.body')}</p>
        <SettingsRow id="s-permission" label={t('notify.permission.label')}>
          {blocked ? <p>{t('notify.permission.denied')}</p> : <Button size="sm">{t('notify.permission.ask')}</Button>}
        </SettingsRow>
        <SettingsRow id="s-default-row" label={t('notify.default.label')}>
          <Segmented id="s-default" label={t('notify.default.label')} value={mode} onChange={setMode} options={[
            { value: 'dms-mentions', label: t('notify.default.dmsMentions') }, { value: 'everything', label: t('notify.default.everything') },
            { value: 'nothing', label: t('notify.default.nothing') }]} />
        </SettingsRow>
        <h3>{t('notify.channels.title')}</h3>
        <p>{t('notify.channels.body')}</p>
        <ul style={{ listStyle: 'none', margin: 0, padding: 0 }}>
          {rows.map(([id, label, value, muted]) => (
            <li key={id}>
              <SettingsRow id={`s-row-${id}`} label={label}>
                <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--group-gap)' }}>
                  <Segmented id={`s-mode-${id}`} label={label} value={value} onChange={noop} options={CHANNEL_MODES} />
                  <Toggle id={`s-mute-${id}`} label={t('notify.mute')} checked={muted} onChange={noop} showLabel />
                </div>
              </SettingsRow>
            </li>
          ))}
        </ul>
      </section>
    </Frame>
  );
}

export const Devices: Story = { render: () => <DevicesSection /> };
export const DevicesRemove: Story = { render: () => <DevicesSection dialog="remove" /> };
export const DevicesRemoveUnlisted: Story = { render: () => <DevicesSection dialog="unlisted" /> };
export const DevicesWrongKey: Story = { render: () => <DevicesSection dialog="wrong" /> };
export const Notifications: Story = { render: () => <NotificationsSection /> };
export const NotificationsBlocked: Story = { render: () => <NotificationsSection blocked /> };
export const NotificationsLong: Story = { render: () => <NotificationsSection rows={LONG_ROWS} /> };
export const Appearance: Story = {
  render: () => (
    <Frame active="appearance">
      <section aria-labelledby="s-appearance" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--group-gap)' }}>
        <h2 id="s-appearance">{t('appearance.title')}</h2>
        <SettingsRow id="s-theme-row" label={t('appearance.theme.label')}>
          <Segmented id="s-theme" label={t('appearance.theme.label')} value="system" onChange={noop} options={[
            { value: 'system', label: t('appearance.theme.system') }, { value: 'signal', label: t('appearance.theme.signal') },
            { value: 'light', label: t('appearance.theme.light') }, { value: 'high-contrast', label: t('appearance.theme.contrast') }]} />
        </SettingsRow>
      </section>
    </Frame>
  ),
};
