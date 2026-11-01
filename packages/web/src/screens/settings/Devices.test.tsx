import { describe, it, expect, vi } from 'vitest';
import { act, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent, { type UserEvent } from '@testing-library/user-event';
import type { Command, DeviceSummary } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { account } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { Devices, orderDevices } from './Devices.tsx';

const NOW = Math.floor(Date.now() / 1000);
const OWN = 'cc'.repeat(16);
const PHONE = 'd1'.repeat(16);
const FORGOTTEN = 'f3'.repeat(16);
const OLD = 'e2'.repeat(16);
const GROUPED = '7K2M-QX9D-H4TB-R8NW-C3VF-J6PZ-A1GE-Y5KS-M0QT-B7XH-W2DN-F9RC-P4ZA';
const dev = (id: string, over: Partial<DeviceSummary> = {}): DeviceSummary =>
  ({ id, tier: 1, signerTier: 1, lastSeen: NOW, revokedAt: null, listed: true, own: false, label: null, ...over });
const DEVICES: DeviceSummary[] = [
  dev(OLD, { lastSeen: NOW - 3 * 86_400, revokedAt: NOW - 3_600, listed: false }),
  dev(FORGOTTEN, { lastSeen: NOW - 120, listed: false }),
  dev(PHONE, { tier: 0, signerTier: 0, lastSeen: NOW - 60 }),
  dev(OWN, { own: true }),
];

// The devices argument is a rest element, not a default parameter: an explicit `undefined` (no slice yet) must not
// fall back to DEVICES, as a default parameter would.
function setup(handler: (c: Command) => Promise<unknown> = () => Promise.resolve(null), ...list: [] | [DeviceSummary[] | undefined]) {
  const devices = list.length === 0 ? DEVICES : list[0];
  window.history.replaceState({ settingsFrom: '/' }, '', '/settings/devices');
  const fake = new FakeClient();
  fake.set('account', account());
  if (devices !== undefined) fake.set('devices', devices);
  fake.handler = handler;
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><Devices /></CoreProvider></div>);
  return { fake, user, view };
}
const rows = () => within(screen.getByRole('list')).getAllByRole('listitem');
// Signal task 10 fix round 2 (Jonas): Rename is in the device row's context menu (right-click, long press, Shift+F10).
async function renameFrom(user: UserEvent, row: HTMLElement): Promise<void> {
  fireEvent.contextMenu(row, { clientX: 10, clientY: 10 });
  await user.click(await screen.findByRole('menuitem', { name: 'Rename' }));
}
const rowOf = (id: string) => {
  const row = rows().find(r => within(r).queryByText(id.slice(0, 8)) !== null);
  if (row === undefined) throw new Error(`no row for ${id}`);
  return row;
};
async function typeKey(user: UserEvent, text = GROUPED) {
  await user.click(screen.getByRole('textbox', { name: 'Recovery key' }));
  await user.paste(text);
}

describe('the device list', () => {
  it('asks for a fresh list once and lists every device, this browser first', async () => {
    const { fake, view } = setup();
    expect(fake.callsOf('refreshDevices')).toEqual([{ m: 'refreshDevices' }]);
    expect(screen.getByRole('heading', { level: 2, name: 'Devices' })).toBeInTheDocument();
    expect(screen.getByText('Every browser that holds your keys.')).toBeInTheDocument();
    expect(rows().map(r => within(r).getByText(/^[0-9a-f]{8}$/).textContent)).toEqual([OWN, PHONE, FORGOTTEN, OLD].map(id => id.slice(0, 8)));
    const own = rowOf(OWN);
    expect(within(own).getByText('This browser')).toBeInTheDocument();
    // Signal task 10 fix round 1 (README §5.9): no tier tags; this browser's row signs out, and every live device can be named.
    expect(within(own).queryByText('Browser')).toBeNull();
    expect(within(own).getAllByRole('button').map(b => b.textContent)).toEqual(['Sign out']);
    expect(within(rowOf(PHONE)).getByRole('button', { name: 'Remove' })).toBeInTheDocument();
    expect(within(rowOf(FORGOTTEN)).getByText('not yet in the device list')).toBeInTheDocument();
    expect(within(rowOf(FORGOTTEN)).getByRole('button', { name: 'Remove' })).toBeInTheDocument();
    expect(within(rowOf(OLD)).getByText('removed')).toBeInTheDocument();
    expect(within(rowOf(OLD)).queryByRole('button')).toBeNull();
    // Fix round 2 (the prototype's sub line): "this browser · active now · {id}" here, "active {when} · {id}" elsewhere.
    expect(within(own).getByText(`this browser · active now · ${OWN.slice(0, 6)}`)).toBeInTheDocument();
    expect(within(rowOf(PHONE)).getByText(`active 1 minute ago · ${PHONE.slice(0, 6)}`)).toBeInTheDocument();
    await expectNoAxeViolations(view.container);
  });
  it('orders by own, then last seen, then removed', () => {
    const tie = dev('aa'.repeat(16), { lastSeen: NOW - 60 });
    expect(orderDevices([...DEVICES, tie]).map(d => d.id)).toEqual([OWN, tie.id, PHONE, FORGOTTEN, OLD]);
  });
  // Signal task 10 fix round 3: Sign out stays reachable while the list is missing.
  it('draws this browser\u2019s row with Sign out from the local identity while the list has not arrived', () => {
    setup(undefined, undefined);
    expect(screen.getByRole('button', { name: 'Sign out' })).toBeInTheDocument();
    expect(screen.getByText('This browser')).toBeInTheDocument();
  });
  it('says when the list did not load', async () => {
    setup(c => (c.m === 'refreshDevices' ? Promise.reject(refusal({ code: 'E_NETWORK' })) : Promise.resolve(null)));
    expect(await screen.findByRole('alert')).toHaveTextContent('That did not work (E_NETWORK). Try again.');
  });
});

describe('removing a device', () => {
  it('needs the whole key, then sends it with the device', async () => {
    const { fake, user, view } = setup();
    await user.click(within(rowOf(PHONE)).getByRole('button', { name: 'Remove' }));
    const dialog = screen.getByRole('dialog', { name: 'Remove this device?' });
    expect(within(dialog).getByText('The device is signed out everywhere and leaves every conversation. Enter your recovery key to confirm.')).toBeInTheDocument();
    const confirm = within(dialog).getByRole('button', { name: 'Remove' });
    expect(confirm).toHaveAttribute('aria-disabled', 'true');
    expect(confirm).toHaveAccessibleDescription('A recovery key has 52 characters.');
    await user.click(confirm);
    expect(fake.callsOf('revokeDevice')).toEqual([]);
    expect(screen.getByRole('textbox', { name: 'Recovery key' })).toHaveFocus();
    await expectNoAxeViolations(view.container);
    await typeKey(user);
    expect(screen.getByRole('textbox', { name: 'Recovery key' })).toHaveAccessibleDescription(expect.stringContaining('52 of 52 characters'));
    expect(confirm).not.toHaveAttribute('aria-disabled');
    await user.click(confirm);
    expect(fake.callsOf('revokeDevice')).toEqual([{ m: 'revokeDevice', deviceId: PHONE, recoveryKey: GROUPED }]);
    expect(screen.queryByRole('dialog', { name: 'Remove this device?' })).toBeNull();
    await user.click(within(rowOf(PHONE)).getByRole('button', { name: 'Remove' }));
    expect(screen.getByRole('textbox', { name: 'Recovery key' })).toHaveValue('');
  });
  it('a wrong key keeps the text and puts focus on the field', async () => {
    const { user } = setup(c => (c.m === 'revokeDevice' ? Promise.reject(refusal({ code: 'E_RECOVERY_KEY' })) : Promise.resolve(null)));
    await user.click(within(rowOf(PHONE)).getByRole('button', { name: 'Remove' }));
    await typeKey(user);
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    const field = screen.getByRole('textbox', { name: 'Recovery key' });
    expect(field).toHaveValue(GROUPED);
    expect(field).toHaveAccessibleDescription(expect.stringContaining('This is not the recovery key of this account.'));
    expect(field).toHaveFocus();
    expect(screen.getByRole('dialog', { name: 'Remove this device?' })).toBeInTheDocument();
  });
  it('another refusal is shown in the dialog by its code', async () => {
    const { user } = setup(c => (c.m === 'revokeDevice'
      ? Promise.reject(refusal({ code: 'E_DEVICE_LIST_STALE', detail: 'E_RECOVERY_KEY', status: 409 })) : Promise.resolve(null)));
    await user.click(within(rowOf(PHONE)).getByRole('button', { name: 'Remove' }));
    await typeKey(user);
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(within(screen.getByRole('dialog', { name: 'Remove this device?' })).getByRole('alert')).toHaveTextContent('That did not work (E_DEVICE_LIST_STALE). Try again.');
    expect(screen.queryByText(/not the recovery key/)).toBeNull();
  });
  it('sends one removal while in flight and cannot be closed meanwhile', async () => {
    const { fake, user } = setup(c => (c.m === 'revokeDevice' ? new Promise(() => {}) : Promise.resolve(null)));
    await user.click(within(rowOf(PHONE)).getByRole('button', { name: 'Remove' }));
    await typeKey(user);
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(screen.getByRole('button', { name: 'Working…' })).toHaveAttribute('aria-disabled', 'true');
    await user.click(screen.getByRole('button', { name: 'Working…' }));
    await user.keyboard('{Escape}');
    expect(fake.callsOf('revokeDevice')).toHaveLength(1);
    expect(screen.getByRole('dialog', { name: 'Remove this device?' })).toBeInTheDocument();
  });
  it('drops the key when the dialog is closed', async () => {
    const { user } = setup();
    await user.click(within(rowOf(PHONE)).getByRole('button', { name: 'Remove' }));
    await typeKey(user);
    await user.click(screen.getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    await user.click(within(rowOf(PHONE)).getByRole('button', { name: 'Remove' }));
    expect(screen.getByRole('textbox', { name: 'Recovery key' })).toHaveValue('');
  });
  it('removes a device that is not in the list without the key', async () => {
    const { fake, user, view } = setup();
    await user.click(within(rowOf(FORGOTTEN)).getByRole('button', { name: 'Remove' }));
    const dialog = screen.getByRole('dialog', { name: 'Remove this device?' });
    expect(within(dialog).getByText('This device signed in with your password but is not in the device list, so it cannot read anything. Removing it needs no recovery key.')).toBeInTheDocument();
    expect(within(dialog).queryByText(/Enter your recovery key/)).toBeNull();
    expect(within(dialog).queryByRole('textbox')).toBeNull();
    await expectNoAxeViolations(view.container);
    await user.click(within(dialog).getByRole('button', { name: 'Remove' }));
    expect(fake.callsOf('revokeDevice')).toEqual([{ m: 'revokeDevice', deviceId: FORGOTTEN, recoveryKey: null }]);
    expect(screen.queryByRole('dialog')).toBeNull();
  });
  it('shows a refused removal of an unlisted device in its dialog, sent once while in flight', async () => {
    let release: (e: unknown) => void = () => {};
    const { fake, user } = setup(c => (c.m === 'revokeDevice' ? new Promise((_, reject) => { release = reject; }) : Promise.resolve(null)));
    await user.click(within(rowOf(FORGOTTEN)).getByRole('button', { name: 'Remove' }));
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(screen.getByRole('button', { name: 'Working…' })).toHaveAttribute('aria-disabled', 'true');
    await user.click(screen.getByRole('button', { name: 'Working…' }));
    expect(fake.callsOf('revokeDevice')).toHaveLength(1);
    await act(async () => { release(refusal({ code: 'E_BAD_INPUT' })); await Promise.resolve(); });
    expect(within(screen.getByRole('dialog', { name: 'Remove this device?' })).getByRole('alert')).toHaveTextContent('That did not work (E_BAD_INPUT). Try again.');
  });
  // Pre-flight ruling 1.5: the button that opened the dialog is gone once the row reads removed, so focus goes to
  // the section heading, never to the page body.
  it.each([
    ['a listed device, with the key', PHONE, true],
    ['an unlisted device, without it', FORGOTTEN, false],
  ] as const)('moves focus to the section heading after removing %s', async (_, id, withKey) => {
    const { user } = setup();
    await user.click(within(rowOf(id)).getByRole('button', { name: 'Remove' }));
    if (withKey) await typeKey(user);
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    const heading = screen.getByRole('heading', { level: 2, name: 'Devices' });
    expect(document.activeElement).toBe(heading);
    expect(heading).toHaveAttribute('tabindex', '-1');
  });
});

// EOD E7: a device carries the owner's own name (readable by the server, not part of the signed device list).
describe('naming a device', () => {
  const NAMED = 'a4'.repeat(16);
  const list = () => [dev(OWN, { own: true }), dev(NAMED, { label: 'work laptop', lastSeen: NOW - 30 }), dev(PHONE, { tier: 0, signerTier: 0, lastSeen: NOW - 60 }),
    dev(OLD, { lastSeen: NOW - 3 * 86_400, revokedAt: NOW - 3_600, listed: false })];
  const rowNamed = (name: string) => {
    const row = rows().find(r => within(r).queryByText(name) !== null);
    if (row === undefined) throw new Error(`no row named ${name}`);
    return row;
  };
  const field = () => screen.getByRole('textbox', { name: 'Device name' });

  it('shows the name where there is one and the first eight characters where there is not, and offers rename on live devices only', () => {
    setup(undefined, list());
    expect(within(rowNamed('work laptop')).getByText(new RegExp(` · ${NAMED.slice(0, 6)}$`))).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Rename' })).toBeNull();
    fireEvent.contextMenu(rowNamed(PHONE.slice(0, 8)), { clientX: 10, clientY: 10 });
    expect(screen.getByRole('menuitem', { name: 'Rename' })).toBeInTheDocument();
    fireEvent.keyDown(screen.getByRole('menuitem', { name: 'Rename' }), { key: 'Escape' });
    fireEvent.contextMenu(rowNamed(OLD.slice(0, 8)), { clientX: 10, clientY: 10 });
    expect(screen.queryByRole('menuitem', { name: 'Rename' })).toBeNull();
  });
  it('keeps the id under a name, the name isolated, so a swapped name does not hide which device it is', () => {
    setup(undefined, list());
    const row = rowNamed('work laptop');
    expect(within(row).getByText('work laptop').tagName).toBe('BDI');
    expect(within(row).getByText(new RegExp(` · ${NAMED.slice(0, 6)}$`))).toBeInTheDocument();
    // An unnamed device shows its id once.
    expect(within(rowNamed(PHONE.slice(0, 8))).getAllByText(PHONE.slice(0, 8))).toHaveLength(1);
  });
  it('names the device by its label and its id in both remove dialogs', async () => {
    const named = [dev(OWN, { own: true }), dev(NAMED, { label: 'work laptop' }), dev(FORGOTTEN, { label: 'old tablet', listed: false })];
    const { user } = setup(undefined, named);
    await user.click(within(rowNamed('work laptop')).getByRole('button', { name: 'Remove' }));
    let dialog = screen.getByRole('dialog', { name: 'Remove this device?' });
    expect(within(dialog).getByText('work laptop').tagName).toBe('BDI');
    expect(within(dialog).getByText(NAMED.slice(0, 8))).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Close' }));
    await user.click(within(rowNamed('old tablet')).getByRole('button', { name: 'Remove' }));
    dialog = screen.getByRole('dialog', { name: 'Remove this device?' });
    expect(within(dialog).getByText('old tablet')).toBeInTheDocument();
    expect(within(dialog).getByText(FORGOTTEN.slice(0, 8))).toBeInTheDocument();
  });
  it('names an unnamed device by its id in the remove dialog', async () => {
    const { user } = setup();
    await user.click(within(rowOf(PHONE)).getByRole('button', { name: 'Remove' }));
    expect(within(screen.getByRole('dialog', { name: 'Remove this device?' })).getByText(PHONE.slice(0, 8))).toBeInTheDocument();
  });
  it('opens on the current name, saves it trimmed with the device, and closes', async () => {
    const { fake, user, view } = setup(undefined, list());
    await renameFrom(user, rowNamed('work laptop'));
    const dialog = screen.getByRole('dialog', { name: 'Name this device' });
    expect(within(dialog).getByText('The server can change it, so check a device by its id before you remove it.')).toBeInTheDocument();
    // web3.12.1: a quiet line under the field says the server can read the name, and it is part of the field's description.
    const reads = within(dialog).getByText('The server can read this, like your display name.');
    expect(reads).toHaveClass('d-text-field__hint');
    expect(field()).toHaveAccessibleDescription(
      'The server can change it, so check a device by its id before you remove it. The server can read this, like your display name.');
    expect(field()).toHaveValue('work laptop');
    expect(field()).toHaveFocus();
    expect(field()).toHaveAttribute('maxlength', '64');
    await expectNoAxeViolations(view.container);
    await user.clear(field());
    await user.type(field(), '  old phone  ');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(fake.callsOf('renameDevice')).toEqual([{ m: 'renameDevice', deviceId: NAMED, label: 'old phone' }]);
    expect(screen.queryByRole('dialog')).toBeNull();
  });
  it('clears the name when the field is emptied', async () => {
    const { fake, user } = setup(undefined, list());
    await renameFrom(user, rowNamed('work laptop'));
    await user.clear(field());
    await user.keyboard('{Enter}');
    expect(fake.callsOf('renameDevice')).toEqual([{ m: 'renameDevice', deviceId: NAMED, label: null }]);
  });
  it('suggests the browser and system on this browser when it has no name, and on no other device', async () => {
    const ua = 'Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0';
    const spy = vi.spyOn(window.navigator, 'userAgent', 'get').mockReturnValue(ua);
    try {
      const { user } = setup(undefined, list());
      await renameFrom(user, rowNamed(OWN.slice(0, 8)));
      expect(field()).toHaveValue('Firefox on Linux');
      await user.click(screen.getByRole('button', { name: 'Close' }));
      await renameFrom(user, rowNamed(PHONE.slice(0, 8)));
      expect(field()).toHaveValue('');
    } finally { spy.mockRestore(); }
  });
  it('shows a refusal by its code and keeps the dialog and the text', async () => {
    const { user } = setup(c => (c.m === 'renameDevice' ? Promise.reject(refusal({ code: 'E_NOT_FOUND' })) : Promise.resolve(null)), list());
    await renameFrom(user, rowNamed(PHONE.slice(0, 8)));
    await user.type(field(), 'tablet');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    const dialog = screen.getByRole('dialog', { name: 'Name this device' });
    expect(within(dialog).getByRole('alert')).toHaveTextContent('That did not work (E_NOT_FOUND). Try again.');
    expect(field()).toHaveValue('tablet');
  });
  it('sends one rename while in flight and cannot be closed meanwhile', async () => {
    const { fake, user } = setup(c => (c.m === 'renameDevice' ? new Promise(() => {}) : Promise.resolve(null)), list());
    await renameFrom(user, rowNamed(PHONE.slice(0, 8)));
    await user.type(field(), 'tablet');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(screen.getByRole('button', { name: 'Working…' })).toHaveAttribute('aria-disabled', 'true');
    await user.click(screen.getByRole('button', { name: 'Working…' }));
    await user.keyboard('{Escape}');
    expect(fake.callsOf('renameDevice')).toHaveLength(1);
    expect(screen.getByRole('dialog', { name: 'Name this device' })).toBeInTheDocument();
  });
});

// BACKUPS-RECOVERY-04: the same alert where the recovery key is used.
describe('a recovery root this account did not seal', () => {
  const TEXT = 'The recovery data stored for this account on dilla.test is not this account’s. The recovery key will not work until the operator resets it.';
  it('heads the section with a danger banner while the account slice reports it', async () => {
    const { fake, view } = setup();
    expect(screen.queryByText(TEXT)).toBeNull();
    act(() => fake.set('account', account({ rootMismatch: true })));
    expect(screen.getByRole('alert')).toHaveTextContent(TEXT);
    await expectNoAxeViolations(view.container);
  });
});

// The own account's trust state, which Privacy does not list, heads the section about this account's devices too.
describe('the own account’s identity', () => {
  const UNLISTED = 'A device that is not on your device list is using your account in some conversations. Sending is paused there. Remove it in Devices.';
  it('heads the section with a danger banner while the account slice reports it', async () => {
    const { fake, view } = setup();
    expect(screen.queryByText(UNLISTED)).toBeNull();
    act(() => fake.set('account', account({ ownTrust: 'unlisted' })));
    expect(screen.getByRole('alert')).toHaveTextContent(UNLISTED);
    await expectNoAxeViolations(view.container);
  });
});

describe('this browser', () => {
  it('signs out and removes itself with the key, and leaves the reload to the App', async () => {
    const { fake, user, view } = setup();
    await user.click(within(rowOf(OWN)).getByRole('button', { name: 'Sign out' }));
    const dialog = screen.getByRole('dialog', { name: 'Sign out and remove this browser?' });
    expect(within(dialog).getByText('This browser is removed from your account and its data here is cleared. You can sign in again later with your password and recovery key. Enter the key to confirm.')).toBeInTheDocument();
    await expectNoAxeViolations(view.container);
    await typeKey(user);
    await user.click(within(dialog).getByRole('button', { name: 'Sign out' }));
    expect(fake.callsOf('signOutRevoke')).toEqual([{ m: 'signOutRevoke', recoveryKey: GROUPED }]);
    expect(fake.callsOf('revokeDevice')).toEqual([]);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(window.location.pathname).toBe('/settings/devices');
  });
});

// Signal task 10 fix round 1 (Jonas): exactly the prototype's rows; no count, refresh, sign-out or forget rows.
describe('the settings grammar', () => {
  it('draws only the device rows, each with its icon tile', () => {
    const { view } = setup();
    expect(view.container.querySelectorAll('.d-settings-row')).toHaveLength(0);
    expect(view.container.querySelectorAll('.d-device-row__icon')).toHaveLength(4);
    expect(screen.queryByRole('button', { name: 'Forget this browser' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Refresh' })).toBeNull();
  });
});
