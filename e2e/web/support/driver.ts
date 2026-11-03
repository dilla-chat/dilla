import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process';
import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { createInterface } from 'node:readline';
import { REPO_ROOT } from './host.ts';

export function parseInvite(banner: string): string | null { return /DILLA_TESTKIT_INVITE=(\S+)/.exec(banner)?.[1] ?? null; }
export function testkitEnv(): Record<string, string> {
  const invite = process.env.DILLA_TESTKIT_INVITE;
  if (!invite) throw new Error('DILLA_TESTKIT_INVITE is unset: the web globalSetup exports it from the test host banner');
  return { DILLA_TESTKIT_INVITE: invite, DILLA_TESTKIT_CONTROL: process.env.DILLA_TESTKIT_CONTROL ?? '' };
}
export function instanceInvite(): string { return testkitEnv().DILLA_TESTKIT_INVITE.split(',')[0]; }
export function testHostUrl(): string {
  const url = process.env.DILLA_TEST_HOST;
  if (!url) throw new Error('DILLA_TEST_HOST is unset: the web globalSetup exports it');
  return url;
}
export function testkitBinary(): string { return process.env.DILLA_TESTKIT ?? join(REPO_ROOT, 'target', 'release', process.platform === 'win32' ? 'dilla-testkit.exe' : 'dilla-testkit'); }

export interface PeerSetup { username: string; display: string; user_id: string; device_id: string; community_id: string; channel_id: string; channel_ids: string[]; invite_code: string; voice_channel_id: string | null }
export interface PeerAttachment { index: number; blob_id: string; size: number; mime: string; name: string; thumb: boolean }
/** A sync row; a row deleted before the peer held it has null identity fields. */
export interface PeerReceived {
  seq: number; body: string; sender_user: string | null; sender_device: string | null; tier: number | null;
  msg_id: string | null; type: number | null; reply_to: string | null; deleted: boolean; attachments: PeerAttachment[];
}
export interface PeerSendOptions { type?: number; replyTo?: string; threadId?: string }
export interface PeerAttachRequest { channelId?: string; name: string; mime: string; body?: string; bytesHex?: string; size?: number; seed?: number; w?: number; h?: number; thumbHex?: string }
export interface PeerAttached { seq: number; msg_id: string; blob_id: string; sha256: string }
export interface PeerFetched { sha256: string; size: number; mime: string; name: string; thumb_sha256: string | null }
export interface PeerSync { epoch: number; members: number; received: PeerReceived[] }
export interface PeerDm { channel_id: string; group_id: string; epoch: number; }
export interface PeerOpenedDm { channel_id: string; group_id: string; epoch: number; created: boolean; }
export interface PeerEnrolled { device_id: string; version: number; epoch: number; scopes: number[] }
/** L-E2E-30: who a call verb acts as, the peer's acting device or the device `enrol` added to its account. */
export type PeerActor = 'peer' | 'second';
export interface PeerCallJoined { group: string; leaf: number }
export interface PeerCallMembers { group: string; devices: string[] }
export interface PeerCallStarted { call: string; created: boolean }
export interface PeerCallRead { group: string | null; call: string | null; participants: { user: string; device: string; flags: number }[] }
export interface PeerCallCheck { code: string | null; leaves: { leaf: number; user: string; outcome: string }[] }
type Pending = { resolve: (v: unknown) => void; reject: (e: Error) => void };

export class WebDriver {
  private next = 1;
  private readonly pending = new Map<number, Pending>();
  private constructor(private readonly child: ChildProcessWithoutNullStreams, readonly seed: number) {
    createInterface({ input: child.stdout }).on('line', (line) => {
      const answer = JSON.parse(line) as { id: number; ok: boolean; error?: string } & Record<string, unknown>;
      const p = this.pending.get(answer.id);
      if (!p) return;
      this.pending.delete(answer.id);
      if (answer.ok) p.resolve(answer);
      else p.reject(new Error(`web-driver: ${answer.error}`));
    });
    child.stderr.on('data', (d: Buffer) => process.stderr.write(d));
    child.on('exit', (code) => {
      for (const p of this.pending.values()) p.reject(new Error(`web-driver exited with ${code}`));
      this.pending.clear();
    });
  }
  static async start(dsUrl: string, env: Record<string, string>, seed = Date.now() % 0xff_ffff): Promise<WebDriver> {
    const bin = testkitBinary();
    if (!existsSync(bin)) throw new Error(`dilla-testkit not found at ${bin}: build it with cargo build -p dilla-testkit --release --locked, or set DILLA_TESTKIT`);
    const child = spawn(bin, ['web-driver', '--ds', dsUrl, '--seed', String(seed)], { env: { ...process.env, ...env }, stdio: ['pipe', 'pipe', 'pipe'] });
    return new WebDriver(child, seed);
  }
  request<T>(op: string, args: Record<string, unknown>): Promise<T> {
    const id = this.next++;
    return new Promise<T>((resolve, reject) => {
      this.pending.set(id, { resolve: resolve as (v: unknown) => void, reject });
      this.child.stdin.write(`${JSON.stringify({ id, op, ...args })}\n`);
    });
  }
  private strip<T>(answer: T & { id: number; ok: boolean }): T {
    const { id: _id, ok: _ok, ...fields } = answer;
    return fields as T;
  }
  async setup(a: { community: string; channel: string; password?: string; channels?: number; voice?: boolean }): Promise<PeerSetup> { return this.strip(await this.request<PeerSetup & { id: number; ok: boolean }>('setup', a)); }
  async register(): Promise<{ group_id: string; epoch: number }> { return this.strip(await this.request<{ group_id: string; epoch: number; id: number; ok: boolean }>('register', {})); }
  async join(a: { community_id: string; channel_id: string; group_id: string; invite_code: string }): Promise<{ group_id: string; epoch: number }> { return this.strip(await this.request<{ group_id: string; epoch: number; id: number; ok: boolean }>('join', a)); }
  async send(body: string, channelId?: string, opts: PeerSendOptions = {}): Promise<{ seq: number; msg_id: string }> {
    return this.strip(await this.request<{ seq: number; msg_id: string; id: number; ok: boolean }>('send', {
      body,
      ...(channelId === undefined ? {} : { channel_id: channelId }),
      ...(opts.type === undefined ? {} : { type: opts.type }),
      ...(opts.replyTo === undefined ? {} : { reply_to: opts.replyTo }),
      ...(opts.threadId === undefined ? {} : { thread_id: opts.threadId }),
    }));
  }
  async sync(channelId?: string): Promise<PeerSync> { return this.strip(await this.request<PeerSync & { id: number; ok: boolean }>('sync', channelId === undefined ? {} : { channel_id: channelId })); }
  async members(channelId?: string): Promise<{ devices: string[] }> { return this.strip(await this.request<{ devices: string[]; id: number; ok: boolean }>('members', channelId === undefined ? {} : { channel_id: channelId })); }
  async update(): Promise<{ epoch: number }> { return this.strip(await this.request<{ epoch: number; id: number; ok: boolean }>('update', {})); }
  /** L-E2E-10 `enrol`: a second Browser-tier device of the peer's account, by host login, pending then listed. */
  async enrol(): Promise<PeerEnrolled> { return this.strip(await this.request<PeerEnrolled & { id: number; ok: boolean }>('enrol', {})); }
  /** L-E2E-10 `revoke`: list v+1 with that device revoked, signed and published by the peer's first device. */
  async revoke(deviceId: string): Promise<{ version: number }> { return this.strip(await this.request<{ version: number; id: number; ok: boolean }>('revoke', { device_id: deviceId })); }
  async trustList(userId: string): Promise<{ version: number; pin: string | null; verified: boolean; changed: string | null; pendingUmk: string | null }> {
    return this.strip(await this.request<{ version: number; pin: string | null; verified: boolean; changed: string | null; pendingUmk: string | null; id: number; ok: boolean }>('trust_list', { user: userId }));
  }
  async rotateUmk(userId?: string): Promise<{ user: string; version: number }> {
    return this.strip(await this.request<{ user: string; version: number; id: number; ok: boolean }>('rotate_umk', userId === undefined ? {} : { user: userId }));
  }
  /** `leave`: the acting device proposes its own removal from the group (a member Remove another member commits). */
  async leave(channelId?: string): Promise<{ group_id: string }> {
    return this.strip(await this.request<{ group_id: string; id: number; ok: boolean }>('leave', channelId === undefined ? {} : { channel_id: channelId }));
  }
  /** The owner gives @everyone `bits` through the roles API (flow 09); answers the new mask. */
  async allowEveryone(bits: number): Promise<{ allow: number }> { return this.strip(await this.request<{ allow: number; id: number; ok: boolean }>('allow_everyone', { bits })); }
  async typing(channelId: string, on: boolean): Promise<void> {
    await this.request('typing', { channel: channelId, on });
  }
  async searchSeed(channelId: string, body: string): Promise<{ seq: number }> {
    return this.strip(await this.request<{ seq: number; id: number; ok: boolean }>('search_seed', { channel: channelId, body }));
  }
  async openDm(userId: string): Promise<PeerOpenedDm> { return this.strip(await this.request<PeerOpenedDm & { id: number; ok: boolean }>('open_dm', { user_id: userId })); }
  async dms(): Promise<{ dms: PeerDm[] }> { return this.strip(await this.request<{ dms: PeerDm[]; id: number; ok: boolean }>('dms', {})); }
  async sendDm(channelId: string, body: string): Promise<{ seq: number }> { return this.strip(await this.request<{ seq: number; id: number; ok: boolean }>('send_dm', { channel_id: channelId, body })); }
  async syncDm(channelId: string): Promise<PeerSync> { return this.strip(await this.request<PeerSync & { id: number; ok: boolean }>('sync_dm', { channel_id: channelId })); }
  async deleteMessage(a: { msgId: string; seq: number; channelId?: string }): Promise<{ seq: number }> {
    return this.strip(await this.request<{ seq: number; id: number; ok: boolean }>('delete', {
      msg_id: a.msgId, seq: a.seq, ...(a.channelId === undefined ? {} : { channel_id: a.channelId }),
    }));
  }
  async attach(a: PeerAttachRequest): Promise<PeerAttached> {
    const wire: Record<string, unknown> = { name: a.name, mime: a.mime };
    if (a.channelId !== undefined) wire.channel_id = a.channelId;
    if (a.body !== undefined) wire.body = a.body;
    if (a.bytesHex !== undefined) wire.bytes_hex = a.bytesHex;
    if (a.size !== undefined) wire.size = a.size;
    if (a.seed !== undefined) wire.seed = a.seed;
    if (a.w !== undefined) wire.w = a.w;
    if (a.h !== undefined) wire.h = a.h;
    if (a.thumbHex !== undefined) wire.thumb_hex = a.thumbHex;
    return this.strip(await this.request<PeerAttached & { id: number; ok: boolean }>('attach', wire));
  }
  async fetchAttachment(a: { seq: number; index: number; channelId?: string }): Promise<PeerFetched> {
    return this.strip(await this.request<PeerFetched & { id: number; ok: boolean }>('fetch_attachment', {
      seq: a.seq, index: a.index, ...(a.channelId === undefined ? {} : { channel_id: a.channelId }),
    }));
  }
  async blobStatus(a: { blobId: string; channelId?: string }): Promise<{ status: number }> {
    return this.strip(await this.request<{ status: number; id: number; ok: boolean }>('blob_status', {
      blob_id: a.blobId, ...(a.channelId === undefined ? {} : { channel_id: a.channelId }),
    }));
  }
  /** L-E2E-30 `call_join`: reads the channel's call group (L-HTTP-100) and joins it, or registers one; answers the actor's leaf. */
  async callJoin(channelId: string, actor: PeerActor = 'peer'): Promise<PeerCallJoined> {
    return this.strip(await this.request<PeerCallJoined & { id: number; ok: boolean }>('call_join', { actor, channel: channelId }));
  }
  /** L-E2E-30 `call_leave`: the actor proposes its own Remove from the call group and forgets it. */
  async callLeave(channelId: string, actor: PeerActor = 'peer'): Promise<void> {
    await this.request('call_leave', { actor, channel: channelId });
  }
  /** L-E2E-30 `call_members`: the actor's call-group roster after a sync that commits any outstanding Remove, sorted. */
  async callMembers(channelId: string, actor: PeerActor = 'peer'): Promise<PeerCallMembers> {
    return this.strip(await this.request<PeerCallMembers & { id: number; ok: boolean }>('call_members', { actor, channel: channelId }));
  }
  /** `call_read`: `GET /v1/channels/{id}/call` (L-HTTP-100) as the actor reads it: the open call group, the live call,
   *  and who is in its room. */
  async callRead(channelId: string, actor: PeerActor = 'peer'): Promise<PeerCallRead> {
    return this.strip(await this.request<PeerCallRead & { id: number; ok: boolean }>('call_read', { actor, channel: channelId }));
  }
  /** L-E2E-30 `call_start`: `POST /v1/channels/{id}/calls`; `created` is a 201, which rings a DM's other participants. */
  async callStart(channelId: string, actor: PeerActor = 'peer'): Promise<PeerCallStarted> {
    return this.strip(await this.request<PeerCallStarted & { id: number; ok: boolean }>('call_start', { actor, channel: channelId }));
  }
  /** L-E2E-30 `call_decline`: declines with the expires_ts of the last op 53 state 1 the actor's gateway received (0 without one). */
  async callDecline(callId: string, actor: PeerActor = 'peer'): Promise<void> {
    await this.request('call_decline', { actor, call: callId });
  }
  /** `forge_device`: a second device of the peer whose signed-list entry carries the wrong tier; the instance admits its leaf, a validating client fails it (E_TIER_MISMATCH). It acts as actor 'second'. */
  async forgeDevice(): Promise<{ device_id: string; version: number }> {
    return this.strip(await this.request<{ device_id: string; version: number; id: number; ok: boolean }>('forge_device', {}));
  }
  /** `call_check`: the signed-list refresh a browser runs for the call group's pending leaves, then its send-block code (null when clear). */
  async callCheck(channelId: string, actor: PeerActor = 'peer'): Promise<PeerCallCheck> {
    return this.strip(await this.request<PeerCallCheck & { id: number; ok: boolean }>('call_check', { actor, channel: channelId }));
  }
  async close(): Promise<void> {
    this.child.stdin.end();
    await new Promise<void>((done) => {
      const timer = setTimeout(() => { this.child.kill('SIGKILL'); done(); }, 5_000);
      this.child.once('exit', () => { clearTimeout(timer); done(); });
    });
  }
}
