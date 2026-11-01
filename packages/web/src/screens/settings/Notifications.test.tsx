import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ChannelSummary, DmSummary } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { account } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { Notifications } from './Notifications.tsx';

const A = 'a1'.repeat(16);
const GEN = 'c3'.repeat(16);
const RAND = 'd4'.repeat(16);
const VOICE = 'e5'.repeat(16);
const DM = '9a'.repeat(16);
const ch = (id: string, name: string, over: Partial<ChannelSummary> = {}): ChannelSummary =>
  ({ id, communityId: A, kind: 0, mode: 0, name, topic: '', parentId: null, position: 0, slowmode: 0, announcement: false, group: 'active', ...over });
const DMS: DmSummary[] = [{ id: DM, kind: 3, members: ['bb'.repeat(16), '28'.repeat(16)], name: 'bob', group: 'active' }];

class FakeNotification {
  static permission: NotificationPermission = 'default';
  static asked = 0;
  static requestPermission(): Promise<NotificationPermission> {
    FakeNotification.asked += 1;
    FakeNotification.permission = 'granted';
    return Promise.resolve('granted');
  }
}

afterEach(() => { vi.unstubAllGlobals(); });

function setup(opts: { settings?: Record<string, string>; permission?: NotificationPermission | null; server?: string | null } = {}) {
  if (opts.permission !== null) {
    FakeNotification.permission = opts.permission ?? 'default';
    FakeNotification.asked = 0;
    vi.stubGlobal('Notification', FakeNotification);
  }
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('communities', [{ id: A, name: 'Midgard', owner: 'ee'.repeat(16) }]);
  fake.set(`channels:${A}`, [ch(RAND, 'random', { position: 2 }), ch(VOICE, 'lounge', { kind: 1, position: 0 }), ch(GEN, 'general', { position: 1 })]);
  fake.set('dms', DMS);
  fake.set('settings', opts.settings ?? {});
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><Notifications communityId={opts.server === undefined ? A : opts.server} /></CoreProvider></div>);
  return { fake, user, view };
}
const group = (name: string) => screen.getByRole('group', { name });

const writes = (fake: FakeClient) => fake.callsOf('setSetting').map(c => [c.key, c.value]);

// Signal task 10 fix round 1 (Jonas): exactly the prototype's four rows (README §5.9).
describe('the prototype rows', () => {
  it('draws the server mode, @everyone, desktop notifications and the sounds as settings rows, and passes axe', async () => {
    const { view } = setup({ permission: 'granted' });
    expect(screen.getByRole('heading', { level: 2, name: 'Notifications' })).toBeInTheDocument();
    expect(screen.getByText('What reaches you, and how.')).toBeInTheDocument();
    expect(Array.from(view.container.querySelectorAll('.d-settings-row__label'), l => l.textContent))
      .toEqual(['Midgard', '@everyone and @here', 'Desktop notifications', 'Join, leave and ring sounds']);
    expect(view.container.querySelectorAll('.d-settings-row__icon')).toHaveLength(4);
    await expectNoAxeViolations(view.container);
  });
  it('drops the server rows with no server selected', () => {
    const { view } = setup({ server: null });
    expect(Array.from(view.container.querySelectorAll('.d-settings-row__label'), l => l.textContent)).toEqual(['Desktop notifications', 'Join, leave and ring sounds']);
  });
});

describe('the server rows', () => {
  it('shows the device default until the server has a mode, then writes notify.community.<hex>', async () => {
    const { fake, user } = setup({ settings: { 'notify.default': 'everything' } });
    const mode = screen.getByRole('radiogroup', { name: 'Notify me for Midgard' });
    expect(within(mode).getByRole('radio', { name: 'All' })).toHaveAttribute('aria-checked', 'true');
    await user.click(within(mode).getByRole('radio', { name: 'Nothing' }));
    expect(writes(fake)).toEqual([[`notify.community.${A}`, 'nothing']]);
  });
  it('turns @everyone and @here off with everyone.community.<hex> = 1, and on by deleting it', async () => {
    const { fake, user } = setup({ settings: { [`everyone.community.${A}`]: '1' } });
    const sw = within(group('@everyone and @here')).getByRole('switch');
    expect(sw).toHaveAttribute('aria-checked', 'false');
    await user.click(sw);
    expect(writes(fake)).toEqual([[`everyone.community.${A}`, null]]);
  });
});

describe('desktop notifications', () => {
  it('asks for the permission only from the switch, never on render', async () => {
    const { fake, user } = setup();
    expect(FakeNotification.asked).toBe(0);
    const sw = within(group('Desktop notifications')).getByRole('switch');
    expect(sw).toHaveAttribute('aria-checked', 'false');
    await user.click(sw);
    expect(FakeNotification.asked).toBe(1);
    expect(writes(fake)).toEqual([['notify.desktop', null]]);
  });
  it('turns off on this device with notify.desktop = off', async () => {
    const { fake, user } = setup({ permission: 'granted' });
    const sw = within(group('Desktop notifications')).getByRole('switch');
    expect(sw).toHaveAttribute('aria-checked', 'true');
    await user.click(sw);
    expect(writes(fake)).toEqual([['notify.desktop', 'off']]);
  });
  it('says why when the browser blocks them or has none', () => {
    setup({ permission: 'denied' });
    expect(within(group('Desktop notifications')).getByText('Blocked in the browser. Allow notifications for this site in the browser’s settings.')).toBeInTheDocument();
    expect(within(group('Desktop notifications')).getByRole('switch')).toHaveAttribute('aria-disabled', 'true');
  });
});

describe('the sounds', () => {
  it('one switch writes both the join and leave sounds and the ring', async () => {
    const { fake, user } = setup({ settings: { 'sound.ring': 'off' } });
    const sw = within(group('Join, leave and ring sounds')).getByRole('switch');
    expect(sw).toHaveAttribute('aria-checked', 'false');
    await user.click(sw);
    expect(writes(fake)).toEqual([['sound.voice', 'on'], ['sound.ring', 'on']]);
  });
  it('shows a refused write by its code', async () => {
    const { fake, user } = setup();
    fake.handler = () => Promise.reject(refusal({ code: 'E_STORE' }));
    await user.click(within(group('Join, leave and ring sounds')).getByRole('switch'));
    expect(await screen.findByText('That did not work (E_STORE). Try again.')).toBeInTheDocument();
  });
});
