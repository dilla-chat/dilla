// The worker's admin controller (web3.4, flow 08): the server owner's and a moderator's commands, the access reads that gate
// them in the page, and the lists of bans and invites. Every client gate is advisory: the instance decides each request,
// and a refusal reaches the page with its own code. A 403 means the access the page holds is stale, so it is read again.
import type { Id } from '../core-port';
import { fromHex, toHex } from '../hex';
import { DillaHttpError } from '../http/errors';
import { BAN_REASON_MAX_BYTES, CHANNEL_NAME_MAX_BYTES, COMMUNITY_NAME_MAX_BYTES, SLOW_STEPS, TOPIC_MAX_BYTES, nameProblem } from '../http/routes';
import type { ChannelRow, OverwriteRow, RoleRow, Routes } from '../http/routes';
import type { BanSummary, CommunitySummary, InviteSummary, SliceName, SliceTypes } from '../state/types';
import type { Command } from '../worker/protocol';
import { AFK_BUILTIN_POSITION, isBuiltinAFK } from './afk';
import { accessOf } from './rank';

/** A ban lasts at least an hour and at most a hundred years (the page offers presets inside that). */
export const BAN_SECONDS_MIN = 3600;
export const BAN_SECONDS_MAX = 3_153_600_000;

export { AFK_BUILTIN_POSITION };
/** The position after a community's last channel, ignoring every position at or above the AFK reservation (R-AFK6): a
 *  create right after the community, before its `afk:` slice is read, still lands above the AFK channel. */
export function nextPosition(positions: Iterable<number>): number {
  let next = 0;
  for (const p of positions) if (p < AFK_BUILTIN_POSITION) next = Math.max(next, p + 1);
  return next;
}

export type AdminRoutes = Pick<Routes, 'getPermissions' | 'listRoles' | 'listOverwrites' | 'patchCommunity' | 'deleteCommunity' | 'leaveCommunity'
  | 'transferOwnership' | 'createCategory' | 'patchChannel' | 'deleteChannel' | 'kickMember' | 'putBan' | 'deleteBan' | 'listBans'
  | 'listInvites' | 'revokeInvite'>;

/** What the admin controller reads from, and changes in, the worker's composition root. */
export interface AdminDeps {
  routes: AdminRoutes;
  /** Epoch milliseconds. */
  now(): number;
  me(): { userId: Id } | null;
  get<K extends SliceName>(name: K): SliceTypes[K] | undefined;
  publish<K extends SliceName>(name: K, value: SliceTypes[K]): void;
  listed(communityId: string): boolean;
  /** The known channels of one server, as the worker keeps them. */
  channelsOf(communityId: string): { id: string; row: ChannelRow }[];
  /** The server a known channel belongs to. */
  communityOf(channelId: string): string | null;
  setChannel(communityId: string, row: ChannelRow): void;
  removeChannel(channelId: string): void;
  publishChannels(communityId: string): void;
  /** A list read that began before this change must be read again (the worker's structureRev). */
  structureChanged(communityId: string): void;
  /** Reads the channel and member lists of a server again; rejects when the read does. */
  reloadCommunity(communityId: string): Promise<void>;
  /** Reads the member list of a server again (not the channels); rejects when the read does. */
  reloadMembers(communityId: string): Promise<void>;
  refreshCommunities(): Promise<void>;
  /** The community list is written here (C4 fix round 2): a list read begun before it is older and is not applied. */
  listWritten(): void;
  /** The server is gone from this account: its slices are cleared and it is neither selected nor listed. A list read
   *  begun before it is older and is not applied. */
  dropCommunity(communityId: string): void;
  track(job: Promise<unknown>): void;
  /** An access read landed (wave 2a): the roles controller keeps the rows and bits and publishes roles and overwrites;
   *  `overwrites` is null when that read failed. */
  accessRead(communityId: string, roles: RoleRow[], bits: bigint, overwrites: OverwriteRow[] | null): void;
  /** A server was dropped: the roles controller forgets what it kept. */
  accessForgotten(communityId: string): void;
  /** The worker's own refusal (its code reaches the page as E_BAD_INPUT and the like). */
  refusal(code: string, detail: string): Error;
}

type AdminCommand = Extract<Command, { m: 'renameCommunity' | 'deleteCommunity' | 'leaveCommunity' | 'transferOwnership' | 'createCategory'
  | 'updateChannel' | 'moveChannel' | 'deleteChannel' | 'kickMember' | 'banMember' | 'unbanMember' | 'loadBans' | 'loadInvites' | 'revokeInvite' }>;
export type AdminMethod = AdminCommand['m'];

const UTF8 = new TextEncoder();
const ID = /^[0-9a-f]{32}$/;
const blank = (v: unknown): boolean => typeof v !== 'string' || v.trim() === '';
function nameOf(v: unknown, max: number): string | null {
  const problem = blank(v) ? 'is blank' : nameProblem(v, max);
  return problem === null ? null : `name ${problem}`;
}

/** The synchronous input checks of the admin commands: an E_BAD_INPUT reason, or null. */
export const ADMIN_VALIDATORS: Record<AdminMethod, ((c: Record<string, unknown>) => string | null) | undefined> = {
  renameCommunity: (c) => nameOf(c.name, COMMUNITY_NAME_MAX_BYTES),
  deleteCommunity: undefined,
  leaveCommunity: undefined,
  transferOwnership: undefined,
  createCategory: (c) => nameOf(c.name, CHANNEL_NAME_MAX_BYTES),
  updateChannel: (c) => {
    if (c.name !== null) { const p = nameOf(c.name, CHANNEL_NAME_MAX_BYTES); if (p !== null) return p; }
    if (c.topic !== null && (typeof c.topic !== 'string' || UTF8.encode(c.topic).length > TOPIC_MAX_BYTES)) return 'topic is too long';
    if (c.parent !== 'keep' && c.parent !== 'top' && !(typeof c.parent === 'string' && ID.test(c.parent))) return 'parent is not keep, top or 32 lowercase hex';
    if (c.slowmode !== null && !(typeof c.slowmode === 'number' && (SLOW_STEPS as readonly number[]).includes(c.slowmode))) return 'slowmode is not a slow-mode step';
    if (c.name === null && c.topic === null && c.parent === 'keep' && c.slowmode === null) return 'nothing to change';
    return null;
  },
  moveChannel: (c) => (c.direction === 'up' || c.direction === 'down' ? null : 'direction is not up or down'),
  deleteChannel: undefined,
  kickMember: undefined,
  banMember: (c) => {
    const s = c.seconds;
    if (s !== null && (typeof s !== 'number' || !Number.isSafeInteger(s) || s < BAN_SECONDS_MIN || s > BAN_SECONDS_MAX)) return 'seconds is out of range';
    if (typeof c.reason !== 'string' || UTF8.encode(c.reason).length > BAN_REASON_MAX_BYTES) return 'reason is too long';
    return null;
  },
  unbanMember: undefined,
  loadBans: undefined,
  loadInvites: undefined,
  revokeInvite: undefined,
};

const statusOf = (e: unknown): number | null => (e instanceof DillaHttpError ? e.status : null);

export class AdminController {
  constructor(private readonly deps: AdminDeps) {}

  // ---- access ----

  /** Reads the caller's permissions, the roles and the overwrites of a server, and publishes them; a failed read publishes
   *  nothing and leaves what the page already holds, except that a failed overwrites read alone clears `overwritesLoaded`.
   *  Never throws. Reads are sequenced per server: one that began before a later one never publishes, and it resolves
   *  once the newest has. */
  async loadAccess(communityId: string): Promise<void> {
    const seq = (this.accessSeq.get(communityId) ?? 0) + 1;
    this.accessSeq.set(communityId, seq);
    const job = this.readAccess(communityId, seq);
    this.accessJob.set(communityId, job);
    await job;
    // Superseded: wait for the newest read, so the caller sees its slices when this resolves.
    let latest = this.accessJob.get(communityId);
    while (latest !== undefined && latest !== job) {
      await latest;
      const again = this.accessJob.get(communityId);
      if (again === latest) break;
      latest = again;
    }
  }
  private readonly accessSeq = new Map<string, number>();
  private readonly accessJob = new Map<string, Promise<void>>();

  private async readAccess(communityId: string, seq: number): Promise<void> {
    try {
      const id = fromHex(communityId);
      const [perms, roles, overwrites] = await Promise.all([this.deps.routes.getPermissions(id), this.deps.routes.listRoles(id),
        this.deps.routes.listOverwrites(id).catch(() => null)]);
      if (this.accessSeq.get(communityId) !== seq) return;
      const me = this.deps.me();
      if (me === null || !this.deps.listed(communityId)) return;
      const owner = (this.deps.get('communities') ?? []).find((c: CommunitySummary) => c.id === communityId)?.owner === toHex(me.userId);
      const everyone = roles.find((r) => r.position === 0);
      this.deps.publish(`permissions:${communityId}`, { ...accessOf(perms.bits, perms.top, owner),
        everyoneId: everyone === undefined ? '' : toHex(everyone.id), overwritesLoaded: overwrites !== null });
      this.deps.accessRead(communityId, roles, perms.bits, overwrites);
    } catch { /* the slices stay as they were (absent before the first read) */ }
  }

  /** Clears everything admin-side that a dropped server held. */
  forget(communityId: string): void {
    this.deps.publish(`roles:${communityId}`, []);
    this.deps.publish(`bans:${communityId}`, []);
    this.deps.publish(`invites:${communityId}`, []);
    this.deps.publish(`overwrites:${communityId}`, {});
    this.deps.accessForgotten(communityId);
    this.accessSeq.set(communityId, (this.accessSeq.get(communityId) ?? 0) + 1);   // a read in flight no longer publishes
    // A slice has no "absent" value: a server the caller left reads as no access at all.
    this.deps.publish(`permissions:${communityId}`, { ...accessOf(0n, 0, false), loaded: false });
  }

  // ---- the server ----

  async renameCommunity(communityId: string, name: string): Promise<null> {
    await this.guarded(communityId, () => this.deps.routes.patchCommunity(fromHex(communityId), { name }));
    const list = this.deps.get('communities') ?? [];
    this.deps.listWritten();
    this.deps.publish('communities', list.map((c) => (c.id === communityId ? { ...c, name } : c)));
    this.track(this.deps.refreshCommunities());
    return null;
  }

  async deleteCommunity(communityId: string): Promise<null> {
    await this.removingSelf(communityId, () => this.deps.routes.deleteCommunity(fromHex(communityId)));
    this.drop(communityId);
    return null;
  }

  async leaveCommunity(communityId: string): Promise<null> {
    await this.removingSelf(communityId, () => this.deps.routes.leaveCommunity(fromHex(communityId)));
    this.drop(communityId);
    return null;
  }

  /** The servers this account left or deleted itself (web3.4 C4): their absence from a list read is no removal notice.
   *  Set before the request, so a read that lands while it is in flight is covered too. A refusal that went nowhere
   *  (400, 403, 409 and the other 4xx but 404) takes it out again, and so does a join of the same server (the worker's
   *  joinPath). A failure that may have gone through (status 0, a lost answer or a timeout; a 5xx) keeps it, so the
   *  person is never told they were removed from a server they left; a list read that still lists the server once no
   *  request is in flight takes it out (the worker's refreshCommunities). A 404 is a retried request whose first attempt
   *  went through: the server is gone, and the command succeeds. */
  readonly selfRemoved = new Set<string>();
  /** The servers whose leave or delete is in flight (C4 fix round 1): a list read that lists one keeps its entry. */
  readonly pendingSelf = new Set<string>();

  private async removingSelf(communityId: string, run: () => Promise<void>): Promise<void> {
    this.selfRemoved.add(communityId);
    this.pendingSelf.add(communityId);
    try {
      await this.guarded(communityId, run);
    } catch (e) {
      const status = statusOf(e);
      if (status === 404) return;
      if (status !== null && status >= 400 && status < 500) this.selfRemoved.delete(communityId);
      throw e;
    } finally {
      this.pendingSelf.delete(communityId);
    }
  }

  async transferOwnership(communityId: string, userId: string): Promise<null> {
    await this.guarded(communityId, () => this.deps.routes.transferOwnership(fromHex(communityId), fromHex(userId)));
    // The owner flag of the access comes from the community list: read it first, so the access read below sees the new owner.
    await this.deps.refreshCommunities().catch(() => undefined);
    await this.afterModeration(communityId);
    return null;
  }

  // ---- channels and categories ----

  async createCategory(communityId: string, name: string): Promise<{ channelId: string }> {
    this.requireListed(communityId);
    const afkId = this.deps.get(`afk:${communityId}`)?.channelId ?? null;
    const position = nextPosition(this.deps.channelsOf(communityId).filter(({ id: known }) => known !== afkId).map(({ row }) => row.position));
    const created = await this.guarded(communityId, () => this.deps.routes.createCategory(fromHex(communityId), { name, position }));
    this.deps.structureChanged(communityId);
    this.deps.setChannel(communityId, { id: created.id, kind: 2, mode: created.mode, visibility: created.visibility, parentId: null,
      name, topic: '', position, slowmode: 0, seq: 0n, textGroupId: null });
    this.deps.publishChannels(communityId);
    this.reload(communityId);
    return { channelId: toHex(created.id) };
  }

  async updateChannel(channelId: string, name: string | null, topic: string | null, parent: string, slowmode: number | null): Promise<null> {
    const { communityId, row } = this.known(channelId);
    const patch: { name?: string; topic?: string; parent?: Uint8Array | 'top'; slowmode?: number } = {};
    if (name !== null) patch.name = name;
    if (topic !== null) patch.topic = topic;
    if (slowmode !== null) patch.slowmode = slowmode;
    if (parent === 'top') patch.parent = 'top';
    else if (parent !== 'keep') patch.parent = fromHex(parent);
    await this.guarded(communityId, () => this.deps.routes.patchChannel(row.id, patch));
    this.deps.structureChanged(communityId);
    this.deps.setChannel(communityId, { ...row, name: patch.name ?? row.name, topic: patch.topic ?? row.topic, slowmode: patch.slowmode ?? row.slowmode,
      parentId: patch.parent === undefined ? row.parentId : patch.parent === 'top' ? null : patch.parent });
    this.deps.publishChannels(communityId);
    this.reload(communityId);
    return null;
  }

  /** Moves a channel one step inside its parent, among the ordinary channels only (R-AFK8): the built-in AFK channel stays
   *  last at 2^30, never moves and is never passed, so a move of AFK, or down from the last ordinary channel, sends nothing.
   *  Positions that are strictly increasing and below 2^30 trade between the mover and its neighbour (two patches). With a
   *  tie anywhere among the ordinary channels, or one of them at or above 2^30 (a rolled-back client's create), those are
   *  renumbered densely 0..n-1 in the new display order, and only the channels whose position changes are patched.
   *  Whatever happens, the lists are read again. */
  async moveChannel(channelId: string, direction: 'up' | 'down'): Promise<null> {
    const { communityId, row } = this.known(channelId);
    const afkId = this.deps.get(`afk:${communityId}`)?.channelId ?? null;
    if (isBuiltinAFK(channelId, row.position, afkId)) return null;
    const parent = row.parentId === null ? null : toHex(row.parentId);
    const siblings = this.deps.channelsOf(communityId)
      .filter((c) => (c.row.parentId === null ? null : toHex(c.row.parentId)) === parent && (c.row.kind === 2) === (row.kind === 2)
        && !isBuiltinAFK(c.id, c.row.position, afkId))
      .sort((a, b) => a.row.position - b.row.position || (a.id < b.id ? -1 : 1));
    const at = siblings.findIndex((c) => c.id === channelId);
    const to = direction === 'up' ? at - 1 : at + 1;
    const other = siblings[to] as { id: string; row: ChannelRow } | undefined;
    if (other === undefined) return null;
    const strict = siblings.every((c, i) => i === 0 || c.row.position > (siblings[i - 1] as { row: ChannelRow }).row.position)
      && siblings.every((c) => c.row.position < AFK_BUILTIN_POSITION);
    let changes: { row: ChannelRow; position: number }[];
    if (strict) {
      changes = [{ row, position: other.row.position }, { row: other.row, position: row.position }];
    } else {
      const order = siblings.slice();
      order[at] = other;
      order[to] = siblings[at];
      changes = order.map((c, i) => ({ row: c.row, position: i })).filter((c) => c.row.position !== c.position);
    }
    try {
      await this.guarded(communityId, async () => {
        for (const change of changes) await this.deps.routes.patchChannel(change.row.id, { position: change.position });
      });
    } catch (e) {
      // Some of the patches may have gone through: what the instance holds decides, not the list the page had.
      this.deps.structureChanged(communityId);
      this.reload(communityId);
      throw e;
    }
    this.deps.structureChanged(communityId);
    for (const change of changes) this.deps.setChannel(communityId, { ...change.row, position: change.position });
    this.deps.publishChannels(communityId);
    this.reload(communityId);
    return null;
  }

  async deleteChannel(channelId: string): Promise<null> {
    const { communityId, row } = this.known(channelId);
    await this.guarded(communityId, () => this.deps.routes.deleteChannel(row.id));
    this.deps.structureChanged(communityId);
    this.deps.removeChannel(channelId);
    this.deps.publishChannels(communityId);
    this.reload(communityId);
    return null;
  }

  // ---- members ----

  /** DELETE .../members/{user}. A 404 (a retried request whose first attempt went through) surfaces, so the page says the
   *  member is already gone; the member list is read again either way. */
  async kickMember(communityId: string, userId: string): Promise<null> {
    this.requireListed(communityId);
    try {
      await this.guarded(communityId, () => this.deps.routes.kickMember(fromHex(communityId), fromHex(userId)));
    } catch (e) {
      if (statusOf(e) === 404) await this.deps.reloadMembers(communityId).catch(() => undefined);
      else await this.staleMembers(communityId, e);
      throw e;
    }
    await this.afterModeration(communityId);
    return null;
  }

  async banMember(communityId: string, userId: string, reason: string, seconds: number | null): Promise<null> {
    this.requireListed(communityId);
    const expires = seconds === null ? null : BigInt(Math.floor(this.deps.now() / 1000) + seconds);
    try {
      await this.guarded(communityId, () => this.deps.routes.putBan(fromHex(communityId), fromHex(userId), { reason, expires }));
    } catch (e) {
      await this.staleMembers(communityId, e);
      throw e;
    }
    await this.afterModeration(communityId);
    if (this.deps.get(`bans:${communityId}`) !== undefined) await this.loadBans(communityId).catch(() => undefined);
    return null;
  }

  /** DELETE .../bans/{user}. A 404 is success (the request was retried after the first one went through). */
  async unbanMember(communityId: string, userId: string): Promise<null> {
    this.requireListed(communityId);
    try {
      await this.guarded(communityId, () => this.deps.routes.deleteBan(fromHex(communityId), fromHex(userId)));
    } catch (e) {
      if (statusOf(e) !== 404) throw e;
    }
    await this.afterModeration(communityId);
    await this.loadBans(communityId).catch(() => undefined);
    return null;
  }

  async loadBans(communityId: string): Promise<null> {
    this.requireListed(communityId);
    const rows = await this.guarded(communityId, () => this.deps.routes.listBans(fromHex(communityId)));
    if (!this.deps.listed(communityId)) return null;
    this.deps.publish(`bans:${communityId}`, rows.map((b): BanSummary => ({
      userId: toHex(b.userId), name: b.display !== '' ? b.display : b.username, reason: b.reason, byUser: toHex(b.byUser),
      created: Number(b.created), expires: b.expires === null ? null : Number(b.expires),
    })));
    return null;
  }

  // ---- invites ----

  /** Only the invites that can still be used: not revoked, not expired, not used up. 0 means no limit (max uses and expiry). */
  async loadInvites(communityId: string): Promise<null> {
    this.requireListed(communityId);
    const rows = await this.guarded(communityId, () => this.deps.routes.listInvites(fromHex(communityId)));
    if (!this.deps.listed(communityId)) return null;
    const now = BigInt(Math.floor(this.deps.now() / 1000));
    this.deps.publish(`invites:${communityId}`, rows
      .filter((i) => i.revokedAt === null && (i.expires === 0n || i.expires > now) && (i.maxUses === 0 || i.uses < i.maxUses))
      .map((i): InviteSummary => ({ id: toHex(i.id), createdBy: i.createdBy === null ? null : toHex(i.createdBy), maxUses: i.maxUses,
        uses: i.uses, created: Number(i.created), expires: Number(i.expires) })));
    return null;
  }

  /** DELETE /v1/invites/{id}. A 404 is success (a retried request whose first attempt went through). */
  async revokeInvite(communityId: string, inviteId: string): Promise<null> {
    this.requireListed(communityId);
    try {
      await this.guarded(communityId, () => this.deps.routes.revokeInvite(fromHex(inviteId)));
    } catch (e) {
      if (statusOf(e) !== 404) throw e;
    }
    await this.loadInvites(communityId).catch(() => undefined);
    return null;
  }

  // ---- internals ----

  private requireListed(communityId: string): void {
    if (!this.deps.listed(communityId)) throw this.deps.refusal('E_BAD_INPUT', 'unknown community');
  }

  private known(channelId: string): { communityId: string; row: ChannelRow } {
    const communityId = this.deps.communityOf(channelId);
    const row = communityId === null ? undefined : this.deps.channelsOf(communityId).find((c) => c.id === channelId)?.row;
    if (communityId === null || row === undefined) throw this.deps.refusal('E_BAD_INPUT', 'unknown channel');
    return { communityId, row };
  }

  /** Runs a request; a 403 (the instance no longer grants what the page offered) reads the permissions again, and the
   *  refusal still reaches the page. */
  private async guarded<T>(communityId: string, run: () => Promise<T>): Promise<T> {
    try {
      return await run();
    } catch (e) {
      if (statusOf(e) === 403) this.track(this.loadAccess(communityId));
      throw e;
    }
  }

  /** After a kick, a ban, an unban or a transfer: the member list and the caller's access, read again. A failed read does
   *  not undo the change. */
  private async afterModeration(communityId: string): Promise<void> {
    await Promise.all([this.deps.reloadMembers(communityId).catch(() => undefined), this.loadAccess(communityId)]);
    if (this.deps.get('communities')?.some((c) => c.id === communityId) === true) this.track(this.deps.refreshCommunities());
  }

  /** A 403 on a member command: the access the page holds is stale (guarded reads the permissions and roles again), and
   *  so may be the member list it acted on. */
  private async staleMembers(communityId: string, e: unknown): Promise<void> {
    if (statusOf(e) === 403) await this.deps.reloadMembers(communityId).catch(() => undefined);
  }

  private drop(communityId: string): void {
    this.deps.publish('communities', (this.deps.get('communities') ?? []).filter((c) => c.id !== communityId));
    this.deps.dropCommunity(communityId);
  }

  private reload(communityId: string): void {
    this.track(this.deps.reloadCommunity(communityId));
  }

  private track(job: Promise<unknown>): void {
    this.deps.track(job.catch(() => undefined));
  }
}
