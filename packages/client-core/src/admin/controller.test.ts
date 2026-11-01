// The admin and moderation commands (web3.4 C3) over the whole worker: the Controller with the routes doubled.
import { describe, expect, it, vi } from 'vitest';
import { Op } from '../gateway/frames';
import type { GatewayEvent } from '../gateway/gateway';
import { toHex } from '../hex';
import type { BanRow, InviteRow, RoleRow } from '../http/routes';
import type { BanSummary, ChannelSummary, CommunityAccess, InviteSummary, RemovalNotice, RoleSummary } from '../state/types';
import { CHANNEL, COMMUNITY, LOCK, STRANGER, USER, refusal, toReady, world, type World } from '../worker/world';

const CID = toHex(COMMUNITY);
const STRANGER_HEX = toHex(STRANGER);
let next = 5000;
/** Sends one command and returns its ret once it has answered. */
async function run(w: World, command: Record<string, unknown>): Promise<unknown> {
  next += 1;
  const id = next;
  w.call(id, command);
  await vi.waitFor(() => expect(w.ret(id)).toBeDefined());
  return w.ret(id);
}
/** Whether a slice that satisfies `pred` was posted before the answer of the last command (the double does not follow a
 *  patch, so the list read that comes after it is not what these tests look at). */
function shownBeforeRet<T>(w: World, name: string, pred: (v: T) => boolean): boolean {
  const ret = w.posted.findIndex((m) => m.t === 'ret' && m.id === next);
  return w.posted.slice(0, ret).some((m) => m.t === 'slice' && m.name === name && pred(m.value as T));
}
const id16 = (n: number): Uint8Array => new Uint8Array(16).fill(n);
const channelRow = (n: number, position: number, over: Partial<Awaited<ReturnType<World['routes']['listChannels']>>[number]> = {}) => ({
  id: id16(n), kind: 0, mode: 0, visibility: 0, parentId: null as Uint8Array | null, name: `c${String(n)}`, topic: '', position,
  slowmode: 0, seq: 1n, textGroupId: null as Uint8Array | null, ...over,
});

describe('AdminController: input checks', () => {
  const channelId = toHex(CHANNEL);
  const bad: [string, Record<string, unknown>][] = [
    ['blank server name', { m: 'renameCommunity', communityId: CID, name: '   ' }],
    ['too long server name', { m: 'renameCommunity', communityId: CID, name: 'x'.repeat(1000) }],
    ['blank category name', { m: 'createCategory', communityId: CID, name: '' }],
    ['too long category name', { m: 'createCategory', communityId: CID, name: 'x'.repeat(1000) }],
    ['blank channel name', { m: 'updateChannel', channelId, name: ' ', topic: null, parent: 'keep', slowmode: null }],
    ['too long channel name', { m: 'updateChannel', channelId, name: 'x'.repeat(1000), topic: null, parent: 'keep', slowmode: null }],
    ['too long topic', { m: 'updateChannel', channelId, name: null, topic: 'x'.repeat(1025), parent: 'keep', slowmode: null }],
    ['a parent that is not keep, top or an id', { m: 'updateChannel', channelId, name: null, topic: null, parent: 'bogus', slowmode: null }],
    ['an upper-case parent id', { m: 'updateChannel', channelId, name: null, topic: null, parent: 'A'.repeat(32), slowmode: null }],
    ['a slow mode that is not a step', { m: 'updateChannel', channelId, name: null, topic: null, parent: 'keep', slowmode: 7 }],
    ['a slow mode that is a string', { m: 'updateChannel', channelId, name: null, topic: null, parent: 'keep', slowmode: '10' }],
    ['an update that changes nothing', { m: 'updateChannel', channelId, name: null, topic: null, parent: 'keep', slowmode: null }],
    ['a direction that is not up or down', { m: 'moveChannel', channelId, direction: 'left' }],
    ['a ban shorter than an hour', { m: 'banMember', communityId: CID, userId: STRANGER_HEX, reason: '', seconds: 3599 }],
    ['a ban longer than a century', { m: 'banMember', communityId: CID, userId: STRANGER_HEX, reason: '', seconds: 3_153_600_001 }],
    ['a ban of a fraction of seconds', { m: 'banMember', communityId: CID, userId: STRANGER_HEX, reason: '', seconds: 7200.5 }],
    ['a ban of NaN seconds', { m: 'banMember', communityId: CID, userId: STRANGER_HEX, reason: '', seconds: Number.NaN }],
    ['a ban reason over 512 bytes', { m: 'banMember', communityId: CID, userId: STRANGER_HEX, reason: 'é'.repeat(257), seconds: null }],
    ['a user id that is not hex', { m: 'kickMember', communityId: CID, userId: 'nope' }],
    ['an invite id that is not hex', { m: 'revokeInvite', communityId: CID, inviteId: 'nope' }],
  ];
  it.each(bad)('refuses %s with E_BAD_INPUT before anything is sent', async (_label, command) => {
    const w = world();
    await toReady(w);
    const before = w.routes.kickMember.mock.calls.length + w.routes.putBan.mock.calls.length + w.routes.patchChannel.mock.calls.length
      + w.routes.patchCommunity.mock.calls.length + w.routes.createCategory.mock.calls.length + w.routes.revokeInvite.mock.calls.length;
    expect(await run(w, command)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
    const after = w.routes.kickMember.mock.calls.length + w.routes.putBan.mock.calls.length + w.routes.patchChannel.mock.calls.length
      + w.routes.patchCommunity.mock.calls.length + w.routes.createCategory.mock.calls.length + w.routes.revokeInvite.mock.calls.length;
    expect(after).toBe(before);
  });

  it('accepts a permanent ban, an hour, a century and a reason of exactly 512 bytes', async () => {
    const w = world();
    await toReady(w);
    for (const seconds of [null, 3600, 3_153_600_000]) {
      expect(await run(w, { m: 'banMember', communityId: CID, userId: STRANGER_HEX, reason: 'x'.repeat(512), seconds })).toMatchObject({ ok: true });
    }
  });

  it('refuses an admin command before ready', async () => {
    const w = world();
    expect(await run(w, { m: 'kickMember', communityId: CID, userId: STRANGER_HEX })).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } });
  });
});

describe('AdminController: access', () => {
  it('publishes the permissions and roles of a server after its lists load', async () => {
    const w = world();
    w.routes.listRoles.mockResolvedValue([
      { id: id16(0x31), name: 'mods', color: 0xff0000, position: 5, allow: 0, deny: 0, hoist: true, mentionable: false },
    ] satisfies RoleRow[]);
    await toReady(w);
    await vi.waitFor(() => expect(w.last<CommunityAccess>(`permissions:${CID}`)).toBeDefined());
    expect(w.last<CommunityAccess>(`permissions:${CID}`)).toEqual({
      top: 2 ** 31, owner: true, manageCommunity: true, viewAuditLog: true, manageChannels: true, kickMembers: true, banMembers: true, moderateMembers: true, createInvite: true,
      manageRoles: true, manageMessages: true, loaded: true, everyoneId: '', overwritesLoaded: true,
    });
    expect(w.last<RoleSummary[]>(`roles:${CID}`)).toEqual([
      { id: toHex(id16(0x31)), name: 'mods', color: 0xff0000, position: 5, hoist: true, mentionable: false, allow: 0, deny: 0, members: 0 },
    ]);
  });

  it('reads a non-owner from the resolved bits and rank', async () => {
    const w = world();
    w.routes.listCommunities.mockResolvedValue([{ id: COMMUNITY, name: 'friends', owner: STRANGER, policyVersion: 1n }]);
    w.routes.getPermissions.mockResolvedValue({ bits: (1n << 12n) | 1n, top: 7 });
    await toReady(w);
    await vi.waitFor(() => expect(w.last<CommunityAccess>(`permissions:${CID}`)).toBeDefined());
    expect(w.last<CommunityAccess>(`permissions:${CID}`)).toMatchObject({ top: 7, owner: false, kickMembers: true, banMembers: false });
  });

  it('a rejected permissions read leaves the slices absent and selectCommunity still resolves', async () => {
    const w = world();
    w.routes.getPermissions.mockRejectedValue(refusal(500, 'E_INTERNAL'));
    await toReady(w);
    expect(await run(w, { m: 'selectCommunity', communityId: CID })).toMatchObject({ ok: true, value: null });
    await vi.waitFor(() => expect(w.routes.getPermissions).toHaveBeenCalled());
    expect(w.last<CommunityAccess>(`permissions:${CID}`)).toBeUndefined();
    expect(w.last<RoleSummary[]>(`roles:${CID}`)).toBeUndefined();
  });

  it('does not wait for the access read: select answers while it is still pending', async () => {
    const w = world();
    await toReady(w);
    w.routes.getPermissions.mockImplementation(() => new Promise(() => {}));
    expect(await run(w, { m: 'selectCommunity', communityId: CID })).toMatchObject({ ok: true });
  });

  it('does not push the access reads into the ordered call log of the older suites', async () => {
    const w = world();
    await toReady(w);
    await vi.waitFor(() => expect(w.last<CommunityAccess>(`permissions:${CID}`)).toBeDefined());
    expect(w.calls.filter((c) => /ermission|oles/.test(c))).toEqual([]);
  });
});

describe('AdminController: moderation', () => {
  it('kick sends the kick, then reads the members and the permissions again', async () => {
    const w = world();
    await toReady(w);
    await vi.waitFor(() => expect(w.routes.getPermissions).toHaveBeenCalled());
    const members = w.routes.listMembers.mock.calls.length;
    const perms = w.routes.getPermissions.mock.calls.length;
    expect(await run(w, { m: 'kickMember', communityId: CID, userId: STRANGER_HEX })).toMatchObject({ ok: true, value: null });
    expect(w.routes.kickMember).toHaveBeenCalledWith(COMMUNITY, STRANGER);
    expect(w.routes.listMembers.mock.calls.length).toBe(members + 1);
    expect(w.routes.getPermissions.mock.calls.length).toBe(perms + 1);
    const kick = w.routes.kickMember.mock.invocationCallOrder[0];
    expect(w.routes.listMembers.mock.invocationCallOrder.at(-1)).toBeGreaterThan(kick);
    expect(w.routes.getPermissions.mock.invocationCallOrder.at(-1)).toBeGreaterThan(kick);
  });

  it('a 403 on kick reads the permissions again and the page still gets the refusal', async () => {
    const w = world();
    await toReady(w);
    await vi.waitFor(() => expect(w.routes.getPermissions).toHaveBeenCalled());
    const perms = w.routes.getPermissions.mock.calls.length;
    w.routes.kickMember.mockRejectedValueOnce(refusal(403, 'E_FORBIDDEN'));
    expect(await run(w, { m: 'kickMember', communityId: CID, userId: STRANGER_HEX })).toMatchObject({
      ok: false, error: { code: 'E_FORBIDDEN', status: 403 },
    });
    await vi.waitFor(() => expect(w.routes.getPermissions.mock.calls.length).toBe(perms + 1));
  });

  it('a 404 on kick (a retried first attempt that went through) surfaces and still reads the members', async () => {
    const w = world();
    await toReady(w);
    const members = w.routes.listMembers.mock.calls.length;
    w.routes.kickMember.mockRejectedValueOnce(refusal(404, 'E_NOT_FOUND'));
    expect(await run(w, { m: 'kickMember', communityId: CID, userId: STRANGER_HEX })).toMatchObject({
      ok: false, error: { code: 'E_NOT_FOUND', status: 404 },
    });
    await vi.waitFor(() => expect(w.routes.listMembers.mock.calls.length).toBe(members + 1));
  });

  it('a ban of 86400 seconds expires floor(now/1000)+86400; a permanent one sends null', async () => {
    const w = world();
    await toReady(w);
    expect(await run(w, { m: 'banMember', communityId: CID, userId: STRANGER_HEX, reason: 'spam', seconds: 86_400 })).toMatchObject({ ok: true });
    expect(w.routes.putBan).toHaveBeenLastCalledWith(COMMUNITY, STRANGER, { reason: 'spam', expires: 1_700_000_000n + 86_400n });
    expect(await run(w, { m: 'banMember', communityId: CID, userId: STRANGER_HEX, reason: '', seconds: null })).toMatchObject({ ok: true });
    expect(w.routes.putBan).toHaveBeenLastCalledWith(COMMUNITY, STRANGER, { reason: '', expires: null });
  });

  it('a ban reads the members and the permissions again', async () => {
    const w = world();
    await toReady(w);
    const members = w.routes.listMembers.mock.calls.length;
    const perms = w.routes.getPermissions.mock.calls.length;
    await run(w, { m: 'banMember', communityId: CID, userId: STRANGER_HEX, reason: '', seconds: null });
    expect(w.routes.listMembers.mock.calls.length).toBe(members + 1);
    expect(w.routes.getPermissions.mock.calls.length).toBe(perms + 1);
  });

  it('unban treats a 404 as done and reads the bans again', async () => {
    const w = world();
    await toReady(w);
    w.routes.deleteBan.mockRejectedValueOnce(refusal(404, 'E_NOT_FOUND'));
    const bans = w.routes.listBans.mock.calls.length;
    expect(await run(w, { m: 'unbanMember', communityId: CID, userId: STRANGER_HEX })).toMatchObject({ ok: true, value: null });
    expect(w.routes.deleteBan).toHaveBeenCalledWith(COMMUNITY, STRANGER);
    expect(w.routes.listBans.mock.calls.length).toBe(bans + 1);
  });

  it('unban lets any other refusal through', async () => {
    const w = world();
    await toReady(w);
    w.routes.deleteBan.mockRejectedValueOnce(refusal(403, 'E_FORBIDDEN'));
    expect(await run(w, { m: 'unbanMember', communityId: CID, userId: STRANGER_HEX })).toMatchObject({ ok: false, error: { code: 'E_FORBIDDEN' } });
  });

  it('revokeInvite treats a 404 as done and reads the invites again', async () => {
    const w = world();
    await toReady(w);
    w.routes.revokeInvite.mockRejectedValueOnce(refusal(404, 'E_NOT_FOUND'));
    const invites = w.routes.listInvites.mock.calls.length;
    const inviteId = toHex(id16(0x71));
    expect(await run(w, { m: 'revokeInvite', communityId: CID, inviteId })).toMatchObject({ ok: true, value: null });
    expect(w.routes.revokeInvite).toHaveBeenCalledWith(id16(0x71));
    expect(w.routes.listInvites.mock.calls.length).toBe(invites + 1);
  });

  it('transferOwnership reads the members and the permissions again', async () => {
    const w = world();
    await toReady(w);
    const members = w.routes.listMembers.mock.calls.length;
    const perms = w.routes.getPermissions.mock.calls.length;
    expect(await run(w, { m: 'transferOwnership', communityId: CID, userId: STRANGER_HEX })).toMatchObject({ ok: true, value: null });
    expect(w.routes.transferOwnership).toHaveBeenCalledWith(COMMUNITY, STRANGER);
    expect(w.routes.listMembers.mock.calls.length).toBe(members + 1);
    expect(w.routes.getPermissions.mock.calls.length).toBe(perms + 1);
  });

  it('loadBans publishes each ban with the display name, else the username, else empty', async () => {
    const w = world();
    await toReady(w);
    w.routes.listBans.mockResolvedValue([
      { userId: id16(1), reason: 'spam', byUser: USER, created: 1_699_000_000n, expires: 1_701_000_000n, username: 'one', display: 'One' },
      { userId: id16(2), reason: '', byUser: USER, created: 1_699_000_001n, expires: null, username: 'two', display: '' },
      { userId: id16(3), reason: '', byUser: USER, created: 1_699_000_002n, expires: null, username: '', display: '' },
    ] satisfies BanRow[]);
    expect(await run(w, { m: 'loadBans', communityId: CID })).toMatchObject({ ok: true, value: null });
    expect(w.last<BanSummary[]>(`bans:${CID}`)).toEqual([
      { userId: toHex(id16(1)), name: 'One', reason: 'spam', byUser: toHex(USER), created: 1_699_000_000, expires: 1_701_000_000 },
      { userId: toHex(id16(2)), name: 'two', reason: '', byUser: toHex(USER), created: 1_699_000_001, expires: null },
      { userId: toHex(id16(3)), name: '', reason: '', byUser: toHex(USER), created: 1_699_000_002, expires: null },
    ]);
  });

  it('loadInvites keeps only invites that are not revoked, not expired and not used up', async () => {
    const w = world();
    await toReady(w);
    const now = 1_700_000_000n;
    const row = (n: number, over: Partial<InviteRow>): InviteRow => ({
      id: id16(n), communityId: COMMUNITY, createdBy: USER, maxUses: 5, uses: 1, created: now - 100n, expires: now + 100n, revokedAt: null, ...over,
    });
    w.routes.listInvites.mockResolvedValue([
      row(1, {}), row(2, { revokedAt: now - 5n }), row(3, { expires: now }), row(4, { expires: now - 1n }),
      row(5, { uses: 5 }), row(6, { uses: 6 }), row(7, { uses: 4 }),
    ]);
    expect(await run(w, { m: 'loadInvites', communityId: CID })).toMatchObject({ ok: true, value: null });
    expect(w.last<InviteSummary[]>(`invites:${CID}`)?.map((i) => i.id)).toEqual([toHex(id16(1)), toHex(id16(7))]);
    expect(w.last<InviteSummary[]>(`invites:${CID}`)?.[0]).toEqual({
      id: toHex(id16(1)), createdBy: toHex(USER), maxUses: 5, uses: 1, created: 1_699_999_900, expires: 1_700_000_100,
    });
  });
});

describe('AdminController: server and channel edits', () => {
  it('renameCommunity patches the name and the list shows it before the answer', async () => {
    const w = world();
    await toReady(w);
    expect(await run(w, { m: 'renameCommunity', communityId: CID, name: 'valhalla' })).toMatchObject({ ok: true, value: null });
    expect(w.routes.patchCommunity).toHaveBeenCalledWith(COMMUNITY, { name: 'valhalla' });
    expect(shownBeforeRet<{ id: string; name: string }[]>(w, 'communities', (l) => l.find((c) => c.id === CID)?.name === 'valhalla')).toBe(true);
  });

  it('deleteCommunity drops the server before it answers and leaves no removal notice', async () => {
    const w = world();
    await toReady(w);
    expect(await run(w, { m: 'deleteCommunity', communityId: CID })).toMatchObject({ ok: true, value: null });
    expect(w.routes.deleteCommunity).toHaveBeenCalledWith(COMMUNITY);
    const ret = w.posted.findIndex((m) => m.t === 'ret' && m.id === next);
    const dropped = w.posted.findIndex((m) => m.t === 'slice' && m.name === 'communities' && !(m.value as { id: string }[]).some((c) => c.id === CID));
    expect(dropped).toBeGreaterThan(-1);
    expect(dropped).toBeLessThan(ret);
    const emptied = w.posted.findIndex((m) => m.t === 'slice' && m.name === `channels:${CID}` && (m.value as unknown[]).length === 0);
    expect(emptied).toBeGreaterThan(-1);
    expect(emptied).toBeLessThan(ret);
    expect(w.posted.some((m) => m.t === 'slice' && m.name === 'removals' && (m.value as unknown[]).length > 0)).toBe(false);
  });

  it('leaveCommunity drops the server the same way', async () => {
    const w = world();
    await toReady(w);
    expect(await run(w, { m: 'leaveCommunity', communityId: CID })).toMatchObject({ ok: true, value: null });
    expect(w.routes.leaveCommunity).toHaveBeenCalledWith(COMMUNITY);
    expect(w.last<{ id: string }[]>('communities')?.some((c) => c.id === CID)).toBe(false);
    expect(w.last<unknown[]>(`members:${CID}`)).toEqual([]);
    expect(w.posted.some((m) => m.t === 'slice' && m.name === 'removals' && (m.value as unknown[]).length > 0)).toBe(false);
  });

  it('dropping a server clears its permissions, roles, bans and invites', async () => {
    const w = world();
    await toReady(w);
    await vi.waitFor(() => expect(w.last<CommunityAccess>(`permissions:${CID}`)).toBeDefined());
    await run(w, { m: 'loadBans', communityId: CID });
    await run(w, { m: 'loadInvites', communityId: CID });
    await run(w, { m: 'leaveCommunity', communityId: CID });
    expect(w.last<unknown[]>(`bans:${CID}`)).toEqual([]);
    expect(w.last<unknown[]>(`invites:${CID}`)).toEqual([]);
    expect(w.last<unknown[]>(`roles:${CID}`)).toEqual([]);
    expect(w.last<CommunityAccess>(`permissions:${CID}`)).toEqual({
      top: 0, owner: false, manageCommunity: false, viewAuditLog: false, manageChannels: false, kickMembers: false, banMembers: false, createInvite: false,
      moderateMembers: false, manageRoles: false, manageMessages: false, loaded: false, everyoneId: '', overwritesLoaded: false,
    });
  });

  it('moveChannel up on the last of three swaps positions with its neighbour and patches only those two', async () => {
    const w = world();
    w.routes.listChannels.mockResolvedValue([channelRow(0xa1, 0), channelRow(0xb1, 1), channelRow(0xc1, 2)]);
    await toReady(w);
    expect(await run(w, { m: 'moveChannel', channelId: toHex(id16(0xc1)), direction: 'up' })).toMatchObject({ ok: true, value: null });
    expect(w.routes.patchChannel.mock.calls).toEqual([[id16(0xc1), { position: 1 }], [id16(0xb1), { position: 2 }]]);
    expect(shownBeforeRet<ChannelSummary[]>(w, `channels:${CID}`,
      (l) => JSON.stringify(l.map((c) => [c.id.slice(0, 2), c.position])) === JSON.stringify([['a1', 0], ['b1', 2], ['c1', 1]]))).toBe(true);
  });

  it('moveChannel down swaps with the next one', async () => {
    const w = world();
    w.routes.listChannels.mockResolvedValue([channelRow(0xa1, 0), channelRow(0xb1, 1), channelRow(0xc1, 2)]);
    await toReady(w);
    await run(w, { m: 'moveChannel', channelId: toHex(id16(0xa1)), direction: 'down' });
    expect(w.routes.patchChannel.mock.calls).toEqual([[id16(0xa1), { position: 1 }], [id16(0xb1), { position: 0 }]]);
  });

  it('moveChannel up on the first channel, and down on the last, send nothing', async () => {
    const w = world();
    w.routes.listChannels.mockResolvedValue([channelRow(0xa1, 0), channelRow(0xb1, 1), channelRow(0xc1, 2)]);
    await toReady(w);
    expect(await run(w, { m: 'moveChannel', channelId: toHex(id16(0xa1)), direction: 'up' })).toMatchObject({ ok: true, value: null });
    expect(await run(w, { m: 'moveChannel', channelId: toHex(id16(0xc1)), direction: 'down' })).toMatchObject({ ok: true, value: null });
    expect(w.routes.patchChannel).not.toHaveBeenCalled();
  });

  it('moveChannel stays inside the channel\'s own category', async () => {
    const w = world();
    const cat = id16(0xd1);
    w.routes.listChannels.mockResolvedValue([
      channelRow(0xa1, 0), channelRow(0xd1, 1, { kind: 2 }), channelRow(0xb1, 2, { parentId: cat }), channelRow(0xc1, 3, { parentId: cat }),
    ]);
    await toReady(w);
    await run(w, { m: 'moveChannel', channelId: toHex(id16(0xb1)), direction: 'up' });
    expect(w.routes.patchChannel).not.toHaveBeenCalled();
    await run(w, { m: 'moveChannel', channelId: toHex(id16(0xc1)), direction: 'up' });
    expect(w.routes.patchChannel.mock.calls).toEqual([[id16(0xc1), { position: 2 }], [id16(0xb1), { position: 3 }]]);
  });

  it('moveChannel refuses a channel it does not know', async () => {
    const w = world();
    await toReady(w);
    expect(await run(w, { m: 'moveChannel', channelId: toHex(id16(0x99)), direction: 'up' })).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
  });

  it('updateChannel sends only what changed: the name, the topic and a parent of keep, top or an id', async () => {
    const w = world();
    await toReady(w);
    const channelId = toHex(CHANNEL);
    await run(w, { m: 'updateChannel', channelId, name: 'lobby', topic: null, parent: 'keep', slowmode: null });
    expect(w.routes.patchChannel).toHaveBeenLastCalledWith(CHANNEL, { name: 'lobby' });
    await run(w, { m: 'updateChannel', channelId, name: null, topic: '', parent: 'top', slowmode: null });
    expect(w.routes.patchChannel).toHaveBeenLastCalledWith(CHANNEL, { topic: '', parent: 'top' });
    await run(w, { m: 'updateChannel', channelId, name: null, topic: 'hello', parent: toHex(id16(0xd1)), slowmode: null });
    expect(w.routes.patchChannel).toHaveBeenLastCalledWith(CHANNEL, { topic: 'hello', parent: id16(0xd1) });
  });

  it('updateChannel sends a slow-mode step to the route, 0 included, and shows it after the reload', async () => {
    const w = world();
    await toReady(w);
    const channelId = toHex(CHANNEL);
    expect(await run(w, { m: 'updateChannel', channelId, name: null, topic: null, parent: 'keep', slowmode: 10 })).toMatchObject({ ok: true });
    expect(w.routes.patchChannel).toHaveBeenLastCalledWith(CHANNEL, { slowmode: 10 });
    expect(shownBeforeRet<ChannelSummary[]>(w, `channels:${CID}`, (l) => l.some((c) => c.id === channelId && c.slowmode === 10))).toBe(true);
    await run(w, { m: 'updateChannel', channelId, name: null, topic: null, parent: 'keep', slowmode: 0 });
    expect(w.routes.patchChannel).toHaveBeenLastCalledWith(CHANNEL, { slowmode: 0 });
    w.routes.listChannels.mockResolvedValue([channelRow(0x31, 0, { id: CHANNEL, slowmode: 30 })]);
    await run(w, { m: 'updateChannel', channelId, name: 'lobby', topic: null, parent: 'keep', slowmode: null });
    expect(w.routes.patchChannel).toHaveBeenLastCalledWith(CHANNEL, { name: 'lobby' });
    await vi.waitFor(() => expect(w.last<ChannelSummary[]>(`channels:${CID}`)?.find((c) => c.id === channelId)?.slowmode).toBe(30));
  });

  it('updateChannel shows the new name at once', async () => {
    const w = world();
    await toReady(w);
    await run(w, { m: 'updateChannel', channelId: toHex(CHANNEL), name: 'lobby', topic: 'hi', parent: 'keep', slowmode: null });
    expect(shownBeforeRet<ChannelSummary[]>(w, `channels:${CID}`,
      (l) => l.some((c) => c.id === toHex(CHANNEL) && c.name === 'lobby' && c.topic === 'hi'))).toBe(true);
  });

  it('deleteChannel deletes it and drops it from the list before it answers', async () => {
    const w = world();
    await toReady(w);
    expect(await run(w, { m: 'deleteChannel', channelId: toHex(CHANNEL) })).toMatchObject({ ok: true, value: null });
    expect(w.routes.deleteChannel).toHaveBeenCalledWith(CHANNEL);
    expect(shownBeforeRet<ChannelSummary[]>(w, `channels:${CID}`, (l) => !l.some((c) => c.id === toHex(CHANNEL)))).toBe(true);
  });

  it('createCategory answers the new id and lists it after the last channel', async () => {
    const w = world();
    await toReady(w);
    expect(await run(w, { m: 'createCategory', communityId: CID, name: 'voice' })).toEqual({
      t: 'ret', id: next, ok: true, value: { channelId: toHex(id16(0x90)) },
    });
    expect(w.routes.createCategory).toHaveBeenCalledWith(COMMUNITY, { name: 'voice', position: 1 });
    expect(shownBeforeRet<ChannelSummary[]>(w, `channels:${CID}`,
      (l) => l.some((c) => c.id === toHex(id16(0x90)) && c.kind === 2 && c.name === 'voice' && c.position === 1))).toBe(true);
  });

  it('createCategory skips the built-in AFK channel at 2^30 when it takes the next position (R42)', async () => {
    const w = world();
    const AFK = id16(0x61);
    w.routes.listChannels.mockResolvedValue([channelRow(0xc1, 0), channelRow(0x61, 2 ** 30, { kind: 1, name: 'AFK' })]);
    w.routes.getAFK.mockResolvedValue({ channelId: AFK, timeoutSeconds: 300, pending: false, away: [] });
    await toReady(w);
    await vi.waitFor(() => expect(w.last<{ channelId: string | null }>(`afk:${CID}`)?.channelId).toBe(toHex(AFK)));
    await vi.waitFor(() => expect(w.last<ChannelSummary[]>(`channels:${CID}`)?.some((c) => c.id === toHex(AFK))).toBe(true));
    expect(await run(w, { m: 'createCategory', communityId: CID, name: 'voice' })).toMatchObject({ ok: true });
    expect(w.routes.createCategory).toHaveBeenCalledWith(COMMUNITY, { name: 'voice', position: 1 });
  });

  it('createCategory skips the AFK position 2^30 before the afk: slice is read (R-AFK6)', async () => {
    const w = world();
    const AFK = id16(0x61);
    w.routes.listChannels.mockResolvedValue([channelRow(0xc1, 0), channelRow(0x61, 2 ** 30, { kind: 1, name: 'AFK' })]);
    // The AFK snapshot never answers: the slice stays unread while the create runs.
    w.routes.getAFK.mockReturnValue(new Promise(() => undefined));
    await toReady(w);
    await vi.waitFor(() => expect(w.last<ChannelSummary[]>(`channels:${CID}`)?.some((c) => c.id === toHex(AFK))).toBe(true));
    expect(w.last(`afk:${CID}`) ?? null).toBeNull();
    expect(await run(w, { m: 'createCategory', communityId: CID, name: 'voice' })).toMatchObject({ ok: true });
    expect(w.routes.createCategory).toHaveBeenCalledWith(COMMUNITY, { name: 'voice', position: 1 });
  });

  describe('R-AFK8: the AFK channel stays last in its run at 2^30, and no move ever touches its position', () => {
    const AFK = id16(0x61);
    const reserved = 2 ** 30;
    type Row = ReturnType<typeof channelRow>;
    /** A double that follows the patches and the channel creates, so the list read after each command is what the instance holds. */
    function instance(rows: Row[], afkRead = true): World {
      const w = world();
      w.routes.listChannels.mockImplementation(() => Promise.resolve(rows.map((r) => ({ ...r }))));
      w.routes.patchChannel.mockImplementation((id: Uint8Array, patch: { position?: number; parent?: Uint8Array | 'top' }) => {
        const r = rows.find((x) => toHex(x.id) === toHex(id));
        if (r !== undefined && patch.position !== undefined) r.position = patch.position;
        if (r !== undefined && patch.parent !== undefined) r.parentId = patch.parent === 'top' ? null : patch.parent;
        return Promise.resolve();
      });
      let made = 0;
      w.routes.createChannel.mockImplementation((_c: Uint8Array, c: { kind: 'text' | 'voice'; name: string; position: number }) => {
        made += 1;
        rows.push(channelRow(0xe0 + made, c.position, { kind: c.kind === 'voice' ? 1 : 0, name: c.name }));
        return Promise.resolve({ id: new Uint8Array(16).fill(0xe0 + made), mode: 0, visibility: 0 });
      });
      w.routes.createCategory.mockImplementation((_c: Uint8Array, c: { name: string; position: number }) => {
        made += 1;
        rows.push(channelRow(0xe0 + made, c.position, { kind: 2, name: c.name }));
        return Promise.resolve({ id: new Uint8Array(16).fill(0xe0 + made), mode: 0, visibility: 0 });
      });
      w.routes.getAFK.mockImplementation(() => (afkRead ? Promise.resolve({ channelId: AFK, timeoutSeconds: 300, pending: false, away: [] })
        : new Promise(() => undefined)));
      return w;
    }
    const shown = (rows: Row[]) => JSON.stringify(rows.map((r) => [r.name, r.position]));
    /** The channel named `name` stands after every other channel but AFK, and below AFK. */
    const ordinaryLast = (rows: Row[], name: string): boolean => {
      const ordinary = rows.filter((r) => toHex(r.id) !== toHex(AFK));
      const made = ordinary.find((r) => r.name === name);
      const afk = rows.find((r) => toHex(r.id) === toHex(AFK));
      return made !== undefined && ordinary.every((r) => r === made || r.position < made.position)
        && (afk === undefined || made.position < afk.position);
    };
    /** The page's list holds what the instance holds. */
    const settle = async (w: World, rows: Row[]): Promise<void> => {
      await vi.waitFor(() => {
        const listed = w.last<ChannelSummary[]>(`channels:${CID}`) ?? [];
        expect(rows.every((r) => listed.some((c) => c.id === toHex(r.id) && c.position === r.position))).toBe(true);
      });
    };
    /** Whether any PATCH sent AFK a position. */
    const afkPatched = (w: World): boolean => w.routes.patchChannel.mock.calls.some(([id, p]) => toHex(id) === toHex(AFK)
      && p.position !== undefined);
    const ready = async (w: World, rows: Row[], afkRead = true): Promise<void> => {
      await toReady(w);
      if (afkRead) await vi.waitFor(() => expect(w.last<{ channelId: string | null }>(`afk:${CID}`)?.channelId).toBe(toHex(AFK)));
      await settle(w, rows);
    };

    it('a sibling move in AFK\'s run is a plain swap that leaves AFK at 2^30, and the next channel is last among the ordinary ones', async () => {
      const rows = [channelRow(0xa1, 0), channelRow(0xb1, 1, { kind: 1, name: 'general' }), channelRow(0x61, reserved, { kind: 1, name: 'AFK' })];
      const w = instance(rows);
      await ready(w, rows);
      expect(await run(w, { m: 'moveChannel', channelId: toHex(id16(0xb1)), direction: 'up' })).toMatchObject({ ok: true, value: null });
      expect(w.routes.patchChannel.mock.calls).toEqual([[id16(0xb1), { position: 0 }], [id16(0xa1), { position: 1 }]]);
      expect(shown(rows)).toBe(JSON.stringify([['c161', 1], ['general', 0], ['AFK', reserved]]));
      await settle(w, rows);
      expect(await run(w, { m: 'createChannel', communityId: CID, kind: 'text', name: 'music' })).toMatchObject({ ok: true });
      expect(w.routes.createChannel).toHaveBeenCalledWith(COMMUNITY, { kind: 'text', name: 'music', position: 2 });
      expect(ordinaryLast(rows, 'music'), shown(rows)).toBe(true);
      expect(afkPatched(w)).toBe(false);
    });

    it('the last ordinary channel above AFK cannot move down, and AFK moves neither up nor down (slice read)', async () => {
      const rows = [channelRow(0xa1, 0), channelRow(0xb1, 1, { kind: 1, name: 'general' }), channelRow(0x61, reserved, { kind: 1, name: 'AFK' })];
      const w = instance(rows);
      await ready(w, rows);
      expect(await run(w, { m: 'moveChannel', channelId: toHex(id16(0xb1)), direction: 'down' })).toMatchObject({ ok: true, value: null });
      expect(await run(w, { m: 'moveChannel', channelId: toHex(AFK), direction: 'up' })).toMatchObject({ ok: true, value: null });
      expect(await run(w, { m: 'moveChannel', channelId: toHex(AFK), direction: 'down' })).toMatchObject({ ok: true, value: null });
      expect(w.routes.patchChannel).not.toHaveBeenCalled();
      expect(shown(rows)).toBe(JSON.stringify([['c161', 0], ['general', 1], ['AFK', reserved]]));
    });

    it('with the slice unread, the channel at exactly 2^30 is AFK: nothing moves past it and it never moves', async () => {
      const rows = [channelRow(0xa1, 0), channelRow(0xb1, 1, { kind: 1, name: 'general' }), channelRow(0x61, reserved, { kind: 1, name: 'AFK' })];
      const w = instance(rows, false);
      await ready(w, rows, false);
      expect(w.last(`afk:${CID}`) ?? null).toBeNull();
      expect(await run(w, { m: 'moveChannel', channelId: toHex(id16(0xb1)), direction: 'down' })).toMatchObject({ ok: true, value: null });
      expect(await run(w, { m: 'moveChannel', channelId: toHex(AFK), direction: 'up' })).toMatchObject({ ok: true, value: null });
      expect(w.routes.patchChannel).not.toHaveBeenCalled();
    });

    it('AFK moved to another category keeps 2^30, and stays last there after a sibling move', async () => {
      const cat = id16(0xd1);
      const rows = [channelRow(0xa1, 0), channelRow(0xd1, 1, { kind: 2, name: 'Voice' }), channelRow(0xb1, 0, { kind: 1, name: 'lounge', parentId: cat }),
        channelRow(0xc1, 1, { kind: 1, name: 'stage', parentId: cat }), channelRow(0x61, reserved, { kind: 1, name: 'AFK' })];
      const w = instance(rows);
      await ready(w, rows);
      expect(await run(w, { m: 'updateChannel', channelId: toHex(AFK), name: null, topic: null, parent: toHex(cat), slowmode: null })).toMatchObject({ ok: true });
      expect(w.routes.patchChannel).toHaveBeenLastCalledWith(AFK, { parent: cat });
      await settle(w, rows);
      expect(w.last<ChannelSummary[]>(`channels:${CID}`)?.find((c) => c.id === toHex(AFK))).toMatchObject({ parentId: toHex(cat), position: reserved });
      expect(await run(w, { m: 'moveChannel', channelId: toHex(id16(0xc1)), direction: 'down' })).toMatchObject({ ok: true, value: null });
      expect(await run(w, { m: 'moveChannel', channelId: toHex(id16(0xc1)), direction: 'up' })).toMatchObject({ ok: true, value: null });
      expect(shown(rows)).toBe(JSON.stringify([['c161', 0], ['Voice', 1], ['lounge', 1], ['stage', 0], ['AFK', reserved]]));
      expect(afkPatched(w)).toBe(false);
    });

    it('a run holding a stray ordinary channel at or above 2^30 (a rolled-back client) is renumbered below 2^30 on the next move', async () => {
      const rows = [channelRow(0xa1, 0), channelRow(0x61, reserved, { kind: 1, name: 'AFK' }), channelRow(0xb1, reserved + 1, { kind: 1, name: 'rolled' })];
      const w = instance(rows);
      await ready(w, rows);
      expect(await run(w, { m: 'moveChannel', channelId: toHex(id16(0xa1)), direction: 'down' })).toMatchObject({ ok: true, value: null });
      expect(shown(rows)).toBe(JSON.stringify([['c161', 1], ['AFK', reserved], ['rolled', 0]]));
      expect(afkPatched(w)).toBe(false);
      await settle(w, rows);
      expect(await run(w, { m: 'createChannel', communityId: CID, kind: 'text', name: 'next' })).toMatchObject({ ok: true });
      expect(ordinaryLast(rows, 'next'), shown(rows)).toBe(true);
    });

    it('a stray ordinary channel at exactly 2^30 is told from AFK by id and renumbered too (the slice is read)', async () => {
      const rows = [channelRow(0xa1, 0), channelRow(0xb1, reserved, { kind: 1, name: 'stray' }), channelRow(0x61, reserved, { kind: 1, name: 'AFK' })];
      const w = instance(rows);
      await ready(w, rows);
      expect(await run(w, { m: 'moveChannel', channelId: toHex(id16(0xb1)), direction: 'up' })).toMatchObject({ ok: true, value: null });
      expect(shown(rows)).toBe(JSON.stringify([['c161', 1], ['stray', 0], ['AFK', reserved]]));
      expect(afkPatched(w)).toBe(false);
    });

    it('a create skips the AFK channel by id when it sits below 2^30 (the slice is read), without tying it', async () => {
      const rows = [channelRow(0xa1, 0), channelRow(0xb1, 1, { kind: 1, name: 'general' }), channelRow(0x61, 5, { kind: 1, name: 'AFK' })];
      const w = instance(rows);
      await ready(w, rows);
      expect(await run(w, { m: 'createCategory', communityId: CID, name: 'Lounge' })).toMatchObject({ ok: true });
      expect(w.routes.createCategory).toHaveBeenCalledWith(COMMUNITY, { name: 'Lounge', position: 2 });
      await settle(w, rows);
      expect(await run(w, { m: 'createChannel', communityId: CID, kind: 'text', name: 'next' })).toMatchObject({ ok: true });
      expect(w.routes.createChannel).toHaveBeenCalledWith(COMMUNITY, { kind: 'text', name: 'next', position: 3 });
    });

    it('with the slice unread, AFK below 2^30 counts as any channel and the create is still last', async () => {
      const rows = [channelRow(0xa1, 0), channelRow(0x61, 1, { kind: 1, name: 'AFK' }), channelRow(0xb1, 2, { kind: 1, name: 'general' })];
      const w = instance(rows, false);
      await toReady(w);
      await settle(w, rows);
      expect(await run(w, { m: 'createChannel', communityId: CID, kind: 'text', name: 'next' })).toMatchObject({ ok: true });
      const ordinary = rows.filter((r) => toHex(r.id) !== toHex(AFK));
      const made = ordinary.find((r) => r.name === 'next');
      expect(made !== undefined && ordinary.every((r) => r === made || r.position < made.position), shown(rows)).toBe(true);
    });
  });

  it('a 403 on a channel edit reads the permissions of its server again', async () => {
    const w = world();
    await toReady(w);
    await vi.waitFor(() => expect(w.routes.getPermissions).toHaveBeenCalled());
    const perms = w.routes.getPermissions.mock.calls.length;
    w.routes.deleteChannel.mockRejectedValueOnce(refusal(403, 'E_FORBIDDEN'));
    expect(await run(w, { m: 'deleteChannel', channelId: toHex(CHANNEL) })).toMatchObject({ ok: false, error: { code: 'E_FORBIDDEN' } });
    await vi.waitFor(() => expect(w.routes.getPermissions.mock.calls.length).toBe(perms + 1));
  });
});

describe('AdminController: fix round 1', () => {
  const positions = (list: ChannelSummary[]): [string, number][] => list.map((c) => [c.id.slice(0, 2), c.position]);
  const patched = (w: World): [string, unknown][] => w.routes.patchChannel.mock.calls.map(([id, patch]) => [toHex(id).slice(0, 2), patch]);

  it('transferOwnership publishes the new owner: owner false and every owner-only flag false', async () => {
    const w = world();
    await toReady(w);
    await vi.waitFor(() => expect(w.last<CommunityAccess>(`permissions:${CID}`)?.owner).toBe(true));
    w.routes.transferOwnership.mockImplementationOnce(() => {
      w.routes.listCommunities.mockResolvedValue([{ id: COMMUNITY, name: 'friends', owner: STRANGER, policyVersion: 1n }]);
      w.routes.getPermissions.mockResolvedValue({ bits: 0n, top: 0 });
      return Promise.resolve();
    });
    expect(await run(w, { m: 'transferOwnership', communityId: CID, userId: STRANGER_HEX })).toMatchObject({ ok: true, value: null });
    await vi.waitFor(() => expect(w.last<CommunityAccess>(`permissions:${CID}`)).toEqual({
      top: 0, owner: false, manageCommunity: false, viewAuditLog: false, manageChannels: false, kickMembers: false, banMembers: false, createInvite: false,
      moderateMembers: false, manageRoles: false, manageMessages: false, loaded: true, everyoneId: '', overwritesLoaded: true,
    }));
  });

  it('transferOwnership still succeeds when the community list cannot be read again', async () => {
    const w = world();
    await toReady(w);
    w.routes.listCommunities.mockRejectedValue(refusal(500, 'E_INTERNAL'));
    expect(await run(w, { m: 'transferOwnership', communityId: CID, userId: STRANGER_HEX })).toMatchObject({ ok: true, value: null });
  });

  it('moveChannel on a tie at the top renumbers the run: [a0,b0], b up gives b0,a1', async () => {
    const w = world();
    w.routes.listChannels.mockResolvedValue([channelRow(0xa1, 0), channelRow(0xb1, 0)]);
    await toReady(w);
    expect(await run(w, { m: 'moveChannel', channelId: toHex(id16(0xb1)), direction: 'up' })).toMatchObject({ ok: true, value: null });
    expect(patched(w)).toEqual([['a1', { position: 1 }]]);
    expect(shownBeforeRet<ChannelSummary[]>(w, `channels:${CID}`,
      (l) => JSON.stringify(positions(l)) === JSON.stringify([['a1', 1], ['b1', 0]]))).toBe(true);
  });

  it('moveChannel on a tie in the middle renumbers the run densely and patches only what changes', async () => {
    const w = world();
    w.routes.listChannels.mockResolvedValue([channelRow(0xa1, 0), channelRow(0xb1, 1), channelRow(0xc1, 1), channelRow(0xd1, 2)]);
    await toReady(w);
    await run(w, { m: 'moveChannel', channelId: toHex(id16(0xc1)), direction: 'up' });
    expect(patched(w)).toEqual([['b1', { position: 2 }], ['d1', { position: 3 }]]);
    expect(shownBeforeRet<ChannelSummary[]>(w, `channels:${CID}`,
      (l) => JSON.stringify(positions(l)) === JSON.stringify([['a1', 0], ['b1', 2], ['c1', 1], ['d1', 3]]))).toBe(true);
  });

  it('moveChannel down onto a tie renumbers the run', async () => {
    const w = world();
    w.routes.listChannels.mockResolvedValue([channelRow(0xa1, 0), channelRow(0xb1, 1), channelRow(0xc1, 1)]);
    await toReady(w);
    await run(w, { m: 'moveChannel', channelId: toHex(id16(0xb1)), direction: 'down' });
    expect(patched(w)).toEqual([['b1', { position: 2 }]]);
  });

  it('moveChannel renumbers when a tie sits elsewhere in the run, not beside the neighbour', async () => {
    const w = world();
    w.routes.listChannels.mockResolvedValue([channelRow(0xa1, 0), channelRow(0xb1, 1), channelRow(0xc1, 2), channelRow(0xd1, 2)]);
    await toReady(w);
    await run(w, { m: 'moveChannel', channelId: toHex(id16(0xa1)), direction: 'down' });
    expect(patched(w)).toEqual([['b1', { position: 0 }], ['a1', { position: 1 }], ['d1', { position: 3 }]]);
  });

  it('moveChannel reloads the community and surfaces the error when a later PATCH fails', async () => {
    const w = world();
    w.routes.listChannels.mockResolvedValue([channelRow(0xa1, 0), channelRow(0xb1, 0), channelRow(0xc1, 1)]);
    await toReady(w);
    const reads = w.routes.listChannels.mock.calls.length;
    w.routes.patchChannel.mockResolvedValueOnce(undefined).mockRejectedValueOnce(refusal(500, 'E_INTERNAL'));
    expect(await run(w, { m: 'moveChannel', channelId: toHex(id16(0xb1)), direction: 'up' })).toMatchObject({
      ok: false, error: { code: 'E_INTERNAL' },
    });
    expect(w.routes.patchChannel).toHaveBeenCalledTimes(2);
    await vi.waitFor(() => expect(w.routes.listChannels.mock.calls.length).toBeGreaterThan(reads));
  });

  it('a 403 on kick reads the members and the roles again as well as the permissions', async () => {
    const w = world();
    await toReady(w);
    await vi.waitFor(() => expect(w.routes.getPermissions).toHaveBeenCalled());
    const members = w.routes.listMembers.mock.calls.length;
    const roles = w.routes.listRoles.mock.calls.length;
    w.routes.kickMember.mockRejectedValueOnce(refusal(403, 'E_FORBIDDEN'));
    expect(await run(w, { m: 'kickMember', communityId: CID, userId: STRANGER_HEX })).toMatchObject({ ok: false, error: { code: 'E_FORBIDDEN' } });
    await vi.waitFor(() => expect(w.routes.listMembers.mock.calls.length).toBe(members + 1));
    await vi.waitFor(() => expect(w.routes.listRoles.mock.calls.length).toBe(roles + 1));
  });

  it('a 403 on ban reads the members and the roles again as well as the permissions', async () => {
    const w = world();
    await toReady(w);
    await vi.waitFor(() => expect(w.routes.getPermissions).toHaveBeenCalled());
    const members = w.routes.listMembers.mock.calls.length;
    const roles = w.routes.listRoles.mock.calls.length;
    w.routes.putBan.mockRejectedValueOnce(refusal(403, 'E_FORBIDDEN'));
    expect(await run(w, { m: 'banMember', communityId: CID, userId: STRANGER_HEX, reason: '', seconds: null })).toMatchObject({
      ok: false, error: { code: 'E_FORBIDDEN' },
    });
    await vi.waitFor(() => expect(w.routes.listMembers.mock.calls.length).toBe(members + 1));
    await vi.waitFor(() => expect(w.routes.listRoles.mock.calls.length).toBe(roles + 1));
  });

  it('unban succeeds when the ban list cannot be read again after the DELETE', async () => {
    const w = world();
    await toReady(w);
    w.routes.listBans.mockRejectedValue(refusal(500, 'E_INTERNAL'));
    expect(await run(w, { m: 'unbanMember', communityId: CID, userId: STRANGER_HEX })).toMatchObject({ ok: true, value: null });
    expect(w.routes.deleteBan).toHaveBeenCalledWith(COMMUNITY, STRANGER);
  });

  it('revokeInvite succeeds when the invite list cannot be read again after the DELETE', async () => {
    const w = world();
    await toReady(w);
    w.routes.listInvites.mockRejectedValue(refusal(500, 'E_INTERNAL'));
    expect(await run(w, { m: 'revokeInvite', communityId: CID, inviteId: toHex(id16(0x71)) })).toMatchObject({ ok: true, value: null });
    expect(w.routes.revokeInvite).toHaveBeenCalledWith(id16(0x71));
  });
});

describe('removal notices (web3.4 C4)', () => {
  const changed: GatewayEvent = { type: 'frame', frame: { op: Op.communityChanged, n: 0n, groupId: null, payload: [COMMUNITY] } };
  const removals = (w: World): RemovalNotice[] | undefined => w.last<RemovalNotice[]>('removals');
  const anyRemoval = (w: World): boolean => w.posted.some((m) => m.t === 'slice' && m.name === 'removals' && (m.value as unknown[]).length > 0);
  const settle = (): Promise<void> => new Promise((resolve) => { setTimeout(resolve, 0); });
  function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void } {
    let resolve: (v: T) => void = () => {};
    let reject: (e: unknown) => void = () => {};
    const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
    return { promise, resolve, reject };
  }

  it('op 54 for a listed server whose re-read lacks it drops it, clears its slices, unselects it and tells once', async () => {
    const w = world();
    await toReady(w);
    await vi.waitFor(() => expect(w.last<CommunityAccess>(`permissions:${CID}`)?.owner).toBe(true));
    expect(await run(w, { m: 'selectCommunity', communityId: CID })).toMatchObject({ ok: true });
    await run(w, { m: 'loadBans', communityId: CID });
    w.state.joined = false;
    w.gateway.emit(changed);
    await vi.waitFor(() => expect(removals(w)).toEqual([{ id: 1, communityId: CID, name: 'friends' }]));
    expect(w.last<{ id: string }[]>('communities')).toEqual([]);
    expect(w.channels()).toEqual([]);
    expect(w.last<unknown[]>(`members:${CID}`)).toEqual([]);
    expect(w.last<unknown[]>(`bans:${CID}`)).toEqual([]);
    expect(w.last<CommunityAccess>(`permissions:${CID}`)?.owner).toBe(false);
    // Nothing is selected any more: once listed again, an unexpected Welcome reads no server's lists.
    w.state.joined = true;
    await run(w, { m: 'refreshCommunities' });
    await settle();
    const reads = w.routes.listChannels.mock.calls.length;
    w.sync.deps!.onUnexpectedWelcome(new Uint8Array(16));
    await new Promise((resolve) => { setTimeout(resolve, 400); });
    expect(w.routes.listChannels.mock.calls.length).toBe(reads);
    // Listed again is not a second notice.
    expect(removals(w)).toHaveLength(1);
  });

  it('leave and delete leave no notice, also when the re-read that follows lacks the server', async () => {
    for (const m of ['leaveCommunity', 'deleteCommunity'] as const) {
      const w = world();
      await toReady(w);
      w.routes[m].mockImplementation(() => { w.state.joined = false; return Promise.resolve(); });
      expect(await run(w, { m, communityId: CID })).toMatchObject({ ok: true });
      await run(w, { m: 'refreshCommunities' });
      expect(anyRemoval(w)).toBe(false);
    }
  });

  it('a re-read that lands while the leave is still in flight leaves no notice', async () => {
    const w = world();
    await toReady(w);
    const held = deferred<undefined>();
    w.routes.leaveCommunity.mockImplementation(() => { w.state.joined = false; return held.promise; });
    next += 1;
    const leave = next;
    w.call(leave, { m: 'leaveCommunity', communityId: CID });
    await vi.waitFor(() => expect(w.routes.leaveCommunity).toHaveBeenCalled());
    await run(w, { m: 'refreshCommunities' });
    expect(w.last<{ id: string }[]>('communities')).toEqual([]);
    held.resolve(undefined);
    await vi.waitFor(() => expect(w.ret(leave)).toMatchObject({ ok: true }));
    expect(anyRemoval(w)).toBe(false);
  });

  it('a refused leave is not a leave: a later removal is told', async () => {
    const w = world();
    await toReady(w);
    w.routes.leaveCommunity.mockRejectedValueOnce(refusal(403, 'E_FORBIDDEN'));
    expect(await run(w, { m: 'leaveCommunity', communityId: CID })).toMatchObject({ ok: false });
    w.state.joined = false;
    await run(w, { m: 'refreshCommunities' });
    expect(removals(w)).toEqual([{ id: 1, communityId: CID, name: 'friends' }]);
  });

  it('a leave whose answer was a 5xx may have gone through: its absence later is no notice', async () => {
    const w = world();
    await toReady(w);
    w.routes.leaveCommunity.mockImplementationOnce(() => { w.state.joined = false; return Promise.reject(refusal(503, 'E_UNAVAILABLE')); });
    expect(await run(w, { m: 'leaveCommunity', communityId: CID })).toMatchObject({ ok: false });
    await run(w, { m: 'refreshCommunities' });
    expect(w.last<{ id: string }[]>('communities')).toEqual([]);
    expect(anyRemoval(w)).toBe(false);
  });

  it('a server left and joined again is told when it is later removed', async () => {
    const w = world();
    await toReady(w);
    w.routes.leaveCommunity.mockImplementation(() => { w.state.joined = false; return Promise.resolve(); });
    await run(w, { m: 'leaveCommunity', communityId: CID });
    w.routes.getInvite.mockResolvedValue({ communityId: COMMUNITY, communityName: 'friends', expires: 0n });
    expect(await run(w, { m: 'joinCommunity', invite: 'AB12CD34' })).toMatchObject({ ok: true });
    expect(w.last<{ id: string }[]>('communities')?.map((c) => c.id)).toEqual([CID]);
    w.state.joined = false;
    await run(w, { m: 'refreshCommunities' });
    expect(removals(w)).toEqual([{ id: 1, communityId: CID, name: 'friends' }]);
  });

  it('final fix I3: kick, notice, rejoin: the notice of that server is gone', async () => {
    const w = world();
    await toReady(w);
    w.state.joined = false;
    w.gateway.emit(changed);
    await vi.waitFor(() => expect(removals(w)).toEqual([{ id: 1, communityId: CID, name: 'friends' }]));
    w.state.joined = true;
    w.routes.getInvite.mockResolvedValue({ communityId: COMMUNITY, communityName: 'friends', expires: 0n });
    expect(await run(w, { m: 'joinCommunity', invite: 'AB12CD34' })).toMatchObject({ ok: true });
    expect(w.last<{ id: string }[]>('communities')?.map((c) => c.id)).toEqual([CID]);
    expect(removals(w)).toEqual([]);
  });

  it('final fix I3: two notices, only server A rejoined: A\'s notice goes, B\'s stays', async () => {
    const w = world();
    const B = id16(0x51);
    const row = (id: Uint8Array, name: string) => ({ id, name, owner: USER, policyVersion: 1n });
    w.routes.listCommunities.mockResolvedValue([row(COMMUNITY, 'friends'), row(B, 'other')]);
    await toReady(w);
    w.routes.listCommunities.mockResolvedValue([]);
    w.gateway.emit(changed);
    await vi.waitFor(() => expect(removals(w)?.map((r) => r.communityId)).toEqual([CID, toHex(B)]));
    w.routes.listCommunities.mockResolvedValue([row(COMMUNITY, 'friends')]);
    w.routes.getInvite.mockResolvedValue({ communityId: COMMUNITY, communityName: 'friends', expires: 0n });
    expect(await run(w, { m: 'joinCommunity', invite: 'AB12CD34' })).toMatchObject({ ok: true });
    expect(w.last<{ id: string }[]>('communities')?.map((c) => c.id)).toEqual([CID]);
    expect(removals(w)).toEqual([{ id: 2, communityId: toHex(B), name: 'other' }]);
  });

  it('the first list at ready tells nothing, even of a server missing from a list cached nowhere', async () => {
    const w = world();
    w.state.joined = false;
    await toReady(w);
    await settle();
    expect(w.last<{ id: string }[]>('communities')).toEqual([]);
    const listed = world();
    await toReady(listed);
    await settle();
    expect(anyRemoval(w)).toBe(false);
    expect(anyRemoval(listed)).toBe(false);
  });

  it('a ninth removal drops the oldest notice', async () => {
    const w = world();
    const ids = Array.from({ length: 9 }, (_, i) => id16(0x21 + i));
    w.routes.listCommunities.mockResolvedValue(ids.map((id, i) => ({ id, name: `s${String(i + 1)}`, owner: USER, policyVersion: 1n })));
    await toReady(w);
    for (let i = 0; i < 8; i++) {
      w.routes.listCommunities.mockResolvedValue(ids.slice(i + 1).map((id, j) => ({ id, name: `s${String(i + j + 2)}`, owner: USER, policyVersion: 1n })));
      await run(w, { m: 'refreshCommunities' });
    }
    expect(removals(w)?.map((r) => r.name)).toEqual(['s1', 's2', 's3', 's4', 's5', 's6', 's7', 's8']);
    w.routes.listCommunities.mockResolvedValue([]);
    await run(w, { m: 'refreshCommunities' });
    expect(removals(w)).toHaveLength(8);
    expect(removals(w)?.map((r) => r.name)).toEqual(['s2', 's3', 's4', 's5', 's6', 's7', 's8', 's9']);
    expect(removals(w)?.map((r) => r.id)).toEqual([2, 3, 4, 5, 6, 7, 8, 9]);
    expect(removals(w)?.[7]).toEqual({ id: 9, communityId: toHex(ids[8]), name: 's9' });
  });

  // ---- C4 fix round 1 ----

  const listOf = (w: World): string[] | undefined => w.last<{ id: string }[]>('communities')?.map((c) => c.id);
  type Rows = Awaited<ReturnType<World['routes']['listCommunities']>>;
  const friends: Rows = [{ id: COMMUNITY, name: 'friends', owner: USER, policyVersion: 1n }];
  /** The next list read waits for the returned gate; `reached` once it was asked. */
  function holdList(w: World): { gate: ReturnType<typeof deferred<Rows>>; reached: () => boolean } {
    const gate = deferred<Rows>();
    let asked = false;
    w.routes.listCommunities.mockImplementationOnce(() => { asked = true; return gate.promise; });
    return { gate, reached: () => asked };
  }

  it('fix 1: a leave whose answer was lost (status 0) keeps the entry: the server gone at the next read is no notice', async () => {
    const w = world();
    await toReady(w);
    w.routes.leaveCommunity.mockImplementationOnce(() => { w.state.joined = false; return Promise.reject(refusal(0, 'E_NETWORK')); });
    expect(await run(w, { m: 'leaveCommunity', communityId: CID })).toMatchObject({ ok: false });
    await run(w, { m: 'refreshCommunities' });
    expect(listOf(w)).toEqual([]);
    expect(anyRemoval(w)).toBe(false);
  });

  it('fix 1: a leave or delete retried to a 404 went through: dropped, success, and no notice', async () => {
    for (const m of ['leaveCommunity', 'deleteCommunity'] as const) {
      const w = world();
      await toReady(w);
      w.routes[m].mockImplementationOnce(() => { w.state.joined = false; return Promise.reject(refusal(404, 'E_NOT_FOUND')); });
      expect(await run(w, { m, communityId: CID })).toMatchObject({ ok: true });
      expect(listOf(w)).toEqual([]);
      await run(w, { m: 'refreshCommunities' });
      expect(anyRemoval(w)).toBe(false);
    }
  });

  it('fix 1: a leave refused with 400, 403 or 409 clears the entry: a later kick is told', async () => {
    for (const status of [400, 403, 409]) {
      const w = world();
      await toReady(w);
      w.routes.leaveCommunity.mockRejectedValueOnce(refusal(status, 'E_REFUSED'));
      expect(await run(w, { m: 'leaveCommunity', communityId: CID })).toMatchObject({ ok: false });
      w.state.joined = false;
      await run(w, { m: 'refreshCommunities' });
      expect(removals(w)).toEqual([{ id: 1, communityId: CID, name: 'friends' }]);
    }
  });

  it('fix 2: an older list read landing after the join\'s read is ignored: no false notice, and the new server stays', async () => {
    const w = world();
    await toReady(w);
    const NEW = id16(0x33);
    const stale = holdList(w);
    next += 1;
    const id = next;
    w.call(id, { m: 'refreshCommunities' });
    await vi.waitFor(() => expect(stale.reached()).toBe(true));
    w.routes.getInvite.mockResolvedValueOnce({ communityId: NEW, communityName: 'new', expires: 0n });
    w.routes.joinCommunity.mockImplementationOnce(() => { w.state.created.push({ id: NEW, name: 'new' }); return Promise.resolve(); });
    expect(await run(w, { m: 'joinCommunity', invite: 'AB12CD34' })).toMatchObject({ ok: true });
    expect(listOf(w)).toEqual([CID, toHex(NEW)]);
    stale.gate.resolve(friends);
    await vi.waitFor(() => expect(w.ret(id)).toBeDefined());
    expect(listOf(w)).toEqual([CID, toHex(NEW)]);
    expect(anyRemoval(w)).toBe(false);
  });

  it('fix 2: an older read that still lists a removed server lands after the newer one: ignored, no second notice', async () => {
    const w = world();
    await toReady(w);
    const stale = holdList(w);
    next += 1;
    const id = next;
    w.call(id, { m: 'refreshCommunities' });
    await vi.waitFor(() => expect(stale.reached()).toBe(true));
    w.state.joined = false;
    await run(w, { m: 'refreshCommunities' });
    expect(removals(w)).toEqual([{ id: 1, communityId: CID, name: 'friends' }]);
    stale.gate.resolve(friends);
    await vi.waitFor(() => expect(w.ret(id)).toBeDefined());
    expect(listOf(w)).toEqual([]);
    await run(w, { m: 'refreshCommunities' });
    expect(removals(w)).toEqual([{ id: 1, communityId: CID, name: 'friends' }]);
  });

  it('fix 3: a forgotten account takes its notices and its own leaves with it', async () => {
    const w = world();
    await toReady(w);
    w.state.joined = false;
    await run(w, { m: 'refreshCommunities' });
    expect(removals(w)).toHaveLength(1);
    w.state.joined = true;
    await run(w, { m: 'refreshCommunities' });
    // A leave of the old account that may have gone through (a 5xx) keeps its entry until the lock goes to another tab.
    const selfRemoved = (w.controller as unknown as { admin: { selfRemoved: Set<string> } }).admin.selfRemoved;
    w.routes.leaveCommunity.mockRejectedValueOnce(refusal(503, 'E_UNAVAILABLE'));
    expect(await run(w, { m: 'leaveCommunity', communityId: CID })).toMatchObject({ ok: false });
    expect([...selfRemoved]).toEqual([CID]);
    let handBack: () => void = () => {};
    const granted = new Promise<void>((resolve) => {
      void w.locks.request(LOCK, {}, () => { resolve(); return new Promise<void>((done) => { handBack = done; }); });
    });
    expect(await run(w, { m: 'resign', nonce: 'aa'.repeat(16) })).toMatchObject({ ok: true });
    expect(removals(w)).toEqual([]);
    expect([...selfRemoved]).toEqual([]);
    await granted;
    handBack();
    await vi.waitFor(() => expect(w.account()?.phase).toBe('ready'));
    expect(removals(w)).toEqual([]);
    // The account that leads now is removed: it is told.
    w.state.joined = false;
    await run(w, { m: 'refreshCommunities' });
    expect(removals(w)?.map((r) => r.communityId)).toEqual([CID]);
  });

  it('fix 4: a 5xx leave and a read that still lists the server: a later kick is told', async () => {
    const w = world();
    await toReady(w);
    w.routes.leaveCommunity.mockRejectedValueOnce(refusal(503, 'E_UNAVAILABLE'));
    expect(await run(w, { m: 'leaveCommunity', communityId: CID })).toMatchObject({ ok: false });
    await run(w, { m: 'refreshCommunities' });
    expect(listOf(w)).toEqual([CID]);
    w.state.joined = false;
    await run(w, { m: 'refreshCommunities' });
    expect(removals(w)).toEqual([{ id: 1, communityId: CID, name: 'friends' }]);
  });

  it('fix 4: a read that lists the server while its leave is still in flight keeps the entry', async () => {
    const w = world();
    await toReady(w);
    const held = deferred<undefined>();
    w.routes.leaveCommunity.mockImplementationOnce(() => held.promise);
    next += 1;
    const leave = next;
    w.call(leave, { m: 'leaveCommunity', communityId: CID });
    await vi.waitFor(() => expect(w.routes.leaveCommunity).toHaveBeenCalled());
    await run(w, { m: 'refreshCommunities' });
    expect(listOf(w)).toEqual([CID]);
    w.state.joined = false;
    held.reject(refusal(503, 'E_UNAVAILABLE'));
    await vi.waitFor(() => expect(w.ret(leave)).toBeDefined());
    await run(w, { m: 'refreshCommunities' });
    expect(anyRemoval(w)).toBe(false);
  });

  it('fix 8: a re-read that lands while the delete is still in flight leaves no notice', async () => {
    const w = world();
    await toReady(w);
    const held = deferred<undefined>();
    w.routes.deleteCommunity.mockImplementation(() => { w.state.joined = false; return held.promise; });
    next += 1;
    const del = next;
    w.call(del, { m: 'deleteCommunity', communityId: CID });
    await vi.waitFor(() => expect(w.routes.deleteCommunity).toHaveBeenCalled());
    await run(w, { m: 'refreshCommunities' });
    expect(listOf(w)).toEqual([]);
    held.resolve(undefined);
    await vi.waitFor(() => expect(w.ret(del)).toMatchObject({ ok: true }));
    expect(anyRemoval(w)).toBe(false);
  });

  it('fix 9: ready publishes an empty removals slice, as it does notices', async () => {
    const w = world();
    await toReady(w);
    expect(removals(w)).toEqual([]);
  });

  it('round 2: an older read landing after a local leave neither lists the server again nor makes a false notice', async () => {
    const w = world();
    await toReady(w);
    const stale = holdList(w);
    next += 1;
    const id = next;
    w.call(id, { m: 'refreshCommunities' });
    await vi.waitFor(() => expect(stale.reached()).toBe(true));
    w.routes.leaveCommunity.mockImplementationOnce(() => { w.state.joined = false; return Promise.resolve(); });
    expect(await run(w, { m: 'leaveCommunity', communityId: CID })).toMatchObject({ ok: true });
    stale.gate.resolve(friends);
    await vi.waitFor(() => expect(w.ret(id)).toBeDefined());
    expect(listOf(w)).toEqual([]);
    await run(w, { m: 'refreshCommunities' });
    expect(anyRemoval(w)).toBe(false);
  });

  it('round 2: an older read landing after a rename does not bring the old name back', async () => {
    const w = world();
    await toReady(w);
    const stale = holdList(w);
    next += 1;
    const id = next;
    w.call(id, { m: 'refreshCommunities' });
    await vi.waitFor(() => expect(stale.reached()).toBe(true));
    // The rename's own re-read is held too, so only the stale read can land.
    holdList(w);
    expect(await run(w, { m: 'renameCommunity', communityId: CID, name: 'renamed' })).toMatchObject({ ok: true });
    stale.gate.resolve(friends);
    await vi.waitFor(() => expect(w.ret(id)).toBeDefined());
    expect(w.last<{ name: string }[]>('communities')?.map((c) => c.name)).toEqual(['renamed']);
  });

  it('round 2: an older read landing after a create listed by the fallback does not drop the new server', async () => {
    const w = world();
    await toReady(w);
    const stale = holdList(w);
    next += 1;
    const id = next;
    w.call(id, { m: 'refreshCommunities' });
    await vi.waitFor(() => expect(stale.reached()).toBe(true));
    w.routes.listCommunities.mockRejectedValueOnce(refusal(503, 'E_UNAVAILABLE'));
    const created = await run(w, { m: 'createCommunity', name: 'midgard' }) as { ok: boolean; value: { communityId: string } };
    expect(created.ok).toBe(true);
    expect(listOf(w)).toEqual([CID, created.value.communityId]);
    stale.gate.resolve(friends);
    await vi.waitFor(() => expect(w.ret(id)).toBeDefined());
    expect(listOf(w)).toEqual([CID, created.value.communityId]);
    expect(anyRemoval(w)).toBe(false);
  });
});
