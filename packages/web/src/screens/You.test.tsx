import { describe, it, expect, vi } from 'vitest';
import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { DeviceSummary } from '@dilla/client-core';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient } from '../test/fake-client.ts';
import { account, ME } from '../test/fixtures.ts';
import { expectNoAxeViolations } from '../test/setup.ts';
import { t } from '../strings/index.ts';
import { You } from './You.tsx';

const A = 'a1'.repeat(16);
const dev = (id: string, over: Partial<DeviceSummary> = {}): DeviceSummary =>
  ({ id, tier: 1, signerTier: 1, lastSeen: 1, revokedAt: null, listed: true, own: false, label: null, ...over });

function setup(onOpenSettings = vi.fn(), connection: { status: 'online' | 'offline' | 'connecting'; generation: string; pingMs?: number } = { status: 'online', generation: '7', pingMs: 24 }) {
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('connection', connection);
  fake.set('devices', [dev('cc'.repeat(16), { own: true }), dev('dd'.repeat(16))]);
  fake.set('communities', [{ id: A, name: 'Midgard', owner: 'ee'.repeat(16) }]);
  fake.set(`members:${A}`, [{ userId: ME.id, username: 'ada', display: 'Ada L', kind: 0, roleIds: [] }]);
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><You onOpenSettings={onOpenSettings} /></CoreProvider></div>);
  return { fake, user, view, onOpenSettings };
}

// Task 11 (Jonas, Q3): the prototype's You: the account, one box with the connection and this device, and the rows Settings
// and Sign out. The full facts stay in Settings → Diagnostics.
describe('You', () => {
  it('has a heading for the tab, the account and its line ("@handle · status")', () => {
    setup();
    expect(screen.getByRole('heading', { level: 2, name: t('you.title') })).toHaveClass('d-sr-only');
    expect(screen.getByText('Ada L')).toBeInTheDocument();
    expect(screen.getByText(t('you.line', { handle: 'ada', status: t('presence.online') }))).toBeInTheDocument();
  });

  it('falls back to the username when no server knows a display name', () => {
    const { fake } = setup();
    act(() => fake.set(`members:${A}`, []));
    expect(screen.getAllByText('ada')).toHaveLength(1);
  });

  it('says where it is connected, with the gateway ping, and names this device', () => {
    setup();
    expect(screen.getByText(t('you.connected', { host: 'dilla.test', ms: 24 }))).toBeInTheDocument();
    expect(document.querySelector('.dw-you__facts')).toHaveTextContent(/device /);
  });

  it('says it is reconnecting while offline', () => {
    setup(vi.fn(), { status: 'offline', generation: '7' });
    expect(screen.getByText(t('you.offline', { host: 'dilla.test' }))).toBeInTheDocument();
  });

  it('asks for the device list once on mount', () => {
    const { fake } = setup();
    expect(fake.callsOf('refreshDevices')).toEqual([{ m: 'refreshDevices' }]);
  });

  it('opens Settings from its row', async () => {
    const { user, onOpenSettings } = setup();
    await user.click(screen.getByRole('button', { name: t('you.settings') }));
    expect(onOpenSettings).toHaveBeenCalledWith(null);
  });

  // Wave B (Jonas): the prototype's You has no status row; the status menu is the sidebar user panel's.
  it('has no status row and no status choices', () => {
    setup();
    expect(screen.queryByRole('button', { name: /status/i })).toBeNull();
    expect(screen.queryByRole('group', { name: /status/i })).toBeNull();
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual([t('you.settings'), t('workspace.signOut')]);
  });

  it('ends with Sign out, which asks for the recovery key, and has no axe violations', async () => {
    const { user, view } = setup();
    const signOut = screen.getByRole('button', { name: t('workspace.signOut') });
    expect(signOut).toHaveAttribute('data-danger', 'true');
    await expectNoAxeViolations(view.container);
    await user.click(signOut);
    expect(await screen.findByRole('dialog', { name: t('devices.signOutTitle') })).toBeInTheDocument();
  });
});
