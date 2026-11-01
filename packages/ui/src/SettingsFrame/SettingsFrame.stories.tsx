import type { Meta, StoryObj } from '@storybook/react-vite';
import { useSyncExternalStore, type ReactNode } from 'react';
import { SettingsFrame } from './SettingsFrame.tsx';
import { SettingsNav } from '../SettingsNav/SettingsNav.tsx';
import { SettingsRow } from '../SettingsRow/SettingsRow.tsx';
import { Segmented } from '../Segmented/Segmented.tsx';
import { Toggle } from '../Toggle/Toggle.tsx';
import { DeviceRow } from '../DeviceRow/DeviceRow.tsx';
import { Button } from '../Button/Button.tsx';
import { SettingsFooter } from './SettingsFooter.tsx';
import { KeyboardTable } from '../KeyboardTable/KeyboardTable.tsx';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';

// Every string is L-COPY-02's (settings.*, devices.*, notify.*). The composed Screens/Settings stories of
// task 19 quote en.ts; these show the frame itself.
const meta: Meta = { title: 'Settings/SettingsFrame', parameters: { layout: 'fullscreen' } };
export default meta;
type Story = StoryObj;
const noop = () => {};
const ITEMS = [{ id: 'devices', label: 'Devices', group: 'Account', icon: 'monitor-smartphone' as const }, { id: 'notifications', label: 'Notifications', group: 'Preferences', icon: 'bell' as const }, { id: 'appearance', label: 'Appearance', group: 'Preferences', icon: 'eye' as const }];
const PHONE_INDEX_ITEMS = [
  { id: 'profile', label: 'profile', group: 'Account' },
  { id: 'sign-in', label: 'sign-in', group: 'Account' },
  { id: 'devices', label: 'devices', group: 'Account' },
  { id: 'appearance', label: 'appearance', group: 'Preferences' },
  { id: 'notifications', label: 'notifications', group: 'Preferences' },
  { id: 'voice', label: 'voice', group: 'Preferences' },
  { id: 'keyboard', label: 'keyboard', group: 'Preferences' },
  { id: 'privacy', label: 'privacy', group: 'Support' },
  { id: 'diagnostics', label: 'diagnostics', group: 'Support' },
  { id: 'sign-out', label: 'sign out', group: 'User action' },
];
const NOTIFY = [{ value: 'dms-mentions', label: 'direct messages and mentions' }, { value: 'everything', label: 'every message' }, { value: 'nothing', label: 'nothing' }];
const MODES = [{ value: 'default', label: 'default' }, { value: 'all', label: 'all' }, { value: 'mentions', label: 'mentions' }, { value: 'nothing', label: 'nothing' }];
const remove = { label: 'remove', onAction: noop };
const PHONE_QUERY = '(max-width: 47.99rem), (pointer: coarse) and (max-height: 31.99rem)';

function subscribePhoneLayout(onChange: () => void) {
  if (typeof window.matchMedia !== 'function') return () => {};
  const query = window.matchMedia(PHONE_QUERY);
  query.addEventListener('change', onChange);
  return () => query.removeEventListener('change', onChange);
}
function phoneSnapshot() {
  return typeof window.matchMedia === 'function' && window.matchMedia(PHONE_QUERY).matches;
}
function ResponsivePhoneLayout({ children }: { children: ReactNode }) {
  const phone = useSyncExternalStore(subscribePhoneLayout, phoneSnapshot, () => false);
  return <PhoneLayoutProvider value={phone}>{children}</PhoneLayoutProvider>;
}

function DevicesSection() {
  return (
    <section aria-labelledby="devices-title">
      <h2 id="devices-title">Devices</h2>
      <p>Every browser and app signed in to your account. Removing a device needs your recovery key.</p>
      <ul role="list" style={{ listStyle: 'none', margin: 0, padding: 0 }}>
        <DeviceRow name="3f9a2c1d" tier="web" tierLabel="browser" own ownLabel="this browser" lastSeen="last seen 21:04" />
        <DeviceRow name="b2d4e6f8" tier="native" tierLabel="app" lastSeen="last seen 21:02" action={remove} />
        <DeviceRow name="0a1b2c3d" tier="web" tierLabel="browser" state="not yet in the device list" lastSeen="last seen 20:40" action={remove} />
        <DeviceRow name="99ee0f11" tier="web" tierLabel="browser" state="removed" lastSeen="last seen 1 Oct 2026" />
      </ul>
      <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', justifyContent: 'space-between', gap: '0.75rem', padding: '0.75rem 0' }}>
        <span>3 devices</span>
        <Button>refresh</Button>
      </div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: '0.5rem', paddingTop: '0.75rem', borderTop: '1px solid var(--hairline)' }}>
        <Button variant="danger">Sign out and remove this browser</Button>
        <Button>Forget this browser</Button>
      </div>
    </section>
  );
}

function NotificationsSection() {
  return (
    <section aria-labelledby="notify-title">
      <h2 id="notify-title">Notifications</h2>
      <p>Desktop notifications show while a dilla tab is open. Nothing is shown when every tab is closed.</p>
      <SettingsRow id="notify-permission" label="desktop notifications"><Button variant="accent">turn on</Button></SettingsRow>
      <SettingsRow id="notify-default" label="notify me about">
        <Segmented id="notify-default-control" label="notify me about" options={NOTIFY} value="dms-mentions" onChange={noop} />
      </SettingsRow>
      <h3>Per channel</h3>
      <p>Every channel and direct message. Rows left on default follow the choice above.</p>
      <SettingsRow id="notify-general" label="#general · Midgard Crew">
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '0.75rem' }}>
          <Segmented id="notify-general-mode" label="#general · Midgard Crew" options={MODES} value="all" onChange={noop} />
          <Toggle id="mute-general" label="mute" checked={false} onChange={noop} showLabel />
        </div>
      </SettingsRow>
      <SettingsRow id="notify-random" label="#random · Midgard Crew">
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '0.75rem' }}>
          <Segmented id="notify-random-mode" label="#random · Midgard Crew" options={MODES} value="nothing" onChange={noop} />
          <Toggle id="mute-random" label="mute" checked onChange={noop} showLabel />
        </div>
      </SettingsRow>
      <SettingsRow id="notify-ada" label="ada">
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '0.75rem' }}>
          <Segmented id="notify-ada-mode" label="ada" options={MODES} value="default" onChange={noop} />
          <Toggle id="mute-ada" label="mute" checked={false} onChange={noop} showLabel />
        </div>
      </SettingsRow>
    </section>
  );
}

const FOOTER = <SettingsFooter status="Unsaved changes"><Button variant="ghost" onClick={noop}>reset</Button><Button variant="accent" onClick={noop}>save</Button></SettingsFooter>;

function Frame({ section }: { section: 'devices' | 'notifications' }) {
  return (
    <ResponsivePhoneLayout>
      <SettingsFrame open title="Settings" closeLabel="Close settings" onClose={noop} footer={FOOTER}
        sectionTitle={section === 'devices' ? 'Devices' : 'Notifications'} onBack={noop}
        nav={<SettingsNav label="settings sections" items={ITEMS} activeId={section} onSelect={noop} />}>
        {section === 'devices' ? <DevicesSection /> : <NotificationsSection />}
      </SettingsFrame>
    </ResponsivePhoneLayout>
  );
}

export const Devices: Story = { render: () => <Frame section="devices" /> };
export const Notifications: Story = { render: () => <Frame section="notifications" /> };
// A 360 × 740 browser viewport exercises the same phone query used by the app shell.
export const Narrow: Story = {
  parameters: { phoneViewport: true },
  render: () => <Frame section="devices" />,
};
// A11Y-DESIGN-01: at 200% zoom, a 1280 × 800 desktop viewport has a 640 × 400 CSS viewport. The existing phone
// breakpoint applies, so the fullscreen section keeps its header and footer fixed while its content scrolls.
export const ZoomedIn: Story = {
  render: () => <Frame section="devices" />,
};

function Keyboard() {
  return (
    <section aria-labelledby="keys-title">
      <h2 id="keys-title">Keyboard</h2>
      <KeyboardTable caption="Keyboard shortcuts" rows={[{ action: 'search', keys: ['ctrl', 'k'] }, { action: 'previous channel', keys: ['alt', '↑'] }, { action: 'next channel', keys: ['alt', '↓'] }, { action: 'close', keys: ['esc'] }]} />
    </section>
  );
}
// One hairline at y=48 across the side title and the bar, and the one sticky save footer (R17, I8).
export const WithFooter: Story = {
  render: () => (
    <SettingsFrame open title="Settings" closeLabel="Close settings" onClose={noop} footer={FOOTER}
      nav={<SettingsNav label="settings sections" items={ITEMS} activeId="notifications" onSelect={noop} />}>
      <NotificationsSection />
    </SettingsFrame>
  ),
};
export const KeyboardSection: Story = {
  render: () => (
    <SettingsFrame open title="Settings" closeLabel="Close settings" onClose={noop}
      nav={<SettingsNav label="settings sections" items={ITEMS} activeId="appearance" onSelect={noop} />}>
      <Keyboard />
    </SettingsFrame>
  ),
};
// A phone layout: a full-width page, the title not repeated, rows stacked, 44px targets.
export const Phone: Story = {
  render: () => (
    <PhoneLayoutProvider value>
      <SettingsFrame open modal={false} title="Settings" closeLabel="Close settings" onClose={noop} footer={FOOTER}
        phonePage="section" sectionTitle="Notifications" backLabel="Back" onBack={noop}
        nav={<SettingsNav label="settings sections" items={ITEMS} activeId="notifications" onSelect={noop} />}>
        <NotificationsSection />
      </SettingsFrame>
    </PhoneLayoutProvider>
  ),
};
export const PhoneIndex: Story = {
  render: () => (
    <ResponsivePhoneLayout>
      <SettingsFrame open phonePage="index" identity={<div>Signed in as Ada</div>} search={<input aria-label="Search settings" placeholder="Search" />}
        title="Settings" closeLabel="Close settings" onClose={noop} nav={<SettingsNav label="settings sections" items={PHONE_INDEX_ITEMS} activeId="devices" onSelect={noop} />}>
        <DevicesSection />
      </SettingsFrame>
    </ResponsivePhoneLayout>
  ),
};
