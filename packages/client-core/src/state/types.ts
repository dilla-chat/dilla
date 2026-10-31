// The state slices the worker publishes and the page renders (L-TS-08). Every id is 32 lower-case hex
// characters; every slice value is immutable and replaced whole.
import type { TrustBlock } from '../core-port';
export type { TrustBlock };
export type BootPhase = 'loading' | 'unsupported' | 'other-tab' | 'paused-error' | 'store-lost' | 'needs-signup' | 'signup-keys' | 'registering' | 'ready' | 'revoked' | 'error'
  | 'signin-login' | 'signin-totp' | 'signin-key' | 'enrolling' | 'cleared'
  // web3.6: a login owing a passkey (needs 2); an account that owes a second factor before the shell.
  | 'signin-passkey' | 'factor-setup';
/** Why the factor-setup phase was entered: a fresh signup, a sign-in on this browser, or an account found owing (a boot,
 *  a reload, an operator reset). */
export type FactorSetupOrigin = 'signup' | 'signin' | 'required';
export interface AccountState {
	/** Persisted account-wide choice. Defaults to online for an older instance. */
  presenceChoice?: PresenceChoice;
  phase: BootPhase;
  /** passwordSignup, totp and passkeys: auth_methods holds 0, 1 and 2. */
  instance: { id: string; name: string; registrationMode: 0 | 1 | 2; passwordSignup: boolean; totp: boolean; passkeys: boolean } | null;
  /** `display` is the account's display name (read with the presence choice); a person with no server has no member row to name them by. */
  user: { id: string; username: string; display?: string; /** EOD E6: absent while empty. */ statusMsg?: string; bio?: string } | null;
  deviceId: string | null;
  recoveryKey: string[] | null;                 // 13 groups of 4; only while phase is 'signup-keys' or 'registering'
  error: WorkerError | null;                    // the last signup, sign-in or boot error
  /** Set during signin-*; null otherwise. `factor` is what the code step asks after: 'totp' after a login owing an app code
   *  (needs 1), 'passkey' after a login owing a passkey (needs 2) and after a passkey that did not verify the user (the
   *  copy says why a code is asked). */
  signIn: { username: string | null; factor: 'totp' | 'passkey' | null } | null;
  /** web3.6: set while the phase is factor-setup; null otherwise. */
  factorSetup: { origin: FactorSetupOrigin } | null;
  /** BACKUPS-RECOVERY-04: the instance holds a root object this account did not seal (E_ROOT_MISMATCH at ready), so
   *  the recovery key cannot open it until the operator resets it; cleared by a later matching check and by a wipe. */
  rootMismatch: boolean;
  /** The core's trust state for this account's own key, which Privacy does not list as a contact: 'changed' while a
   *  signed list under another key is pending for this user, 'unlisted' while the core refuses an own leaf against the
   *  own signed list (a device this person did not add). null otherwise; re-derived at each trust publish. */
  ownTrust: 'changed' | 'unlisted' | null;
}
/** Page-bound errors have a stable code and no detail. `store` is set only on E_STORE_NEWER: the app schema of the
 *  store in this browser and the highest this build supports (C4x, web3.6). */
export interface WorkerError { code: string; detail: string; status: number; retryAfterMs: number | null;
  /** A stable key for a local refusal the page maps to copy (none since web3.8 R5 removed the last); never the detail text. */
  reason?: string;
  store?: { found: number; supported: number }; }
export interface ConnectionState { status: 'offline' | 'connecting' | 'online'; generation: string | null;
  /** Task 11: the last gateway heartbeat's round trip in ms while online; absent before the first ack or while not online. */
  pingMs?: number;
  /** Actual current socket selection, cleared on disconnect. */
  wireVersion?: number;
  /** Verified entry hint from a wire-5 socket; the page still verifies the manifest. */
  appBuild?: string;
  reason?: 'version'; }                        // only while idle because the server refused this client's version (gateway close 4006)
/** owner: the 32-hex user id of the server's owner (GET /v1/communities element 2). The owner holds every permission; the
 *  instance serves no role bits, so it is the one permission fact a page has (v0.1.0-web3.1: who may add a channel). */
export interface CommunitySummary { id: string; name: string; owner: string; }
/** The answer of createInvite: the instance's invite link, its code and its expiry (unix seconds). */
export interface InviteCreatedResult { url: string; code: string; expires: number; }
export type ChannelGroupState = 'none' | 'joining' | 'active' | 'resync' | 'not-member' | 'unsupported';
export interface ChannelSummary { id: string; communityId: string; kind: 0 | 1 | 2; mode: 0 | 1; name: string; topic: string; parentId: string | null; position: number; slowmode: number;
  /** A text channel whose @everyone overwrite denies send_messages (derived from the overwrites the page can read; false while
   *  they are unknown). */
  announcement: boolean; group: ChannelGroupState; }
export interface MemberSummary { userId: string; username: string; display: string; kind: 0 | 1; roleIds: string[];
  /** EOD E6: the account's status message and bio, absent while empty. */
  statusMsg?: string; bio?: string; }   // roleIds: hex of member-row element 3, in order
export type PresenceChoice = 'online' | 'idle' | 'dnd' | 'invisible';
export type PresenceStatus = 'offline' | 'online' | 'idle' | 'dnd';
/** null means the authenticated snapshot has not arrived yet. */
export type PresenceState = Record<string, PresenceStatus> | null;
export type TimelineItemState = 'ok' | 'pending' | 'failed' | 'cannot-read' | 'deleted';
/** `threadRoot` (web3.7): the msg id of the thread root the quoted message shows in, null for a channel message. */
export interface ReplyRef { msgId: string; state: 'ok' | 'missing' | 'deleted'; senderUser: string | null; excerpt: string; seq: string | null; threadRoot: string | null; }
export interface ReactionSummary { emoji: string; count: number; mine: boolean; }
export interface AttachmentSummary { index: number; size: number; mime: string; name: string; w: number | null; h: number | null;
  thumb: boolean; kind: 'image' | 'audio' | 'video' | 'file'; tooLarge: boolean; }   // kind: attachmentKind (L-TS-58)
export interface PendingAction { msgId: string; type: 1 | 2 | 3 | 4 | 5 | 6; state: 'pending' | 'failed'; reason: string; }   // msgId: the fold row's OWN outbox msg id (retrySend/discardSend take it); the target is the item it hangs on
export interface TimelineItem {
  key: string;                                  // 'o<msg id hex>' for an outbox row and for a stored row of this device whose msgId is
                                                // known (so a sent message keeps its key when it is confirmed and is not re-announced);
                                                // 's<seq>' for every other stored row
  state: TimelineItemState;
  reason: string;                               // the raw code for 'cannot-read' and 'failed', else ''
  senderUser: string | null; senderDevice: string | null;
  own: boolean; web: boolean;
  /**
   * The sender credential's kind is Bot. The credential is signed by the sender's own SSK, so this is the sender's claim:
   * the web shows a sender as a Bot only when the server's member row is of account kind 1 as well (Task S1).
   */
  bot: boolean;
  ts: number;                                   // unix seconds
  body: string;
  msgId: string | null;
  seq: string | null;                           // the stored seq as decimal; null for an outbox item
  edited: boolean;                              // the core folded an edit onto it (edited_seq > 0)
  reply: ReplyRef | null;                       // the message it answers, as the core resolved it (an outbox item: from the stored rows)
  reactions: ReactionSummary[];                 // the core's per-user counts, in the core's order
  pinned: boolean;
  attachments: AttachmentSummary[];             // what the envelope names; never a key, a nonce or a blob id
  mention: boolean;                             // the core's flag, set once at apply (L-CORE-35)
  actions: PendingAction[];                     // pending or failed folds of the own outbox aimed at this message
  thread: { count: number; lastTs: number } | null;   // a held root with shown replies: how many, and the latest (unix seconds)
}
/** The open thread of a channel (web3.7): its root (null when the root is not held), the replies oldest first (newest last,
 *  as the timeline orders items) and whether older replies exist. */
export interface ThreadState { channelId: string; rootMsgId: string; root: TimelineItem | null; items: TimelineItem[]; hasEarlier: boolean }
/** One thread of a channel (web3.7): its held root's seq and excerpt. */
export interface ThreadSummary { rootMsgId: string; rootSeq: string | null; count: number; lastTs: number; excerpt: string }
/** ts: the target's recv_ts, unix seconds. threadRoot (web3.7): the thread the pinned message shows in, null for a channel message. */
export interface PinnedItem { msgId: string; seq: string; senderUser: string | null; excerpt: string; pinnedBy: string; pinnedSeq: string; ts: number;
  threadRoot: string | null; }   // ts: the target's recv_ts, unix seconds
export interface TrayItem { id: string; name: string; size: number; mime: string; image: boolean;
  phase: 'reading' | 'preparing' | 'uploading' | 'ready' | 'failed'; reason: string; }
export interface TimelineState { channelId: string; group: ChannelGroupState; items: TimelineItem[]; hasEarlier: boolean; }
export interface DmSummary { id: string; kind: 3 | 4; members: string[]; name: string; group: ChannelGroupState; }
export interface DeviceSummary { id: string; tier: 0 | 1; signerTier: 0 | 1; lastSeen: number; revokedAt: number | null; listed: boolean; own: boolean;
  /** The owner's name for the device, or null when unnamed (the server holds it readable; it is not in the signed device list). */ label: string | null; }
/**
 * L-TS-40's trust row. Ruling (c) of 2026-10-13 adds two worker-derived Privacy states, present only while they hold:
 * `unlisted` (one of the contact's leaves failed its signed list, or Flow 05's "history pending": the list read has not
 * completed, which shares the unlisted copy and its no-Verify rule) and `removedIn` (channels whose validated Remove of the
 * changed leaf cleared that group's send block while the key stays changed and unverified).
 */
export interface TrustSummary { userId: string; name: string; shared: boolean; umk: string; firstSeen: number; verified: boolean; changedUmk: string | null;
  unlisted?: true; removedIn?: string[]; blocked?: TrustBlock }
/**
 * Fix wave C1 (ABI 8): `blocked` is the core's stored verdict of the contact's leaves still in an active group, present only
 * while one holds. `verified` is true only while nothing is blocked. 'E_UMK_CHANGED' reads as a changed key (also when
 * `changedUmk` is null: a leaf under another key that no validated list presents); any other code reads as unlisted.
 *
 * `sendBlocks`: channel id (hex) → the code the core refuses a send with in that channel's group (send_block_code), only for
 * blocked channels, and `own` when only own-user leaves hold it (send_block_own). The composer pauses on it and says why
 * (W-01, R1): the own account's copy for an own block, else the changed-key copy for a changed contact of the conversation
 * (with or without a key to compare), else privacy.leafFailed.
 */
export interface SendBlock { code: TrustBlock; own: boolean }
export type SendBlocksState = Record<string, SendBlock>;
export interface SearchState { requestId: number; hits: { channelId: string; communityId: string | null; seq: string; snippet: string; rank: string; threadRoot: string | null;
    /** J1: who wrote the hit and when (unix seconds), read from the stored row; absent when the row cannot be read. */
    author?: string; ts?: number }[];
  cursor: { token: string } | null }
export interface DiagnosticsState { connection: 'online' | 'offline' | 'connecting'; generation: string | null; instance: string;
  devices: number; errors: { code: string; at: number }[] }
export interface TypingState { userId: string; name: string; expiresAt: number }
export interface BadgeState { unread: number; mentions: number; }
// threadRoot (FX-core): the 32-hex msg id of the thread root the message shows under (a reply whose root is held), else null.
export interface Notice { id: number; channelId: string; communityId: string | null; kind: 'message' | 'mention' | 'dm'; senderUser: string | null; senderName: string; body: string; ts: number;
  threadRoot: string | null; }
export interface NoticesState { nextId: number; items: Notice[]; }      // items ≤ 32, oldest first; ids increase for the worker's life
// ---- web-3: calls, voice presence and rings (L-TS-51). A call's media key is never in a slice or a command result: it
// reaches the page only as a `callKeys` push (L-TS-52).
export type IceServerTuple = [urls: string[], username: string, credential: string];
export interface CallKeys { groupId: string; epoch: string; baseKey: ArrayBuffer; selfLeaf: number;
  roster: { leaf: number; deviceId: string; userId: string }[] }
// No key bytes in a command result: the join epoch's CallKeys arrive as a `callKeys` push posted
// immediately before this result's `ret` (same port, FIFO), carrying `epoch` below.
export interface CallJoinResult { callId: string; groupId: string; livekitUrl: string; token: string;
  iceServers: IceServerTuple[]; caps: [number, number, number]; epoch: string }
export type CallStatsReport = [candidateType: number, relayProtocol: number | null, rttMs: number,
  fractionLostPermille: number, decryptFailures: number, framesEncrypted: number];
// 'paused' (RV-3, Jonas): a mid-call identity change (E_UMK_CHANGED); still in the group and the room, no keys sent or installed.
// Also the hold of WB security I1: a new epoch the gate refused for any other reason (error: E_DEVICE_LIST_STALE or the
// core's code, changedUser null); the page sends and plays nothing and drops its media keys until the next key arrives.
export type CallPhase = 'idle' | 'joining' | 'in' | 'paused' | 'leaving' | 'blocked' | 'removed' | 'failed';
export interface CallState { phase: CallPhase; channelId: string | null; callId: string | null; epoch: string | null;
  code: string | null; error: WorkerError | null; roster: { userId: string; deviceId: string; leaf: number }[];
  // User names; DM calls this device opened. notRung (WB product walk I6): participants the instance did not ring (its
  // pair rule held the ring back, or a cap), so the caller is never told "ringing" for a ring nobody got.
  outgoing: { ringing: string[]; declined: string[]; notRung: string[] } | null;
  // Ruling I2 (2026-10-13): the 32-hex user id of the changed contact while the phase is paused (RV-3) or after an
  // E_UMK_CHANGED join refusal (the first by user_id when several changed); null otherwise. Fills {name} in
  // voice.paused and voice.blockedChanged, also when a refused join left `roster` empty.
  changedUser: string | null;
  // f10app m2: true while the join waits (at most 30 s) for this device's earlier Remove to be committed; the page says
  // so in the dock's status region. False otherwise.
  rejoining: boolean }
export interface VoiceParticipant { userId: string; deviceId: string; name: string; flags: number; own: boolean; web: boolean; bot: boolean }
export interface VoiceCall { callId: string; participants: VoiceParticipant[] }
export type VoiceState = Record<string, VoiceCall>;   // channel id (hex) → its live call
export interface Ring { callId: string; channelId: string; callerUser: string; callerName: string;
  ringTs: number;               // op 53's expires_ts as received (seconds); callDecline echoes it
  expiresAt: number;            // ms, min(expires_ts*1000, receivedAt + 30_000)
  busy: boolean;                // this device is in another call: no sound, no notification
  quiet: boolean }              // the DM is muted (mute.channel.<id>): no sound, no notification
export type RingsState = Ring[];
/** The initial values of the three web-3 slices (L-TS-51). */
export const CALL_IDLE: CallState = { phase: 'idle', channelId: null, callId: null, epoch: null, code: null, error: null, roster: [], outgoing: null,
  changedUser: null, rejoining: false };
export const VOICE_EMPTY: VoiceState = {};
export const RINGS_EMPTY: RingsState = [];
/** What the caller may do in one server (GET /v1/communities/{id}/permissions); `top` is their rank, 0..2^31. */
/** `loaded` is false for a server whose access has not been read (or that is gone): the page shows a loading state, not
 *  "no rights". */
export interface CommunityAccess { top: number; owner: boolean; manageCommunity: boolean; viewAuditLog?: boolean; manageChannels: boolean;
  kickMembers: boolean; banMembers: boolean; moderateMembers?: boolean; manageMessages?: boolean; createInvite: boolean; manageRoles: boolean; loaded: boolean;
  /** The id of the position-0 role (@everyone); '' until the roles are read. */
  everyoneId: string;
  /** Whether the last read of the overwrites succeeded; the `overwrites:` slice keeps its last value for display only. */
  overwritesLoaded: boolean }
/** One role with its permission bits; `members` counts the member rows that hold it (every member for @everyone, 0 while
 *  the member list is absent). */
export interface RoleSummary { id: string; name: string; color: number; position: number; hoist: boolean; mentionable: boolean;
  allow: number; deny: number; members: number }
/** One channel overwrite; `kind` 0 targets a role, 1 a member. */
export interface OverwriteSummary { kind: 0 | 1; targetId: string; allow: number; deny: number }
/** One ban; `name` is the display name, else the username, else ''. */
export interface BanSummary { userId: string; name: string; reason: string; byUser: string; created: number; expires: number | null }
export interface InviteSummary { id: string; createdBy: string | null; maxUses: number; uses: number; created: number; expires: number }
/** A server the caller was removed from, to be told once. */
export interface RemovalNotice { id: number; communityId: string; name: string }
/** web3.6: the account's second factors (GET /v1/auth/factors). Never a secret: the TOTP secret, the otpauth URI and the
 *  backup codes travel only in command results. `totp` is null without a confirmed app; a passkey `id` is its credential
 *  id in lower-case hex; times are unix seconds. */
export interface FactorsState { owed: boolean; totp: { confirmedAt: number } | null;
  passkeys: { id: string; name: string; created: number; lastUsed: number | null }[]; backupCodesLeft: number; hasPassword: boolean;
  /** Task 11 (Q7): when the password was last changed, unix seconds; null when unknown. */
  passwordChanged?: number | null }
/** CB2: a channel's slow-mode wait for this account, published by the worker for a channel with slow mode on. until: ms
 *  since the epoch when the next post may go (0 = none); queued: a post of this device waits for it in the outbox. */
export interface SlowState { until: number; queued: boolean }
/** The built-in AFK channel of one community; null means the initial snapshot is still loading. `pending`: AFK cut this
 *  device from its call (the sweep, or AFK chosen on another device; web3.11 joins nothing on it). */
export interface AFKState {
  channelId: string | null; timeoutSeconds: number; pending: boolean;
  /** web3.9 R-AFK4: rises with every landed read (one counter for the worker's life), so each read that says `pending`
   *  is a move of its own: the page keys a move on (community, seq). Set by client-core on every read; a fixture may
   *  leave it out. */
  seq?: number;
  /** AW4: the community's away roster, sorted by user id (lower-case hex): each member listed under the AFK channel with
   *  no voice connection, and the unix second it became away there. Set by client-core on every read and kept current by
   *  op 55; a fixture may leave it out. */
  away?: { userId: string; since: number }[];
  /** AW5 fix round 3: why this device is pending (device-scoped: every community's read says it): 1 the idle sweep moved
   *  it, 2 this user chose AFK on another device or tab, 0 none. Set by client-core on every read; a fixture may leave it
   *  out (then `pending` reads as 1). */
  pendingReason?: 0 | 1 | 2;
}
export type SliceName = 'account' | 'connection' | 'communities' | `channels:${string}` | `members:${string}` | `presence:${string}` | `timeline:${string}`
  | `communityChanges:${string}`
  | `afk:${string}`
  | 'dms' | 'devices' | 'badges' | 'notices' | 'settings' | 'trust' | 'sendBlocks' | 'search' | 'diagnostics'
  | `typing:${string}` | `pins:${string}` | `tray:${string}` | 'call' | 'voice' | 'rings'
  | `permissions:${string}` | `roles:${string}` | `overwrites:${string}` | `bans:${string}` | `invites:${string}` | 'removals' | 'factors'
  | `slow:${string}` | `thread:${string}` | `threads:${string}`;
export interface SliceTypes { account: AccountState; connection: ConnectionState; communities: CommunitySummary[];
  [k: `communityChanges:${string}`]: number;
  [k: `channels:${string}`]: ChannelSummary[]; [k: `members:${string}`]: MemberSummary[]; [k: `presence:${string}`]: PresenceState; [k: `timeline:${string}`]: TimelineState;
  [k: `afk:${string}`]: AFKState | null;
  [k: `pins:${string}`]: PinnedItem[]; [k: `tray:${string}`]: TrayItem[];
  [k: `typing:${string}`]: TypingState[];
  [k: `permissions:${string}`]: CommunityAccess; [k: `roles:${string}`]: RoleSummary[];
  /** channel id (hex) → its overwrites; a channel with none is absent. */
  [k: `overwrites:${string}`]: Record<string, OverwriteSummary[]>; [k: `bans:${string}`]: BanSummary[];
  [k: `invites:${string}`]: InviteSummary[]; removals: RemovalNotice[]; factors: FactorsState;
  [k: `slow:${string}`]: SlowState; [k: `thread:${string}`]: ThreadState | null; [k: `threads:${string}`]: ThreadSummary[];
  dms: DmSummary[]; devices: DeviceSummary[]; badges: Record<string, BadgeState>; notices: NoticesState; settings: Record<string, string>;
  trust: TrustSummary[]; sendBlocks: SendBlocksState; search: SearchState; diagnostics: DiagnosticsState;
  call: CallState; voice: VoiceState; rings: RingsState }
export const TIMELINE_PAGE = 100;
