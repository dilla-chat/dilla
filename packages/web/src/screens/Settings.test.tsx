import { describe, it, expect } from 'vitest';
import { act, render, screen, within, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { DeviceSummary } from '@dilla/client-core';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient, refusal } from '../test/fake-client.ts';
import { account } from '../test/fixtures.ts';
import { expectNoAxeViolations } from '../test/setup.ts';
import { Settings } from './Settings.tsx';
import { PHONE_QUERY } from '../layout.ts';

const C = 'a1'.repeat(16);
const CH = 'c3'.repeat(16);

function setup(path: string, from: string | null, seed?: (fake: FakeClient) => void) {
  window.history.replaceState(from === null ? null : { settingsFrom: from }, '', path);
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('devices', []);
  fake.set('settings', {});
  fake.set('communities', []);
  fake.set('dms', []);
  seed?.(fake);
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><Settings /></CoreProvider></div>);
  return { fake, user, view };
}
const nav = () => screen.getByRole('navigation', { name: 'Settings sections' });
const navButton = (name: string) => within(nav()).getByRole('button', { name: new RegExp(`^${name}$`, 'i') });

describe('Settings', () => {
  // Task 11 (Jonas, Q7): Profile saves as you type; the shared footer shows only while a change cannot be saved.
  it('saves Profile edits as you type, with no footer', async () => {
    const { fake, user } = setup('/settings/profile', null);
    const status = screen.getByRole('textbox', { name: /status message/i });
    await user.type(status, 'here');
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
    await waitFor(() => expect(fake.callsOf('updateProfile')).toEqual([{ m: 'updateProfile', statusMsg: 'here' }]), { timeout: 3000 });
    expect(await screen.findByText('Profile saved')).toBeInTheDocument();
  });
  it('keeps failed Profile edits available in the footer and resets them to the latest account values', async () => {
    const { fake, user } = setup('/settings/profile', null);
    const status = screen.getByRole('textbox', { name: /status message/i });
    fake.handler = () => Promise.reject(refusal({ code: 'E_NETWORK', status: 0 }));
    await user.type(status, 'draft');
    expect(await screen.findByRole('alert', {}, { timeout: 3000 })).toHaveTextContent('Could not save your profile. Try again.');
    expect(status).toHaveValue('draft');
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: 'Reset' }));
    expect(status).toHaveValue('');
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
  });
  it('focuses every selected section heading', async () => {
    const { user } = setup('/settings/devices', null);
    for (const [section, heading] of [['Sign-in', 'Sign-in'], ['Notifications', 'Notifications'], ['Appearance', 'Appearance'], ['Voice', 'Voice'], ['Privacy', 'Privacy'], ['Keyboard', 'Keyboard'], ['Diagnostics', 'Diagnostics'], ['Devices', 'Devices']]) {
      await user.click(navButton(section));
      expect(screen.getByRole('heading', { name: heading, level: 2 })).toHaveFocus();
    }
  });
  // Task 10 fix round 1 (README §5.10): a phone has no settings index; /settings opens Profile, as the prototype's You tab
  // does, with the nav's chips above it; the chips switch sections.
  it('opens Profile on a phone, and a section keeps the nav as chips', async () => {
    window.matchMedia = ((query: string) => ({ matches: query === PHONE_QUERY, media: query,
      addEventListener: () => undefined, removeEventListener: () => undefined })) as unknown as typeof window.matchMedia;
    try {
      const { user } = setup('/settings', '/you');
      const dialog = screen.getByRole('dialog', { name: 'Settings' });
      expect(dialog).toHaveAttribute('data-phone-page', 'section');
      expect(within(dialog).getByRole('heading', { level: 2, name: 'Profile' })).toBeInTheDocument();
      expect(within(dialog).getByRole('button', { name: 'Profile' })).toHaveAttribute('aria-current', 'page');
      await user.click(within(dialog).getByRole('button', { name: 'Devices' }));
      expect(window.location.pathname).toBe('/settings/devices');
      expect(within(dialog).queryByRole('button', { name: 'Back to settings' })).toBeNull();
      expect(within(dialog).getByRole('button', { name: 'Close settings' })).toBeInTheDocument();
    } finally { Reflect.deleteProperty(window, 'matchMedia'); }
  });
  it('does not focus a matching heading outside the settings content after a privacy comparison', async () => {
    const record = { userId: 'aa'.repeat(16), name: 'Ada', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false, changedUmk: null };
    const { fake, user } = setup('/settings/privacy', null, client => client.set('trust', [record]));
    fake.handler = command => command.m === 'safetyNumber' ? Promise.resolve('00001 '.repeat(11) + '00012') : Promise.resolve(null);
    const outside = document.createElement('h2');
    outside.dataset.privacyHeading = '';
    outside.textContent = 'Privacy outside settings';
    document.body.append(outside);
    try {
      await user.click(screen.getByRole('button', { name: 'Compare with Ada' }));
      await screen.findByText('00012');
      await user.click(screen.getByRole('checkbox', { name: 'Yes, all 12 blocks match' }));
      await user.click(screen.getByRole('button', { name: 'All 12 blocks match' }));
      await waitFor(() => expect(screen.getByRole('heading', { name: 'Privacy', level: 2 })).toHaveFocus());
      expect(outside).not.toHaveFocus();
    } finally { outside.remove(); }
  });
  it('keeps comparison open and reports a refused changed-key verification', async () => {
    const userId = 'aa'.repeat(16);
    const record = { userId, name: 'Ada', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false, changedUmk: 'cc'.repeat(32) };
    const { fake, user } = setup('/settings/privacy', null, client => client.set('trust', [record]));
    fake.handler = command => command.m === 'safetyNumber' ? Promise.resolve('00001 '.repeat(11) + '00012')
      : command.m === 'verifyContact' && command.umk !== fake.get('trust')?.[0]?.changedUmk
        ? Promise.reject(refusal({ code: 'E_CORE_INPUT' })) : Promise.resolve(null);
    await user.click(screen.getByRole('button', { name: 'Compare with Ada' }));
    await screen.findByText('cc'.repeat(32));
    act(() => fake.set('trust', [{ ...record, changedUmk: 'dd'.repeat(32) }]));
    await user.click(screen.getByRole('checkbox', { name: 'Yes, all 12 blocks match' }));
    await user.click(screen.getByRole('button', { name: 'All 12 blocks match' }));
    expect(fake.callsOf('verifyContact')).toEqual([{ m: 'verifyContact', userId, umk: 'cc'.repeat(32) }]);
    expect(screen.getByRole('dialog', { name: 'Compare safety numbers' })).toBeInTheDocument();
    // W-04: ceremony copy, never the code, and no "try again" for something that cannot work.
    await waitFor(() => expect(within(screen.getByRole('dialog', { name: 'Compare safety numbers' })).getByRole('alert'))
      .toHaveTextContent('This could not be marked as verified. Close this and compare again.'));
    expect(screen.getByRole('dialog', { name: 'Compare safety numbers' })).not.toHaveTextContent('E_CORE_INPUT');
  });
  it('says the key moved when the core refuses the approval as a changed key', async () => {
    const userId = 'aa'.repeat(16);
    const record = { userId, name: 'Ada', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false, changedUmk: 'cc'.repeat(32) };
    const { fake, user } = setup('/settings/privacy', null, client => client.set('trust', [record]));
    fake.handler = command => command.m === 'safetyNumber' ? Promise.resolve('00001 '.repeat(11) + '00012')
      : command.m === 'verifyContact' ? Promise.reject(refusal({ code: 'E_UMK_CHANGED' })) : Promise.resolve(null);
    await user.click(screen.getByRole('button', { name: 'Compare with Ada' }));
    await screen.findByText('cc'.repeat(32));
    await user.click(screen.getByRole('checkbox', { name: 'Yes, all 12 blocks match' }));
    await user.click(screen.getByRole('button', { name: 'All 12 blocks match' }));
    const dialog = screen.getByRole('dialog', { name: 'Compare safety numbers' });
    await waitFor(() => expect(within(dialog).getByRole('alert')).toHaveTextContent('The key changed while you compared. Close this and compare the new number.'));
    expect(within(dialog).getByRole('button', { name: 'All 12 blocks match' })).toHaveAttribute('aria-disabled', 'true');
  });
  // W-11 (b): a failed number keeps the dialog open and says why; while it loads the dialog says so.
  it('says the number is loading, and keeps the dialog open when it cannot be worked out', async () => {
    const record = { userId: 'aa'.repeat(16), name: 'Ada', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false, changedUmk: null };
    const { fake, user } = setup('/settings/privacy', null, client => client.set('trust', [record]));
    let fail!: (e: Error) => void;
    fake.handler = command => command.m === 'safetyNumber' ? new Promise((_r, reject) => { fail = reject; }) : Promise.resolve(null);
    await user.click(screen.getByRole('button', { name: 'Compare with Ada' }));
    const dialog = screen.getByRole('dialog', { name: 'Compare safety numbers' });
    expect(within(dialog).getByRole('status')).toHaveTextContent('Working out the number…');
    await act(async () => { fail(refusal({ code: 'E_CORE_INPUT' })); await Promise.resolve(); });
    expect(within(dialog).getByRole('alert')).toHaveTextContent('The number could not be worked out. Close this and try again.');
    expect(screen.getByRole('dialog', { name: 'Compare safety numbers' })).toBeInTheDocument();
  });
  // W-03: the dialog's own close is "Close", not the Settings frame's "Close settings".
  it('labels the compare dialog close as Close, and focuses Privacy after an approval', async () => {
    const userId = 'aa'.repeat(16);
    const record = { userId, name: 'Ada', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false, changedUmk: null };
    const { fake, user } = setup('/settings/privacy', null, client => client.set('trust', [record]));
    fake.handler = command => {
      if (command.m === 'safetyNumber') return Promise.resolve('00001 '.repeat(11) + '00012');
      if (command.m === 'verifyContact') { fake.set('trust', [{ ...record, verified: true }]); return Promise.resolve(null); }
      return Promise.resolve(null);
    };
    await user.click(screen.getByRole('button', { name: 'Compare with Ada' }));
    const dialog = screen.getByRole('dialog', { name: 'Compare safety numbers' });
    expect(within(dialog).getByRole('button', { name: /^Close/ })).toHaveTextContent(/^Close(esc)?$/);
    expect(within(dialog).queryByRole('button', { name: /Close settings/ })).toBeNull();
    await within(dialog).findByText('00012');
    await user.click(screen.getByRole('checkbox', { name: 'Yes, all 12 blocks match' }));
    await user.click(screen.getByRole('button', { name: 'All 12 blocks match' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Compare safety numbers' })).toBeNull());
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Privacy', level: 2 })).toHaveFocus());
  });
  it('opens as a dialog on the section of the path, with its heading focused', async () => {
    const { fake, view } = setup('/settings/devices', `/c/${C}/${CH}`);
    const dialog = screen.getByRole('dialog', { name: 'Settings' });
    expect(within(dialog).getByRole('heading', { level: 2, name: 'Devices' })).toHaveFocus();
    const items = within(nav()).getAllByRole('button');
    expect(items).toEqual(['Profile', 'Sign-in', 'Devices', 'Notifications', 'Voice', 'Keyboard', 'Appearance', 'Privacy', 'Diagnostics'].map(name => navButton(name)));
    expect(navButton('Devices')).toHaveAttribute('aria-current', 'page');
    expect(screen.getByRole('heading', { level: 2, name: 'Devices' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Close settings' })).toBeInTheDocument();
    expect(fake.callsOf('refreshDevices')).toEqual([{ m: 'refreshDevices' }]);
    await expectNoAxeViolations(view.container);
  });
  it('switches sections in place, keeping where it came from', async () => {
    const { user, view } = setup('/settings/devices', `/c/${C}/${CH}`);
    const depth = window.history.length;
    await user.click(navButton('Notifications'));
    expect(window.location.pathname).toBe('/settings/notifications');
    expect(window.history.length).toBe(depth);
    expect(window.history.state).toMatchObject({ settingsFrom: `/c/${C}/${CH}` });
    expect(screen.getByRole('heading', { level: 2, name: 'Notifications' })).toBeInTheDocument();
    expect(navButton('Notifications')).toHaveAttribute('aria-current', 'page');
    await expectNoAxeViolations(view.container);
    await user.click(navButton('Appearance'));
    expect(screen.getByRole('heading', { level: 2, name: 'Appearance' })).toBeInTheDocument();
    await expectNoAxeViolations(view.container);
  });
  it('closes to where it came from on Escape, replacing its entry', async () => {
    const { user } = setup('/settings/notifications', `/c/${C}/${CH}`);
    const depth = window.history.length;
    await user.keyboard('{Escape}');
    expect(window.location.pathname).toBe(`/c/${C}/${CH}`);
    expect(window.history.length).toBe(depth);
    expect((window.history.state as Record<string, unknown>)?.settingsFrom).toBeUndefined();
    expect(screen.queryByRole('dialog')).toBeNull();
  });
  it('closes by the live layout: a viewport flipped back to desktop closes the desktop way in the same tick', async () => {
    let phoneNow = true;
    window.matchMedia = ((query: string) => ({
      get matches() { return phoneNow && query === PHONE_QUERY; }, media: query,
      addEventListener: () => undefined, removeEventListener: () => undefined,
    })) as unknown as typeof window.matchMedia;
    try {
      const { user } = setup('/settings/notifications', `/c/${C}/${CH}`);
      // The change event has not reached React yet (no re-render): only the live query knows.
      phoneNow = false;
      await user.keyboard('{Escape}');
      expect(window.location.pathname).toBe(`/c/${C}/${CH}`);
    } finally { Reflect.deleteProperty(window, 'matchMedia'); }
  });
  it('closes to / from a deep link, by the close button', async () => {
    const { user } = setup('/settings', null);
    expect(screen.getByRole('heading', { level: 2, name: 'Profile' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Close settings' }));
    expect(window.location.pathname).toBe('/');
  });
  it('renders nothing for another route', () => {
    setup(`/c/${C}/${CH}`, null);
    expect(screen.queryByRole('dialog')).toBeNull();
  });
  it('opens all nine sections and a shared contact’s compare dialog', async () => {
    const userId = 'aa'.repeat(16);
    const umk = 'bb'.repeat(32);
    const { fake, user, view } = setup('/settings/devices', null, client => {
      client.set('trust', [{ userId, name: 'Ada', shared: true, umk, firstSeen: 1, verified: false, changedUmk: null }]);
    });
    expect(within(nav()).getAllByRole('button').map(button => button.textContent)).toEqual(['Profile', 'Sign-in', 'Devices', 'Notifications', 'Voice', 'Keyboard', 'Appearance', 'Privacy', 'Diagnostics']);
    await user.click(navButton('Privacy'));
    expect(screen.getByRole('heading', { name: 'Privacy' })).toBeInTheDocument();
    expect(screen.getByText('Ada')).toBeInTheDocument();
    expect(screen.getByText('First seen here — not verified')).toBeInTheDocument();
    expect(screen.getByText('This is the first key this device saw for Ada. The server could have shown a different one. Compare numbers to verify.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Compare with Ada' }));
    expect(screen.getByRole('dialog', { name: 'Compare safety numbers' })).toHaveTextContent('This is the first key this device saw for Ada. The server could have shown a different one. Compare numbers to verify.');
    expect(fake.callsOf('safetyNumber')).toEqual([{ m: 'safetyNumber', userId, umk }]);
    await expectNoAxeViolations(view.container);
  });

  it('explains when there is no shared group and offers no comparison', async () => {
    const { user } = setup('/settings/privacy', null, client => {
      client.set('trust', [{ userId: 'aa'.repeat(16), name: 'Ada', shared: false, umk: 'bb'.repeat(32), firstSeen: 1, verified: false, changedUmk: null }]);
    });
    await user.click(navButton('Privacy'));
    expect(screen.getByText('Open a conversation with this person to compare numbers.')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Compare with Ada' })).toBeNull();
  });

  it('preserves the changed alert until exact-key approval', async () => {
    const userId = 'aa'.repeat(16);
    const umk = 'cc'.repeat(32);
    const { fake, user } = setup('/settings/privacy', null, client => {
      client.set('trust', [{ userId, name: 'Ada', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false, changedUmk: umk }]);
    });
    fake.handler = command => command.m === 'safetyNumber' ? Promise.resolve('00001 '.repeat(11) + '00012') : Promise.resolve(null);
    expect(screen.getByRole('alert')).toHaveTextContent('This contact’s identity changed. Compare the number before sending.');
    await user.click(screen.getByRole('button', { name: 'Compare with Ada' }));
    expect(await screen.findByText(umk)).toBeInTheDocument();
    const approve = screen.getByRole('button', { name: 'All 12 blocks match' });
    await user.click(screen.getByRole('button', { name: 'Copy number' }));
    expect(approve).toHaveAttribute('aria-disabled', 'true');
    await user.click(screen.getByRole('checkbox', { name: 'Yes, all 12 blocks match' }));
    await user.click(approve);
    expect(fake.callsOf('verifyContact')).toEqual([{ m: 'verifyContact', userId, umk }]);
    expect(screen.getByRole('alert')).toBeInTheDocument();
  });
  it('approves only the key and number shown when comparison opened', async () => {
    const userId = 'aa'.repeat(16);
    const first = 'cc'.repeat(32);
    const next = 'dd'.repeat(32);
    const record = { userId, name: 'Ada', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false, changedUmk: first };
    const { fake, user } = setup('/settings/privacy', null, client => client.set('trust', [record]));
    fake.handler = command => command.m === 'safetyNumber' ? Promise.resolve('00001 '.repeat(11) + '00012') : Promise.resolve(null);
    await user.click(screen.getByRole('button', { name: 'Compare with Ada' }));
    expect(await screen.findByText(first)).toBeInTheDocument();
    act(() => fake.set('trust', [{ ...record, changedUmk: next }]));
    expect(screen.getByRole('dialog', { name: 'Compare safety numbers' })).toHaveTextContent(first);
    expect(screen.getByRole('dialog', { name: 'Compare safety numbers' })).not.toHaveTextContent(next);
    await user.click(screen.getByRole('checkbox', { name: 'Yes, all 12 blocks match' }));
    await user.click(screen.getByRole('button', { name: 'All 12 blocks match' }));
    expect(fake.callsOf('verifyContact')).toEqual([{ m: 'verifyContact', userId, umk: first }]);
  });
  it('names the compared state in the dialog and never approves while a device is unlisted', async () => {
    const userId = 'aa'.repeat(16);
    const record = { userId, name: 'Ada', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false, changedUmk: 'cc'.repeat(32), unlisted: true as const };
    const { fake, user } = setup('/settings/privacy', null, client => client.set('trust', [record]));
    fake.handler = command => command.m === 'safetyNumber' ? Promise.resolve('00001 '.repeat(11) + '00012') : Promise.resolve(null);
    expect(screen.getByText('This device is not on the contact’s list.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Compare with Ada' }));
    const dialog = screen.getByRole('dialog', { name: 'Compare safety numbers' });
    await within(dialog).findByText('cc'.repeat(32));
    expect(within(dialog).getByText(/^Ada · /)).toHaveTextContent('Ada · This contact’s identity changed. Compare the number before sending.');
    await user.click(screen.getByRole('checkbox', { name: 'Yes, all 12 blocks match' }));
    expect(screen.getByRole('button', { name: 'All 12 blocks match' })).toHaveAttribute('aria-disabled', 'true');
    await user.click(screen.getByRole('button', { name: 'All 12 blocks match' }));
    expect(fake.callsOf('verifyContact')).toEqual([]);
  });
  it('labels a first-seen comparison with Flow 05’s state text', async () => {
    const record = { userId: 'aa'.repeat(16), name: 'Ada', shared: true, umk: 'bb'.repeat(32), firstSeen: 1, verified: false, changedUmk: null };
    const { user } = setup('/settings/privacy', null, client => client.set('trust', [record]));
    await user.click(screen.getByRole('button', { name: 'Compare with Ada' }));
    const dialog = screen.getByRole('dialog', { name: 'Compare safety numbers' });
    expect(within(dialog).getByText(/^Ada · /)).toHaveTextContent('Ada · First seen here — not verified');
    expect(dialog).not.toHaveTextContent('first-seen');
  });
  // Pre-flight ruling 1.2: Escape in a dialog nested in the frame closes only that dialog, and does nothing at all
  // while its command runs; Settings stays open on its route either way.
  it('leaves Settings open when Escape closes a dialog inside it, and while a removal runs', async () => {
    const PHONE = 'd1'.repeat(16);
    const devices: DeviceSummary[] = [
      { id: 'cc'.repeat(16), tier: 1, signerTier: 1, lastSeen: 1, revokedAt: null, listed: true, own: true, label: null },
      { id: PHONE, tier: 0, signerTier: 0, lastSeen: 1, revokedAt: null, listed: true, own: false, label: null },
    ];
    const { fake, user } = setup('/settings/devices', `/c/${C}/${CH}`);
    fake.handler = c => (c.m === 'revokeDevice' ? new Promise(() => {}) : Promise.resolve(null));
    act(() => fake.set('devices', devices));
    await user.click(screen.getByRole('button', { name: 'Remove' }));
    expect(screen.getByRole('dialog', { name: 'Remove this device?' })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: 'Remove this device?' })).toBeNull();
    expect(screen.getByRole('dialog', { name: 'Settings' })).toBeInTheDocument();
    expect(window.location.pathname).toBe('/settings/devices');
    await user.click(screen.getByRole('button', { name: 'Remove' }));
    await user.click(screen.getByRole('textbox', { name: 'Recovery key' }));
    await user.paste('7K2M-QX9D-H4TB-R8NW-C3VF-J6PZ-A1GE-Y5KS-M0QT-B7XH-W2DN-F9RC-P4ZA');
    await user.click(within(screen.getByRole('dialog', { name: 'Remove this device?' })).getByRole('button', { name: 'Remove' }));
    expect(screen.getByRole('button', { name: 'Working…' })).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(fake.callsOf('revokeDevice')).toHaveLength(1);
    expect(screen.getByRole('dialog', { name: 'Remove this device?' })).toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'Settings' })).toBeInTheDocument();
    expect(window.location.pathname).toBe('/settings/devices');
  });
});
