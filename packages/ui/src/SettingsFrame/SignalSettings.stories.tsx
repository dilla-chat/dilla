import type { Meta, StoryObj } from '@storybook/react-vite';
import { useEffect, useSyncExternalStore, type ReactNode } from 'react';
import { SettingsFrame } from './SettingsFrame.tsx';
import { SettingsNav } from '../SettingsNav/SettingsNav.tsx';
import { SettingsRow } from '../SettingsRow/SettingsRow.tsx';
import { Segmented } from '../Segmented/Segmented.tsx';
import { Toggle } from '../Toggle/Toggle.tsx';
import { VoiceSettings } from '../VoiceSettings/VoiceSettings.tsx';
import { TextField } from '../TextField/TextField.tsx';
import { Select } from '../Select/Select.tsx';
import { SAMPLE_MC, SampleCore } from '../AppShell/sampleCore.fixture.tsx';
import { Profile as WebProfile } from '../../../web/src/screens/settings/Profile.tsx';
import { SignInFactors as WebSignIn } from '../../../web/src/screens/settings/SignInFactors.tsx';
import { OverviewSection as WebOverview } from '../../../web/src/screens/admin/OverviewSection.tsx';
import { Devices as WebDevices } from '../../../web/src/screens/settings/Devices.tsx';
import { Keyboard as WebKeyboard } from '../../../web/src/screens/settings/Keyboard.tsx';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';
import { Button } from '../Button/Button.tsx';
import { LiveRoleEditor } from '../RoleEditor/RoleEditor.fixture.tsx';
import { LiveInvites } from '../InvitePanel/InvitePanel.fixture.tsx';
import type { IconName } from '../Icon/Icon.tsx';
// The shipped copy and section registry of @dilla/web, so these whole screens draw what Settings draws.
import { t } from '../../../web/src/strings/index.ts';
import { SETTINGS_ICONS, SETTINGS_REGISTRY } from '../../../web/src/settings/registry.ts';
import '../../../web/src/settings/workspace.css';
// Task 11: the web's own section styles (Profile's card, the phone rows), as the app loads them.

/**
 * Task 10, restyle rule 3e: the whole Settings workspace as packages/web composes it (fix round 1: one workspace; the nav is
 * settings/SettingsNavigation.tsx's "You" group and the selected server's group under its name, with only the sections an
 * owner may use; each section as its screens/settings/*.tsx draws it), at 1280 × 800 and on a 390 × 844 phone.
 */
const meta: Meta = { title: 'Settings/SignalSettings', parameters: { layout: 'fullscreen', webStyles: true } };
export default meta;
type Story = StoryObj;
const noop = () => {};

const PHONE_QUERY = '(max-width: 47.99rem), (pointer: coarse) and (max-height: 31.99rem)';
function subscribe(onChange: () => void) {
  const query = window.matchMedia(PHONE_QUERY);
  query.addEventListener('change', onChange);
  return () => query.removeEventListener('change', onChange);
}
const snapshot = () => window.matchMedia(PHONE_QUERY).matches;
function Responsive({ children }: { children: ReactNode }) {
  return <PhoneLayoutProvider value={useSyncExternalStore(subscribe, snapshot, () => false)}>{children}</PhoneLayoutProvider>;
}

const SERVER = 'Midgard Crew';
// An owner's server sections (availableSettings('server', access) with every permission).
const ITEMS = [
  ...SETTINGS_REGISTRY.filter(entry => entry.scope === 'user')
    .map(entry => ({ id: entry.id, label: t(entry.labelKey), group: t(entry.group), icon: SETTINGS_ICONS[entry.id] })),
  ...SETTINGS_REGISTRY.filter(entry => entry.scope === 'server')
    .map(entry => ({ id: `server:${entry.id}`, label: t(entry.labelKey), group: SERVER, icon: SETTINGS_ICONS[entry.id] })),
];
// Fix round 2 (story only): on a phone the section chips scroll sideways; bring the selected one into view once laid out, as a
// tap on it would leave it.
function useSelectedChipInView() {
  useEffect(() => {
    const frame = requestAnimationFrame(() => {
      document.querySelector('.d-settings-nav [aria-current="page"]')?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    });
    return () => cancelAnimationFrame(frame);
  }, []);
}
function Workspace({ active, children }: { active: string; children: ReactNode }) {
  useSelectedChipInView();
  const nav = <SettingsNav label={t('settings.nav.label')} activeId={active} onSelect={noop} items={ITEMS} />;
  return (
    <Responsive>
      <SettingsFrame open title={t('settings.title')} closeLabel={t('settings.close')} onClose={noop} nav={nav} phonePage="section">
        {children}
      </SettingsFrame>
    </Responsive>
  );
}

const MODES = [
  { value: 'all', label: t('notify.mode.all') }, { value: 'mentions', label: t('notify.mode.mentions') }, { value: 'nothing', label: t('notify.mode.nothing') },
];
// screens/settings/Notifications.tsx (fix round 1): the prototype's four rows for the selected server.
function Notifications() {
  return (
    <section className="dw-settings-section" aria-labelledby="notify-title">
      <h2 id="notify-title" tabIndex={-1}>{t('notify.title')}</h2>
      <p>{t('notify.sub')}</p>
      <SettingsRow id="notify-server" icon="bell" label={SERVER} hint={t('notify.server.hint')}>
        <Segmented id="notify-server-mode" label={t('notify.server.label', { server: SERVER })} options={MODES} value="mentions" onChange={noop} />
      </SettingsRow>
      <SettingsRow id="notify-everyone" icon="at-sign" label={t('notify.everyone.label')} hint={t('notify.everyone.hint')}>
        <Toggle id="notify-everyone-switch" label={t('notify.everyone.label')} checked onChange={noop} />
      </SettingsRow>
      <SettingsRow id="notify-desktop" icon="monitor-smartphone" label={t('notify.desktop.label')} hint={t('notify.desktop.hint')}>
        <Toggle id="notify-desktop-switch" label={t('notify.desktop.label')} checked={false} onChange={noop} />
      </SettingsRow>
      <SettingsRow id="notify-sounds" icon="volume-2" label={t('notify.sounds.label')} hint={t('notify.sounds.hint')}>
        <Toggle id="notify-sounds-switch" label={t('notify.sounds.label')} checked onChange={noop} />
      </SettingsRow>
    </section>
  );
}

// screens/settings/Voice.tsx: VoiceSettings with the shipped labels, as the app passes them at rest (task 10b: volumes 100 %,
// automatic sensitivity at the resting −48 dB, not testing, release delay 200 ms, echo cancellation and automatic gain on).
function Voice({ ptt }: { ptt?: boolean }) {
  return (
    <section className="dw-settings-section" aria-labelledby="voice-settings-title">
      <h2 id="voice-settings-title" tabIndex={-1}>{t('settings.voice.section')}</h2>
      <p>{t('signalSettings.voice.body')}</p>
      <VoiceSettings inputs={[{ id: 'yeti', label: 'Blue Yeti' }]} outputs={[{ id: 'phones', label: 'Headphones' }]} input="" output=""
        canSelectOutput inputVolume={100} outputVolume={100} mode={ptt ? 'ptt' : 'vad'} pttKey={ptt ? 'F13' : null} capturing={false}
        pttReleaseMs={200} noise="automatic" echo agc
        micTest={{ testing: false, available: true, levelDb: -100, thresholdDb: -48, auto: true, open: false }}
        labels={{ input: t('settings.voice.input'), output: t('settings.voice.output'), systemDefault: t('settings.voice.systemDefault'),
          outputUnsupported: t('settings.voice.outputUnsupported'), mode: t('settings.voice.mode'), vad: t('settings.voice.vad'),
          ptt: t('settings.voice.ptt'), pttKey: t('settings.voice.pttKey'), pttNone: t('settings.voice.pttNone'), pttSet: t('settings.voice.pttSet'), pttListening: t('signalSettings.voice.pttListening'), pttCancel: t('signalSettings.voice.pttCancel'),
          pttPrompt: t('settings.voice.pttPrompt'), pttRefused: t('settings.voice.pttRefused'), noise: t('settings.voice.noise'),
          automatic: t('settings.voice.automatic'), lightweight: t('settings.voice.lightweight'), off: t('settings.voice.off'),
          devices: t('signalSettings.voice.devices'), test: t('signalSettings.voice.test'), inputMode: t('signalSettings.voice.inputMode'), processing: t('signalSettings.voice.processing'),
          pttHint: t('signalSettings.voice.pttHint'),
          noiseHints: { automatic: t('signalSettings.voice.noiseHint.automatic'), lightweight: t('signalSettings.voice.noiseHint.lightweight'), off: t('signalSettings.voice.noiseHint.off') },
          noiseTag: t('signalSettings.voice.noiseTag'),
          volume: t('signalSettings.voice.volume'), inputVolume: t('signalSettings.voice.inputVolume'), outputVolume: t('signalSettings.voice.outputVolume'),
          testStart: t('signalSettings.voice.testStart'), testStop: t('signalSettings.voice.testStop'), testHint: t('signalSettings.voice.testHint'),
          testUnavailable: t('settings.voice.testCall'), testEchoOff: t('signalSettings.voice.testEchoOff'), heard: t('signalSettings.voice.heard'), tooQuiet: t('signalSettings.voice.tooQuiet'),
          notTesting: t('signalSettings.voice.notTesting'), levelValue: t('signalSettings.voice.levelValue', { value: '{value}' }), thresholdValue: t('signalSettings.voice.thresholdValue', { value: '{value}' }),
          noLevel: t('signalSettings.voice.noLevel'), threshold: t('signalSettings.voice.threshold'), auto: t('signalSettings.voice.auto'),
          autoHintOn: t('signalSettings.voice.autoHintOn'), autoHintOff: t('signalSettings.voice.autoHintOff'),
          release: t('signalSettings.voice.release'), releaseHint: t('signalSettings.voice.releaseHint'),
          echo: t('signalSettings.voice.echo'), echoHint: t('signalSettings.voice.echoHint'), agc: t('signalSettings.voice.agc'), agcHint: t('signalSettings.voice.agcHint') }}
        onInput={noop} onOutput={noop} onInputVolume={noop} onOutputVolume={noop} onMode={noop} onCaptureKey={noop} onPttRelease={noop}
        onNoise={noop} onMicTest={noop} onThreshold={noop} onAuto={noop} onEcho={noop} onAgc={noop} />
    </section>
  );
}

// Fix round 2 (rule 3d): the web's own screens/settings/Keyboard.tsx.
const Keyboard = WebKeyboard;

// Fix round 2 (rule 3d): the web's own screens/settings/Devices.tsx over the sample's two browsers.
function Devices() {
  return <SampleCore><WebDevices /></SampleCore>;
}

// Task 11 (Jonas, Q7; rule 3d): the web's own screens/settings/Profile.tsx over the Midgard sample (Mira, "packing rope", her
// bio): the three fields, the note and the preview card; it saves as you type.
function Profile() {
  return <SampleCore><WebProfile /></SampleCore>;
}
// Task 11 (Jonas, Q7; rule 3d): the web's own screens/settings/SignInFactors.tsx over the sample's factors (a password changed
// three weeks ago, an authenticator app, the passkey "MacBook Touch ID", eight backup codes).
function SignIn() {
  return <SampleCore><WebSignIn announce={noop} live="" /></SampleCore>;
}
// Task 11 (Jonas, Q7; rule 3d): the web's own admin/OverviewSection.tsx for Midgard Crew over the sample (a 15-minute
// inactive timeout): the server name row (dilla's), the segmented timeout and Leave server in one card.
function Overview() {
  return <SampleCore><WebOverview communityId={SAMPLE_MC} serverName={SERVER} announce={noop} /></SampleCore>;
}
// Fix round 3 (story-a11y geometry): a group of settings rows touches. Each lower row starts where the upper one ends, one
// hairline parts them (the lower row's top edge), and the group keeps its outer top and bottom edges: 56n + 2.
function rowsTouch({ canvasElement }: { canvasElement: HTMLElement }) {
  const rows = Array.from(canvasElement.ownerDocument.querySelectorAll<HTMLElement>('.d-settings-row'));
  for (const upper of rows) {
    const lower = upper.nextElementSibling;
    if (!(lower instanceof HTMLElement) || !lower.classList.contains('d-settings-row')) continue;
    const gap = lower.getBoundingClientRect().top - upper.getBoundingClientRect().bottom;
    if (Math.abs(gap) > 0.5) throw new Error(`settings rows ${upper.id} and ${lower.id} are ${String(gap)}px apart`);
    const lines = parseFloat(getComputedStyle(upper).borderBottomWidth) + parseFloat(getComputedStyle(lower).borderTopWidth);
    if (lines !== 1) throw new Error(`settings rows ${upper.id} and ${lower.id} are parted by ${String(lines)} hairlines`);
  }
  for (const row of rows) {
    const prev = row.previousElementSibling, next = row.nextElementSibling;
    if (!prev?.classList.contains('d-settings-row') && parseFloat(getComputedStyle(row).borderTopWidth) !== 1) throw new Error(`${row.id} opens a group without its top edge`);
    if (!next?.classList.contains('d-settings-row') && parseFloat(getComputedStyle(row).borderBottomWidth) !== 1) throw new Error(`${row.id} closes a group without its bottom edge`);
  }
}

export const ProfileDesktop: Story = { render: () => <Workspace active="profile"><Profile /></Workspace> };
export const SignInDesktop: Story = { play: rowsTouch, render: () => <Workspace active="signin"><SignIn /></Workspace> };
export const OverviewDesktop: Story = { play: rowsTouch, render: () => <Workspace active="server:overview"><Overview /></Workspace> };

export const NotificationsDesktop: Story = { play: rowsTouch, render: () => <Workspace active="notifications"><Notifications /></Workspace> };
export const ProfilePhone: Story = { parameters: { phoneViewport: true }, render: () => <Workspace active="profile"><section className="dw-settings-section" aria-labelledby="p-title"><h2 id="p-title">{t('workspace.profile')}</h2></section></Workspace> };
export const VoiceDesktop: Story = { play: rowsTouch, render: () => <Workspace active="voice"><Voice /></Workspace> };
export const VoicePushToTalk: Story = { render: () => <Workspace active="voice"><Voice ptt /></Workspace> };
export const KeyboardDesktop: Story = { render: () => <Workspace active="keyboard"><Keyboard /></Workspace> };
export const DevicesDesktop: Story = { play: rowsTouch, render: () => <Workspace active="devices"><Devices /></Workspace> };
export const NotificationsPhone: Story = { play: rowsTouch, parameters: { phoneViewport: true }, render: () => <Workspace active="notifications"><Notifications /></Workspace> };
export const VoicePhone: Story = { play: rowsTouch, parameters: { phoneViewport: true }, render: () => <Workspace active="voice"><Voice /></Workspace> };

// Task 10c: Server settings → Roles, Invites, Bot and Music as packages/web composes them (screens/admin/RolesSection.tsx,
// InvitesSection.tsx and BotSection.tsx's 'bot' and 'music' pages), owner's view, prototype sample data.
function Roles() {
  return (
    <section className="dw-settings-section" data-measure="full" aria-labelledby="roles-title">
      <h2 id="roles-title" tabIndex={-1}>{t('roles.title')}</h2>
      <p>{t('serverSettings.roles.body')}</p>
      <LiveRoleEditor />
    </section>
  );
}
function Invites() {
  return (
    <section className="dw-settings-section" data-measure="full" aria-labelledby="invites-title">
      <h2 id="invites-title" tabIndex={-1}>{t('admin.invites.title')}</h2>
      <p>{t('serverSettings.invites.body', { server: SERVER })}</p>
      <LiveInvites />
    </section>
  );
}
const CAPS: readonly { name: string; icon: IconName; on: boolean }[] = [
  { name: 'music', icon: 'music', on: true }, { name: 'polls', icon: 'vote', on: true }, { name: 'reminders', icon: 'alarm-clock', on: true },
  { name: 'welcome', icon: 'hand', on: false }, { name: 'roles', icon: 'user-plus', on: false }, { name: 'announcements', icon: 'megaphone', on: false },
  { name: 'warnings', icon: 'triangle-alert', on: true }, { name: 'removal', icon: 'message-square-x', on: true }, { name: 'kick', icon: 'user-x', on: true },
  { name: 'ban', icon: 'ban', on: true },
];
const GLENN = 'Glenn';
function Bot() {
  return (
    <section className="dw-settings-section dw-bot-settings" data-measure="full" aria-labelledby="bot-title">
      <h2 id="bot-title" tabIndex={-1}>{t('admin.bot.title')}</h2>
      <p>{t('serverSettings.bot.body', { bot: GLENN, server: SERVER })}</p>
      <SettingsRow id="bot-glenn" icon="bot" label={GLENN} tag={t('serverSettings.bot.tag')}
        hint={t('serverSettings.bot.cardVersion', { presence: t('serverSettings.bot.online'), version: '0.4.2' })}
        menu={{ label: t('serverSettings.bot.menu', { bot: GLENN }), onSelect: noop, items: [{ id: 'rename', label: t('serverSettings.bot.menu.rename') },
          { id: 'refresh', label: t('serverSettings.bot.menu.refresh') }, { id: 'disable', label: t('serverSettings.bot.menu.disable', { bot: GLENN }), danger: true, sep: true }] }}>
        <Button size="sm" onClick={noop}>{t('serverSettings.bot.restart')}</Button>
      </SettingsRow>
      {CAPS.map(c => (
        <SettingsRow key={c.name} id={`bot-cap-${c.name}`} icon={c.icon} label={t(`serverSettings.bot.cap.${c.name}` as Parameters<typeof t>[0])}
          hint={t(`serverSettings.bot.cap.${c.name}.hint` as Parameters<typeof t>[0])}>
          <Toggle id={`bot-cap-${c.name}-switch`} label={t(`serverSettings.bot.cap.${c.name}` as Parameters<typeof t>[0])} checked={c.on} onChange={noop} />
        </SettingsRow>
      ))}
      <SettingsRow id="bot-voice" icon="lock" label={t('serverSettings.bot.voice')} hint={t('serverSettings.bot.voiceHint', { bot: GLENN })}>
        <Button size="sm" onClick={noop}>{t('serverSettings.bot.voiceRoles')}</Button>
      </SettingsRow>
    </section>
  );
}
function Music() {
  return (
    <section className="dw-settings-section dw-bot-settings" data-measure="full" aria-labelledby="music-title">
      <h2 id="music-title" tabIndex={-1}>{t('serverSettings.music.title')}</h2>
      <p>{t('serverSettings.music.body', { bot: GLENN })}</p>
      <SettingsRow id="music-dj" icon="disc-3" label={t('serverSettings.music.dj')} hint={t('serverSettings.music.djRoles', { roles: 'DJ' })}>
        <Button size="sm" onClick={noop}>{t('serverSettings.music.djEdit')}</Button>
      </SettingsRow>
      <SettingsRow id="music-provider-youtube" anchor="music-providers" icon="youtube" label={t('serverSettings.music.youtube')} hint={t('serverSettings.music.youtubeReady')}
          tag={t('serverSettings.music.youtubeTag')}>
        <Button size="sm" onClick={noop}>{t('serverSettings.music.checkAgain')}</Button>
      </SettingsRow>
      <SettingsRow id="music-volume" icon="volume-2" label={t('serverSettings.music.volume')} hint={t('serverSettings.music.volumeHint')}>
        <Segmented id="music-volume-choice" label={t('serverSettings.music.volume')} value="50" onChange={noop}
          options={[25, 50, 75, 100].map(v => ({ value: String(v), label: String(v) }))} />
      </SettingsRow>
      <SettingsRow id="music-idle" icon="timer" label={t('serverSettings.music.idle')} hint={t('serverSettings.music.idleHint', { bot: GLENN })}>
        <Segmented id="music-idle-choice" label={t('serverSettings.music.idle')} value="15" onChange={noop}
          options={[5, 15, 30].map(v => ({ value: String(v), label: t('serverSettings.music.idle.option', { minutes: v }) }))} />
      </SettingsRow>
    </section>
  );
}

export const RolesDesktop: Story = { render: () => <Workspace active="server:roles"><Roles /></Workspace> };
export const InvitesDesktop: Story = { render: () => <Workspace active="server:invites"><Invites /></Workspace> };
export const BotDesktop: Story = { play: rowsTouch, render: () => <Workspace active="server:bot"><Bot /></Workspace> };
export const MusicDesktop: Story = { play: rowsTouch, render: () => <Workspace active="server:music"><Music /></Workspace> };
export const RolesPhone: Story = { parameters: { phoneViewport: true }, render: () => <Workspace active="server:roles"><Roles /></Workspace> };
export const InvitesPhone: Story = { parameters: { phoneViewport: true }, render: () => <Workspace active="server:invites"><Invites /></Workspace> };
export const BotPhone: Story = { play: rowsTouch, parameters: { phoneViewport: true }, render: () => <Workspace active="server:bot"><Bot /></Workspace> };
export const MusicPhone: Story = { play: rowsTouch, parameters: { phoneViewport: true }, render: () => <Workspace active="server:music"><Music /></Workspace> };
