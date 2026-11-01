// One typed function per web-1 route. Core-built bodies pass through unchanged; raw answers return unchanged.
import { CborError, arr, bin, decode, encode, opt, str, u53, u64, type CborInput, type CborValue } from '../cbor';
import { decodeBoundedArray } from '../state/bounded';
import { toHex } from '../hex';
import { HttpClient, type Bucket, type HttpRequest } from './client';
import { DillaHttpError } from './errors';
import { negotiateSession, type SessionCapabilities } from './negotiate-session';
import { BIO_MAX_CHARS, DISPLAY_MAX_CHARS, STATUS_MAX_CHARS, profileTextProblem, profileTextValue, type ProfileTextProblem } from '../profile-text';
export { BIO_MAX_CHARS, DISPLAY_MAX_CHARS, STATUS_MAX_CHARS, profileTextProblem, profileTextValue };
export type { ProfileTextProblem };

export const MEMBERS_PAGE = 200;
export const HANDSHAKES_MAX = 512;
export const MESSAGES_MAX = 256;
export const WELCOMES_PAGE = 64;
export const MODERATION_REMOVALS_PAGE = 250;
// A backup is capped (protocol/09: 256 B kind 0, about 1 MiB kind 1) and sits on the sign-up, sign-in and revoke
// paths, so it gets a longer per-attempt bound than the 30 s default rather than none. Blobs stay unbounded.
const BACKUP_TIMEOUT_MS = 120_000;

export interface Instance { instanceId: Uint8Array; generation: bigint; name: string; registrationMode: 0 | 1 | 2;
  authMethods: number[]; policyVersion: bigint; externalSenderPub: Uint8Array; }
export interface InviteInfo { communityId: Uint8Array | null; communityName: string | null; expires: bigint; }
export interface AccountCreated { userId: Uint8Array; deviceId: Uint8Array; token: string; expires: bigint; }
export interface Challenge { nonce: Uint8Array; expires: bigint; }
export interface EstablishedSession { token: string; scope: 0 | 1 | 2; userId: Uint8Array; deviceId: Uint8Array;
  expires: bigint; idleExpires: bigint; generation: bigint; }
/** A served device-list head; `raw` is the response body (on wire 4 it carries the account-kind attestation as a fifth
 *  element: state/trust.ts `chainRow` is the four-element list body the core decodes). */
export interface DeviceListRecord { version: bigint; blob: Uint8Array; raw: Uint8Array; }
/** What a login assertion still owes (protocol/09 § Second factors): 0 nothing, 1 a TOTP code or a backup code, 2 a passkey
 *  login or a backup code (the account has passkeys and no app). */
export interface PasswordLogin { assertion: string; needs: 0 | 1 | 2; }
export interface BackupItem { kind: 0 | 1; chunkSeq: number; size: number; created: bigint; }
export interface BackupObject { object: Uint8Array; created: bigint; }
export interface BackupStored { blobId: Uint8Array; size: number; created: boolean; }
export interface DeviceRow { id: Uint8Array; tier: 0 | 1; signerTier: 0 | 1; verifiedAt: bigint | null; revokedAt: bigint | null; lastSeen: bigint;
  /** The owner's name for the device; null when unnamed, and for a row from an instance before device names. */ label: string | null; }
export interface DeviceListHistory { raw: Uint8Array; count: number; lastVersion: bigint | null; }
export interface DmOpened { channelId: Uint8Array; created: boolean; }
export interface DmRow { channelId: Uint8Array; kind: 3 | 4; members: Uint8Array[]; }
export interface Ticket { ticket: string; expires: bigint; }
export interface CommunityRow { id: Uint8Array; name: string; owner: Uint8Array; policyVersion: bigint; }
/** web3.9: the built-in AFK channel. `pending` is true when the server's sweep moved this device and it has not joined yet.
 *  AW4: `away` is the community's away roster, each member and the unix second it became away; empty from a web3.9 host. */
/** `pending`: this device has a pending cut. AW5 fix round 3: `reason` says why (device-scoped, any community's read):
 *  1 the idle sweep moved it, 2 this user chose AFK on another device or tab; 0 none. A fixture may leave `reason` out
 *  (then 1 when pending). */
export interface AFKSnapshot { channelId: Uint8Array | null; timeoutSeconds: number; pending: boolean; reason?: 0 | 1 | 2; away: AFKAwayRow[] }
export interface AFKAwayRow { userId: Uint8Array; since: number }
/** 09 § GET /v1/communities/{id}/afk: the away roster holds at most 1024 entries. */
const AFK_AWAY_MAX = 1024;
export interface AFKSettings { channelId: Uint8Array | null; timeoutSeconds: number }
export interface ChannelRow { id: Uint8Array; kind: number; mode: number; visibility: number; parentId: Uint8Array | null;
  name: string; topic: string; position: number; slowmode: number; seq: bigint; textGroupId: Uint8Array | null; }
export interface MemberRow { userId: Uint8Array; username: string; display: string; kind: 0 | 1; nick: string; roleIds: Uint8Array[];
  /** The account's status message and bio (EOD E6); '' from an instance that predates them. */
  statusMsg?: string; bio?: string; }
export interface SeqEpoch { seq: bigint; epoch: bigint; }
export interface RowsPage { raw: Uint8Array; count: number; lastSeq: bigint | null; }
export interface ModerationRemoval { revision: bigint; proof: Uint8Array; signature: Uint8Array; acceptedAt: bigint; }
export interface ModerationRemovalPage { rows: ModerationRemoval[]; nextRevision: bigint; }
/** Opaque current-interaction-context proof; only the core can verify or interpret its signed payload. */
export interface InteractionAdmissionContextProof { payload: Uint8Array; signature: Uint8Array; }
export interface BotCommandStatusRow { installationId: Uint8Array; botUserId: Uint8Array | null; state: number; revision: bigint; }
export interface WelcomesPage { raw: Uint8Array; count: number; lastId: bigint | null; }
export interface MessageUploaded { raw: Uint8Array; seq: bigint; }
export interface AccountMe { userId: Uint8Array; username: string; display: string; kind: number; flags: bigint; presenceChoice?: number;
  /** EOD E6: '' from an instance that predates them. */
  statusMsg?: string; bio?: string; }
/** PATCH /v1/accounts/me: a field left out is left alone, '' clears it. */
export interface ProfilePatch { display?: string; statusMsg?: string; bio?: string; }
export interface Limits { maxCiphertextBytes: number; keypackagesPerDevice: number; keypackageRefillThreshold: number;
  heartbeatMs: number; maxFrameBytes: number; }
// ---- web-3 calls (L-HTTP-100..104, L-TS-55) ----
/** The decode bounds of L-TS-55: participants of one call and calls of one community; beyond them E_CBOR_SHAPE. */
export const CALL_PARTICIPANTS_MAX = 1024;
export const COMMUNITY_CALLS_MAX = 4096;
export interface CallParticipant { userId: Uint8Array; deviceId: Uint8Array; flags: number; }
export interface ChannelCall { groupId: Uint8Array | null; callId: Uint8Array | null; participants: CallParticipant[]; }
export interface CommunityCall { channelId: Uint8Array; callId: Uint8Array; participants: CallParticipant[]; }
export type IceServer = [urls: string[], username: string, credential: string];
export interface CallStarted { created: boolean; callId: Uint8Array; groupId: Uint8Array; livekitUrl: string; token: string;
  iceServers: IceServer[]; caps: [number, number, number];
  /** The users a start that opened a DM or group-DM call rang (the seventh element, WB product walk I6); null when the
   *  answer has six elements (any other start, or an instance from before it). */
  rung: Uint8Array[] | null; }
export type CallStats = [number, number | null, number, number, number, number];
// ---- v0.1.0-web3.1: create a server, a channel and an invite ----
/** The instance's own bounds (internal/api): maxCommunityNameBytes, maxChannelNameBytes, maxInviteUses, maxInviteTTL. */
export const COMMUNITY_NAME_MAX_BYTES = 255;
export const CHANNEL_NAME_MAX_BYTES = 100;
export const INVITE_USES_MAX = 1000;
export const INVITE_TTL_MAX_S = 30 * 24 * 3600;
/** The largest position the instance stores (maxChannelPosition). */
const CHANNEL_POSITION_MAX = 2 ** 31 - 1;
export interface CommunityCreated { id: Uint8Array; roleEveryone: Uint8Array; policyVersion: bigint; }
export interface ChannelCreated { id: Uint8Array; mode: number; visibility: number; }
export interface InviteCreated { inviteId: Uint8Array; code: string; url: string; expires: bigint; }
export type NewChannelKind = 'text' | 'voice';
/** The slow-mode steps in seconds the instance accepts (0 is off; the instance's own ceiling is six hours). */
export const SLOW_STEPS = [0, 5, 10, 30, 60, 300, 900, 3600, 21600] as const;

// ---- wave 1: server admin and moderation (flow 08) ----
/** The instance's own bounds (internal/api): the owner's rank, roles per community, bans, invites, a topic and a ban reason. */
export const OWNER_TOP = 2 ** 31;
export const ROLES_MAX = 250;
export const BANS_MAX = 10_000;
export const INVITES_MAX = 10_000;
export const TOPIC_MAX_BYTES = 1024;
export const BAN_REASON_MAX_BYTES = 512;
/** GET /v1/communities/{id}/permissions: the caller's resolved community-wide bits and rank (0..OWNER_TOP). */
export interface PermissionsRow { bits: bigint; top: number }
/** The installation (protocol/10). A widened read (the manager's GET and the music-settings answer, Task 10c) adds the
 *  restart counters, the runtime's reported version and the Music page's defaults; the mutation answers carry only the
 *  eight base fields, so those are absent there. */
export interface BotInstallationRow { id: Uint8Array; communityId: Uint8Array; state: 0 | 1 | 2 | 3 | 4 | 5;
  revision: bigint; userId: Uint8Array | null; deviceId: Uint8Array | null; capabilities: number; updatedAt: bigint;
  extra?: { restartRequested: bigint; restartHandled: bigint; version: string | null; musicVolume: number; musicIdleMinutes: number } }
export interface BotJoinRoleRow { roleId: Uint8Array | null; installationId: Uint8Array }
/** Capability bits (protocol/10): 0..7 the original byte, 8 polls and 9 reminders (Task 10c). */
export const BOT_CAPABILITIES_MAX = 0x3ff;
/** The Music page's options (Task 10c); the instance and the Bot accept nothing else. */
export const BOT_MUSIC_VOLUMES = [25, 50, 75, 100] as const;
export const BOT_MUSIC_IDLE_MINUTES = [5, 15, 30] as const;
export interface BotSpotifyAttempt { attemptId: Uint8Array; authorizeUrl: string; expires: bigint }
export interface SelfRoleRow { id: Uint8Array; name: string; position: number; selfSelected: boolean }
export interface RolePanelOptionInput { emoji: string; roleId: Uint8Array }
export interface RoleRow { id: Uint8Array; name: string; color: number; position: number; allow: number; deny: number;
  hoist: boolean; mentionable: boolean }
export interface BanRow { userId: Uint8Array; reason: string; byUser: Uint8Array; created: bigint; expires: bigint | null;
  username: string; display: string }
export interface InviteRow { id: Uint8Array; communityId: Uint8Array | null; createdBy: Uint8Array | null; maxUses: number;
  uses: number; created: bigint; expires: bigint; revokedAt: bigint | null }
export interface ChannelPatch { name?: string; topic?: string; parent?: Uint8Array | 'top'; position?: number; slowmode?: number }

// ---- wave 2a: roles and private channels (flow 09) ----
/** The instance's own bounds (internal/api roles.go, channels.go): overwrites per community, a role name, a role colour,
 *  the 26 permission bits, and overwrites in one channel create. */
export const OVERWRITES_MAX = 10_000;
export const ROLE_NAME_MAX_BYTES = 100;
export const ROLE_COLOR_MAX = 0xffffff;
export const PERM_ALL = 0x3ffffff;
export const OVERWRITES_PER_CREATE_MAX = 100;
export interface OverwriteRow { channelId: Uint8Array; kind: 0 | 1; targetId: Uint8Array; allow: number; deny: number }
export interface NewOverwrite { kind: 0 | 1; targetId: Uint8Array; allow: number; deny: number }
export interface RolePatch { name?: string; color?: number; position?: number; allow?: number; deny?: number; hoist?: boolean; mentionable?: boolean }
export interface NewRole { name: string; color: number; position: number; allow: number; deny: number; hoist: boolean; mentionable: boolean }

// ---- web3.6: second factors (flow 10) ----
/** The instance's own bounds (internal/api auth.go): backup codes in one answer, passkeys on one account, a passkey name. */
export const BACKUP_CODES_MAX = 16;
export const PASSKEYS_MAX = 1024;
export const PASSKEY_NAME_MAX_BYTES = 64;
export interface PasskeyRow { credId: Uint8Array; name: string; created: number; lastUsed: number | null }
export interface Factors { owed: boolean; totp: number | null; passkeys: PasskeyRow[]; backupCodesLeft: number; hasPassword: boolean;
  /** Task 11 (Q7): when the password was last changed (unix seconds); null when unknown, absent from an older server. */
  passwordChanged?: number | null }
export interface TotpEnrol { secret: string; uri: string }
export interface Ceremony { ceremonyId: Uint8Array; options: string }
export interface PasskeyRegistered { credId: Uint8Array; backupCodes: string[] }
export interface PasskeyLogin { assertion: string; needs: 0 | 1 }
export interface StepUpToken { token: string; expires: bigint }
export interface GroupHealState { epoch: bigint; nextSeq: bigint; generation: bigint; needFromSeq: bigint; needsHeal: boolean }

/** E8 (internal/api/gifs.go): the query, offset and id bounds, and the size caps of a preview and a full GIF. */
export const GIF_QUERY_MAX_BYTES = 100;
export const GIF_OFFSET_MAX = 4999;
export const GIF_PREVIEW_MAX_BYTES = 5 * 1024 * 1024;
export const GIF_FULL_MAX_BYTES = 25 * 1024 * 1024;
const GIF_ID = /^[A-Za-z0-9]{1,64}$/;
/** A Giphy key as the instance accepts it: 8 to 64 letters and digits. It is only ever sent, never kept or shown back. */
const GIF_KEY = /^[A-Za-z0-9]{8,64}$/;
export const GIF_RATINGS = ['g', 'pg', 'pg-13', 'r'] as const;
export type GifRating = typeof GIF_RATINGS[number];
export interface GifStatus { on: boolean; rating: GifRating }
/** A search result. `width` and `height` are the preview's size in pixels: whole numbers of 1 to GIF_DIM_MAX, or both 0 when unknown. */
export interface GifItem { id: string; title: string; width: number; height: number }
/** The largest preview side the instance passes on. */
export const GIF_DIM_MAX = 4000;
/** A preview's width and height as small positive integers, or both 0: the instance is not trusted to have checked. */
export function gifDims(w: number, h: number): [number, number] {
  const ok = (n: number) => Number.isInteger(n) && n >= 1 && n <= GIF_DIM_MAX;
  return ok(w) && ok(h) ? [w, h] : [0, 0];
}
export interface GifPage { items: GifItem[]; next: number | null }
function id(name: string, v: Uint8Array): string {
  if (v.length !== 16) throw new RangeError('E_ROUTE_INPUT: ' + name + ' must be 16 bytes');
  return toHex(v);
}
/** An id given by the caller, checked as input (E_ROUTE_INPUT), not decoded (E_CBOR_SHAPE). */
function inputId(name: string, v: Uint8Array): Uint8Array {
  id(name, v);
  return v;
}
function digest(name: string, v: Uint8Array): string {
  if (v.length !== 32) throw new RangeError('E_ROUTE_INPUT: ' + name + ' must be 32 bytes');
  return toHex(v);
}
function blobPath(channelId: Uint8Array, blobId: Uint8Array): string {
  return `/v1/channels/${id('channelId', channelId)}/blobs/${digest('blobId', blobId)}`;
}
function count(name: string, v: bigint): string {
  if (v < 0n) throw new RangeError('E_ROUTE_INPUT: ' + name + ' must not be negative');
  return String(v);
}
function kindOf(kind: number): string {
  if (kind !== 0 && kind !== 1) throw new RangeError('E_ROUTE_INPUT: kind must be 0 or 1');
  return String(kind);
}
function limit(name: string, v: number, max: number): number {
  if (!Number.isInteger(v) || v < 1 || v > max) throw new RangeError('E_ROUTE_INPUT: ' + name + ' must be 1..' + max);
  return v;
}
function atLeast(v: CborValue, n: number): CborValue[] {
  const items = arr(v);
  if (items.length < n) throw new CborError('E_CBOR_SHAPE', 'expected at least ' + n + ' elements, got ' + items.length);
  return items;
}
function oneOf<T extends number>(v: CborValue, allowed: readonly T[]): T {
  const n = u53(v);
  if (!allowed.includes(n as T)) throw new CborError('E_CBOR_RANGE', 'expected one of ' + allowed.join(',') + ', got ' + n);
  return n as T;
}
function page(raw: Uint8Array): { raw: Uint8Array; count: number; last: bigint | null } {
  const rows = arr(decode(raw));
  const last = rows.length ? u64(atLeast(rows[rows.length - 1], 1)[0]) : null;
  return { raw, count: rows.length, last };
}
function compareBytes(a: Uint8Array, b: Uint8Array): number {
  for (let i = 0; i < 16; i++) if (a[i] !== b[i]) return a[i] - b[i];
  return 0;
}
/** A list of at most `max` elements; a longer one is E_CBOR_SHAPE (L-TS-55's decode bound). */
function bounded(v: CborValue, max: number, what: string): CborValue[] {
  const items = arr(v);
  if (items.length > max) throw new CborError('E_CBOR_SHAPE', `${what}: more than ${String(max)}`);
  return items;
}
function participants(v: CborValue): CallParticipant[] {
  return bounded(v, CALL_PARTICIPANTS_MAX, 'participants').map((row) => {
    const a = atLeast(row, 3);
    const flags = u53(a[2]);
    if (flags > 31) throw new CborError('E_CBOR_RANGE', 'voice flags above 31');
    return { userId: bin(a[0], 16), deviceId: bin(a[1], 16), flags };
  });
}
const UTF8 = new TextEncoder();
const CONTROL = /\p{Cc}/u;
/** A name as the instance checks it (validCommunityName, validChannelText): 1..max UTF-8 bytes, no control character. */
export function nameProblem(v: unknown, maxBytes: number): string | null {
  if (typeof v !== 'string') return 'is not a string';
  const bytes = UTF8.encode(v).length;
  if (bytes < 1 || bytes > maxBytes) return `must be 1..${String(maxBytes)} bytes`;
  if (CONTROL.test(v)) return 'contains a control character';
  return null;
}
const PROFILE_PROBLEM_TEXT: Record<ProfileTextProblem, string> = { long: 'is too long', chars: 'has characters that cannot be shown', blank: 'is only whitespace' };
function checkedProfile(field: string, v: string | undefined, maxChars: number, multiline = false): string | null {
  if (v === undefined) return null;
  const problem = profileTextProblem(v, maxChars, multiline);
  if (problem !== null) throw new RangeError(`E_ROUTE_INPUT: ${field} ${PROFILE_PROBLEM_TEXT[problem]}`);
  return profileTextValue(v, multiline);
}
function checkedName(field: string, v: string, maxBytes: number): string {
  const problem = nameProblem(v, maxBytes);
  if (problem !== null) throw new RangeError(`E_ROUTE_INPUT: ${field} ${problem}`);
  return v;
}
function between(field: string, v: number, min: number, max: number): number {
  if (!Number.isSafeInteger(v) || v < min || v > max) throw new RangeError(`E_ROUTE_INPUT: ${field} must be ${String(min)}..${String(max)}`);
  return v;
}
function slowStep(v: number): number {
  if (!(SLOW_STEPS as readonly number[]).includes(v)) throw new RangeError('E_ROUTE_INPUT: slowmode must be one of ' + SLOW_STEPS.join(', '));
  return v;
}
/** An invite link as the instance answers it: an absolute http(s) URL (it is shown and copied, never followed here) whose
 *  path is `/i/<code>` for the answered code, with no query or fragment (WB m6): the instance's own landing, nothing else. */
function inviteUrl(v: CborValue, code: string): string {
  const s = str(v);
  let url: URL | null = null;
  try { url = new URL(s); } catch { /* not a URL */ }
  if (url === null || (url.protocol !== 'https:' && url.protocol !== 'http:')) {
    throw new CborError('E_CBOR_RANGE', 'the invite url is not an http(s) URL');
  }
  if (url.pathname !== '/i/' + encodeURIComponent(code) || url.search !== '' || url.hash !== '' || s.includes('#')) {
    throw new CborError('E_CBOR_RANGE', 'the invite url is not the landing of the answered code');
  }
  return s;
}
function callPath(callId: Uint8Array, rest = ''): string {
  return `/v1/calls/${id('callId', callId)}${rest}`;
}
function memberRow(value: CborValue): MemberRow {
  const row = atLeast(value, 7);
  return { userId: bin(row[0], 16), nick: str(row[2]), roleIds: row[3] === null ? [] : arr(row[3]).map((v) => bin(v, 16)),
    username: str(row[4]), display: str(row[5]), kind: oneOf(row[6], [0, 1]),
    statusMsg: row.length > 7 ? str(row[7]) : '', bio: row.length > 8 ? str(row[8]) : '' };
}

/** Free text as the instance checks it (validChannelText, multiline): at most `maxBytes` UTF-8 bytes; newline and tab pass,
 *  every other control character does not. An empty text is allowed (a cleared topic, a ban with no reason). */
function checkedText(field: string, v: string, maxBytes: number): string {
  if (typeof v !== 'string') throw new RangeError(`E_ROUTE_INPUT: ${field} is not a string`);
  if (UTF8.encode(v).length > maxBytes) throw new RangeError(`E_ROUTE_INPUT: ${field} must be at most ${String(maxBytes)} bytes`);
  for (const ch of v) if (ch !== '\n' && ch !== '\t' && CONTROL.test(ch)) throw new RangeError(`E_ROUTE_INPUT: ${field} contains a control character`);
  return v;
}
/** AW5 fix round 3: GET …/afk's pending reason (protocol/07): 0 none, 1 the idle sweep, 2 AFK chosen elsewhere. Round 3b:
 *  any non-zero value is pending, and one this client does not know (3 or more) reads as 2, the calm reason; never an
 *  error. */
function pendingReason(v: CborValue): 0 | 1 | 2 {
  const n = u53(v);
  return n === 0 ? 0 : n === 1 ? 1 : 2;
}
function flag(v: CborValue): boolean {
  const n = u53(v);
  if (n > 1) throw new CborError('E_CBOR_RANGE', 'expected 0 or 1, got ' + String(n));
  return n === 1;
}
/** A permission word as the instance stores it: only the 25 defined bits are kept (a 64-bit word may carry more). */
function bits(v: CborValue): number {
  return Number(u64(v) & BigInt(PERM_ALL));
}
function overwriteRow(value: CborValue): OverwriteRow {
  const a = atLeast(value, 5);
  return { channelId: bin(a[0], 16), kind: oneOf(a[1], [0, 1] as const), targetId: bin(a[2], 16), allow: bits(a[3]), deny: bits(a[4]) };
}
function permBits(field: string, v: number): number {
  return between(field, v, 0, PERM_ALL);
}
function roleName(v: string): string {
  return checkedName('name', v, ROLE_NAME_MAX_BYTES);
}
function overwritePath(channelId: Uint8Array, kind: number, targetId: Uint8Array): string {
  return `/v1/channels/${id('channelId', channelId)}/overwrites/${kindOf(kind)}/${id('targetId', targetId)}`;
}
function rolePath(communityId: Uint8Array, userId: Uint8Array, roleId: Uint8Array): string {
  return `/v1/communities/${id('communityId', communityId)}/members/${id('userId', userId)}/roles/${id('roleId', roleId)}`;
}
function roleRow(value: CborValue): RoleRow {
  const a = atLeast(value, 8);
  return { id: bin(a[0], 16), name: str(a[1]), color: u53(a[2]), position: u53(a[3]), allow: bits(a[4]), deny: bits(a[5]),
    hoist: flag(a[6]), mentionable: flag(a[7]) };
}
function banRow(value: CborValue): BanRow {
  const a = atLeast(value, 5);
  return { userId: bin(a[0], 16), reason: str(a[1]), byUser: bin(a[2], 16), created: u64(a[3]), expires: opt(a[4], u64),
    username: a.length > 5 ? str(a[5]) : '', display: a.length > 6 ? str(a[6]) : '' };
}
function botInstallationRow(value: CborValue, communityId: Uint8Array): BotInstallationRow {
  const a = arr(value);
  if (a.length !== 8 && a.length !== 13) throw new CborError('E_CBOR_SHAPE', 'expected eight or thirteen bot installation fields');
  const row: BotInstallationRow = { id: bin(a[0], 16), communityId: bin(a[1], 16), state: oneOf(a[2], [0, 1, 2, 3, 4, 5] as const),
    revision: u64(a[3]), userId: opt(a[4], v => bin(v, 16)), deviceId: opt(a[5], v => bin(v, 16)),
    capabilities: u53(a[6]), updatedAt: u64(a[7]) };
  if (toHex(row.communityId) !== toHex(communityId) || row.revision === 0n || row.capabilities > BOT_CAPABILITIES_MAX) {
    throw new CborError('E_CBOR_RANGE', 'bot installation scope, revision or capabilities invalid');
  }
  if (a.length === 13) {
    const version = opt(a[10], str);
    const extra = { restartRequested: u64(a[8]), restartHandled: u64(a[9]), version, musicVolume: u53(a[11]), musicIdleMinutes: u53(a[12]) };
    if (extra.restartHandled > extra.restartRequested || (version !== null && (version.length < 1 || version.length > 32))
      || !(BOT_MUSIC_VOLUMES as readonly number[]).includes(extra.musicVolume)
      || !(BOT_MUSIC_IDLE_MINUTES as readonly number[]).includes(extra.musicIdleMinutes)) {
      throw new CborError('E_CBOR_RANGE', 'bot installation restart, version or music settings invalid');
    }
    row.extra = extra;
  }
  return row;
}
function botSpotifyAttempt(value: CborValue): BotSpotifyAttempt {
  const a = arr(value);
  if (a.length !== 3) throw new CborError('E_CBOR_SHAPE', 'expected three Spotify attempt fields');
  const attemptId = bin(a[0], 16);
  const authorizeUrl = str(a[1]);
  const expires = u64(a[2]);
  let url: URL;
  try { url = new URL(authorizeUrl); } catch { throw new CborError('E_CBOR_RANGE', 'invalid Spotify authorization URL'); }
  const names = ['response_type', 'client_id', 'redirect_uri', 'scope', 'state', 'code_challenge_method', 'code_challenge'];
  const unique = names.every(name => url.searchParams.getAll(name).length === 1);
  const redirect = url.searchParams.get('redirect_uri');
  let redirectUrl: URL | null = null;
  if (redirect !== null) { try { redirectUrl = new URL(redirect); } catch { /* rejected below */ } }
  if (url.protocol !== 'https:' || url.hostname !== 'accounts.spotify.com' || url.port !== '' || url.pathname !== '/authorize'
    || url.username !== '' || url.password !== '' || url.hash !== '' || !unique || [...url.searchParams.keys()].some(k => !names.includes(k))
    || url.searchParams.get('response_type') !== 'code' || url.searchParams.get('scope') !== 'streaming user-read-email user-read-private'
    || url.searchParams.get('code_challenge_method') !== 'S256'
    || !(url.searchParams.get('client_id') ?? '').trim() || redirectUrl === null || redirectUrl.protocol !== 'https:'
    || redirectUrl.hostname === '' || redirectUrl.username !== '' || redirectUrl.password !== '' || redirectUrl.hash !== ''
    || !/^[A-Za-z0-9_-]{43}$/u.test(url.searchParams.get('state') ?? '')
    || !/^[A-Za-z0-9_-]{43}$/u.test(url.searchParams.get('code_challenge') ?? '')) {
    throw new CborError('E_CBOR_RANGE', 'invalid Spotify authorization URL');
  }
  if (expires === 0n) throw new CborError('E_CBOR_RANGE', 'invalid Spotify attempt expiry');
  return { attemptId, authorizeUrl, expires };
}
function inviteRow(value: CborValue): InviteRow {
  const a = atLeast(value, 9);
  return { id: bin(a[0], 16), communityId: opt(a[1], (v) => bin(v, 16)), createdBy: opt(a[2], (v) => bin(v, 16)),
    maxUses: u53(a[4]), uses: u53(a[5]), created: u64(a[6]), expires: u64(a[7]), revokedAt: opt(a[8], u64) };
}

function oneOfInput<T extends number>(name: string, v: T, allowed: readonly T[]): T {
  if (!allowed.includes(v)) throw new RangeError('E_ROUTE_INPUT: ' + name + ' must be one of ' + allowed.join(','));
  return v;
}
function backupCodes(v: CborValue): string[] {
  return bounded(v, BACKUP_CODES_MAX, 'backup codes').map(str);
}
function ceremony(v: CborValue): Ceremony {
  const a = atLeast(v, 2);
  return { ceremonyId: bin(a[0], 16), options: str(a[1]) };
}
function stepUpToken(raw: Uint8Array): StepUpToken {
  const a = atLeast(decode(raw), 2);
  return { token: str(a[0]), expires: u64(a[1]) };
}
function passkeyRow(value: CborValue): PasskeyRow {
  const a = atLeast(value, 4);
  return { credId: bin(a[0]), name: str(a[1]), created: u53(a[2]), lastUsed: opt(a[3], u53) };
}
export class Routes {
  constructor(private readonly http: HttpClient) {}

  /** Negotiate capabilities for the current authenticated HTTP bearer session. */
  negotiateSession(): Promise<SessionCapabilities> {
    return negotiateSession(this.http);
  }

  private async body(method: HttpRequest['method'], path: string, bucket: Bucket, idempotent: boolean,
    options: Pick<HttpRequest, 'body' | 'auth' | 'accept' | 'ok' | 'retry429' | 'contentType' | 'maxBodyBytes' | 'signal'> = {}): Promise<Uint8Array> {
    return (await this.http.request({ method, path, bucket, idempotent, ...options })).body;
  }

  /** GET /v1/instance with the session's bearer whenever one exists (`E, F` under instance.discovery = "session"); before
   *  sign-in there is no token and nothing is sent. */
  async getInstance(): Promise<Instance> {
    const a = atLeast(decode(await this.body('GET', '/v1/instance', 'read', true)), 11);
    return { instanceId: bin(a[3], 16), generation: u64(a[4]), name: str(a[5]), registrationMode: oneOf(a[6], [0, 1, 2]),
      authMethods: arr(a[7]).map(u53), policyVersion: u64(a[8]), externalSenderPub: bin(a[10], 32) };
  }
  async getInvite(code: string): Promise<InviteInfo> {
    const a = atLeast(decode(await this.body('GET', '/i/' + encodeURIComponent(code), 'none', true,
      { auth: false, accept: 'application/cbor' })), 4);
    return { communityId: opt(a[1], (v) => bin(v, 16)), expires: u64(a[2]), communityName: opt(a[3], str) };
  }
  async postAccount(body: Uint8Array): Promise<AccountCreated> {
    const a = atLeast(decode(await this.body('POST', '/v1/accounts', 'none', false, { body, auth: false })), 4);
    return { userId: bin(a[0], 16), deviceId: bin(a[1], 16), token: str(a[2]), expires: u64(a[3]) };
  }
  async postChallenge(deviceId: Uint8Array): Promise<Challenge> {
    const path = `/v1/devices/${id('deviceId', deviceId)}/sessions/challenge`;
    const a = atLeast(decode(await this.body('POST', path, 'none', true, { body: encode([]), auth: false })), 2);
    return { nonce: bin(a[0], 32), expires: u64(a[1]) };
  }
  async postSession(deviceId: Uint8Array, body: Uint8Array): Promise<EstablishedSession> {
    const path = `/v1/devices/${id('deviceId', deviceId)}/sessions`;
    const a = atLeast(decode(await this.body('POST', path, 'none', false, { body, auth: false, retry429: false })), 7);
    return { token: str(a[0]), scope: oneOf(a[1], [0, 1, 2]), userId: bin(a[2], 16), deviceId: bin(a[3], 16),
      expires: u64(a[4]), idleExpires: u64(a[5]), generation: u64(a[6]) };
  }
  async postSessionPending(deviceId: Uint8Array, body: Uint8Array): Promise<EstablishedSession> {
    return this.postSession(deviceId, body);
  }
  async putDeviceList(userId: Uint8Array, body: Uint8Array): Promise<void> {
    await this.body('PUT', `/v1/users/${id('userId', userId)}/device-list`, 'write', true, { body });
  }
  async getDeviceList(userId: Uint8Array): Promise<DeviceListRecord | null> {
    const response = await this.http.request({ method: 'GET', path: `/v1/users/${id('userId', userId)}/device-list`,
      bucket: 'read', idempotent: true, ok: [200, 404], maxBodyBytes: 1024 * 1024 });
    if (response.status === 404) return null;
    const a = atLeast(decode(response.body), 2);
    return { version: u64(a[0]), blob: bin(a[1]), raw: response.body };
  }
  async postKeyPackages(body: Uint8Array): Promise<number> {
    return u53(atLeast(decode(await this.body('POST', '/v1/keypackages', 'write', false, { body })), 1)[0]);
  }
  async postTicket(): Promise<Ticket> {
    const a = atLeast(decode(await this.body('POST', '/v1/gateway/ticket', 'write', false, { body: encode([]) })), 2);
    return { ticket: str(a[0]), expires: u64(a[1]) };
  }
  async listCommunities(): Promise<CommunityRow[]> {
    return arr(decode(await this.body('GET', '/v1/communities', 'read', true))).map((value) => {
      const a = atLeast(value, 4);
      return { id: bin(a[0], 16), name: str(a[1]), owner: bin(a[2], 16), policyVersion: u64(a[3]) };
    });
  }
  /** GET /v1/communities/{id}/afk: [id|null, timeout, [] (the retired occupant list, ignored), pending?, away?]; the dilla
   *  CBOR has no booleans, so pending and exempt are uint 0 or 1; a 3-element answer is not pending, and an answer of
   *  fewer than five elements (web3.9) has no one away (AW4). */
  async getAFK(communityId: Uint8Array): Promise<AFKSnapshot> {
    const a = atLeast(decode(await this.body('GET', `/v1/communities/${id('communityId', communityId)}/afk`, 'read', true)), 3);
    bounded(a[2], 1024, 'AFK occupants');
    // Fix round 1 (N3): a row with since 0 (cleared) is skipped, and a repeated user keeps its first row.
    const away: AFKAwayRow[] = [];
    const seen = new Set<string>();
    for (const value of a.length > 4 ? bounded(a[4], AFK_AWAY_MAX, 'AFK away') : []) {
      const row = atLeast(value, 2);
      const userId = bin(row[0], 16);
      const since = u53(row[1]);
      const key = toHex(userId);
      if (since === 0 || seen.has(key)) continue;
      seen.add(key);
      away.push({ userId, since });
    }
    const reason = a.length > 3 ? pendingReason(a[3]) : 0;
    return { channelId: opt(a[0], v => bin(v, 16)), timeoutSeconds: u53(a[1]), pending: reason !== 0, reason, away };
  }
  /** PUT /v1/communities/{id}/afk/away: [] (AW4, R-AW3). The caller's user goes to the AFK channel: the instance cuts every
   *  device of the user from its call, then marks the user away. There is no route that clears away (Q3). */
  async putAFKAway(communityId: Uint8Array): Promise<void> {
    await this.body('PUT', `/v1/communities/${id('communityId', communityId)}/afk/away`, 'write', true,
      { body: encode([]), ok: [204] });
  }
  async getAFKSettings(communityId: Uint8Array): Promise<AFKSettings> {
    const a = atLeast(decode(await this.body('GET', `/v1/communities/${id('communityId', communityId)}/afk/settings`, 'read', true)), 2);
    return { channelId: opt(a[0], v => bin(v, 16)), timeoutSeconds: u53(a[1]) };
  }
  /** PUT /v1/communities/{id}/afk/settings: [timeout_seconds]; the channel is the built-in one. */
  async setAFKSettings(communityId: Uint8Array, timeoutSeconds: number): Promise<void> {
    await this.body('PUT', `/v1/communities/${id('communityId', communityId)}/afk/settings`, 'write', true,
      { body: encode([timeoutSeconds]), ok: [204] });
  }
  /** POST /v1/communities/{id}/afk/activity: [channel] or, from a device that cannot be idle-detected, [channel, true]. */
  async sendAFKActivity(communityId: Uint8Array, channelId: Uint8Array, exempt?: boolean): Promise<void> {
    await this.body('POST', `/v1/communities/${id('communityId', communityId)}/afk/activity`, 'write', true,
      { body: encode(exempt === true ? [inputId('channelId', channelId), 1] : [inputId('channelId', channelId)]), ok: [204] });
  }
  async joinCommunity(communityId: Uint8Array, invite: string | null): Promise<void> {
    await this.body('POST', `/v1/communities/${id('communityId', communityId)}/join`, 'none', true, { body: encode([invite]) });
  }
  /** POST /v1/communities: [name, policy, min_account_age_seconds, require_mod_2fa] with the instance's defaults (an empty
   *  policy is "{}"); the caller becomes its owner and first member. */
  async createCommunity(communityName: string): Promise<CommunityCreated> {
    const body = encode([checkedName('name', communityName, COMMUNITY_NAME_MAX_BYTES), new Uint8Array(0), 0, 0]);
    const a = atLeast(decode(await this.body('POST', '/v1/communities', 'write', false, { body })), 3);
    return { id: bin(a[0], 16), roleEveryone: bin(a[1], 16), policyVersion: u64(a[2]) };
  }
  /** POST /v1/communities/{id}/channels: [kind, mode, visibility, parent_id, name, topic, position, slowmode_seconds] for a
   *  channel in mode 0 (the server cannot read it) and visibility 0, with no topic and no slow mode; top level, or inside the
   *  category `parentId` (the server answers 404 for one the caller cannot view). */
  async createChannel(communityId: Uint8Array,
    channel: { kind: NewChannelKind; name: string; position: number; parentId?: Uint8Array; overwrites?: readonly NewOverwrite[] }): Promise<ChannelCreated> {
    const kind = channel.kind === 'text' ? 0 : channel.kind === 'voice' ? 1 : -1;
    if (kind < 0) throw new RangeError('E_ROUTE_INPUT: kind must be text or voice');
    const fields: CborInput[] = [kind, 0, 0, channel.parentId === undefined ? null : inputId('parentId', channel.parentId), checkedName('name', channel.name, CHANNEL_NAME_MAX_BYTES), '',
      between('position', channel.position, 0, CHANNEL_POSITION_MAX), 0];
    const overwrites = channel.overwrites ?? [];
    if (overwrites.length > OVERWRITES_PER_CREATE_MAX) throw new RangeError('E_ROUTE_INPUT: overwrites must be at most ' + String(OVERWRITES_PER_CREATE_MAX));
    // A non-empty list is the ninth element, so the channel is private from birth; an empty one keeps the eight-element form.
    if (overwrites.length > 0) {
      fields.push(overwrites.map((o) => [Number(kindOf(o.kind)), inputId('targetId', o.targetId), permBits('allow', o.allow), permBits('deny', o.deny)]));
    }
    const body = encode(fields);
    const raw = await this.body('POST', `/v1/communities/${id('communityId', communityId)}/channels`, 'write', false, { body });
    const a = atLeast(decode(raw), 3);
    return { id: bin(a[0], 16), mode: u53(a[1]), visibility: u53(a[2]) };
  }
  /** POST /v1/communities/{id}/invites: [max_uses, ttl_seconds, grants_admin 0]; the answer's url is the instance's own
   *  invite link (`<base>/i/<code>`), which the join dialog and the invite landing page both accept. max_uses 0 is any
   *  number of uses and ttl_seconds 0 never expires (Task 10c; the instance asks manage_community for either); the
   *  answer's expires is then 0. */
  async createInvite(communityId: Uint8Array, invite: { maxUses: number; ttlSeconds: number }): Promise<InviteCreated> {
    const body = encode([between('maxUses', invite.maxUses, 0, INVITE_USES_MAX), between('ttlSeconds', invite.ttlSeconds, 0, INVITE_TTL_MAX_S), 0]);
    const raw = await this.body('POST', `/v1/communities/${id('communityId', communityId)}/invites`, 'write', false, { body });
    const a = atLeast(decode(raw), 4);
    const code = str(a[1]);
    return { inviteId: bin(a[0], 16), code, url: inviteUrl(a[2], code), expires: u64(a[3]) };
  }
  async listChannels(communityId: Uint8Array): Promise<ChannelRow[]> {
    const raw = await this.body('GET', `/v1/communities/${id('communityId', communityId)}/channels`, 'read', true);
    return arr(decode(raw)).map((value) => {
      const a = atLeast(value, 11);
      return { id: bin(a[0], 16), kind: u53(a[1]), mode: u53(a[2]), visibility: u53(a[3]),
        parentId: opt(a[4], (v) => bin(v, 16)), name: str(a[5]), topic: str(a[6]), position: u53(a[7]), slowmode: u53(a[8]),
        seq: u64(a[9]), textGroupId: opt(a[10], (v) => bin(v, 16)) };
    });
  }
  async listMembers(communityId: Uint8Array): Promise<MemberRow[]> {
    const path = `/v1/communities/${id('communityId', communityId)}/members`;
    const result: MemberRow[] = [];
    let after: Uint8Array | null = null;
    for (;;) {
      const raw = await this.body('GET', path + (after ? '?after=' + toHex(after) : ''), 'read', true);
      const rows = arr(decode(raw));
      const parsed = rows.map(memberRow);
      result.push(...parsed);
      if (rows.length < MEMBERS_PAGE) return result;
      const last = parsed[parsed.length - 1].userId;
      if (after && compareBytes(last, after) <= 0) throw new CborError('E_CBOR_SHAPE', 'members page did not advance');
      after = last;
    }
  }
  // ---- wave 1: server admin and moderation (flow 08) ----
  /** GET /v1/communities/{id}/permissions: [bits, top]; top beyond 0..OWNER_TOP is E_CBOR_RANGE. */
  async getPermissions(communityId: Uint8Array): Promise<PermissionsRow> {
    const a = atLeast(decode(await this.body('GET', `/v1/communities/${id('communityId', communityId)}/permissions`, 'read', true)), 2);
    const top = u53(a[1]);
    if (top > OWNER_TOP) throw new CborError('E_CBOR_RANGE', 'rank above ' + String(OWNER_TOP));
    return { bits: u64(a[0]), top };
  }
  /** POST a ClientCore-signed moderation proof; the instance rechecks permission and hierarchy atomically. */
  async postModerationRemoval(groupId: Uint8Array, signedBody: Uint8Array): Promise<void> {
    await this.body('POST', `/v1/groups/${id('groupId', groupId)}/moderation/removals`, 'write', true,
      { body: signedBody, maxBodyBytes: 128 });
  }
  /** CBOR null means the community has no active installation; refusals remain visible. */
  async getBotInstallation(communityId: Uint8Array): Promise<BotInstallationRow | null> {
    const path = `/v1/communities/${id('communityId', communityId)}/bot`;
    const row = decode(await this.body('GET', path, 'read', true));
    return row === null ? null : botInstallationRow(row, communityId);
  }
  /** Member-safe status for deciding whether to show the shared Bot command transport. */
  async getBotCommandStatus(communityId: Uint8Array): Promise<BotCommandStatusRow | null> {
    const path = `/v1/communities/${id('communityId', communityId)}/bot/command-status`;
    const value = decode(await this.body('GET', path, 'read', true));
    if (value === null) return null;
    const a = arr(value, 5);
    if (a[0] !== 1n) throw new CborError('E_CBOR_SHAPE', 'unsupported Bot command status version');
    const installationId = bin(a[1], 16);
    const botUserId = opt(a[2], value => bin(value, 16));
    const state = u53(a[3]);
    if (state < 1 || state > 4) throw new CborError('E_CBOR_RANGE', 'invalid Bot installation state');
    return { installationId, botUserId, state, revision: u64(a[4]) };
  }
  async patchBotProfile(communityId: Uint8Array, display: string): Promise<string> {
    const normalized = checkedProfile('display', display, DISPLAY_MAX_CHARS);
    if (!normalized) throw new RangeError('E_ROUTE_INPUT: bot display name is required');
    const raw = await this.body('PATCH', `/v1/communities/${id('communityId', communityId)}/bot/profile`, 'write', true,
      { body: encode([normalized]), retry429: false });
    const fields = arr(decode(raw), 1);
    const saved = str(fields[0]);
    if (saved === '' || profileTextProblem(saved, DISPLAY_MAX_CHARS) !== null) throw new CborError('E_CBOR_SHAPE', 'invalid bot display name');
    return saved;
  }
  /** GET /bot-join-role returns [role id or null, active installation id]. 404 means no active bot. */
  async getBotJoinRole(communityId: Uint8Array): Promise<BotJoinRoleRow | null> {
    try {
      const value = decode(await this.body('GET', `/v1/communities/${id('communityId', communityId)}/bot-join-role`, 'read', true));
      const a = arr(value);
      if (a.length !== 2) throw new CborError('E_CBOR_SHAPE', 'expected bot join-role and installation ids');
      return { roleId: a[0] === null ? null : bin(a[0], 16), installationId: bin(a[1], 16) };
    } catch (e) { if (e instanceof DillaHttpError && e.status === 404) return null; throw e; }
  }
  /** Request ID is retained across HTTP retries; a zero role ID clears the setting. */
  async configureBotJoinRole(communityId: Uint8Array, requestId: Uint8Array, roleId: Uint8Array, installationId: Uint8Array): Promise<Uint8Array> {
    const ack = arr(decode(await this.body('PUT', `/v1/communities/${id('communityId', communityId)}/bot-join-role`, 'write', true,
      { body: encode([inputId('requestId', requestId), inputId('roleId', roleId), inputId('installationId', installationId)]), retry429: false })));
    if (ack.length !== 2 || !bin(ack[0], 16).every((v, i) => v === requestId[i])) {
      throw new CborError('E_CBOR_SHAPE', 'invalid bot join-role acknowledgement');
    }
    return bin(ack[1], 16);
  }
  /** POST /v1/communities/{id}/bot/restart (Task 10c): asks the runtime to restart this installation; a request while
   *  one is pending joins it. Answers [restart_requested, restart_handled]. */
  async restartBot(communityId: Uint8Array, installationId: Uint8Array): Promise<{ requested: bigint; handled: bigint }> {
    const a = arr(decode(await this.body('POST', `/v1/communities/${id('communityId', communityId)}/bot/restart`, 'write', true,
      { body: encode([inputId('installationId', installationId)]), retry429: false })));
    if (a.length !== 2) throw new CborError('E_CBOR_SHAPE', 'expected restart counters');
    const requested = u64(a[0]), handled = u64(a[1]);
    if (handled > requested) throw new CborError('E_CBOR_RANGE', 'restart counters invalid');
    return { requested, handled };
  }
  /** PUT /v1/communities/{id}/bot/music-settings (Task 10c): the Music page's default volume and idle-leave minutes. */
  async setBotMusicSettings(communityId: Uint8Array, installationId: Uint8Array, volume: number, idleMinutes: number): Promise<BotInstallationRow> {
    if (!(BOT_MUSIC_VOLUMES as readonly number[]).includes(volume) || !(BOT_MUSIC_IDLE_MINUTES as readonly number[]).includes(idleMinutes)) {
      throw new RangeError('E_ROUTE_INPUT: volume must be 25, 50, 75 or 100 and idle minutes 5, 15 or 30');
    }
    return botInstallationRow(decode(await this.body('PUT', `/v1/communities/${id('communityId', communityId)}/bot/music-settings`, 'write', true,
      { body: encode([inputId('installationId', installationId), volume, idleMinutes]), retry429: false })), communityId);
  }
  /** GET /v1/communities/{id}/self-roles: manager-approved roles and this member's self grants. */
  async listSelfRoles(communityId: Uint8Array): Promise<SelfRoleRow[]> {
    const rows = bounded(decode(await this.body('GET', `/v1/communities/${id('communityId', communityId)}/self-roles`, 'read', true)), ROLES_MAX, 'self roles');
    return rows.map(value => {
      const a = arr(value);
      if (a.length !== 4) throw new CborError('E_CBOR_SHAPE', 'expected four self-role fields');
      const position = u53(a[2]);
      if (position > OWNER_TOP) throw new CborError('E_CBOR_RANGE', 'self-role position out of range');
      return { id: bin(a[0], 16), name: str(a[1]), position, selfSelected: oneOf(a[3], [0, 1] as const) === 1 };
    });
  }
  async configureSelfRoles(communityId: Uint8Array, requestId: Uint8Array, roleIds: Uint8Array[]): Promise<void> {
    if (roleIds.length > ROLES_MAX) throw new RangeError('E_ROUTE_INPUT: at most 250 roles');
    const seen = new Set<string>();
    for (const roleId of roleIds) { const hex = id('roleId', roleId); if (seen.has(hex)) throw new RangeError('E_ROUTE_INPUT: duplicate role'); seen.add(hex); }
    const ack = arr(decode(await this.body('PUT', `/v1/communities/${id('communityId', communityId)}/self-roles/policy`, 'write', true,
      { body: encode([inputId('requestId', requestId), roleIds]), retry429: false })));
    if (ack.length !== 2 || !bin(ack[0], 16).every((v, i) => v === requestId[i]) || u53(ack[1]) !== roleIds.length) {
      throw new CborError('E_CBOR_SHAPE', 'invalid self-role policy acknowledgement');
    }
  }
  async selectSelfRole(communityId: Uint8Array, roleId: Uint8Array, requestId: Uint8Array, enabled: boolean): Promise<void> {
    await this.http.request({ method: 'PUT', path: `/v1/communities/${id('communityId', communityId)}/self-roles/${id('roleId', roleId)}`,
      bucket: 'write', idempotent: true, body: encode([inputId('requestId', requestId), enabled ? 1 : 0]), retry429: false,
      ok: [204], maxBodyBytes: 0 });
  }
  /** Registers mappings only after the Bot-authored message has an accepted sequence. */
  async registerRolePanel(communityId: Uint8Array, requestId: Uint8Array, channelId: Uint8Array, messageSeq: bigint,
    options: RolePanelOptionInput[]): Promise<void> {
    if (messageSeq < 1n || messageSeq > (1n << 63n) - 1n || options.length < 1 || options.length > 100) {
      throw new RangeError('E_ROUTE_INPUT: invalid role panel identity or option count');
    }
    const emojis = new Set<string>(); const roles = new Set<string>();
    const rows = options.map(option => {
      const emoji = str(option.emoji);
      const emojiBytes = new TextEncoder().encode(emoji).length;
      if (emojiBytes < 1 || emojiBytes > 32) throw new RangeError('E_ROUTE_INPUT: invalid role panel emoji');
      const role = id('roleId', option.roleId);
      if (emojis.has(emoji) || roles.has(role)) throw new RangeError('E_ROUTE_INPUT: duplicate role panel mapping');
      emojis.add(emoji); roles.add(role);
      return [emoji, inputId('roleId', option.roleId)];
    });
    const ack = arr(decode(await this.body('PUT', `/v1/communities/${id('communityId', communityId)}/role-panels`, 'write', true,
      { body: encode([inputId('requestId', requestId), inputId('channelId', channelId), messageSeq, rows]), retry429: false })));
    if (ack.length !== 2 || !bin(ack[0], 16).every((v, i) => v === requestId[i]) || u53(ack[1]) !== options.length) {
      throw new CborError('E_CBOR_SHAPE', 'invalid role panel acknowledgement');
    }
  }
  /** Applies a human-authenticated role-panel action only after the matching panel event was accepted. */
  async applyRolePanelReaction(communityId: Uint8Array, channelId: Uint8Array, messageSeq: bigint,
    groupId: Uint8Array, eventSeq: bigint, emoji: string, enabled: boolean): Promise<void> {
    if (messageSeq < 1n || messageSeq > (1n << 63n) - 1n || eventSeq < 1n || eventSeq > (1n << 63n) - 1n
      || new TextEncoder().encode(emoji).length < 1 || new TextEncoder().encode(emoji).length > 32) {
      throw new RangeError('E_ROUTE_INPUT: invalid role panel reaction');
    }
    await this.http.request({ method: 'POST',
      path: `/v1/communities/${id('communityId', communityId)}/role-panels/${id('channelId', channelId)}/${messageSeq}/reaction`,
      bucket: 'write', idempotent: true, body: encode([inputId('groupId', groupId), eventSeq, emoji, enabled ? 1 : 0]),
      retry429: false, ok: [204], maxBodyBytes: 0 });
  }
  /** Request ID is retained across HTTP retries; the server binds it to actor, payload and result. */
  async configureBotInstallation(communityId: Uint8Array, requestId: Uint8Array, expectedRevision: bigint | null,
    capabilities: number, stepUp: string): Promise<BotInstallationRow> {
    if (expectedRevision !== null && expectedRevision < 1n) throw new RangeError('E_ROUTE_INPUT: revision must be positive');
    if (!Number.isSafeInteger(capabilities) || capabilities < 0 || capabilities > BOT_CAPABILITIES_MAX) throw new RangeError('E_ROUTE_INPUT: invalid capabilities');
    if (typeof stepUp !== 'string' || stepUp.length === 0) throw new RangeError('E_ROUTE_INPUT: step-up token is required');
    const body = encode([inputId('requestId', requestId), expectedRevision, capabilities, stepUp]);
    // v=2 (Task 10c fix round 1): this client sends the widened mask, Polls and Reminders included; without it
    // the server keeps those two bits as they are.
    return botInstallationRow(decode(await this.body('PUT', `/v1/communities/${id('communityId', communityId)}/bot?v=2`,
      'write', true, { body, retry429: false })), communityId);
  }
  /** Arms a one-use automatic moderation grant. Config plaintext is represented only by its digest here. */
  async armAutoModerationGrant(groupId: Uint8Array, requestId: Uint8Array, communityId: Uint8Array,
    expectedRevision: bigint, requestExpires: bigint, configHash: Uint8Array, stepUp: string): Promise<{ token: string; expires: bigint }> {
    if (expectedRevision < 0n || expectedRevision > (1n << 63n) - 1n || requestExpires <= 0n
      || configHash.length !== 32 || stepUp.length < 1 || stepUp.length > 4096) throw new RangeError('E_ROUTE_INPUT: invalid automatic moderation grant tuple');
    const raw = await this.body('POST', `/v1/groups/${id('groupId', groupId)}/automod-grants/arm`, 'write', true,
      { body: encode([inputId('requestId', requestId), inputId('communityId', communityId), expectedRevision, requestExpires, configHash, stepUp]), retry429: false });
    const fields = arr(decode(raw), 2);
    if (typeof fields[0] !== 'string' || fields[0].length < 1 || fields[0].length > 128) throw new CborError('E_CBOR_SHAPE', 'invalid automatic moderation grant token');
    return { token: fields[0], expires: u64(fields[1]) };
  }
  async disableBotInstallation(communityId: Uint8Array, requestId: Uint8Array, expectedRevision: bigint): Promise<BotInstallationRow> {
    if (expectedRevision < 1n) throw new RangeError('E_ROUTE_INPUT: revision must be positive');
    const body = encode([inputId('requestId', requestId), expectedRevision]);
    return botInstallationRow(decode(await this.body('POST', `/v1/communities/${id('communityId', communityId)}/bot/disable`,
      'write', true, { body, retry429: false })), communityId);
  }
  async getBotSpotifyConfig(communityId: Uint8Array): Promise<{ clientId: string; redirectUrl: string }> {
    const raw = await this.body('GET', `/v1/communities/${id('communityId', communityId)}/bot/spotify/config`, 'read', true);
    const a = arr(decode(raw));
    if (a.length !== 2) throw new CborError('E_CBOR_SHAPE', 'invalid Spotify application setting');
    return { clientId: str(a[0]), redirectUrl: str(a[1]) };
  }
  async beginBotSpotifyAttempt(communityId: Uint8Array, stepUp: string, clientId: string): Promise<BotSpotifyAttempt> {
    if (typeof stepUp !== 'string' || stepUp.length < 1 || stepUp.length > 4096) throw new RangeError('E_ROUTE_INPUT: invalid step-up token');
    if (!/^[A-Za-z0-9]{1,256}$/u.test(clientId)) throw new RangeError('E_ROUTE_INPUT: invalid Spotify client ID');
    const path = `/v1/communities/${id('communityId', communityId)}/bot/spotify/attempts`;
    const body = encode([stepUp, clientId]);
    const response = await this.http.request({ method: 'POST', path, bucket: 'write', idempotent: false,
      body, retry429: false, ok: [200], maxBodyBytes: 4096 });
    return botSpotifyAttempt(decode(response.body));
  }
  async callbackBotSpotifyAttempt(communityId: Uint8Array, attemptId: Uint8Array, state: string, code: string): Promise<void> {
    const stateBytes = UTF8.encode(state);
    const codeBytes = UTF8.encode(code);
    if (stateBytes.length < 1 || stateBytes.length > 256 || CONTROL.test(state)
      || codeBytes.length < 1 || codeBytes.length > 2048 || /[\r\n\0]/u.test(code)) {
      throw new RangeError('E_ROUTE_INPUT: invalid Spotify callback');
    }
    const path = `/v1/communities/${id('communityId', communityId)}/bot/spotify/attempts/${id('attemptId', attemptId)}/callback`;
    const response = await this.http.request({ method: 'POST', path, bucket: 'write', idempotent: false,
      body: encode([state, code]), retry429: false, ok: [202], maxBodyBytes: 256 });
    const a = arr(decode(response.body));
    if (a.length !== 1 || u53(a[0]) !== 1) throw new CborError('E_CBOR_SHAPE', 'invalid Spotify callback acknowledgement');
  }
  async cancelBotSpotifyAttempt(communityId: Uint8Array, attemptId: Uint8Array): Promise<void> {
    await this.http.request({ method: 'DELETE',
      path: `/v1/communities/${id('communityId', communityId)}/bot/spotify/attempts/${id('attemptId', attemptId)}`,
      bucket: 'write', idempotent: true, ok: [204], maxBodyBytes: 0 });
  }
  /** GET /v1/communities/{id}/roles: [[id, name, color, position, allow, deny, hoist, mentionable]], at most ROLES_MAX. */
  async listRoles(communityId: Uint8Array): Promise<RoleRow[]> {
    const raw = await this.body('GET', `/v1/communities/${id('communityId', communityId)}/roles`, 'read', true);
    return bounded(decode(raw), ROLES_MAX, 'roles').map(roleRow);
  }
  /** PATCH /v1/communities/{id}: [name, policy, min_account_age, require_mod_2fa] with only the name set; answers [policy_version]. */
  async patchCommunity(communityId: Uint8Array, patch: { name: string }): Promise<bigint> {
    const body = encode([checkedName('name', patch.name, COMMUNITY_NAME_MAX_BYTES), null, null, null]);
    const a = atLeast(decode(await this.body('PATCH', `/v1/communities/${id('communityId', communityId)}`, 'write', true, { body })), 1);
    return u64(a[0]);
  }
  async deleteCommunity(communityId: Uint8Array): Promise<void> {
    await this.body('DELETE', `/v1/communities/${id('communityId', communityId)}`, 'write', true);
  }
  async leaveCommunity(communityId: Uint8Array): Promise<void> {
    await this.body('POST', `/v1/communities/${id('communityId', communityId)}/leave`, 'write', false, { body: encode([]) });
  }
  /** POST /v1/communities/{id}/owner: [user_id]; the owner hands the server to another member. */
  async transferOwnership(communityId: Uint8Array, userId: Uint8Array): Promise<void> {
    await this.body('POST', `/v1/communities/${id('communityId', communityId)}/owner`, 'write', false, { body: encode([bin(userId, 16)]) });
  }
  /** POST /v1/communities/{id}/channels with kind 2 (a category): [2, 0, 0, null, name, '', position, 0]. */
  async createCategory(communityId: Uint8Array, category: { name: string; position: number }): Promise<ChannelCreated> {
    const body = encode([2, 0, 0, null, checkedName('name', category.name, CHANNEL_NAME_MAX_BYTES), '',
      between('position', category.position, 0, CHANNEL_POSITION_MAX), 0]);
    const a = atLeast(decode(await this.body('POST', `/v1/communities/${id('communityId', communityId)}/channels`, 'write', false, { body })), 3);
    return { id: bin(a[0], 16), mode: u53(a[1]), visibility: u53(a[2]) };
  }
  /** PATCH /v1/channels/{id}: [name, topic, mode, visibility, parent_id, position, slowmode], null leaves a field alone and
   *  parent 'top' (sixteen zero bytes) moves the channel out of its category. */
  async patchChannel(channelId: Uint8Array, patch: ChannelPatch): Promise<void> {
    const parent = patch.parent === undefined ? null : patch.parent === 'top' ? new Uint8Array(16) : bin(patch.parent, 16);
    const body = encode([patch.name === undefined ? null : checkedName('name', patch.name, CHANNEL_NAME_MAX_BYTES),
      patch.topic === undefined ? null : checkedText('topic', patch.topic, TOPIC_MAX_BYTES), null, null, parent,
      patch.position === undefined ? null : between('position', patch.position, 0, CHANNEL_POSITION_MAX),
      patch.slowmode === undefined ? null : slowStep(patch.slowmode)]);
    await this.body('PATCH', `/v1/channels/${id('channelId', channelId)}`, 'write', true, { body });
  }
  async deleteChannel(channelId: Uint8Array): Promise<void> {
    await this.body('DELETE', `/v1/channels/${id('channelId', channelId)}`, 'write', true);
  }
  async kickMember(communityId: Uint8Array, userId: Uint8Array): Promise<void> {
    await this.body('DELETE', `/v1/communities/${id('communityId', communityId)}/members/${id('userId', userId)}`, 'write', true);
  }
  /** PUT /v1/communities/{id}/bans/{user}: [reason, expires|null]. */
  async putBan(communityId: Uint8Array, userId: Uint8Array, ban: { reason: string; expires: bigint | null }): Promise<void> {
    const body = encode([checkedText('reason', ban.reason, BAN_REASON_MAX_BYTES), ban.expires]);
    await this.body('PUT', `/v1/communities/${id('communityId', communityId)}/bans/${id('userId', userId)}`, 'write', true, { body });
  }
  async deleteBan(communityId: Uint8Array, userId: Uint8Array): Promise<void> {
    await this.body('DELETE', `/v1/communities/${id('communityId', communityId)}/bans/${id('userId', userId)}`, 'write', true);
  }
  /** GET /v1/communities/{id}/members/{user_id}/timeout: [expires|null], as Unix seconds. */
  async getMemberTimeout(communityId: Uint8Array, userId: Uint8Array): Promise<bigint | null> {
    const a = atLeast(decode(await this.body('GET', `/v1/communities/${id('communityId', communityId)}/members/${id('userId', userId)}/timeout`, 'read', true)), 1);
    return opt(a[0], u64);
  }
  /** PUT .../timeout: [request_id, expires]. The request id is stable across HTTP retries. */
  async putMemberTimeout(communityId: Uint8Array, userId: Uint8Array, requestId: Uint8Array, expires: bigint): Promise<bigint> {
    if (expires < 0n || expires > 0xffff_ffff_ffff_ffffn) throw new RangeError('E_ROUTE_INPUT: expires must be uint64');
    const a = atLeast(decode(await this.body('PUT', `/v1/communities/${id('communityId', communityId)}/members/${id('userId', userId)}/timeout`,
      'write', true, { body: encode([inputId('requestId', requestId), expires]) })), 2);
    if (!bin(a[0], 16).every((v, i) => v === requestId[i])) throw new CborError('E_CBOR_SHAPE', 'timeout response request id mismatch');
    const answeredExpires = u64(a[1]);
    if (answeredExpires !== expires) throw new CborError('E_CBOR_SHAPE', 'timeout response expiry mismatch');
    return answeredExpires;
  }
  /** DELETE .../timeout: [request_id]; the server answers 204 No Content. */
  async deleteMemberTimeout(communityId: Uint8Array, userId: Uint8Array, requestId: Uint8Array): Promise<void> {
    await this.body('DELETE', `/v1/communities/${id('communityId', communityId)}/members/${id('userId', userId)}/timeout`, 'write', true,
      { body: encode([inputId('requestId', requestId)]), ok: [204] });
  }
  /** POST .../warning: [request_id] → [request_id, action_id, community_revision, accepted_at]. The request carries no reason. */
  async warnMember(communityId: Uint8Array, userId: Uint8Array, requestId: Uint8Array): Promise<{ requestId: Uint8Array; actionId: Uint8Array; revision: number; acceptedAt: number }> {
    const a = atLeast(decode(await this.body('POST', `/v1/communities/${id('communityId', communityId)}/members/${id('userId', userId)}/warning`,
      'write', true, { body: encode([inputId('requestId', requestId)]) })), 4);
    if (!bin(a[0], 16).every((v, i) => v === requestId[i])) throw new CborError('E_CBOR_SHAPE', 'warning response request id mismatch');
    return { requestId: bin(a[0], 16), actionId: bin(a[1], 16), revision: u53(a[2]), acceptedAt: u53(a[3]) };
  }
  /** GET authoritative accepted manual-warning receipt: [request, action, community, actor user, actor device, target, revision, acceptedAt]. */
  async acceptedManualWarning(installationId: Uint8Array, requestId: Uint8Array): Promise<{ requestId: Uint8Array; actionId: Uint8Array; communityId: Uint8Array; actorUserId: Uint8Array; actorDeviceId: Uint8Array; targetUserId: Uint8Array; revision: number; acceptedAt: number }> {
    const a = atLeast(decode(await this.body('GET', `/v1/bot-runtime/installations/${id('installationId', installationId)}/moderation/warnings/${id('requestId', requestId)}`, 'read', true)), 8);
    if (!bin(a[0], 16).every((v, i) => v === requestId[i])) throw new CborError('E_CBOR_SHAPE', 'warning receipt request id mismatch');
    return { requestId: bin(a[0], 16), actionId: bin(a[1], 16), communityId: bin(a[2], 16), actorUserId: bin(a[3], 16),
      actorDeviceId: bin(a[4], 16), targetUserId: bin(a[5], 16), revision: u53(a[6]), acceptedAt: u53(a[7]) };
  }
  /** GET /v1/communities/{id}/bans: [[user_id, reason, by_user, created, expires|null, username, display]]; the last two
   *  are absent from an older instance and read as ''. */
  async listBans(communityId: Uint8Array): Promise<BanRow[]> {
    const raw = await this.body('GET', `/v1/communities/${id('communityId', communityId)}/bans`, 'read', true);
    return bounded(decode(raw), BANS_MAX, 'bans').map(banRow);
  }
  async listInvites(communityId: Uint8Array): Promise<InviteRow[]> {
    const raw = await this.body('GET', `/v1/communities/${id('communityId', communityId)}/invites`, 'read', true);
    return bounded(decode(raw), INVITES_MAX, 'invites').map(inviteRow);
  }
  async revokeInvite(inviteId: Uint8Array): Promise<void> {
    await this.body('DELETE', `/v1/invites/${id('inviteId', inviteId)}`, 'write', true);
  }
  // ---- wave 2a: roles and private channels (flow 09) ----
  /** POST /v1/communities/{id}/roles: [name, color, position, allow, deny, hoist, mentionable]; answers [role_id]. */
  async createRole(communityId: Uint8Array, role: NewRole): Promise<{ roleId: Uint8Array }> {
    const body = encode([roleName(role.name), between('color', role.color, 0, ROLE_COLOR_MAX), between('position', role.position, 1, CHANNEL_POSITION_MAX),
      permBits('allow', role.allow), permBits('deny', role.deny), role.hoist ? 1 : 0, role.mentionable ? 1 : 0]);
    const a = atLeast(decode(await this.body('POST', `/v1/communities/${id('communityId', communityId)}/roles`, 'write', false, { body })), 1);
    return { roleId: bin(a[0], 16) };
  }
  /** PATCH /v1/roles/{id}: [name, color, position, allow, deny, hoist, mentionable], null leaves a field alone. */
  async patchRole(roleId: Uint8Array, patch: RolePatch): Promise<void> {
    const flagOf = (v: boolean | undefined): number | null => (v === undefined ? null : v ? 1 : 0);
    const body = encode([patch.name === undefined ? null : roleName(patch.name),
      patch.color === undefined ? null : between('color', patch.color, 0, ROLE_COLOR_MAX),
      patch.position === undefined ? null : between('position', patch.position, 1, CHANNEL_POSITION_MAX),
      patch.allow === undefined ? null : permBits('allow', patch.allow), patch.deny === undefined ? null : permBits('deny', patch.deny),
      flagOf(patch.hoist), flagOf(patch.mentionable)]);
    await this.body('PATCH', `/v1/roles/${id('roleId', roleId)}`, 'write', true, { body });
  }
  async deleteRole(roleId: Uint8Array): Promise<void> {
    await this.body('DELETE', `/v1/roles/${id('roleId', roleId)}`, 'write', true);
  }
  /** PUT /v1/communities/{id}/members/{user}/roles/{role} with an empty array body. */
  async grantRole(communityId: Uint8Array, userId: Uint8Array, roleId: Uint8Array): Promise<void> {
    await this.body('PUT', rolePath(communityId, userId, roleId), 'write', true, { body: encode([]) });
  }
  async revokeRole(communityId: Uint8Array, userId: Uint8Array, roleId: Uint8Array): Promise<void> {
    await this.body('DELETE', rolePath(communityId, userId, roleId), 'write', true);
  }
  /** PUT /v1/channels/{id}/overwrites/{kind}/{target}: [allow, deny]; kind 0 is a role, 1 a member. */
  async putOverwrite(channelId: Uint8Array, kind: 0 | 1, targetId: Uint8Array, bitsPair: { allow: number; deny: number }): Promise<void> {
    const path = overwritePath(channelId, kind, targetId);
    const body = encode([permBits('allow', bitsPair.allow), permBits('deny', bitsPair.deny)]);
    await this.body('PUT', path, 'write', true, { body });
  }
  async deleteOverwrite(channelId: Uint8Array, kind: 0 | 1, targetId: Uint8Array): Promise<void> {
    await this.body('DELETE', overwritePath(channelId, kind, targetId), 'write', true);
  }
  /** GET /v1/communities/{id}/overwrites: [[channel_id, kind, target_id, allow, deny]] for every channel the caller may view. */
  async listOverwrites(communityId: Uint8Array): Promise<OverwriteRow[]> {
    const raw = await this.body('GET', `/v1/communities/${id('communityId', communityId)}/overwrites`, 'read', true);
    return bounded(decode(raw), OVERWRITES_MAX, 'overwrites').map(overwriteRow);
  }
  async postGroup(body: Uint8Array): Promise<{ nextSeq: bigint }> {
    const a = atLeast(decode(await this.body('POST', '/v1/groups', 'write', false, { body })), 2);
    return { nextSeq: u64(a[1]) };
  }
  async getGroupInfo(groupId: Uint8Array): Promise<Uint8Array> {
    return this.body('GET', `/v1/groups/${id('groupId', groupId)}/info`, 'read', true);
  }
  async getGroupTree(groupId: Uint8Array): Promise<Uint8Array> {
    return this.body('GET', `/v1/groups/${id('groupId', groupId)}/tree`, 'read', true);
  }
  /** GET /v1/groups/{id}/heal-state: member-scoped server cursor and whether restored history needs repair. */
  async getGroupHealState(groupId: Uint8Array): Promise<GroupHealState> {
    const a = arr(decode(await this.body('GET', `/v1/groups/${id('groupId', groupId)}/heal-state`, 'read', true)));
    if (a.length !== 5) throw new CborError('E_CBOR_SHAPE', 'group heal state has five elements');
    const needsHeal = u53(a[4]);
    if (needsHeal !== 0 && needsHeal !== 1) throw new CborError('E_CBOR_SHAPE', 'needs_heal is 0 or 1');
    return { epoch: u64(a[0]), nextSeq: u64(a[1]), generation: u64(a[2]), needFromSeq: u64(a[3]), needsHeal: needsHeal === 1 };
  }
  /** POST /v1/groups/{id}/heal: sends the core's CBOR [group_info, tail, ratchet_tree] snapshot. */
  async postGroupHeal(groupId: Uint8Array, snapshot: Uint8Array): Promise<{ epoch: bigint; nextSeq: bigint }> {
    const a = arr(decode(await this.body('POST', `/v1/groups/${id('groupId', groupId)}/heal`, 'write', false, { body: snapshot })));
    if (a.length !== 2) throw new CborError('E_CBOR_SHAPE', 'group heal result has two elements');
    return { epoch: u64(a[0]), nextSeq: u64(a[1]) };
  }
  /** GET /v1/groups/{id}/interaction-admission-context: preserve the signed payload for core verification. */
  async getInteractionAdmissionContext(groupId: Uint8Array, signal?: AbortSignal): Promise<InteractionAdmissionContextProof> {
    const raw = await this.body('GET', `/v1/groups/${id('groupId', groupId)}/interaction-admission-context`, 'read', true,
      { ok: [200], maxBodyBytes: 160 * 1024, signal });
    const proof = arr(decode(raw));
    if (proof.length !== 2) throw new CborError('E_CBOR_SHAPE', 'expected current interaction admission payload and signature');
    return { payload: bin(proof[0]), signature: bin(proof[1], 64) };
  }
  async getHandshakes(groupId: Uint8Array, from: bigint, limitValue: number): Promise<RowsPage> {
    const path = `/v1/groups/${id('groupId', groupId)}/handshakes?from=${count('from', from)}&limit=${limit('limit', limitValue, HANDSHAKES_MAX)}`;
    const p = page(await this.body('GET', path, 'read', true));
    return { raw: p.raw, count: p.count, lastSeq: p.last };
  }
  async getMessages(groupId: Uint8Array, from: bigint, limitValue: number): Promise<RowsPage> {
    const path = `/v1/groups/${id('groupId', groupId)}/messages?from=${count('from', from)}&limit=${limit('limit', limitValue, MESSAGES_MAX)}`;
    const p = page(await this.body('GET', path, 'read', true));
    return { raw: p.raw, count: p.count, lastSeq: p.last };
  }
  async getModerationRemovals(groupId: Uint8Array, after: bigint, limitValue = MODERATION_REMOVALS_PAGE): Promise<ModerationRemovalPage> {
    const path = `/v1/groups/${id('groupId', groupId)}/moderation/removals?after=${count('after', after)}&limit=${limit('limit', limitValue, MODERATION_REMOVALS_PAGE)}`;
    const raw = await this.body('GET', path, 'read', true, { maxBodyBytes: 65_536 });
    const tuple = arr(decode(raw), 2);
    const rows = arr(tuple[0]);
    if (rows.length > limitValue) throw new CborError('E_CBOR_SHAPE', 'too many moderation removals');
    const nextRevision = u64(tuple[1]);
    let expected = after;
    const decoded = rows.map((value) => {
      const row = arr(value, 4);
      expected += 1n;
      const revision = u64(row[0]);
      if (revision !== expected) throw new CborError('E_CBOR_RANGE', 'moderation removal revision gap');
      return { revision, proof: bin(row[1]), signature: bin(row[2], 64), acceptedAt: u64(row[3]) };
    });
    if (nextRevision !== expected || (rows.length === 0 && nextRevision !== after)) throw new CborError('E_CBOR_RANGE', 'moderation removal cursor mismatch');
    return { rows: decoded, nextRevision };
  }
  async getProposals(groupId: Uint8Array): Promise<Uint8Array> {
    return this.body('GET', `/v1/groups/${id('groupId', groupId)}/proposals`, 'read', true);
  }
  async postCommit(groupId: Uint8Array, body: Uint8Array): Promise<SeqEpoch> {
    const a = atLeast(decode(await this.body('POST', `/v1/groups/${id('groupId', groupId)}/commit`, 'commit', false, { body })), 2);
    return { seq: u64(a[0]), epoch: u64(a[1]) };
  }
  async postMessage(groupId: Uint8Array, body: Uint8Array): Promise<MessageUploaded> {
    // CB2: a 429 (slow mode) goes back to the send lane, which requeues the row and arms its own timer; a sleep here
    // would hold the group's lane (frames and catch-up) for up to RETRY.maxWaitMs.
    const raw = await this.body('POST', `/v1/groups/${id('groupId', groupId)}/message`, 'message', false, { body, retry429: false });
    return { raw, seq: u64(atLeast(decode(raw), 1)[0]) };
  }
  async postCursor(groupId: Uint8Array, body: Uint8Array): Promise<void> {
    await this.body('POST', `/v1/groups/${id('groupId', groupId)}/cursor`, 'write', true, { body });
  }
  async postResync(groupId: Uint8Array, body: Uint8Array): Promise<SeqEpoch> {
    const a = atLeast(decode(await this.body('POST', `/v1/groups/${id('groupId', groupId)}/resync`, 'commit', false, { body })), 2);
    return { seq: u64(a[0]), epoch: u64(a[1]) };
  }
  async getWelcomes(after: bigint): Promise<WelcomesPage> {
    const p = page(await this.body('GET', `/v1/welcomes?after=${count('after', after)}&limit=${WELCOMES_PAGE}`, 'read', true));
    return { raw: p.raw, count: p.count, lastId: p.last };
  }
  async deleteWelcome(welcomeId: bigint): Promise<void> {
    await this.body('DELETE', `/v1/welcomes/${count('welcomeId', welcomeId)}`, 'write', true, { ok: [204, 404] });
  }
  async getAccountMe(): Promise<AccountMe> {
    const a = atLeast(decode(await this.body('GET', '/v1/accounts/me', 'read', true)), 5);
    return { userId: bin(a[0], 16), username: str(a[1]), display: str(a[2]), kind: u53(a[3]), flags: u64(a[4]),
      presenceChoice: a.length > 6 ? u53(a[6]) : 1, statusMsg: a.length > 7 ? str(a[7]) : '', bio: a.length > 8 ? str(a[8]) : '' };
  }
  /** PATCH /v1/accounts/me (EOD E6): [display|null, status_msg|null, bio|null] → 204. The text is stored readable by the instance. */
  async patchMe(patch: ProfilePatch): Promise<void> {
    const body = encode([checkedProfile('display', patch.display, DISPLAY_MAX_CHARS), checkedProfile('statusMsg', patch.statusMsg, STATUS_MAX_CHARS),
      checkedProfile('bio', patch.bio, BIO_MAX_CHARS, true)]);
    await this.body('PATCH', '/v1/accounts/me', 'write', true, { body, ok: [204] });
  }
  async setPresenceChoice(choice: number): Promise<void> {
    await this.body('PATCH', '/v1/accounts/me/presence', 'write', true, { body: encode([choice]), ok: [204] });
  }
  async sendPresenceActivity(): Promise<void> {
    await this.body('POST', '/v1/accounts/me/presence/activity', 'write', true, { ok: [204] });
  }
  async getPresenceSnapshot(communityId: Uint8Array): Promise<{ userId: Uint8Array; status: number }[]> {
    return arr(decode(await this.body('GET', `/v1/communities/${toHex(communityId)}/presence`, 'read', true))).map(row => {
      const a = atLeast(row, 2);
      return { userId: bin(a[0], 16), status: u53(a[1]) };
    });
  }
  async getLimits(): Promise<Limits> {
    const a = atLeast(decode(await this.body('GET', '/v1/instance/limits', 'read', true, { auth: false })), 9);
    return { maxCiphertextBytes: u53(a[0]), keypackagesPerDevice: u53(a[4]), keypackageRefillThreshold: u53(a[5]),
      heartbeatMs: u53(a[7]), maxFrameBytes: u53(a[8]) };
  }
  async passwordLogin(username: string, password: string): Promise<PasswordLogin> {
    const a = atLeast(decode(await this.body('POST', '/v1/auth/password/login', 'none', false,
      { body: encode([username, password]), auth: false, retry429: false })), 2);
    return { assertion: str(a[0]), needs: oneOf(a[1], [0, 1, 2]) };
  }
  /** A 429 (the account's lockout) has spent the assertion, so it is not waited out: the person logs in again later. */
  async totpVerify(assertion: string, code: string): Promise<{ assertion: string }> {
    const a = atLeast(decode(await this.body('POST', '/v1/auth/totp/verify', 'none', false,
      { body: encode([assertion, code]), auth: false, retry429: false })), 1);
    return { assertion: str(a[0]) };
  }
  async listBackups(): Promise<BackupItem[]> {
    return arr(decode(await this.body('GET', '/v1/backups', 'read', true))).map((value) => {
      const a = atLeast(value, 4);
      return { kind: oneOf(a[0], [0, 1]), chunkSeq: u53(a[1]), size: u53(a[2]), created: u64(a[3]) };
    });
  }
  async getBackup(kind: 0 | 1): Promise<BackupObject | null> {
    const response = await this.http.request({ method: 'GET', path: `/v1/backups/${kindOf(kind)}/0`, bucket: 'read',
      idempotent: true, ok: [200, 404], timeoutMs: BACKUP_TIMEOUT_MS });
    if (response.status === 404) return null;
    const a = atLeast(decode(response.body), 2);
    return { object: bin(a[0]), created: u64(a[1]) };
  }
  async putBackup(kind: 0 | 1, object: Uint8Array): Promise<BackupStored> {
    const response = await this.http.request({ method: 'PUT', path: `/v1/backups/${kindOf(kind)}/0`, bucket: 'write',
      idempotent: true, body: encode([object]), timeoutMs: BACKUP_TIMEOUT_MS });
    const a = atLeast(decode(response.body), 2);
    return { blobId: bin(a[0], 32), size: u53(a[1]), created: response.status === 201 };
  }
  async listDevices(): Promise<DeviceRow[]> {
    return arr(decode(await this.body('GET', '/v1/devices', 'read', true))).map((value) => {
      const a = atLeast(value, 6);
      return { id: bin(a[0], 16), tier: oneOf(a[1], [0, 1]), signerTier: oneOf(a[2], [0, 1]),
        verifiedAt: opt(a[3], u64), revokedAt: opt(a[4], u64), lastSeen: u64(a[5]),
        label: a.length > 6 ? opt(a[6], str) : null };
    });
  }
  /** PATCH /v1/devices/{id}: [label|null] → 204. Null clears the name. The label is not in the signed device list. */
  async patchDevice(deviceId: Uint8Array, label: string | null): Promise<void> {
    await this.body('PATCH', `/v1/devices/${id('deviceId', deviceId)}`, 'write', true, { body: encode([label]), ok: [204] });
  }
  async deleteSessions(deviceId: Uint8Array): Promise<void> {
    await this.body('DELETE', `/v1/devices/${id('deviceId', deviceId)}/sessions`, 'write', true, { ok: [204] });
  }
  async getDeviceListHistory(userId: Uint8Array, after: bigint): Promise<DeviceListHistory> {
    const path = `/v1/users/${id('userId', userId)}/device-list?after=${count('after', after)}`;
    const raw = await this.body('GET', path, 'read', true, { maxBodyBytes: 1_048_576 });
    const rows = decodeBoundedArray(raw, 64);
    const lastVersion = rows.length ? u64(atLeast(rows[rows.length - 1], 1)[0]) : null;
    return { raw, count: rows.length, lastVersion };
  }
  async postDm(recipients: Uint8Array[]): Promise<DmOpened> {
    if (recipients.length < 1 || recipients.length > 64) throw new RangeError('E_ROUTE_INPUT: recipients must be 1..64');
    recipients.forEach((recipient) => id('recipient', recipient));
    const response = await this.http.request({ method: 'POST', path: '/v1/dms', bucket: 'write', idempotent: false,
      body: encode([recipients]) });
    return { channelId: bin(atLeast(decode(response.body), 1)[0], 16), created: response.status === 201 };
  }
  async listDms(): Promise<DmRow[]> {
    return arr(decode(await this.body('GET', '/v1/dms', 'read', true))).map((value) => {
      const a = atLeast(value, 3);
      return { channelId: bin(a[0], 16), kind: oneOf(a[1], [3, 4]),
        members: a[2] === null ? [] : arr(a[2]).map((member) => bin(member, 16)) };
    });
  }
  async getChannel(channelId: Uint8Array): Promise<ChannelRow> {
    const a = atLeast(decode(await this.body('GET', `/v1/channels/${id('channelId', channelId)}`, 'read', true)), 12);
    return { id: bin(a[0], 16), kind: u53(a[2]), mode: u53(a[3]), visibility: u53(a[4]),
      parentId: opt(a[5], (v) => bin(v, 16)), name: str(a[6]), topic: str(a[7]), position: u53(a[8]), slowmode: u53(a[9]),
      seq: u64(a[10]), textGroupId: opt(a[11], (v) => bin(v, 16)) };
  }
  async deleteDevice(deviceId: Uint8Array): Promise<void> {
    await this.body('DELETE', `/v1/devices/${id('deviceId', deviceId)}`, 'write', false, { ok: [204] });
  }
  async deleteGroupMessage(groupId: Uint8Array, seq: bigint): Promise<'deleted' | 'gone'> {
    const response = await this.http.request({ method: 'DELETE', path: `/v1/groups/${id('groupId', groupId)}/messages/${count('seq', seq)}`,
      bucket: 'write', idempotent: true, ok: [204, 404] });
    if (response.status === 204) return 'deleted';
    // The DS also says 404 when this device lost membership. A member-only read distinguishes that refusal from
    // an absent message, so a purge is not marked done after a removal that raced the DELETE.
    try {
      await this.getMessages(groupId, 0n, 1);
    } catch (error) {
      // The DS checks membership before its retention floor, so E_PRUNED also proves membership.
      if (!(error instanceof DillaHttpError && error.status === 410 && error.code === 'E_PRUNED')) throw error;
    }
    return 'gone';
  }
  async putBlob(channelId: Uint8Array, blobId: Uint8Array, stored: Uint8Array): Promise<{ created: boolean; size: number }> {
    const response = await this.http.request({ method: 'PUT', path: blobPath(channelId, blobId), body: stored,
      contentType: 'application/octet-stream', bucket: 'upload', idempotent: true, ok: [200, 201], timeoutMs: null });
    const a = atLeast(decode(response.body), 2);
    return { created: response.status === 201, size: u53(a[1]) };
  }
  async confirmBlob(channelId: Uint8Array, blobId: Uint8Array): Promise<void> {
    await this.body('POST', blobPath(channelId, blobId) + '/confirm', 'write', true, { ok: [204] });
  }
  async getBlob(channelId: Uint8Array, blobId: Uint8Array, maxBytes?: number): Promise<Uint8Array | null> {
    const response = await this.http.request({ method: 'GET', path: blobPath(channelId, blobId),
      bucket: 'read', idempotent: true, ok: [200, 404], maxBodyBytes: maxBytes, timeoutMs: null });
    return response.status === 404 ? null : response.body;
  }
  // ---- E8: the GIF picker (protocol/09 § GIFs). The Giphy key is a per-server setting; the instance searches and fetches
  // for a member of the server. No Giphy URL is ever built or read here, and the key only ever goes up. ----

  /** GET /v1/communities/{id}/gifs: whether this server has a key, and its rating. */
  async gifStatus(communityId: Uint8Array): Promise<GifStatus> {
    const a = atLeast(decode(await this.body('GET', `/v1/communities/${id('communityId', communityId)}/gifs`, 'read', true)), 2);
    const rating = str(a[1]);
    if (!(GIF_RATINGS as readonly string[]).includes(rating)) throw new CborError('E_CBOR_RANGE', 'unknown gif rating');
    return { on: oneOf(a[0], [0, 1]) === 1, rating: rating as GifRating };
  }
  /** PUT …/gifs/key: sets or replaces the server's key (a manager). The key is checked here as the instance checks it and is in no error. */
  async gifSetKey(communityId: Uint8Array, key: string): Promise<void> {
    if (!GIF_KEY.test(key)) throw new RangeError('E_ROUTE_INPUT: a Giphy key is 8 to 64 letters and digits');
    await this.body('PUT', `/v1/communities/${id('communityId', communityId)}/gifs/key`, 'write', true, { body: encode([key]), ok: [204] });
  }
  /** DELETE …/gifs/key. */
  async gifClearKey(communityId: Uint8Array): Promise<void> {
    await this.body('DELETE', `/v1/communities/${id('communityId', communityId)}/gifs/key`, 'write', true, { ok: [204] });
  }
  /** PUT …/gifs/rating. */
  async gifSetRating(communityId: Uint8Array, rating: GifRating): Promise<void> {
    if (!GIF_RATINGS.includes(rating)) throw new RangeError('E_ROUTE_INPUT: rating must be g, pg, pg-13 or r');
    await this.body('PUT', `/v1/communities/${id('communityId', communityId)}/gifs/rating`, 'write', true, { body: encode([rating]), ok: [204] });
  }
  /** GET …/gifs/search?q=&offset=: 24 results a page; an empty q is trending. 404 E_NOT_FOUND while the server has no key. */
  async gifSearch(communityId: Uint8Array, q: string, offset: number): Promise<GifPage> {
    if (new TextEncoder().encode(q).length > GIF_QUERY_MAX_BYTES || /\p{Cc}/u.test(q)) {
      throw new RangeError('E_ROUTE_INPUT: q must be at most ' + GIF_QUERY_MAX_BYTES + ' bytes with no control character');
    }
    if (!Number.isInteger(offset) || offset < 0 || offset > GIF_OFFSET_MAX) {
      throw new RangeError('E_ROUTE_INPUT: offset must be 0..' + GIF_OFFSET_MAX);
    }
    const path = `/v1/communities/${id('communityId', communityId)}/gifs/search?q=${encodeURIComponent(q)}&offset=${offset}`;
    const a = atLeast(decode(await this.body('GET', path, 'read', true, { maxBodyBytes: 1_048_576 })), 2);
    const items: GifItem[] = [];
    for (const value of arr(a[0])) {
      const row = atLeast(value, 4);
      const gifId = str(row[0]);
      // An id the instance should never send is dropped, not trusted into a path.
      if (!GIF_ID.test(gifId)) continue;
      const [width, height] = gifDims(u53(row[2]), u53(row[3]));
      items.push({ id: gifId, title: str(row[1]), width, height });
    }
    return { items, next: opt(a[1], u53) };
  }
  /** GET …/gifs/media/{gif_id}?r=preview|full: the GIF's bytes through the instance, so this browser never contacts Giphy. */
  async gifMedia(communityId: Uint8Array, gifId: string, variant: 'preview' | 'full', signal?: AbortSignal): Promise<Uint8Array> {
    if (!GIF_ID.test(gifId)) throw new RangeError('E_ROUTE_INPUT: a gif id is 1..64 letters and digits');
    if (variant !== 'preview' && variant !== 'full') throw new RangeError('E_ROUTE_INPUT: r must be preview or full');
    const bytes = await this.body('GET', `/v1/communities/${id('communityId', communityId)}/gifs/media/${gifId}?r=${variant}`, 'read', true,
      { maxBodyBytes: variant === 'preview' ? GIF_PREVIEW_MAX_BYTES : GIF_FULL_MAX_BYTES, signal });
    if (!(bytes.length >= 6 && bytes[0] === 0x47 && bytes[1] === 0x49 && bytes[2] === 0x46 && bytes[3] === 0x38
      && (bytes[4] === 0x37 || bytes[4] === 0x39) && bytes[5] === 0x61)) {
      throw new DillaHttpError({ status: 503, code: 'E_UNAVAILABLE', detail: 'the gif service sent something that is not a gif',
        retryAfterMs: null, extra: [] });
    }
    return bytes;
  }
  async deleteBlob(channelId: Uint8Array, blobId: Uint8Array): Promise<void> {
    await this.body('DELETE', blobPath(channelId, blobId), 'write', true, { ok: [204, 404] });
  }

  // ---- web-3 calls (L-HTTP-100..104, L-TS-55) ----

  /** L-HTTP-100: the channel's call group (the live call's, else the newest open one, else null), its live call and who
   *  is in it. `signal` (web3.7 3a-client: the boot sweep's) aborts it and its retries. */
  async getChannelCall(channelId: Uint8Array, signal?: AbortSignal): Promise<ChannelCall> {
    const a = atLeast(decode(await this.body('GET', `/v1/channels/${id('channelId', channelId)}/call`, 'read', true, { signal })), 3);
    return { groupId: opt(a[0], (v) => bin(v, 16)), callId: opt(a[1], (v) => bin(v, 16)), participants: participants(a[2]) };
  }
  /** L-HTTP-101: the live calls of the community's voice channels the caller may view. */
  async getCommunityCalls(communityId: Uint8Array): Promise<CommunityCall[]> {
    const raw = await this.body('GET', `/v1/communities/${id('communityId', communityId)}/calls`, 'read', true);
    return bounded(decode(raw), COMMUNITY_CALLS_MAX, 'calls').map((row) => {
      const a = atLeast(row, 3);
      return { channelId: bin(a[0], 16), callId: bin(a[1], 16), participants: participants(a[2]) };
    });
  }
  /** POST /v1/channels/{id}/calls: 201 opens the call, 200 joins the live one; the body names the decoders, if any. */
  async startCall(channelId: Uint8Array, vdec: string): Promise<CallStarted> {
    const response = await this.http.request({ method: 'POST', path: `/v1/channels/${id('channelId', channelId)}/calls`,
      bucket: 'write', idempotent: false, body: encode(vdec === '' ? [] : [vdec]), ok: [200, 201] });
    const a = atLeast(decode(response.body), 6);
    const caps = arr(a[5]);
    if (caps.length !== 3) throw new CborError('E_CBOR_SHAPE', 'caps has three elements');
    return { created: response.status === 201, callId: bin(a[0], 16), groupId: bin(a[1], 16), livekitUrl: str(a[2]), token: str(a[3]),
      iceServers: arr(a[4]).map((server): IceServer => {
        const s = atLeast(server, 3);
        return [arr(s[0]).map(str), str(s[1]), str(s[2])];
      }),
      caps: [u53(caps[0]), u53(caps[1]), u53(caps[2])],
      rung: a.length > 6 ? arr(a[6]).map((u) => bin(u, 16)) : null };
  }
  async shareCall(callId: Uint8Array): Promise<void> {
    await this.body('POST', callPath(callId, '/share'), 'write', false, { body: encode([]), ok: [204] });
  }
  /** `DELETE /v1/calls/{call_id}?if_alone=1`: ends the call (its room and call group close) only while this device is the
   *  call group's only current leaf, decided under the group lock; true once it has ended (204, also when it already
   *  had), false when another device holds a leaf (409 E_CALL_NOT_ALONE: the call goes on). The client never sends the
   *  unconditional form (web-3 task 10 fix, f10app I1). */
  async endCallIfAlone(callId: Uint8Array, signal?: AbortSignal): Promise<boolean> {
    try {
      await this.body('DELETE', callPath(callId, '?if_alone=1'), 'write', true, { ok: [204], signal });
      return true;
    } catch (e) {
      if (e instanceof DillaHttpError && e.status === 409 && e.code === 'E_CALL_NOT_ALONE') return false;
      throw e;
    }
  }
  async unshareCall(callId: Uint8Array): Promise<void> {
    await this.body('DELETE', callPath(callId, '/share'), 'write', true, { ok: [204] });
  }
  /** A 429 is not waited out: the worker drops that report (L-TS-56 step 6). */
  async postCallStats(callId: Uint8Array, report: CallStats): Promise<void> {
    await this.body('POST', callPath(callId, '/stats'), 'write', false, { body: encode(report), ok: [204], retry429: false });
  }
  /** Signal task 8c: `POST /v1/calls/{call_id}/rtt` `[ms]` (0..5000) or `[]` (clear): this device's RTT to the SFU, which
   *  the instance writes as its dilla.rtt participant attribute. A 429 is not waited out: the next sample replaces it. */
  async postCallRtt(callId: Uint8Array, ms: number | null): Promise<void> {
    await this.body('POST', callPath(callId, '/rtt'), 'write', false, { body: encode(ms === null ? [] : [ms]), ok: [204], retry429: false });
  }
  /** L-HTTP-103: a core-built [epoch, proposal] body, unchanged. */
  async postProposal(groupId: Uint8Array, body: Uint8Array, signal?: AbortSignal): Promise<{ seq: bigint }> {
    const a = atLeast(decode(await this.body('POST', `/v1/groups/${id('groupId', groupId)}/proposal`, 'commit', false, { body, signal })), 1);
    return { seq: u64(a[0]) };
  }
  /** L-HTTP-104: the body echoes the ring's expires_ts, so a late decline never stops a later ring of the same DM. */
  async declineCall(callId: Uint8Array, ringTs: number): Promise<void> {
    if (!Number.isSafeInteger(ringTs) || ringTs < 0) throw new RangeError('E_ROUTE_INPUT: ringTs must be a non-negative integer');
    await this.body('POST', callPath(callId, '/decline'), 'write', true, { body: encode([ringTs]), ok: [204] });
  }

  // ---- web3.6: second factors (flow 10) ----
  /** POST /v1/auth/recovery/verify: [assertion, code] → [assertion]; accepts an assertion owing needs 1 or 2. */
  async recoveryVerify(assertion: string, code: string): Promise<{ assertion: string }> {
    const a = atLeast(decode(await this.body('POST', '/v1/auth/recovery/verify', 'none', false,
      { body: encode([assertion, code]), auth: false, retry429: false })), 1);
    return { assertion: str(a[0]) };
  }
  /** GET /v1/auth/factors: [owed, totp confirmed_at | null, [[cred_id, name, created, last_used | null]], backup_codes_left, password,
   *  password_changed | null]; the sixth element is Task 11's and an older server leaves it out. */
  async getFactors(): Promise<Factors> {
    const a = atLeast(decode(await this.body('GET', '/v1/auth/factors', 'read', true)), 5);
    return { owed: flag(a[0]), totp: opt(a[1], u53), passkeys: bounded(a[2], PASSKEYS_MAX, 'passkeys').map(passkeyRow),
      backupCodesLeft: u53(a[3]), hasPassword: flag(a[4]), passwordChanged: a.length > 5 ? opt(a[5], u53) : null };
  }
  /** POST /v1/auth/totp/enroll: [stepup | null] → [secret, otpauth_url]. The secret is returned, never kept. */
  async totpEnroll(stepup: string | null): Promise<TotpEnrol> {
    const a = atLeast(decode(await this.body('POST', '/v1/auth/totp/enroll', 'write', false, { body: encode([stepup]) })), 2);
    return { secret: str(a[0]), uri: str(a[1]) };
  }
  /** POST /v1/auth/totp/confirm: [code] → [backup_codes]; the list is empty when the account already held unused codes. */
  async totpConfirm(code: string): Promise<string[]> {
    const a = atLeast(decode(await this.body('POST', '/v1/auth/totp/confirm', 'write', false, { body: encode([code]) })), 1);
    return backupCodes(a[0]);
  }
  /** POST /v1/auth/passkey/register/begin: [stepup | null] → [ceremony_id, options]. */
  async passkeyRegisterBegin(stepup: string | null): Promise<Ceremony> {
    return ceremony(decode(await this.body('POST', '/v1/auth/passkey/register/begin', 'write', false, { body: encode([stepup]) })));
  }
  /** POST /v1/auth/passkey/register/finish: [ceremony_id, response, name] → [cred_id, backup_codes]. */
  async passkeyRegisterFinish(ceremonyId: Uint8Array, response: string, name: string): Promise<PasskeyRegistered> {
    const body = encode([inputId('ceremonyId', ceremonyId), response, checkedName('name', name, PASSKEY_NAME_MAX_BYTES)]);
    const a = atLeast(decode(await this.body('POST', '/v1/auth/passkey/register/finish', 'write', false, { body })), 2);
    return { credId: bin(a[0]), backupCodes: backupCodes(a[1]) };
  }
  /** POST /v1/auth/passkey/login/begin: [] → [ceremony_id, options]; unauthenticated and discoverable. */
  async passkeyLoginBegin(): Promise<Ceremony> {
    return ceremony(decode(await this.body('POST', '/v1/auth/passkey/login/begin', 'none', false,
      { body: encode([]), auth: false, retry429: false })));
  }
  /** POST /v1/auth/passkey/login/finish: [ceremony_id, response] → [assertion, needs]; needs 1 when the authenticator did
   *  not verify the user. A passkey this instance does not hold is 401 E_PASSKEY_UNKNOWN. */
  async passkeyLoginFinish(ceremonyId: Uint8Array, response: string): Promise<PasskeyLogin> {
    const a = atLeast(decode(await this.body('POST', '/v1/auth/passkey/login/finish', 'none', false,
      { body: encode([inputId('ceremonyId', ceremonyId), response]), auth: false, retry429: false })), 2);
    return { assertion: str(a[0]), needs: oneOf(a[1], [0, 1]) };
  }
  /** POST /v1/auth/stepup: [method, secret] → 201 [stepup, expires]; method 0 password, 1 code, 2 backup code. A wrong
   *  secret is 403 E_STEP_UP. A 429 is not waited out: the person decides when to try again. */
  async stepUp(method: 0 | 1 | 2, secret: string): Promise<StepUpToken> {
    const body = encode([oneOfInput('method', method, [0, 1, 2]), secret]);
    return stepUpToken(await this.body('POST', '/v1/auth/stepup', 'write', false, { body, retry429: false }));
  }
  /** POST /v1/auth/stepup/passkey/begin: [] → [ceremony_id, options] (this account's passkeys, user verification required). */
  async stepUpPasskeyBegin(): Promise<Ceremony> {
    return ceremony(decode(await this.body('POST', '/v1/auth/stepup/passkey/begin', 'write', false, { body: encode([]) })));
  }
  /** POST /v1/auth/stepup/passkey/finish: [ceremony_id, response] → 201 [stepup, expires]. */
  async stepUpPasskeyFinish(ceremonyId: Uint8Array, response: string): Promise<StepUpToken> {
    const body = encode([inputId('ceremonyId', ceremonyId), response]);
    return stepUpToken(await this.body('POST', '/v1/auth/stepup/passkey/finish', 'write', false, { body, retry429: false }));
  }
  /** POST /v1/auth/factors/remove: [stepup, kind, cred_id | null] → 204; kind 0 the app (no cred_id), 1 a passkey. The last
   *  factor is 409 E_INVALID_REQUEST. */
  async removeFactor(stepup: string, kind: 0 | 1, credId: Uint8Array | null): Promise<void> {
    oneOfInput('kind', kind, [0, 1]);
    if ((kind === 1) !== (credId !== null)) throw new RangeError('E_ROUTE_INPUT: credId is for a passkey and only for a passkey');
    await this.body('POST', '/v1/auth/factors/remove', 'write', false, { body: encode([stepup, kind, credId]), ok: [204] });
  }
  /** POST /v1/auth/password: [old|null, new, stepup|null] → 204. With a password on the account the old one is the step-up
   *  (wrong is 403 E_FORBIDDEN, throttled is 429 with retry-after); a first password on an account that holds a second factor
   *  needs a step-up token instead. A 429 is not waited out, which would send the password again: the person decides. */
  async changePassword(old: string | null, next: string, stepup: string | null): Promise<void> {
    await this.body('POST', '/v1/auth/password', 'write', false, { body: encode([old, next, stepup]), ok: [204], retry429: false });
  }
  /** POST /v1/auth/recovery/codes: [stepup] → [backup_codes]; every earlier code stops working. */
  async regenerateBackupCodes(stepup: string): Promise<string[]> {
    const a = atLeast(decode(await this.body('POST', '/v1/auth/recovery/codes', 'write', false, { body: encode([stepup]) })), 1);
    return backupCodes(a[0]);
  }
}
