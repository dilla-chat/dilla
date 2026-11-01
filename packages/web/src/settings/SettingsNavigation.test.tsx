import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient } from '../test/fake-client.ts';
import { SettingsNavigation } from './SettingsNavigation.tsx';

const SERVER = 'a1'.repeat(16);

function setup(access?: Record<string, boolean>, communityId: string | null = SERVER) {
  const fake = new FakeClient();
  fake.set('communities', [{ id: SERVER, name: 'Midgard Crew', owner: 'ee'.repeat(16) }]);
  if (access !== undefined) fake.set(`permissions:${SERVER}`, access as never);
  const onUser = vi.fn(); const onServer = vi.fn();
  const view = render(<CoreProvider client={fake}>
    <SettingsNavigation active={{ scope: 'user', id: 'profile' }} communityId={communityId} onUser={onUser} onServer={onServer} />
  </CoreProvider>);
  return { fake, onUser, onServer, view };
}

// Signal task 10 fix round 1 (Jonas): one settings workspace, its nav the group "You" and the selected server's name.
describe('the one settings workspace nav', () => {
  it('lists "You" first, then the selected server under its name with only the sections the person may use', () => {
    const { onUser, onServer } = setup({ loaded: true, manageCommunity: true });
    const nav = screen.getByRole('navigation', { name: 'Settings sections' });
    expect(within(nav).getAllByRole('heading', { level: 2 }).map(h => h.textContent)).toEqual(['You', 'Midgard Crew']);
    expect(within(nav).getByRole('button', { name: 'Profile' })).toHaveAttribute('aria-current', 'page');
    expect(within(nav).getByRole('button', { name: 'Overview' })).toBeInTheDocument();
    expect(within(nav).queryByRole('button', { name: 'Roles' })).toBeNull();
    fireEvent.click(within(nav).getByRole('button', { name: 'Voice' }));
    expect(onUser).toHaveBeenCalledWith('voice');
    fireEvent.click(within(nav).getByRole('button', { name: 'Overview' }));
    expect(onServer).toHaveBeenCalledWith('overview');
  });
  it('has no server group while the permissions load, and drops sections on permission loss', () => {
    const { fake } = setup(undefined);
    expect(screen.queryByRole('heading', { name: 'Midgard Crew' })).toBeNull();
    act(() => fake.set(`permissions:${SERVER}`, { loaded: true, manageCommunity: true } as never));
    expect(screen.getByRole('button', { name: 'Music' })).toBeInTheDocument();
    act(() => fake.set(`permissions:${SERVER}`, { loaded: true } as never));
    expect(screen.queryByRole('button', { name: 'Music' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Members' })).toBeInTheDocument();
  });
  it('has no search field (settings search is the palette’s) and no server group with no server selected', () => {
    setup({ loaded: true }, null);
    expect(screen.queryByRole('searchbox')).toBeNull();
    expect(screen.getAllByRole('heading', { level: 2 }).map(h => h.textContent)).toEqual(['You']);
  });
});
