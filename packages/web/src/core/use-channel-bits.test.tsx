import { describe, expect, it } from 'vitest';
import { renderHook } from '@testing-library/react';
import type { ReactNode } from 'react';
import { PERM, type CommunityAccess, type RoleSummary } from '@dilla/client-core';
import { CoreProvider } from './context.tsx';
import { useChannelBits } from './use-channel-bits.ts';
import { FakeClient } from '../test/fake-client.ts';
import { account, ME } from '../test/fixtures.ts';

const SERVER = 'a1'.repeat(16);
const CH = 'c1'.repeat(16);
const EVERYONE = 'e0'.repeat(16);
const OWNER = 'ee'.repeat(16);
const role = (id: string, position: number, allow: number): RoleSummary =>
  ({ id, name: id.slice(0, 2), color: 0, position, hoist: false, mentionable: false, allow, deny: 0, members: 0 });
const ACCESS: CommunityAccess = { top: 0, owner: false, manageCommunity: false, manageChannels: false, kickMembers: false, banMembers: false,
  createInvite: false, manageRoles: false, loaded: true, everyoneId: EVERYONE, overwritesLoaded: true };

function filled(): FakeClient {
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('communities', [{ id: SERVER, name: 'Midgard', owner: OWNER }]);
  fake.set(`members:${SERVER}`, [{ userId: ME.id, username: 'ada', display: '', kind: 0, roleIds: [] }]);
  fake.set(`roles:${SERVER}`, [role(EVERYONE, 0, PERM.viewChannel | PERM.sendMessages)]);
  fake.set(`permissions:${SERVER}`, ACCESS);
  fake.set(`overwrites:${SERVER}`, {});
  return fake;
}
const bits = (fake: FakeClient, community: string | null = SERVER, channel: string | null = CH) =>
  renderHook(() => useChannelBits(community, channel), { wrapper: ({ children }: { children: ReactNode }) => <CoreProvider client={fake}>{children}</CoreProvider> }).result.current;

describe('useChannelBits', () => {
  it('resolves my bits in the channel from the roles, members, overwrites and access', () => {
    expect(bits(filled())).toBe(PERM.viewChannel | PERM.sendMessages);
  });
  it('applies the channel’s overwrites', () => {
    const fake = filled();
    fake.set(`overwrites:${SERVER}`, { [CH]: [{ kind: 0, targetId: EVERYONE, allow: 0, deny: PERM.sendMessages }] });
    expect(bits(fake)).toBe(PERM.viewChannel);
  });
  it('is null for a DM, and while any slice is absent', () => {
    expect(bits(filled(), null, null)).toBeNull();
    for (const gone of [`members:${SERVER}`, `roles:${SERVER}`, `overwrites:${SERVER}`, `permissions:${SERVER}`, 'communities', 'account'] as const) {
      const fake = filled();
      fake.set(gone, undefined as never);
      expect(bits(fake), gone).toBeNull();
    }
  });
  it('is null while the access or the overwrites are unread or stale', () => {
    const stale = filled();
    stale.set(`permissions:${SERVER}`, { ...ACCESS, overwritesLoaded: false });
    expect(bits(stale)).toBeNull();
    const unloaded = filled();
    unloaded.set(`permissions:${SERVER}`, { ...ACCESS, loaded: false, everyoneId: '' });
    expect(bits(unloaded)).toBeNull();
  });
  it('is null when I am not in the member list, and everything for the owner', () => {
    const outsider = filled();
    outsider.set(`members:${SERVER}`, []);
    expect(bits(outsider)).toBeNull();
    const owner = filled();
    owner.set('communities', [{ id: SERVER, name: 'Midgard', owner: ME.id }]);
    expect(bits(owner)).toBe(0x3ffffff);
  });
});
