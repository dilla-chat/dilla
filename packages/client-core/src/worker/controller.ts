// The composition root inside the core worker (L-TS-09, L-TS-23): it answers the page's commands, owns the boot
// and sign-in phases, wires the HTTP client, the session, the enrolment, the gateway and the sync engine
// together, and publishes the state slices (L-TS-08, L-TS-24). It posts only ret and slice messages, to its
// own page only.
import type { BootOutcome } from '../account/boot';
import { Enrol, ensureBackups, refreshOwnDeviceList, repairBackupState, type EnrolFetched } from '../account/enrol';
import { refillKeyPackages } from '../account/keypackages';
import { Session } from '../account/session';
import { Signup, publishDeviceList } from '../account/signup';
import { AttachmentError, MAX_ATTACHMENTS } from '../attachments/crypto';
import { fetchAttachment, thumbnailBlob } from '../attachments/download';
import { safeName } from '../attachments/name';
import type { ThumbDeps } from '../attachments/thumb';
import { Tray, type TrayEntry } from '../attachments/tray';
import { CborError } from '../cbor';
import { decodeBotResponse, encodeBotRequestBytes, validateBotResponseForRequest, validateBotContext } from '../bot/protocol';
import { arr, decode as decodeCbor, type CborInput } from '../cbor';
import {
  CoreError, storeNewerOf, type ActivityRow, type ApplyResult, type AttachmentDescriptor, type CorePort, type ExpectedGroup, type GroupInfo, type Id,
  type HumanInteractionEvent, type OutboxRow, type SearchCursor, type SendRequest, type SignedLists, type TimelineRow, type TrustRecord,
} from '../core-port';
import { CLIENT_CLOSE, GATEWAY, Gateway, type GatewayDeps, type GatewayEvent, type ReadyInfo } from '../gateway/gateway';
import { Op, afkAwayOf, communityChangedOf, voiceStateOf, type AFKAwayFrame, type Frame } from '../gateway/frames';
import { fromHex, toHex } from '../hex';
import { HttpClient } from '../http/client';
import { DillaHttpError } from '../http/errors';
import {
  BIO_MAX_CHARS, CHANNEL_NAME_MAX_BYTES, COMMUNITY_NAME_MAX_BYTES, DISPLAY_MAX_CHARS, GIF_OFFSET_MAX, GIF_QUERY_MAX_BYTES,
  INVITE_TTL_MAX_S, INVITE_USES_MAX, BOT_CAPABILITIES_MAX, BOT_MUSIC_VOLUMES, BOT_MUSIC_IDLE_MINUTES, STATUS_MAX_CHARS, Routes, nameProblem, profileTextProblem, profileTextValue,
  type ChannelRow, type BotInstallationRow, type Factors, type Instance, type Limits, type MemberRow, type NewChannelKind,
} from '../http/routes';
import { NOTICE_READ_MAX, appendNotices, buildBadges, noticeOf, type NoticeDraft } from '../state/badges';
import { buildTimeline, channelGroup, channelGroupState, type GroupMembership } from '../state/derive';
import { PENDING_JOIN_KEY, PENDING_ROLE_PANEL_ACTION_PREFIX, isSettingKey, isSettingValue, pageSettings } from '../state/settings';
import { chainRow, historyFrom, ingestAccountKind, readDeviceListHistory, signedHeadList } from '../state/trust';
import { SliceStore } from '../state/store';
import {
  TIMELINE_PAGE, type AccountState, type BootPhase, type ChannelGroupState, type ChannelSummary, type ConnectionState,
  type DeviceSummary, type DiagnosticsState, type DmSummary, type InviteCreatedResult, type NoticesState, type PinnedItem, type ThreadState, type TimelineItem, type TrayItem,
  type TrustSummary, type SendBlocksState, type WorkerError, type RemovalNotice, type VoiceState, type FactorSetupOrigin,
  type PresenceChoice, type PresenceStatus,
} from '../state/types';
import { SyncEngine, type SyncDeps } from '../sync/engine';
import { LEAF_VERDICTS, SyncError } from '../sync/errors';
import { recoverModerationRemovals } from '../sync/removals';
import { LEADER, acquireLeadership, lockName, type Leadership, type LockManagerLike } from './leader';
import { isTestHook, isToWorker, type Command, type FromWorker } from './protocol';
import { CommandBarrier, SearchGate } from './barrier';
import { CallController, WAIT_LIST_MS } from '../calls/controller';
import { ADMIN_VALIDATORS, AdminController, nextPosition } from '../admin/controller';
import { channelBitsOf, PERM } from '../admin/perm';
import { ROLES_VALIDATORS, RolesController, announceProblem, privateProblem } from '../admin/roles';
import { BACKUP_CODE, FACTOR_VALIDATORS, FactorsController, NO_FACTORS, factorsState, normaliseBackupCode } from '../account/factors';
import { authorityFor } from '../admin/authority';

export interface ControllerDeps {
  origin: string;                                  // the page origin, e.g. http://127.0.0.1:8453
  fetch: typeof fetch; WebSocket: typeof WebSocket; locks: LockManagerLike;
  now(): number;                                   // ms
  /** A monotonic clock (ms, performance.now): elapsed-time bounds that a wall-clock step must not stretch or cut
   *  (task-10 fix review N2). Defaults to now. */
  monotonic?(): number;
  random(): number;
  setTimeout(fn: () => void, ms: number): number; clearTimeout(id: number): void;
  sleep(ms: number, signal?: AbortSignal): Promise<void>;
  boot(instance: Instance): Promise<BootOutcome>;
  runtimeReady?(): Promise<null>;                    // local WASM/persistence capability; no identity work
  resetDevice(instance: Instance): Promise<void>;  // removes the KEK record and the OPFS directory dilla/<instance hex>
  /** Posts to the own page; `transfer` moves buffers (a callKeys key, L-TS-52) instead of copying them. */
  post(message: FromWorker, transfer?: Transferable[]): void;
  testHooks: boolean;
  parts?: Partial<ControllerParts>;                // unit tests and the core-worker harness only; src/worker/entry.ts passes none
  thumbs?: ThumbDeps | null;                       // unit tests only; absent: probed from the worker global (L-TS-35)
}

/** The composition seams. REAL_PARTS builds the real classes; a unit test replaces any of them with a double
 *  cast through unknown (a class with private members is not structurally satisfiable). */
export interface ControllerParts {
  routes(http: HttpClient): Routes;
  session(deps: { core: CorePort; routes: Routes; now(): number }): Session;
  signup(deps: { core: CorePort; routes: Routes; session: Session; now(): number }): Signup;
  gateway(deps: GatewayDeps): Gateway;
  sync(deps: SyncDeps): SyncEngine;
  enrol(deps: { core: CorePort; routes: Routes; session: Session; now(): number }): Enrol;
  publishDeviceList(core: CorePort, routes: Routes): Promise<void>;
  ensureBackups(core: CorePort, routes: Routes): Promise<void>;
  refreshOwnDeviceList(core: CorePort, routes: Routes, userId: Id): Promise<{ version: bigint; listed: boolean }>;
  repairBackupState(core: CorePort, routes: Routes): Promise<boolean>;
}
// No EnrolRate class and no E_ENROL_RATE code (head ruling 38 as amended): registration answers no per-user 429.

export const REAL_PARTS: ControllerParts = {
  routes: (h) => new Routes(h),
  session: (d) => new Session(d),
  signup: (d) => new Signup(d),
  gateway: (d) => new Gateway(d),
  sync: (d) => new SyncEngine(d),
  enrol: (d) => new Enrol(d),
  publishDeviceList,
  ensureBackups,
  refreshOwnDeviceList,
  repairBackupState,
};

/** A refusal of the controller's own (E_BAD_INPUT, E_NOT_READY, E_INVITE_NOT_COMMUNITY, E_SETTING_KEY). */
class Refusal extends Error {
  constructor(readonly code: string, readonly detail: string, readonly reason?: string) {
    super(detail === '' ? code : `${code}: ${detail}`);
    this.name = 'Refusal';
  }
}

const ACCOUNT_CODES = new Set([
  'E_KEK_EXISTS', 'E_KEK_UNWRAP', 'E_SESSION_SCOPE', 'E_NO_BACKUP', 'E_LIST_RACE', 'E_DEVICE_UNLISTED', 'E_NO_ASSERTION',
  'E_SIGNIN_EVICTED', 'E_SIGNIN_EXPIRED',
]);

/** dillad's AssertionTTL is 5 minutes; a 401 to a backup code this long after the assertion was minted is its expiry
 *  (the minting is earlier on the instance's clock than the answer here, by the round trip). */
const ASSERTION_EXPIRED_MS = 300_000;
/** A failed KeyPackage publish is tried again after 5 s, doubling to 5 min. */
const KEYPACKAGE_RETRY_MIN_MS = 5_000;
const KEYPACKAGE_RETRY_MAX_MS = 300_000;

const ALLOWED_CODES = new Set(`E_ATTACHMENT_COUNT E_ATTACHMENT_FAILED E_ATTACHMENT_INPUT E_ATTACHMENT_MISSING E_ATTACHMENT_TOO_LARGE
E_BAD_INPUT E_BINDING E_BLOB_HASH E_BLOB_OPEN E_BODY_TOO_LARGE E_CAPACITY E_CBOR_DEPTH E_CBOR_NONCANONICAL
E_CBOR_RANGE E_CBOR_SHAPE E_CBOR_TRAILING E_CBOR_TRUNCATED E_CBOR_TYPE E_CBOR_UTF8 E_CHANNEL_GONE
E_COMMITMENT_INVALID E_COMMIT_CONFLICT E_COMMIT_INVALID E_COMMIT_REQUIRED E_CORE_DECODE E_CORE_INPUT
E_CORE_LIMIT E_CORE_MLS E_CORE_NOT_FOUND E_CORE_NO_IDENTITY E_CORE_RELOAD E_CORE_STATE E_CORE_STORAGE E_CORE_WASM
E_CREDENTIAL E_DEVICE_LIST_STALE E_DEVICE_REVOKED E_DEVICE_UNLISTED E_ENVELOPE_LIMIT E_ENVELOPE_SHAPE E_FORBIDDEN
E_FRAME_CBOR E_FRAME_SHAPE E_GONE E_GROUP_EXISTS E_HTTP E_INTERNAL E_INVALID_REQUEST E_INVITE_INVALID
E_INVITE_NOT_COMMUNITY E_KEK_DB E_KEK_EXISTS E_KEK_UNWRAP E_LEAF_NOT_CURRENT E_LIST_RACE E_NETWORK
E_NOT_FOUND E_NOT_READY E_NOT_UPLOADER E_NO_ASSERTION E_NO_BACKUP E_OWN_UNKNOWN E_PRUNED E_RATE_LIMITED
E_READ_MAX E_RECOVERY_KEY E_REGISTER_RACE E_ROOT_MISMATCH E_ROUTE_INPUT E_SENDER_MISMATCH E_SESSION_SCOPE
E_SETTING_KEY E_SIGNIN_EVICTED E_SIGNIN_EXPIRED E_STORAGE_FULL E_STORE_BUSY E_STORE_KEY E_STORE_NEWER E_STORE_OPEN E_STORE_PAUSED E_SYNC_STOPPED
E_TIER_MISMATCH E_TRAY_NOT_READY E_UMK_CHANGED E_UNAUTHENTICATED E_UNAVAILABLE E_UNREADABLE E_UNSUPPORTED
E_CALL_FULL E_CALL_SHARERS_FULL E_CALL_ACTIVE E_REMOVE_PENDING E_NO_CALL E_DISCONNECTED E_MEDIA_MISROUTE
E_FACTOR_REQUIRED E_PASSKEY_UNKNOWN E_PASSKEY_OTHER_ACCOUNT E_STEP_UP E_TARGET_NO_FACTOR E_KEYPACKAGES_EXHAUSTED`.split(/\s+/));

const NO_INTERACTION_GROUP = 'no active interaction group for this community';
const AMBIGUOUS_INTERACTION_GROUP = 'multiple active interaction groups make the Bot target ambiguous';
const INTERACTION_CLOCK_SKEW = 'no active interaction group: its Welcome was refused because this device clock is off from the instance clock';
/** The only E_CORE_STATE refusals whose detail leaves the worker, each as a fixed code: a bounded allowlist of local
 *  states, never message content, ids or key material. Every other refusal keeps detail ''. */
const CORE_STATE_DETAILS: ReadonlyMap<string, string> = new Map([
  [NO_INTERACTION_GROUP, 'missing_interaction_group'],
  [AMBIGUOUS_INTERACTION_GROUP, 'ambiguous_interaction_group'],
  [INTERACTION_CLOCK_SKEW, 'interaction_clock_skew'],
]);

/** The sole mapping of worker-local exceptions into page-bound data. */
export function sanitizeWorkerError(e: unknown): WorkerError {
  // A store a newer client wrote (C4x): core_open rejects with a plain Error, so it is read from the message here.
  const store = storeNewerOf(e);
  if (store !== null) return { code: 'E_STORE_NEWER', detail: '', status: 0, retryAfterMs: null, store };
  let code = 'E_INTERNAL';
  if (e instanceof DillaHttpError || e instanceof CoreError || e instanceof SyncError || e instanceof CborError
    || e instanceof AttachmentError || e instanceof Refusal) code = e.code;
  else if (e instanceof Error && ACCOUNT_CODES.has(e.message)) code = e.message;
  else if (typeof e === 'object' && e !== null && 'code' in e && e.code === 'E_NOT_READY') code = 'E_NOT_READY';
  if (!ALLOWED_CODES.has(code)) code = 'E_INTERNAL';
  const status = e instanceof DillaHttpError && Number.isInteger(e.status) && e.status >= 100 && e.status <= 599 ? e.status : 0;
  const retry = e instanceof DillaHttpError ? e.retryAfterMs : null;
  const detail = e instanceof Refusal && code === 'E_CORE_STATE' ? CORE_STATE_DETAILS.get(e.detail) ?? '' : '';
  return { code, detail, status, retryAfterMs: typeof retry === 'number' && Number.isFinite(retry) && retry >= 0 && retry <= 86_400_000 ? retry : null,
    ...(e instanceof Refusal && e.reason !== undefined && REFUSAL_REASONS.has(e.reason) ? { reason: e.reason } : {}) };
}
/** The stable reasons a local refusal may carry to the page. None since web3.8 R5 removed 'announce.noHeldRole' (an
 *  announcement's creator gets her own send entry, so no create is refused for the roles she holds). */
const REFUSAL_REASONS: ReadonlySet<string> = new Set<string>();
const errorOf = sanitizeWorkerError;
function sanitizePageError(error: WorkerError): WorkerError {
  const store = error.code === 'E_STORE_NEWER' && error.store !== undefined
    && Number.isSafeInteger(error.store.found) && Number.isSafeInteger(error.store.supported)
    ? { found: error.store.found, supported: error.store.supported } : null;
  return { code: ALLOWED_CODES.has(error.code) ? error.code : 'E_INTERNAL', detail: '',
    status: Number.isInteger(error.status) && error.status >= 100 && error.status <= 599 ? error.status : 0,
    retryAfterMs: typeof error.retryAfterMs === 'number' && Number.isFinite(error.retryAfterMs)
      && error.retryAfterMs >= 0 && error.retryAfterMs <= 86_400_000 ? error.retryAfterMs : null,
    ...(store === null ? {} : { store }), ...(error.reason !== undefined && REFUSAL_REASONS.has(error.reason) ? { reason: error.reason } : {}) };
}

/** An account code thrown as Error(code) by the account modules (enrol.ts, signup.ts, session.ts). */
function isCode(e: unknown, code: string): boolean {
  return e instanceof Error && e.message === code;
}

function isStatus(e: unknown, status: number): boolean {
  return e instanceof DillaHttpError && e.status === status;
}

// The core's own refusal details the controller acts on where it makes the call (pre-flight rulings (c) and (e)
// of task 14; core/dilla-core/src/client/identity.rs). They are matched only here, inside the worker, at the
// call site; the page never switches on a detail: what crosses is E_LIST_RACE or E_NO_BACKUP.
const LIST_CONFLICT: ReadonlySet<string> = new Set([
  'the instance served an older device list', 'the instance served a different device list at the stored version',
]);
const STATE_UNUSABLE: ReadonlySet<string> = new Set(['the backup state is missing', 'the backup state could not be read']);
function coreInput(e: unknown, details: ReadonlySet<string>): boolean {
  return e instanceof CoreError && e.code === 'E_CORE_INPUT' && details.has(e.detail);
}

type Method = Command['m'];
const COMMANDS: ReadonlySet<string> = new Set<Method>([
  'start',
  'runtimeReady',
  'signupBegin',
  'signupSubmit',
  'signupReset',
  'resetDevice',
  'joinCommunity',
  'selectCommunity',
  'createCommunity',
  'createChannel',
  'createInvite',
  'refreshCommunities',
  'getBotInstallation',
  'getBotCommandStatus',
  'patchBotProfile',
  'listModerationCases',
  'listManualModerationEvidence',
  'humanInteractionRequest',
  'readBotInteractionResponses',
  'ackBotInteractionResponses',
  'readHumanInteractionResponses',
  'ackHumanInteractionResponses',
  'getBotJoinRole',
  'configureBotJoinRole',
  'restartBot',
  'setBotMusicSettings',
  'listSelfRoles',
  'configureSelfRoles',
  'selectSelfRole',
  'registerRolePanel',
  'configureBotInstallation',
  'disableBotInstallation',
  'openChannel',
  'closeChannel',
  'loadEarlier',
  'openThread',
  'closeThread',
  'loadThreads',
  'send',
  'retrySend',
  'discardSend',
  'signInBegin',
  'signInOIDC',
  'signInLogin',
  'signInTotp',
  'signInKey',
  'signInCancel',
  'signInBack',
  'signInPasskeyBegin',
  'signInPasskey',
  'signInBackupCode',
  'factorSetupDone',
  'refreshFactors',
  'factorTotpBegin',
  'factorTotpConfirm',
  'factorPasskeyBegin',
  'factorPasskeyFinish',
  'stepUp',
  'stepUpPasskeyBegin',
  'stepUpPasskeyFinish',
  'removeFactor',
  'changePassword',
  'freshSession',
  'regenerateBackupCodes',
  'refreshDevices',
  'revokeDevice',
  'renameDevice',
  'signOutRevoke',
  'forgetBrowser',
  'markRead',
  'setSetting',
  'setPresenceChoice',
  'updateProfile',
  'presenceActivity',
  'getAFKSettings',
  'setAFK',
  'afkActivity',
  'afkAway',
  'afkRead',
  'openDm',
  'editMessage',
  'deleteMessage',
  'moderateRemoveMessage',
  'resolveManualRemoval',
  'react',
  'pin',
  'loadPins',
  'closePins',
  'attachFiles',
  'checkpointFiles',
  'discardAttachment',
  'openAttachment',
  'gifStatus',
  'gifSetKey',
  'gifClearKey',
  'gifSetRating',
  'gifSearch',
  'gifFetch',
  'refreshTrust',
  'verifyContact',
  'safetyNumber',
  'search',
  'typing',
  'voiceSelf',
  'resign',
  'callJoin',
  'callLeave',
  'callShare',
  'callRtt',
  'callRefresh',
  'callStats',
  'callNote',
  'loadVoice',
  'callDecline',
  'renameCommunity',
  'deleteCommunity',
  'leaveCommunity',
  'transferOwnership',
  'createCategory',
  'updateChannel',
  'moveChannel',
  'deleteChannel',
  'kickMember',
  'banMember',
  'unbanMember',
  'getMemberTimeout',
  'setMemberTimeout',
  'clearMemberTimeout',
  'warnMember',
  'loadBans',
  'loadInvites',
  'revokeInvite',
  'createRole',
  'updateRole',
  'moveRole',
  'deleteRole',
  'grantRole',
  'revokeRole',
  'setOverwrite',
  'setChannelPrivate',
]);
/** The account's user entry with its profile read from `me`: the status message and bio only when set. */
function profileOf(user: NonNullable<AccountState['user']>, me: { display: string; statusMsg?: string; bio?: string }): NonNullable<AccountState['user']> {
  const next = { ...user, display: me.display };
  if (me.statusMsg) next.statusMsg = me.statusMsg; else delete next.statusMsg;
  if (me.bio) next.bio = me.bio; else delete next.bio;
  return next;
}

/** The saved profile fields as a member row carries them: '' for a cleared one. */
function savedOf(saved: { display?: string; statusMsg?: string; bio?: string }): { display?: string; statusMsg?: string; bio?: string } {
  return {
    ...(saved.display === undefined ? {} : { display: saved.display }),
    ...(saved.statusMsg === undefined ? {} : { statusMsg: saved.statusMsg }),
    ...(saved.bio === undefined ? {} : { bio: saved.bio }),
  };
}

const ID = /^[0-9a-f]{32}$/;
const botInstallationView = (row: BotInstallationRow) => ({
  id: toHex(row.id), communityId: toHex(row.communityId), state: row.state,
  revision: row.revision.toString(), userId: row.userId === null ? null : toHex(row.userId),
  deviceId: row.deviceId === null ? null : toHex(row.deviceId), capabilities: row.capabilities,
  updatedAt: row.updatedAt.toString(),
  // Task 10c: only a widened read carries these; a mutation's answer leaves them out.
  ...(row.extra === undefined ? {} : { restartRequested: row.extra.restartRequested.toString(), restartHandled: row.extra.restartHandled.toString(),
    version: row.extra.version, musicVolume: row.extra.musicVolume, musicIdleMinutes: row.extra.musicIdleMinutes }),
});
const TOTP = /^[0-9]{6}$/;
/** A backup code (web3.6): 26 base32 characters once '-' and spaces are dropped and it is upper-cased. */
const backupCode = normaliseBackupCode;
/** The WebAuthn response a page hands over for a passkey sign-in: at most 64 KiB of JSON. */
const PASSKEY_RESPONSE_MAX_BYTES = 65_536;
const ROLE_PANEL_ACTION_RETRY_BASE_MS = 500;
const ROLE_PANEL_ACTION_RETRY_MAX_MS = 8_000;
const ROLE_PANEL_ACTION_RETRY_LIMIT = 5;
type PendingRolePanelAction = { communityId: string; channelId: string; messageSeq: string; groupId: string; eventSeq: string;
  emoji: string; enabled: boolean; userId: string; deviceId: string };
/** The account slice's view of GET /v1/instance: auth_methods 0 password, 1 totp, 2 passkey. */
const instanceStateOf = (instance: Instance): NonNullable<AccountState['instance']> => ({
  id: toHex(instance.instanceId), name: instance.name, registrationMode: instance.registrationMode,
  passwordSignup: instance.authMethods.includes(0), totp: instance.authMethods.includes(1), passkeys: instance.authMethods.includes(2), oidc: instance.authMethods.includes(3),
});
const signInOf = (username: string | null, factor: 'totp' | 'passkey' | null, passkey = false): NonNullable<AccountState['signIn']> =>
  ({ username, factor, ...(passkey ? { passkey: true as const } : {}) });
const SEQ = /^[1-9][0-9]{0,19}$/;
const GIF_ID = /^[A-Za-z0-9]{1,64}$/;
const GIF_KEY = /^[A-Za-z0-9]{8,64}$/;
const TRAY_ID_MAX = 64;
const EMOJI_MAX_BYTES = 32;
const UTF8 = new TextEncoder();
/** AW4: the away roster is sorted by user id, as the instance sorts it. */
const byUserId = (a: { userId: string }, b: { userId: string }): number => (a.userId < b.userId ? -1 : a.userId > b.userId ? 1 : 0);
/** AW4: the roster after one op 55 change (since 0 = cleared), or null when it changes nothing: a clear of a member who
 *  is not away, or a set no newer than the held since (stale). */
function awayPatched(away: readonly { userId: string; since: number }[], change: { userId: string; since: number }):
  { userId: string; since: number }[] | null {
  const at = away.findIndex((row) => row.userId === change.userId);
  if (change.since === 0) return at < 0 ? null : away.filter((_, i) => i !== at);
  if (at >= 0 && away[at].since >= change.since) return null;
  return [...away.filter((_, i) => i !== at), change].sort(byUserId);
}
const ID_FIELDS: Partial<Record<Method, readonly ('communityId' | 'channelId' | 'callId' | 'msgId' | 'deviceId' | 'userId' | 'inviteId' | 'roleId' | 'targetId' | 'rootMsgId' | 'requestId' | 'installationId' | 'proofRequestId')[]>> = {
  selectCommunity: ['communityId'],
  getBotInstallation: ['communityId'],
  getBotCommandStatus: ['communityId'],
  patchBotProfile: ['communityId'],
  listModerationCases: ['communityId', 'requestId'],
  listManualModerationEvidence: ['communityId', 'requestId'],
  humanInteractionRequest: ['communityId', 'requestId'],
  readBotInteractionResponses: ['communityId'],
  ackBotInteractionResponses: ['communityId'],
  readHumanInteractionResponses: ['communityId'],
  ackHumanInteractionResponses: ['communityId'],
  getBotJoinRole: ['communityId'],
  configureBotJoinRole: ['communityId', 'requestId', 'roleId', 'installationId'],
  restartBot: ['communityId', 'installationId'],
  setBotMusicSettings: ['communityId', 'installationId'],
  listSelfRoles: ['communityId'],
  configureSelfRoles: ['communityId', 'requestId'],
  selectSelfRole: ['communityId', 'roleId', 'requestId'],
  registerRolePanel: ['communityId', 'requestId', 'channelId'],
  configureBotInstallation: ['communityId', 'requestId'],
  disableBotInstallation: ['communityId', 'requestId'],
  createChannel: ['communityId'],
  createRole: ['communityId'],
  updateRole: ['communityId', 'roleId'],
  moveRole: ['communityId', 'roleId'],
  deleteRole: ['communityId', 'roleId'],
  grantRole: ['communityId', 'userId', 'roleId'],
  revokeRole: ['communityId', 'userId', 'roleId'],
  setOverwrite: ['channelId', 'targetId'],
  setChannelPrivate: ['channelId'],
  createInvite: ['communityId'],
  openChannel: ['channelId'],
  closeChannel: ['channelId'],
  loadEarlier: ['channelId'],
  openThread: ['channelId', 'rootMsgId'],
  closeThread: ['channelId'],
  loadThreads: ['channelId'],
  send: ['channelId'],
  retrySend: ['msgId'],
  discardSend: ['msgId'],
  revokeDevice: ['deviceId'],
  renameDevice: ['deviceId'],
  markRead: ['channelId'],
  openDm: ['userId'],
  editMessage: ['channelId', 'msgId'],
  deleteMessage: ['channelId', 'msgId'],
  moderateRemoveMessage: ['channelId', 'msgId'],
  resolveManualRemoval: ['proofRequestId'],
  react: ['channelId', 'msgId'],
  pin: ['channelId', 'msgId'],
  loadPins: ['channelId'],
  closePins: ['channelId'],
  attachFiles: ['channelId'],
  checkpointFiles: ['channelId'],
  discardAttachment: ['channelId'],
  openAttachment: ['channelId'],
  gifStatus: ['communityId'],
  gifSetKey: ['communityId'],
  gifClearKey: ['communityId'],
  gifSetRating: ['communityId'],
  gifSearch: ['communityId'],
  gifFetch: ['communityId'],
  verifyContact: ['userId'],
  safetyNumber: ['userId'],
  typing: ['channelId'],
  voiceSelf: ['callId'],
  callJoin: ['channelId'],
  callDecline: ['callId'],
  renameCommunity: ['communityId'],
  getAFKSettings: ['communityId'],
  setAFK: ['communityId'],
  afkActivity: ['communityId', 'channelId'],
  afkAway: ['communityId'],
  afkRead: ['communityId'],
  deleteCommunity: ['communityId'],
  leaveCommunity: ['communityId'],
  transferOwnership: ['communityId', 'userId'],
  createCategory: ['communityId'],
  updateChannel: ['channelId'],
  moveChannel: ['channelId'],
  deleteChannel: ['channelId'],
  kickMember: ['communityId', 'userId'],
  banMember: ['communityId', 'userId'],
  unbanMember: ['communityId', 'userId'],
  getMemberTimeout: ['communityId', 'userId'],
  setMemberTimeout: ['communityId', 'userId', 'requestId'],
  clearMemberTimeout: ['communityId', 'userId', 'requestId'],
  warnMember: ['communityId', 'userId', 'requestId'],
  loadBans: ['communityId'],
  loadInvites: ['communityId'],
  revokeInvite: ['communityId', 'inviteId'],
};
/** The commands each phase accepts besides start, which every phase accepts (requirement 1). Exported for its tests. */
export const ACCEPTS: Record<BootPhase, readonly Method[]> = {
  'loading': [], 'other-tab': [], 'paused-error': [], 'unsupported': [], 'error': [], 'registering': [], 'enrolling': [], 'cleared': [],
  // Pre-flight ruling (d) of task 14: a revoked browser can start over (the splash's "forget this browser").
  'revoked': ['forgetBrowser'],
  'store-lost': ['resetDevice'],
  'needs-signup': ['signupBegin', 'signInBegin'],
  'signup-keys': ['signupSubmit', 'signupReset'],
  // web3.6: a passkey on its own signs in from the login step (W2's usePasskey), so signin-login takes its finish too.
  'signin-login': ['signInOIDC', 'signInLogin', 'signInPasskeyBegin', 'signInPasskey', 'signInCancel'],
  'signin-totp': ['signInTotp', 'signInPasskeyBegin', 'signInPasskey', 'signInBackupCode', 'signInCancel', 'signInBack'],
  'signin-passkey': ['signInPasskeyBegin', 'signInPasskey', 'signInBackupCode', 'signInCancel', 'signInBack'],
  'signin-key': ['signInKey', 'signInCancel', 'signInBack'],
  // web3.6: a person owing a factor enrols one (the five that need no step-up) and reads the list; the rest need a factor held.
  // Walk M9: a tab on the forced screen hands the store to a tab that asks ("Use this tab"), as a ready one does.
  'factor-setup': ['factorSetupDone', 'forgetBrowser', 'refreshFactors', 'factorTotpBegin', 'factorTotpConfirm', 'factorPasskeyBegin', 'factorPasskeyFinish', 'resign'],
  'ready': [
    'joinCommunity',
    'selectCommunity',
    'createCommunity',
    'createChannel',
    'createInvite',
    'refreshCommunities',
    'getBotInstallation',
    'getBotCommandStatus',
    'patchBotProfile',
    'listModerationCases',
    'listManualModerationEvidence',
    'humanInteractionRequest',
    'readBotInteractionResponses',
    'ackBotInteractionResponses',
    'readHumanInteractionResponses',
    'ackHumanInteractionResponses',
    'getBotJoinRole',
    'configureBotJoinRole',
    'restartBot',
    'setBotMusicSettings',
    'listSelfRoles',
    'configureSelfRoles',
    'selectSelfRole',
    'registerRolePanel',
    'configureBotInstallation',
    'disableBotInstallation',
    'openChannel',
    'closeChannel',
    'loadEarlier',
    'send',
    'retrySend',
    'discardSend',
    'refreshDevices',
    'revokeDevice',
    'renameDevice',
    'signOutRevoke',
    'forgetBrowser',
    'refreshFactors',
    'factorTotpBegin',
    'factorTotpConfirm',
    'factorPasskeyBegin',
    'factorPasskeyFinish',
    'stepUp',
    'stepUpPasskeyBegin',
    'stepUpPasskeyFinish',
    'removeFactor',
    'changePassword',
    'freshSession',
    'regenerateBackupCodes',
    'markRead',
    'setSetting',
    'setPresenceChoice',
    'updateProfile',
    'presenceActivity',
    'getAFKSettings',
    'setAFK',
    'afkActivity',
    'afkAway',
    'afkRead',
    'openDm',
    'editMessage',
    'deleteMessage',
    'moderateRemoveMessage',
    'resolveManualRemoval',
    'react',
    'pin',
    'loadPins',
    'closePins',
    'openThread',
    'closeThread',
    'loadThreads',
    'attachFiles',
    'checkpointFiles',
    'discardAttachment',
    'openAttachment',
    'gifStatus',
    'gifSetKey',
    'gifClearKey',
    'gifSetRating',
    'gifSearch',
    'gifFetch',
    'refreshTrust',
    'verifyContact',
    'safetyNumber',
    'search',
    'typing',
    'voiceSelf',
    'resign',
    'callJoin',
    'callLeave',
    'callShare',
    'callRtt',
    'callRefresh',
    'callStats',
    'callNote',
    'loadVoice',
    'callDecline',
    'renameCommunity',
    'deleteCommunity',
    'leaveCommunity',
    'transferOwnership',
    'createCategory',
    'updateChannel',
    'moveChannel',
    'deleteChannel',
    'kickMember',
    'banMember',
    'unbanMember',
    'getMemberTimeout',
    'setMemberTimeout',
    'clearMemberTimeout',
    'warnMember',
    'loadBans',
    'loadInvites',
    'revokeInvite',
    'createRole',
    'updateRole',
    'moveRole',
    'deleteRole',
    'grantRole',
    'revokeRole',
    'setOverwrite',
    'setChannelPrivate',
  ],
};

const blank = (value: unknown): boolean => typeof value !== 'string' || value.trim() === '';
/** A device name's length limit, in characters; the server applies the same shared text rule as to a display name. */
const DEVICE_LABEL_MAX_CHARS = 64;
/** A device name as the server stores it (NormalizeUserText, one line): null when the value is not a valid one. Empty is not
 *  a name; clearing is null. */
const deviceLabel = (value: unknown): string | null => {
  if (typeof value !== 'string' || profileTextProblem(value, DEVICE_LABEL_MAX_CHARS, false) !== null) return null;
  const label = profileTextValue(value);
  return label === '' ? null : label;
};

/** L-TS-50: '' or one to three of vp8, h264 and vp9, comma-separated, none twice. */
const VDEC = /^(vp8|h264|vp9)(,(vp8|h264|vp9)){0,2}$/;
const validVdec = (v: unknown): boolean => typeof v === 'string' && (v === '' || (VDEC.test(v) && new Set(v.split(',')).size === v.split(',').length));
const STATS_MAX = 1_048_576;
/** fix/firefox-misrouted-audio: the stable codes a page may note for Diagnostics (callNote); nothing else is taken. */
const CALL_NOTES: ReadonlySet<string> = new Set(['E_MEDIA_MISROUTE']);
/** f10app m3: a voice-driven member-list re-read of a community starts no sooner than this after the last one ended, and
 *  at most this many users per community enter the looked-up set from voice_state frames (the instance's, unauthenticated). */
export const MEMBER_REREAD_MIN_MS = 5_000;
/** WB client m5: resign waits at most this long for the call's leave before it releases the store. */
export const RESIGN_LEAVE_MS = 2_000;
export const VOICE_LOOKUPS_MAX = 256;
/** web3.8 VM: how often per join of a call the own voice_self is sent again while the instance's voice_state lacks it. */
export const VOICE_SELF_RESENDS = 3;
/** web3.8 MUTE: how often a voice_self made while offline is sent by the reconnect itself (its own budget, apart from
 *  VOICE_SELF_RESENDS). */
export const VOICE_SELF_RECONNECT_SENDS = 1;
const statsField = (v: unknown): boolean => typeof v === 'number' && Number.isSafeInteger(v) && v >= 0 && v <= STATS_MAX;
/** L-TS-53: six non-negative integers of at most 1,048,576; relayProtocol (element 1) may be null. */
const validReport = (r: unknown): boolean => Array.isArray(r) && r.length === 6
  && r.every((v: unknown, i) => statsField(v) || (i === 1 && v === null));
const optionalId = (v: unknown): boolean => v === null || (typeof v === 'string' && ID.test(v));
const isRecord = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);

/** Per-feature synchronous checks (wave 1+): each returns an E_BAD_INPUT reason or null. */
const VALIDATORS: Partial<Record<Method, (c: Record<string, unknown>) => string | null>> = {
  ...ADMIN_VALIDATORS,
  ...ROLES_VALIDATORS,
  ...FACTOR_VALIDATORS,
};

/** The synchronous input checks, in the order requirement 2 fixes; null when the command is well formed.
 *  Every refusal here answers only the owning page's own malformed command (deps.post reaches only that page). */
function invalid(command: Record<string, unknown>): string | null {
  const m = command.m as string;
  if (!COMMANDS.has(m)) return `unknown command ${m}`;
  if (m === 'runtimeReady' && Object.keys(command).length !== 1) return 'runtimeReady takes no fields';
  for (const field of ID_FIELDS[m as Method] ?? []) {
    const value = command[field];
    if (typeof value !== 'string' || !ID.test(value)) return `${field} is not 32 lowercase hex`;
  }
  if (m === 'joinCommunity' && blank(command.invite)) return 'invite is empty';
  if (m === 'moderateRemoveMessage') {
    const reason = command.reason; const excerpt = command.excerpt;
    if (typeof reason !== 'string' || reason.trim().length === 0 || new TextEncoder().encode(reason).length > 1024
      || typeof excerpt !== 'string' || new TextEncoder().encode(excerpt).length > 512) return 'manual moderation reason or excerpt is invalid';
  }
  if (m === 'warnMember') {
    const reason = command.reason;
    if (typeof reason !== 'string' || reason.trim().length === 0 || /[\u0000-\u0008\u000b\u000c\u000d\u000e-\u001f\u007f-\u009f]/.test(reason)
      || new TextEncoder().encode(reason).length > 512) return 'manual warning reason is invalid';
  }
  if (m === 'createCommunity') {
    const problem = blank(command.name) ? 'is blank' : nameProblem(command.name, COMMUNITY_NAME_MAX_BYTES);
    if (problem !== null) return `name ${problem}`;
  }
  if (m === 'createChannel') {
    if (command.kind !== 'text' && command.kind !== 'voice' && command.kind !== 'announcement') return 'kind is not text, voice or announcement';
    if (command.parentId !== undefined && !(typeof command.parentId === 'string' && ID.test(command.parentId))) return 'parentId is not 32 lowercase hex';
    const problem = blank(command.name) ? 'is blank' : nameProblem(command.name, CHANNEL_NAME_MAX_BYTES);
    if (problem !== null) return `name ${problem}`;
    if (command.private !== undefined) {
      if (command.kind === 'announcement') return 'an announcement channel is not private';
      const p = privateProblem(command.private);
      if (p !== null) return p;
    }
    if (command.announce !== undefined) {
      if (command.kind !== 'announcement') return 'announce is only for an announcement channel';
      const p = announceProblem(command.announce);
      if (p !== null) return p;
    }
  }
  if (m === 'createInvite') {
    const { maxUses, ttlSeconds } = command;
    // 0 is "∞" uses and "never" expires (Task 10c); the instance asks manage_community for either.
    if (typeof maxUses !== 'number' || !Number.isSafeInteger(maxUses) || maxUses < 0 || maxUses > INVITE_USES_MAX) {
      return `maxUses is not an integer 0..${String(INVITE_USES_MAX)}`;
    }
    if (typeof ttlSeconds !== 'number' || !Number.isSafeInteger(ttlSeconds) || ttlSeconds < 0 || ttlSeconds > INVITE_TTL_MAX_S) {
      return `ttlSeconds is not an integer 0..${String(INVITE_TTL_MAX_S)}`;
    }
  }
  if (m === 'configureSelfRoles') {
    if (!Array.isArray(command.roleIds) || command.roleIds.length > 250
      || command.roleIds.some((value: unknown) => typeof value !== 'string' || !ID.test(value))
      || new Set(command.roleIds).size !== command.roleIds.length) return 'roleIds are invalid';
  }
  if (m === 'configureBotJoinRole' && typeof command.roleId !== 'string') return 'roleId is invalid';
  if (m === 'setBotMusicSettings' && (!(BOT_MUSIC_VOLUMES as readonly unknown[]).includes(command.volume)
    || !(BOT_MUSIC_IDLE_MINUTES as readonly unknown[]).includes(command.idleMinutes))) return 'volume or idle minutes are not an option';
  if (m === 'registerRolePanel') {
    const seq = command.messageSeq;
    if (typeof seq !== 'string' || !/^[1-9][0-9]{0,18}$/u.test(seq) || BigInt(seq) > 0x7fff_ffff_ffff_ffffn) return 'messageSeq is invalid';
    if (!Array.isArray(command.options) || command.options.length < 1 || command.options.length > 100) return 'role panel options are invalid';
    const emojis = new Set<string>(); const roles = new Set<string>();
    for (const option of command.options) {
      if (!isRecord(option) || typeof option.emoji !== 'string' || new TextEncoder().encode(option.emoji).length < 1
        || new TextEncoder().encode(option.emoji).length > 32 || typeof option.roleId !== 'string' || !ID.test(option.roleId)
        || option.roleId === '0'.repeat(32) || emojis.has(option.emoji) || roles.has(option.roleId)) return 'role panel options are invalid';
      emojis.add(option.emoji); roles.add(option.roleId);
    }
  }
  if (m === 'listModerationCases' && command.offset !== 0) return 'offset must be zero';
  if (m === 'listModerationCases' && command.requestId === '0'.repeat(32)) return 'requestId must be nonzero';
  if (m === 'listManualModerationEvidence' && (command.requestId === '0'.repeat(32)
    || command.cursor !== null && (typeof command.cursor !== 'string' || !/^(?:[0-9a-f]{2}){1,256}$/u.test(command.cursor)))) return 'manual evidence cursor is invalid';
  if (m === 'humanInteractionRequest') {
    const operation = command.operation;
    if (typeof operation !== 'number' || !Number.isInteger(operation)
      || !(operation === 0 || operation === 1 || operation === 2 || operation === 3 || operation >= 10 && operation <= 25 || operation === 31
        || operation >= 50 && operation <= 52 || operation >= 61 && operation <= 63 || operation >= 70 && operation <= 75
        || operation >= 80 && operation <= 84)) return 'interaction operation is invalid';
    if (command.requestId === '0'.repeat(32)) return 'requestId must be nonzero';
    const music = operation >= 10 && operation <= 23 || operation === 25;
    if (command.channelId !== null && (typeof command.channelId !== 'string' || !ID.test(command.channelId))) return 'channelId is invalid';
    if (music !== (typeof command.channelId === 'string')) return 'interaction channel context is invalid';
    const count = operation === 1 || operation === 50 || [11, 12, 17, 18, 20, 21, 25, 70, 71, 74, 80, 81, 82, 83, 84].includes(operation) ? 1
      : operation === 2 || operation === 22 || operation === 31 || operation === 51 || operation === 52 || operation === 63 || operation === 73 ? 2
        : operation === 61 || operation === 72 || operation === 75 ? 3 : 0;
    if (!Array.isArray(command.args) || command.args.length !== count && !(operation === 15 && command.args.length === 1)) return 'interaction arguments are invalid';
    if (operation === 15 && command.args.length === 1 && (typeof command.args[0] !== 'string' || command.args[0] !== ''
      && (!ID.test(command.args[0]) || command.args[0] === '0'.repeat(32)))) return 'skip current item is invalid';
    if (operation === 62 && command.args.length !== 0) return 'automatic moderation read arguments are invalid';
    if ((operation === 1 || operation === 2) && (typeof command.args[0] !== 'number' || !Number.isInteger(command.args[0]) || command.args[0] < 0 || command.args[0] > 5)) return 'settings section is invalid';
    if (operation === 2 && (!(command.args[1] instanceof Uint8Array) || command.args[1].length === 0 || command.args[1].length > 2400)) return 'settings document is invalid';
    if (operation === 31 && (typeof command.args[0] !== 'string' || !ID.test(command.args[0])
      || !(command.args[1] instanceof Uint8Array) || command.args[1].length === 0 || command.args[1].length > 2200)) return 'role panel request is invalid';
    if (operation === 50 && (typeof command.args[0] !== 'number' || !Number.isSafeInteger(command.args[0]) || command.args[0] < 0)) return 'announcement list offset is invalid';
    if (operation === 51 && (command.args[0] !== null && (typeof command.args[0] !== 'string' || !ID.test(command.args[0]))
      || !(command.args[1] instanceof Uint8Array) || command.args[1].length === 0 || command.args[1].length > 2200)) return 'announcement schedule is invalid';
    if (operation === 52 && (typeof command.args[0] !== 'string' || !ID.test(command.args[0])
      || typeof command.args[1] !== 'number' || !Number.isSafeInteger(command.args[1]) || command.args[1] < 1)) return 'announcement delete is invalid';
    if (operation === 61 && (typeof command.args[0] !== 'string' || !ID.test(command.args[0])
      || typeof command.args[1] !== 'string' || new TextEncoder().encode(command.args[1]).length < 1 || new TextEncoder().encode(command.args[1]).length > 1024
      || typeof command.args[2] !== 'string' || new TextEncoder().encode(command.args[2]).length > 512)) return 'manual moderation evidence is invalid';
    if (operation === 63 && (typeof command.args[0] !== 'string' || !(command.args[1] instanceof Uint8Array)
      || command.args[1].length === 0 || command.args[1].length > 2400 || typeof command.stepUp !== 'string'
      || command.stepUp.length < 1 || command.stepUp.length > 4096)) return 'automatic moderation save is invalid';
    if ((operation === 11 || operation === 12) && (typeof command.args[0] !== 'string' || command.args[0].trim().length === 0)) return 'music query is invalid';
    if ([17, 18, 20].includes(operation) && (typeof command.args[0] !== 'number' || !Number.isSafeInteger(command.args[0])
      || command.args[0] < 0 || command.args[0] > (operation === 18 ? 100 : operation === 20 ? 2 : Number.MAX_SAFE_INTEGER))) return 'music numeric argument is invalid';
    if ((operation === 21 || operation === 22) && (typeof command.args[0] !== 'string' || !ID.test(command.args[0]))) return 'music queue item id is invalid';
    if (operation === 25 && (typeof command.args[0] !== 'string' || !ID.test(command.args[0]) || command.revision !== '0')) return 'music announcement is invalid';
    if (operation === 22 && command.args[1] !== null && (typeof command.args[1] !== 'string' || !ID.test(command.args[1]))) return 'music destination id is invalid';
    const mutation = operation === 2 || operation >= 12 && operation <= 23 || operation === 25 || operation === 31 || operation === 51 || operation === 52
      || operation === 61 || operation === 63 || operation === 70 || operation === 73 || operation === 74 || operation === 80
      || operation === 82 || operation === 83;
    const revision = command.revision;
    if (mutation ? typeof revision !== 'string' || !/^(0|[1-9][0-9]{0,19})$/u.test(revision) || BigInt(revision) > 0xffff_ffff_ffff_ffffn
      : revision !== null) return 'interaction revision is invalid';
  }
  if (m === 'readBotInteractionResponses' && (!Array.isArray(command.operations) || command.operations.length === 0
    || command.operations.length > 35 || command.operations.some((op: unknown) => typeof op !== 'number' || !Number.isInteger(op)
    || !(op === 0 || op === 1 || op === 2 || op === 3 || op >= 10 && op <= 25 || op === 31 || op >= 50 && op <= 52
      || op >= 61 && op <= 63 || op >= 70 && op <= 75 || op >= 80 && op <= 84))
    || new Set(command.operations).size !== command.operations.length)) return 'response operations are invalid';
  if ((m === 'readBotInteractionResponses' || m === 'readHumanInteractionResponses') && command.requestIds !== undefined
    && (!Array.isArray(command.requestIds) || command.requestIds.length < 1 || command.requestIds.length > 100
      || command.requestIds.some((id: unknown) => typeof id !== 'string' || !ID.test(id))
      || new Set(command.requestIds).size !== command.requestIds.length)) return 'response request ids are invalid';
  if (m === 'ackHumanInteractionResponses' && (typeof command.through !== 'string' || !/^[0-9a-f]{32}$/u.test(command.through))) return 'through is invalid';
  if (m === 'ackBotInteractionResponses' && (typeof command.through !== 'string' || !/^[0-9a-f]{32}$/u.test(command.through))) return 'through is invalid';
  if (m === 'selectSelfRole' && typeof command.enabled !== 'boolean') return 'enabled is invalid';
  if (m === 'configureBotInstallation' || m === 'disableBotInstallation') {
    const revision = command.expectedRevision;
    if (!(m === 'configureBotInstallation' && revision === null)
      && (typeof revision !== 'string' || !SEQ.test(revision) || BigInt(revision) > 0xffff_ffff_ffff_ffffn)) {
      return 'expectedRevision is not a valid revision';
    }
    if (m === 'configureBotInstallation') {
      const caps = command.capabilities;
      if (typeof caps !== 'number' || !Number.isSafeInteger(caps) || caps < 0 || caps > BOT_CAPABILITIES_MAX) return 'capabilities are not 0..1023';
      if (typeof command.stepUp !== 'string' || command.stepUp.length < 1 || command.stepUp.length > 4096) return 'stepUp is invalid';
    }
  }
  if (m === 'patchBotProfile' && (typeof command.display !== 'string' || command.display.trim() === ''
    || profileTextProblem(command.display, DISPLAY_MAX_CHARS, false) !== null)) return 'bot display name is invalid';
  if (m === 'setMemberTimeout') {
    const expires = command.expires;
    if (typeof expires !== 'string' || !SEQ.test(expires) || BigInt(expires) > 0xffff_ffff_ffff_ffffn) {
      return 'expires is not a valid Unix-second timestamp';
    }
  }
  if (m === 'send') {
    const { replyTo, attachments, threadId } = command;
    if (replyTo !== undefined && replyTo !== null && (typeof replyTo !== 'string' || !ID.test(replyTo))) return 'replyTo is not 32 lowercase hex';
    if (threadId !== undefined && threadId !== null && (typeof threadId !== 'string' || !ID.test(threadId))) return 'threadId is not 32 lowercase hex';
    if (attachments !== undefined && !(Array.isArray(attachments) && attachments.length <= MAX_ATTACHMENTS
      && attachments.every((t) => typeof t === 'string' && t.length >= 1 && t.length <= TRAY_ID_MAX))) {
      return `attachments is not a list of at most ${String(MAX_ATTACHMENTS)} tray ids`;
    }
    // DEV-W2-60: a message of attachments alone is allowed.
    if (blank(command.text) && !(Array.isArray(attachments) && attachments.length > 0)) return 'the message is empty';
  }
  if (m === 'editMessage' && blank(command.text)) return 'the message is empty';
  if (m === 'react') {
    const { emoji } = command;
    if (typeof emoji !== 'string' || emoji === '' || UTF8.encode(emoji).length > EMOJI_MAX_BYTES) return 'emoji must be 1..=32 bytes';
  }
  if ((m === 'react' || m === 'pin') && typeof command.on !== 'boolean') return 'on is not a boolean';
  if (m === 'attachFiles') {
    const { files } = command;
    if (!Array.isArray(files) || files.length === 0 || !files.every((f) => typeof File !== 'undefined' && f instanceof File)) {
      return 'files is not a non-empty list of files';
    }
  }
  if (m === 'gifSearch') {
    const { q, offset } = command;
    if (typeof q !== 'string' || UTF8.encode(q).length > GIF_QUERY_MAX_BYTES || /\p{Cc}/u.test(q)) return `q is not at most ${String(GIF_QUERY_MAX_BYTES)} bytes of text`;
    if (typeof offset !== 'number' || !Number.isSafeInteger(offset) || offset < 0 || offset > GIF_OFFSET_MAX) return `offset is not an integer 0..${String(GIF_OFFSET_MAX)}`;
  }
  // The key is checked for its shape only; the refusal never names or echoes it.
  if (m === 'gifSetKey' && (typeof command.key !== 'string' || !GIF_KEY.test(command.key))) return 'key is not 8 to 64 letters and digits';
  if (m === 'gifSetRating' && !['g', 'pg', 'pg-13', 'r'].includes(command.rating as string)) return 'rating is not g, pg, pg-13 or r';
  if (m === 'gifFetch') {
    if (typeof command.gifId !== 'string' || !GIF_ID.test(command.gifId)) return 'gifId is not 1..64 letters and digits';
    if (command.variant !== 'preview' && command.variant !== 'full') return 'variant is not preview or full';
  }
  if (m === 'discardAttachment' && (typeof command.trayId !== 'string' || command.trayId === '')) return 'trayId is empty';
  if (m === 'openAttachment') {
    if (typeof command.seq !== 'string' || !SEQ.test(command.seq)) return 'seq is not a decimal sequence number';
    const { index } = command;
    if (typeof index !== 'number' || !Number.isSafeInteger(index) || index < 0) return 'index is not a non-negative integer';
    if (typeof command.thumb !== 'boolean') return 'thumb is not a boolean';
  }
  if (m === 'signupSubmit') {
    for (const field of ['invite', 'username', 'display'] as const) {
      if (typeof command[field] !== 'string') return `${field} is not a string`;
    }
    if (command.password !== null && typeof command.password !== 'string') return 'password is not a string';
    if (command.recoveryKeyAcknowledged !== true) return 'the recovery key is not acknowledged';
  }
  if (m === 'signInOIDC' && (typeof command.assertion !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(command.assertion) || ![0, 1, 2].includes(command.needs as number))) return 'invalid OIDC assertion';
  if (m === 'signInLogin') {
    if (blank(command.username)) return 'username is empty';
    if (typeof command.password !== 'string' || command.password === '') return 'password is empty';
  }
  if (m === 'signInTotp' && (typeof command.code !== 'string' || !TOTP.test(command.code))) return 'code is not six digits';
  if (m === 'signInBackupCode' && (typeof command.code !== 'string' || !BACKUP_CODE.test(backupCode(command.code)))) {
    return 'backup code is not 26 letters and digits';
  }
  if (m === 'signInPasskey') {
    if (typeof command.ceremonyId !== 'string' || !ID.test(command.ceremonyId)) return 'ceremonyId is not 32 lowercase hex';
    const { response } = command;
    if (typeof response !== 'string' || response === '' || UTF8.encode(response).length > PASSKEY_RESPONSE_MAX_BYTES) {
      return 'response is not a string of 1..65536 bytes';
    }
  }
  if (m === 'factorSetupDone' && command.invite !== null && typeof command.invite !== 'string') return 'invite is not a string or null';
  // web3.8 (pre-flight C-3): only the key's commands carry a key; signInLogin, signInTotp and signInPasskey take none.
  if ((m === 'signInKey' || m === 'signInBackupCode' || m === 'signOutRevoke') && blank(command.recoveryKey)) {
    return 'the recovery key is empty';
  }
  if (m === 'revokeDevice' && command.recoveryKey !== null && typeof command.recoveryKey !== 'string') {
    return 'the recovery key is not a string or null';
  }
  if (m === 'renameDevice' && command.label !== null && deviceLabel(command.label) === null) return 'label is not 1..64 characters without control or invisible characters';
  if (m === 'updateProfile') {
    for (const [field, max, multiline] of [['display', DISPLAY_MAX_CHARS, false], ['statusMsg', STATUS_MAX_CHARS, false], ['bio', BIO_MAX_CHARS, true]] as const) {
      const value = command[field];
      if (value === undefined) continue;
      const problem = profileTextProblem(value, max, multiline);
      if (problem !== null) return `${field}: ${problem}`;
    }
  }
  if (m === 'setPresenceChoice' && !['online', 'idle', 'dnd', 'invisible'].includes(command.choice as string)) return 'invalid presence choice';
  if (m === 'setSetting') {
    if (typeof command.key !== 'string') return 'key is not a string';
    if (command.value !== null && typeof command.value !== 'string') return 'value is not a string or null';
  }
  if (m === 'verifyContact' && (typeof command.umk !== 'string' || !/^[0-9a-f]{64}$/.test(command.umk))) return 'umk is not 64 lowercase hex';
  if (m === 'safetyNumber' && (typeof command.umk !== 'string' || !/^[0-9a-f]{64}$/.test(command.umk))) return 'umk is not 64 lowercase hex';
  if (m === 'voiceSelf' && (typeof command.flags !== 'number' || !Number.isInteger(command.flags) || command.flags < 0 || command.flags > 3)) return 'flags are not 0..3';
  if (m === 'typing' && typeof command.on !== 'boolean') return 'on is not a boolean';
  if (m === 'callJoin' && !validVdec(command.vdec)) return 'vdec is not a list of distinct decoders';
  if (m === 'callLeave' && command.reason !== 'user' && command.reason !== 'pagehide' && command.reason !== 'switch' && command.reason !== 'lost') return 'invalid leave reason';
  if (m === 'callShare' && typeof command.on !== 'boolean') return 'on is not a boolean';
  if (m === 'callRtt' && command.ms !== null && (typeof command.ms !== 'number' || !Number.isInteger(command.ms) || command.ms < 0 || command.ms > 5000)) return 'ms is not null or 0..5000';
  if (m === 'callStats' && !validReport(command.report)) return 'invalid call stats report';
  if (m === 'callNote' && !CALL_NOTES.has(command.code as string)) return 'not a call note code';
  if (m === 'loadVoice' && (!optionalId(command.communityId) || !optionalId(command.channelId)
    || (command.communityId === null) === (command.channelId === null))) return 'loadVoice needs exactly one id';
  if (m === 'callDecline' && (typeof command.ringTs !== 'number' || !Number.isSafeInteger(command.ringTs) || command.ringTs < 0)) {
    return 'ringTs is not a non-negative integer';
  }
  if (m === 'resign' && (typeof command.nonce !== 'string' || !ID.test(command.nonce))) return 'nonce is not 32 lowercase hex';
  if (m === 'search') {
    if (command.scope !== 'all' && command.scope !== 'channel' && command.scope !== 'community') return 'invalid search scope';
    if (command.scope === 'all' ? command.id !== null : typeof command.id !== 'string' || !ID.test(command.id)) return 'invalid search id';
    if (typeof command.query !== 'string' || UTF8.encode(command.query.trim()).length < 1 || UTF8.encode(command.query.trim()).length > 128) return 'invalid search query';
    const cursor = command.cursor;
    if (cursor !== null && (typeof cursor !== 'object' || cursor === undefined || Object.keys(cursor).length !== 1
      || !('token' in cursor) || typeof cursor.token !== 'string' || !/^[1-9][0-9]*$/.test(cursor.token))) return 'invalid search cursor';
  }
  const extra = VALIDATORS[m as Method]?.(command) ?? null;
  if (extra !== null) return extra;
  return null;
}

interface KnownChannel { communityId: string; row: ChannelRow }
interface KnownDm { kind: 3 | 4; members: Id[]; groupId: Id | null }
interface OpenChannel { groupId: Id | null; limit: number }
/** A channel or a DM as the channel paths see it (requirement 17): a DM's group is a text group targeted at it. */
interface Target { communityId: string | null; dm: boolean; channel: { id: Id; kind: number; mode: number }; textGroupId: Id | null }

const EMPTY_NOTICES: NoticesState = { nextId: 1, items: [] };
/** A gateway ticket is valid 30 seconds from its mint (protocol/02 § Gateway, 09 § Sessions endpoints). */
const TICKET_TTL_S = 30n;
/** A burst of op 54 community_changed (a server's starter channels, a reorder) costs one list read per server. */
export const COMMUNITY_RELOAD_DEBOUNCE_MS = 250;
/** web3.4 C4: the removals slice keeps this many notices, the newest (the page dismisses them locally). */
export const REMOVALS_MAX = 8;

/** CI1: the reasons a row waits for its sender's trust (core revalidate_pending's set), and the most such live rows
 *  the worker remembers for their notice. E_DEVICE_REVOKED is not one: a revoked device's row never opens, so nothing
 *  waits for it (trust.rs apply_verdict). */
const HELD_REASONS: ReadonlySet<string> = new Set(['E_DEVICE_LIST_STALE', 'E_UMK_CHANGED', 'E_CREDENTIAL', 'E_DEVICE_UNLISTED', 'E_TIER_MISMATCH']);
export const HELD_LIVE_MAX = 64;
/** web3.8 R8: how long (elapsed ms) a live-held row keeps markRead one below it; one held longer stops holding the
 *  marker, so no badge sticks. */
export const HELD_READ_HOLD_MS = 60_000;
const heldForTrust = (row: TimelineRow): boolean => row.status === 1 && HELD_REASONS.has(row.reason);
/** A live row the core holds for its sender's trust, and when it was held (elapsed ms, R8). */
type HeldLive = { groupId: Id; seq: bigint; heldAt: number };

export class Controller {
  private readonly parts: ControllerParts;
  /** The id of the last removal notice (web3.4 C4). */
  private removalSeq = 0;
  /** The sequence of the last community list read begun, and of the last one applied (C4 fix round 1). */
  private listSeq = 0;
  private listApplied = 0;
  private readonly slices: SliceStore;
  private readonly routes: Routes;
  private instance: Instance | null = null;
  private limits: Limits | null = null;
  private starting: Promise<null> | null = null;
  private resetting: Promise<null> | null = null;
  private graceTimer: number | null = null;
  /** A session cut just now. The instance wants one younger than its reauth window for a first password, and the step-up token
   *  is bound to the session it was made in, so the page runs this before the step-up, not after. */
  private async freshSession(): Promise<null> {
    if (this.session === null) throw new Refusal('E_NOT_READY', 'no session');
    // reauthenticate: a refused establish is a revocation (enterRevoked), and a wipe in progress mints nothing.
    if (!(await this.reauthenticate())) throw new Error('E_SIGNIN_EVICTED');
    return null;
  }
  /** The socket re-establish retry onRevokedSocket armed after a network failure (I4). */
  private reauthTimer: number | null = null;
  private phase: BootPhase = 'loading';
  private accountState: AccountState = {
    phase: 'loading', instance: null, user: null, deviceId: null, recoveryKey: null, error: null, signIn: null, rootMismatch: false, ownTrust: null,
    factorSetup: null,
  };
  private connectionState: ConnectionState = { status: 'offline', generation: null };
  private core: CorePort | null = null;
  private readonly acceptedHumanInteractionSends = new Set<string>();
  private readonly autoModerationGrants = new Map<string, { token: string; groupId: Id; requestExpires: bigint; revision: string; document: Uint8Array }>();
  private readonly pendingHumanInteractionAcks = new Map<string, bigint[]>();
  private readonly pendingBotInteractionAcks = new Map<string, bigint[]>();
  private interactionAckLeaseGeneration = 0;
  /** The store this worker paused when it released the lead: resumed, not opened again, when it leads again (C2). */
  private pausedCore: CorePort | null = null;
  private me: { userId: Id; deviceId: Id } | null = null;
  private session: Session | null = null;
  private signup: Signup | null = null;
  private enrol: Enrol | null = null;
  private fetched: EnrolFetched | null = null;
  private signInRunning = false;
  /** When the held sign-in assertion was minted, on the monotonic clock (elapsed()), to tell its expiry from a refused
   *  backup code; a wall-clock step must not age it (C1 fix m3). */
  private assertionAt: number | null = null;
  /** web3.6: the community a signup that stopped at factor-setup was to join (null: none), joined by factorSetupDone. */
  private pendingJoin: { communityId: Id | null; invite: string } | null = null;
  /** A signup this worker completed has sent no KeyPackages yet (a V route, refused while owed): enterReady sends them
   *  once past the factor gate, before any join. */
  private keyPackagesOwed = false;
  /** KeyPackage publication: what Diagnostics shows, the one attempt allowed at a time, and the backoff between failures. */
  private keyPackages: DiagnosticsState['keyPackages'] = { count: null, lastAttempt: null, lastError: null };
  private keyPackagesBusy: CorePort | null = null;
  private keyPackagesDelayMs = 0;
  private keyPackagesRetry: number | null = null;
  /** Set when the gateway came back from a loss; the next ready checks the server's count instead of trusting its own. */
  private keyPackagesRecheck = false;
  /** factorSetupDone in flight: a second one is refused E_NOT_READY (as step() does for sign-in), so a double submit never
   *  runs two enterReady at once. */
  private factorSetupRunning = false;
  private storeCleared = false;
  private wiping = false;
  private gateway: Gateway | null = null;
  private sync: SyncEngine | null = null;
  private wasDown = false;
  private lastPresenceActivityAt = -Infinity;
  private selected: string | null = null;
  /** The pending debounced list read per community (op 54 and the unexpected-Welcome belt), by community hex. */
  private readonly communityReloads = new Map<string, number>();
  private readonly channels = new Map<string, KnownChannel>();
  /** CB2: the `until` last published on each channel's `slow:` slice (ms since the epoch, 0 = cleared), by channel hex. */
  private readonly slowUntil = new Map<string, number>();
  private readonly dms = new Map<string, KnownDm>();
  private dmsLoading: Promise<void> | null = null;
  private dmsAgain = false;
  private lastActivity = new Map<string, ActivityRow>();
  private noticesState: NoticesState = EMPTY_NOTICES;
  /** Pre-flight ruling (a): rows the server received before this second (the session's first ready, then every
   *  gateway ready) were caught up, not delivered live; they badge but raise no notice. null before ready. */
  private noticeFloor: bigint | null = null;
  /** CI1: live rows the core held for their sender's trust, oldest first, at most HELD_LIVE_MAX (noticeReleased). */
  private heldLive: HeldLive[] = [];
  /** R8 fix I2: a markRead a young held row stopped short, by group (hex): the target the page asked for, re-applied on
   *  a release and by the group's one timer at the held row's age-out. */
  private readonly heldReads = new Map<string, { groupId: Id; target: bigint; timer: number }>();
  /** The instance clock when the last gateway ticket was minted (the connection that reports the next ready). */
  private ticketTime: bigint | null = null;
  /** This browser's clock (ms) when that ticket's answer arrived: the instance clock advances from ticketTime by what
   *  has elapsed here since. */
  private ticketMintedMs = 0;
  private readonly refusedChannels = new Set<string>();
  private readonly notMember = new Set<string>();
  private readonly resyncing = new Set<string>();
  private readonly lookedUp = new Set<string>();
  /** The member-list read running per community (lookUpUsers), and whether another is owed after it. */
  private readonly memberReads = new Map<string, { again: boolean }>();
  /** When the last voice-driven member-list read of a community ended (this worker's clock, ms): f10app m3. */
  private readonly memberReadEnded = new Map<string, number>();
  /** How many users voice_state frames have put in `lookedUp`, per community (bounded by VOICE_LOOKUPS_MAX). */
  private readonly voiceLookups = new Map<string, number>();
  private readonly open = new Map<string, OpenChannel>();
  private readonly opening = new Map<string, { run: Promise<null>; entry: OpenChannel | null }>();
  /** One attachment tray per channel hex (L-TS-35); its descriptors live in worker memory only. */
  private readonly trays = new Map<string, Tray>();
  private readonly pendingRolePanelActionJobs = new Set<string>();
  private readonly pendingRolePanelActionRetries = new Map<string, number>();
  /** Channels whose pins view is open: touchGroup keeps their pins slice fresh until closePins or closeChannel. */
  private readonly pinsOpen = new Set<string>();
  /** web3.7: channel hex → the root msg id hex of its one open thread; refreshTimeline reads it again. */
  private readonly threadOpen = new Map<string, string>();
  /** web3.7: channels whose threads list was loaded; refreshTimeline keeps their threads slice fresh until closeChannel. */
  private readonly threadsOpen = new Set<string>();
  /** web3.7: this worker's own pending thread replies (msg id hex → the root and the group they went to). The outbox row
   *  does not name its thread, so this keeps the reply out of the main timeline and in the open thread until it is stored. */
  private readonly threadSends = new Map<string, { root: string; group: string }>();
  /** web3.7: set only around the synchronous sendRequest of a thread reply. sendRequest tells the page the outbox changed
   *  before it returns the msg id, so the new row is attributed to its thread from here until threadSends holds it. */
  private threadIntent: { root: string; group: string; known: ReadonlySet<string> } | null = null;
  /** community hex → the own role hexes last applied to the core, used only to skip a redundant ownRolesSet. */
  private readonly roles = new Map<string, string[]>();
  /** web3.5 C4: group hex → the role view last handed to the core (authoritySet) in this attach, and its server. An
   *  unchanged view is not sent again; a re-attach, a dropped server and a group back to active clear entries. */
  private readonly authority = new Map<string, { communityId: string; key: string }>();
  /** group hex → the channel group state last seen for it (pushAuthority, onGroupChanged, onMembership). */
  private readonly authorityStates = new Map<string, ChannelGroupState>();
  /** Servers whose member list and whose access (roles and overwrites) were read in this attach. */
  private readonly membersRead = new Set<string>();
  private readonly accessRead = new Set<string>();
  /** community hex → the number of the newest member-list read begun (readMembers). */
  private readonly memberSeq = new Map<string, number>();
  /** community hex → how many channels this device created there: a list read overtaken by a create is read again. */
  private readonly structureRev = new Map<string, number>();
  private purging: Promise<void> | null = null;
  private purgeAgain = false;
  private trustRefresh: Promise<null> | null = null;
  private trustRefreshAgain = false;
  private seenTrustGeneration: bigint | null = null;
  // Ruling (c) of 2026-10-13, narrowed by fix wave C1 (F4): the verified/failed/changed state is the core's trust_state
  // `blocked`, read from the store. The worker keeps only what the core stores no answer for: a user whose leaf ingest the
  // core refused with a leaf verdict (that refusal writes nothing), until a clean ingest of that user.
  private readonly unlistedUsers = new Set<string>();
  // Own-user leaves (`group:leafIndex`) whose list ingest the core refused with E_UMK_CHANGED (an own-user history under
  // another key): an own-trust signal, held while the core still reports the leaf, so the banner never waits for a store read.
  private readonly ownRefusedLeaves = new Set<string>();
  // Flow 05 "history pending": users whose last signed-list read failed (kept until a read succeeds, since a failed leaf
  // is not read again before the next ready), and the published union of those with users whose history is still unknown.
  private readonly historyFailedUsers = new Set<string>();
  private pendingUsers = new Set<string>();
  // user hex -> groups (hex) where pendingLeaves last reported a 'changed' leaf of that user.
  private changedLeafGroups = new Map<string, Set<string>>();
  // user hex -> still active groups whose changed leaf has since left the tree (a validated Remove cleared their block).
  private readonly removedGroups = new Map<string, Set<string>>();
  private readonly fetchedLeaves = new Set<string>();
  private readonly barrier = new CommandBarrier();
  private readonly searchGate = new SearchGate();
  private searchCursor: { token: string; scope: Extract<Command, { m: 'search' }>; position: SearchCursor } | null = null;
  private leadership: Leadership | null = null;
  private readonly typingOn = new Set<string>();
  private readonly incomingTyping = new Map<string, Map<string, number>>();
  /** The one pending expiry sweep per channel, and the deadline (deps.now() ms) it is set for. */
  private readonly typingSweeps = new Map<string, { handle: number; at: number }>();
  private readonly diagnosticsErrors: { code: string; at: number }[] = [];
  /** Per kept kind-3 Welcome group (hex): its community (hex, from the proof; '' when unreadable) and the deps.now() ms
   *  it was last refused for clock skew (sync's onWelcomeClockSkew). A Bot request names the skew only for its own
   *  community. The last E_CLOCK_SKEW diagnostics entry: a kept Welcome is refused on every poll, so the entry repeats
   *  at most once a minute. */
  private readonly welcomeClockSkew = new Map<string, { communityHex: string; atMs: number }>();
  private welcomeClockSkewLoggedMs: number | null = null;
  private deviceCount = 0;
  /** web-3: voice presence, calls and rings (L-TS-56). */
  private readonly calls: CallController;
  /** web3.4: server admin and moderation (flow 08). */
  private readonly admin: AdminController;
  /** Wave 2a: roles, overwrites and private channels (flow 09). */
  private readonly rolesCtl: RolesController;
  /** web3.6: second-factor management (flow 10). */
  private readonly factorsCtl: FactorsController;
  /** The last voice_self the page sent, and the calls this device's own voice_state shows it in (the re-send rule). */
  private lastVoiceSelf: { callId: string; flags: 0 | 1 | 2 | 3 } | null = null;
  private readonly ownInCall = new Set<string>();
  /** web3.8 VM: the re-sends of the own voice_self left in this join of the call (VOICE_SELF_RESENDS). */
  private selfResends: { callId: string; left: number } | null = null;
  /** web3.8 MUTE: a voice_self the page made while offline, still owed to the instance: the reconnect sends it. */
  private selfOffline: { callId: string; left: number } | null = null;

  constructor(private readonly deps: ControllerDeps) {
    this.parts = { ...REAL_PARTS, ...deps.parts };
    this.slices = new SliceStore((name, rev, value) => { deps.post({ t: 'slice', name, rev, value }); });
    const http = new HttpClient({
      baseUrl: deps.origin,
      fetch: deps.fetch,
      now: () => deps.now(),
      sleep: (ms, signal) => deps.sleep(ms, signal),
      random: () => deps.random(),
      token: () => this.session?.token() ?? null,
      reauthenticate: () => this.reauthenticate(),
      onGeneration: (generation) => { this.setConnection({ generation: generation.toString() }); },
      // web3.6: an instance that answers E_FACTOR_REQUIRED to a ready account (an operator reset) owns the next step.
      // Once per transition: onFactorRequired leaves 'ready' synchronously, so the refusals that follow (and any while
      // factor-setup, signup or sign-in runs) do not re-run it or overwrite its origin. This filter is the C2 brief's
      // latch; a separate flag could never be observed (C2 review finding 7).
      onRefusal: (code) => { if (code === 'E_FACTOR_REQUIRED' && this.phase === 'ready') this.onFactorRequired(); },
    });
    this.routes = this.parts.routes(http);
    this.calls = new CallController({
      core: () => this.core,
      routes: this.routes,
      sendStats: (callId, report) => this.gateway?.sendCallStats(callId, report) ?? false,
      post: (message, transfer) => { deps.post(message, transfer); },
      publish: (name, value) => {
        if (name === 'voice') this.slices.set('voice', this.visibleVoice(value as VoiceState));
        else this.slices.set(name, value);
      },
      errorOf: (e) => errorOf(e),
      now: () => deps.now(),
      monotonic: () => (deps.monotonic ?? deps.now)(),
      setTimeout: (fn, ms) => deps.setTimeout(fn, ms),
      clearTimeout: (id) => { deps.clearTimeout(id); },
      randomId: () => crypto.getRandomValues(new Uint8Array(16)),
      me: () => this.me,
      instance: () => this.instance,
      settings: () => pageSettings(this.core?.settings() ?? {}),
      nameOf: (user) => this.nameOf(user),
      refreshLeaves: (g) => this.refreshCallLeaves(g),
      serial: (g, job) => (this.sync === null ? job() : this.sync.runOnGroup(g, job)),
      callEnded: (call) => {
        if (this.lastVoiceSelf?.callId === call) this.lastVoiceSelf = null;
        if (this.selfResends?.callId === call) this.selfResends = null;
        if (this.selfOffline?.callId === call) this.selfOffline = null;
        this.ownInCall.delete(call);
      },
      onCommitted: (g, result) => { this.onGroupChanged(g, result); },
      afterJoin: (g) => {
        // After the join's answer has been posted: the engine takes up frames it held while the group was staged.
        const sync = this.sync;
        deps.setTimeout(() => { if (this.sync === sync && sync !== null) void this.barrier.track(sync.activate(g)).catch(() => undefined); }, 0);
      },
      track: (job) => { void this.barrier.track(job).catch(() => undefined); },
      participant: (user, device, flags, channel) => {
        const own = this.me !== null && toHex(this.me.deviceId) === device;
        // Web-3 task 10, concern 4: a participant no loaded member list has is looked up in the channel's community.
        const community = this.channels.get(channel)?.communityId ?? null;
        if (community !== null) this.lookUpUsers(community, [user], 'voice');
        return { userId: user, deviceId: device, name: this.nameOf(user), flags, own, web: own, bot: this.isBot(user) };
      },
      voiceChannels: (community) => [...this.channels].filter(([, k]) => k.communityId === community && k.row.kind === 1).map(([id]) => id),
      known: (channel) => this.channels.has(channel) || this.dms.has(channel),
      callTarget: (channel) => {
        const known = this.channels.get(channel);
        if (known !== undefined) return known.row.kind === 1 ? { communityId: known.communityId, members: [] } : undefined;
        const dm = this.dms.get(channel);
        return dm === undefined ? undefined : { communityId: null, members: dm.members.map(toHex) };
      },
      communityListed: (community) => this.listed(community),
    });
    this.admin = new AdminController({
      routes: this.routes,
      now: () => deps.now(),
      me: () => this.me,
      get: (name) => this.slices.get(name),
      publish: (name, value) => { this.slices.set(name, value); },
      listed: (communityId) => this.listed(communityId),
      channelsOf: (communityId) => [...this.channels].filter(([, k]) => k.communityId === communityId).map(([id, k]) => ({ id, row: k.row })),
      communityOf: (channelId) => this.channels.get(channelId)?.communityId ?? null,
      setChannel: (communityId, row) => { this.channels.set(toHex(row.id), { communityId, row }); },
      removeChannel: (channelId) => { this.channels.delete(channelId); this.dropOpen(channelId); },
      publishChannels: (communityId) => { this.publishChannels(communityId); this.updateExpected(); this.publishBadges(); },
      structureChanged: (communityId) => { this.structureRev.set(communityId, (this.structureRev.get(communityId) ?? 0) + 1); },
      reloadCommunity: (communityId) => this.loadCommunity(communityId),
      reloadMembers: (communityId) => this.reloadMembers(communityId),
      refreshCommunities: () => this.refreshCommunities(),
      listWritten: () => { this.listWritten(); },
      // C4 fix round 2: the local drop outranks a list read already in flight, which still lists the server.
      dropCommunity: (communityId) => { this.listWritten(); this.dropCommunity(communityId); },
      track: (job) => { void this.barrier.track(job).catch(() => undefined); },
      accessRead: (communityId, roles, bits, overwrites) => {
        this.rolesCtl.accessRead(communityId, roles, bits, overwrites);
        // The announcement flag follows the overwrites; the list is published again only when a flag changed.
        const shown = this.slices.get(`channels:${communityId}`) ?? [];
        if (shown.some((c) => c.announcement !== this.isAnnouncement(communityId, c.id))) this.publishChannels(communityId);
        this.accessRead.add(communityId);
        this.pushAuthority(communityId);
      },
      accessForgotten: (communityId) => { this.rolesCtl.forget(communityId); this.accessRead.delete(communityId); },
      refusal: (code, detail) => new Refusal(code, detail),
    });
    this.factorsCtl = new FactorsController({ routes: this.routes, holdsFactors: () => this.phase === 'ready' || this.phase === 'factor-setup', publish: (name, value) => { this.slices.set(name, value); } });
    this.rolesCtl = new RolesController({
      routes: this.routes,
      me: () => this.me,
      get: (name) => this.slices.get(name),
      publish: (name, value) => { this.slices.set(name, value); },
      listed: (communityId) => this.listed(communityId),
      communityOf: (channelId) => this.channels.get(channelId)?.communityId ?? null,
      reloadMembers: (communityId) => this.reloadMembers(communityId),
      loadAccess: (communityId) => this.admin.loadAccess(communityId),
      refusal: (code, detail, reason) => new Refusal(code, detail, reason),
    });
  }

  /** L-TS-56: the list refresh of a call group's pending leaves is the worker's trust refresh (no second one), bounded by
   *  WAIT_LIST_MS for the whole batch; it never throws. */
  private async refreshCallLeaves(groupId: Id): Promise<void> {
    let timer: number | null = null;
    const bound = new Promise<void>((resolve) => { timer = this.deps.setTimeout(resolve, WAIT_LIST_MS); });
    try { await Promise.race([this.refreshGroupTrust(groupId), bound]); }
    finally { if (timer !== null) this.deps.clearTimeout(timer); }
  }

  /** A bot account (member kind 1) in any loaded member list: the bot mark of a voice row. */
  private isBot(userHex: string): boolean {
    for (const community of this.slices.get('communities') ?? []) {
      if (this.slices.get(`members:${community.id}`)?.some((m) => m.userId === userHex && m.kind === 1)) return true;
    }
    return false;
  }

  /** Requirement 12: a pending scope in phase 2 is a revocation; a wipe in progress mints nothing (head ruling 27).
   *  While signInKey runs (phase `enrolling`) a refused session is the sign-in's own error: the core may already be in
   *  phase 2 there, and a row replaced before its list PUT is E_SIGNIN_EVICTED, not the revoked splash
   *  (REGISTRATION-DEVICES-02). */
  private async reauthenticate(): Promise<boolean> {
    if (this.wiping || this.storeCleared) return false;
    const session = this.session;
    if (session === null) return false;
    const enrolling = this.phase === 'enrolling';
    let ok: boolean;
    try {
      ok = await session.establish();
    } catch (e) {
      if (isCode(e, 'E_SESSION_SCOPE') && this.core?.identity().phase === 2 && !enrolling) { this.enterRevoked(); return false; }
      throw e;
    }
    // In phase 3 a refused session is the sign-in step's own error, not a revocation.
    if (!ok && this.core?.identity().phase !== 3 && !enrolling) this.enterRevoked();
    if (ok && this.core?.identity().phase === 2 && !enrolling) await this.checkInstanceSignerPin();
    return ok;
  }

  /** Authenticated discovery must agree with the pin established at enrollment before normal operation resumes. */
  private async checkInstanceSignerPin(): Promise<void> {
    const core = this.requireCore();
    const identity = core.identity();
    let read: Instance;
    try { read = await this.routes.getInstance(); }
    catch (e) {
      if (this.isInvalidInstancePin(e)) this.failInstanceSignerPin(e);
      throw e;
    }
    if (identity.instanceId === null || toHex(read.instanceId) !== toHex(identity.instanceId)) {
      const error = new CoreError('E_CORE_STATE', 'authenticated instance discovery does not match identity');
      this.failInstanceSignerPin(error);
      throw error;
    }
    try {
      // The core writes only a missing pin (browsers enrolled before this check existed) and refuses a changed one.
      // This runs after an enrolled session was authenticated, including on every gateway reconnection.
      core.instanceSignerKeyPin(read.instanceId, read.externalSenderPub);
    } catch (e) {
      if (this.isInvalidInstancePin(e)) this.failInstanceSignerPin(e);
      throw e;
    }
  }

  private isInvalidInstancePin(error: unknown): boolean {
    return error instanceof CborError || error instanceof CoreError && (error.code === 'E_CORE_STATE' || error.code === 'E_CORE_INPUT');
  }

  private failInstanceSignerPin(cause: unknown): void {
    const error = cause instanceof CoreError && cause.code === 'E_CORE_STATE'
      ? cause : new CoreError('E_CORE_STATE', 'authenticated instance discovery does not match the pinned identity');
    this.gateway?.stop();
    if (!this.revoked() && !this.wiping && !this.storeCleared) {
      this.setAccount({ phase: 'error', error: errorOf(error) });
    }
  }

  private scheduleSocketPinRetry(gateway: Gateway | null): void {
    this.clearReauthTimer();
    this.reauthTimer = this.deps.setTimeout(() => {
      this.reauthTimer = null;
      void this.restartAfterPinCheck(gateway);
    }, GATEWAY.reconnectCapMs);
  }

  private async restartAfterPinCheck(gateway: Gateway | null): Promise<void> {
    if (this.revoked() || this.wiping || this.leadership === null || this.phase !== 'ready' || this.gateway !== gateway) return;
    try {
      if (this.core?.identity().phase === 2) await this.checkInstanceSignerPin();
    } catch (e) {
      if (this.isInvalidInstancePin(e)) this.failInstanceSignerPin(e);
      else if (!this.revoked() && !this.wiping && this.phase === 'ready' && this.gateway === gateway) {
        this.scheduleSocketPinRetry(gateway);
      }
      return;
    }
    if (!this.revoked() && !this.wiping && this.leadership !== null && this.phase === 'ready' && this.gateway === gateway) gateway?.start();
  }

  handle(data: unknown): void {
    if (isTestHook(data)) {
      if (!this.deps.testHooks || this.gateway === null) return;
      if (data.op === 'gateway-stop') this.gateway.stop();
      else this.gateway.start();
      return;
    }
    if (!isToWorker(data)) return;
    const { id } = data;
    const problem = invalid(data.command);
    if (problem !== null) { this.refuse(id, 'E_BAD_INPUT', problem); return; }
    const command = data.command;
    if (this.barrier.isClosed()) { this.refuse(id, 'E_NOT_READY', ''); return; }
    if (command.m !== 'start' && command.m !== 'runtimeReady' && !ACCEPTS[this.phase].includes(command.m)) {
      this.refuse(id, 'E_NOT_READY', `the phase is ${this.phase}`);
      return;
    }
    const searchId = command.m === 'search' ? this.searchGate.next() : null;
    if (searchId !== null) this.slices.set('search', { requestId: searchId, hits: [], cursor: null });
    if (command.m === 'resign') {
      // Close synchronously, outside the accepted set, so the drain cannot wait for itself.
      this.answer(id, this.resign(command.nonce));
    } else this.answer(id, this.barrier.admit(() => this.run(command, searchId)));
  }

  // ---- answers ----

  private refuse(id: number, code: string, detail: string): void {
    const error = sanitizeWorkerError(new Refusal(code, detail));
    this.recordError(error.code);
    this.deps.post({ t: 'ret', id, ok: false, error });
  }

  private answer(id: number, work: Promise<unknown>): void {
    void work.then(
      (value) => { this.deps.post({ t: 'ret', id, ok: true, value }); },
      (e: unknown) => { const error = errorOf(e); this.recordError(error.code); this.deps.post({ t: 'ret', id, ok: false, error }); },
    );
  }

  /** Every command answers through its promise, also when it throws before its first await. */
  private run(command: Command, searchId: number | null = null): Promise<unknown> {
    try { return this.dispatch(command, searchId); }
    catch (e) { return Promise.reject(e instanceof Error ? e : new Error(String(e))); }
  }

  private dispatch(command: Command, searchId: number | null): Promise<unknown> {
    switch (command.m) {
      case 'start': return this.start();
      case 'runtimeReady': return this.deps.runtimeReady ? this.deps.runtimeReady() : Promise.reject(new Refusal('E_NOT_READY', ''));
      case 'signupBegin': return this.signupBegin();
      case 'signupSubmit': return this.signupSubmit(command);
      case 'signupReset': return this.signupReset();
      case 'resetDevice': return this.resetDevice();
      case 'joinCommunity': return this.joinCommunity(command.invite);
      case 'selectCommunity': return this.selectCommunity(command.communityId);
      case 'createCommunity': return this.createCommunity(command.name);
      case 'createChannel': return this.createChannel(command.communityId, command.kind, command.name, command.private, command.announce, command.parentId);
      case 'createInvite': return this.createInvite(command.communityId, command.maxUses, command.ttlSeconds);
      case 'refreshCommunities': return this.refreshCommunities().then(() => null);
      case 'getBotInstallation': return this.getBotInstallation(command.communityId);
      case 'getBotCommandStatus': return this.getBotCommandStatus(command.communityId);
      case 'patchBotProfile': return this.patchBotProfile(command.communityId, command.display);
      case 'listModerationCases': return this.listModerationCases(command);
      case 'listManualModerationEvidence': return this.listManualModerationEvidence(command);
      case 'humanInteractionRequest': return this.humanInteractionRequest(command);
      case 'readBotInteractionResponses': return this.readBotInteractionResponses(command.communityId, command.operations, command.requestIds);
      case 'ackBotInteractionResponses': return Promise.resolve(this.ackBotInteractionResponses(command.communityId, command.through));
      case 'readHumanInteractionResponses': return this.readHumanInteractionResponses(command.communityId, command.requestIds);
      case 'ackHumanInteractionResponses': return this.ackHumanInteractionResponses(command.communityId, command.through);
      case 'getBotJoinRole': return this.getBotJoinRole(command.communityId);
      case 'configureBotJoinRole': return this.configureBotJoinRole(command);
      case 'restartBot': return this.restartBot(command);
      case 'setBotMusicSettings': return this.setBotMusicSettings(command);
      case 'listSelfRoles': return this.listSelfRoles(command.communityId);
      case 'configureSelfRoles': return this.configureSelfRoles(command);
      case 'selectSelfRole': return this.selectSelfRole(command);
      case 'registerRolePanel': return this.registerRolePanel(command);
      case 'configureBotInstallation': return this.configureBotInstallation(command);
      case 'disableBotInstallation': return this.disableBotInstallation(command);
      case 'openChannel': return this.openChannel(command.channelId);
      case 'closeChannel': return this.closeChannel(command.channelId);
      case 'loadEarlier': return this.loadEarlier(command.channelId);
      case 'send': return this.send(command);
      case 'retrySend': return this.retrySend(command.msgId);
      case 'discardSend': return this.discardSend(command.msgId);
      case 'signInBegin': return this.signInBegin();
      case 'signInOIDC': return this.signInOIDC(command.assertion, command.needs);
      case 'signInLogin': return this.step(() => this.signInLogin(command.username, command.password));
      case 'signInTotp': return this.step(() => this.signInTotp(command.code));
      case 'signInKey': return this.step(() => this.signInKey(command.recoveryKey));
      case 'signInCancel': return this.signInCancel();
      case 'signInBack': return this.signInBack();
      case 'signInPasskeyBegin': return this.signInPasskeyBegin();
      case 'signInPasskey': return this.step(() => this.signInPasskey(command.ceremonyId, command.response));
      case 'signInBackupCode': return this.step(() => this.signInBackupCode(command.code, command.recoveryKey));
      case 'factorSetupDone': {
        if (this.factorSetupRunning) return Promise.reject(new Refusal('E_NOT_READY', 'factorSetupDone is running'));
        this.factorSetupRunning = true;
        return this.factorSetupDone(command.invite).finally(() => { this.factorSetupRunning = false; });
      }
      case 'refreshFactors': return this.factorsCtl.refresh();
      case 'factorTotpBegin': return this.factorsCtl.totpBegin(command.stepup);
      case 'factorTotpConfirm': return this.factorsCtl.totpConfirm(command.code);
      case 'factorPasskeyBegin': return this.factorsCtl.passkeyBegin(command.stepup);
      case 'factorPasskeyFinish': return this.factorsCtl.passkeyFinish(command.ceremonyId, command.response, command.name);
      case 'stepUp': return this.factorsCtl.stepUp(command.method, command.secret);
      case 'stepUpPasskeyBegin': return this.factorsCtl.stepUpPasskeyBegin();
      case 'stepUpPasskeyFinish': return this.factorsCtl.stepUpPasskeyFinish(command.ceremonyId, command.response);
      case 'freshSession': return this.freshSession();
      case 'changePassword': return this.factorsCtl.changePassword(command.old, command.next, command.stepup);
      case 'removeFactor': return this.factorsCtl.removeFactor(command.stepup, command.kind, command.credId);
      case 'regenerateBackupCodes': return this.factorsCtl.regenerateBackupCodes(command.stepup);
      case 'refreshDevices': return this.refreshDevices();
      case 'revokeDevice': return this.revokeDevice(command.deviceId, command.recoveryKey);
      case 'renameDevice': return this.renameDevice(command.deviceId, command.label);
      case 'signOutRevoke': return this.signOutRevoke(command.recoveryKey);
      case 'forgetBrowser': return this.forgetBrowser();
      case 'markRead': return this.markRead(command.channelId);
      case 'setSetting': return this.setSetting(command.key, command.value);
      case 'setPresenceChoice': return this.setPresenceChoice(command.choice);
      case 'updateProfile': return this.updateProfile(command);
      case 'presenceActivity':
        if (this.deps.now() - this.lastPresenceActivityAt < 5000) return Promise.resolve(null);
        this.lastPresenceActivityAt = this.deps.now();
        this.gateway?.sendPresenceActivity();
        return Promise.resolve(null);
      case 'getAFKSettings': return this.routes.getAFKSettings(fromHex(command.communityId))
        .then(settings => ({ channelId: settings.channelId === null ? null : toHex(settings.channelId),
          timeoutSeconds: settings.timeoutSeconds }));
      case 'setAFK': return this.setAFK(command.communityId, command.timeoutSeconds);
      case 'afkActivity': return this.routes.sendAFKActivity(fromHex(command.communityId), fromHex(command.channelId), command.exempt).then(() => null);
      // AW4 (R-AW3): choosing AFK. The page leaves its call first; the roster changes by op 55, not here.
      case 'afkAway': return this.afkAway(command.communityId);
      case 'afkRead': return this.afkRead(command.communityId, command.timeoutMs);
      case 'openDm': return this.openDm(command.userId);
      case 'editMessage': return this.editMessage(command.channelId, command.msgId, command.text);
      case 'deleteMessage': return this.deleteMessage(command.channelId, command.msgId, command.seq);
      case 'moderateRemoveMessage': return this.moderateRemoveMessage(command.channelId, command.msgId, command.seq, command.reason, command.excerpt);
      case 'resolveManualRemoval': return this.resolveManualRemoval(command.proofRequestId);
      case 'react': return this.react(command.channelId, command.msgId, command.emoji, command.on);
      case 'pin': return this.pin(command.channelId, command.msgId, command.on);
      case 'openThread': return this.openThread(command.channelId, command.rootMsgId);
      case 'closeThread': return this.closeThread(command.channelId);
      case 'loadThreads': return this.loadThreads(command.channelId);
      case 'loadPins': return this.loadPins(command.channelId);
      case 'closePins': return this.closePins(command.channelId);
      case 'attachFiles': return this.attachFiles(command.channelId, command.files);
      case 'checkpointFiles': return Promise.resolve(this.trays.get(command.channelId)?.files() ?? []);
      case 'discardAttachment': return this.discardAttachment(command.channelId, command.trayId);
      case 'openAttachment': return this.openAttachment(command.channelId, command.seq, command.index, command.thumb);
      case 'gifStatus': return this.routes.gifStatus(fromHex(command.communityId));
      case 'gifSetKey': return this.routes.gifSetKey(fromHex(command.communityId), command.key).then(() => null);
      case 'gifClearKey': return this.routes.gifClearKey(fromHex(command.communityId)).then(() => null);
      case 'gifSetRating': return this.routes.gifSetRating(fromHex(command.communityId), command.rating).then(() => null);
      case 'gifSearch': return this.routes.gifSearch(fromHex(command.communityId), command.q, command.offset);
      case 'gifFetch': return this.routes.gifMedia(fromHex(command.communityId), command.gifId, command.variant)
        .then(bytes => ({ blob: new Blob([bytes as Uint8Array<ArrayBuffer>], { type: 'image/gif' }) }));
      case 'refreshTrust': return Promise.all((this.slices.get('communities') ?? []).map(community =>
        this.reloadMembers(community.id).catch(() => undefined))).then(() => this.refreshTrust());
      case 'verifyContact': return this.verifyContact(command.userId, command.umk);
      case 'safetyNumber': return Promise.resolve(this.safetyNumber(command.userId, command.umk));
      case 'search': return this.search(command, searchId ?? this.searchGate.next());
      case 'typing': return this.typing(command.channelId, command.on);
      case 'voiceSelf': return this.voiceSelf(command.callId, command.flags);
      case 'loadVoice': return this.calls.loadVoice(command.communityId, command.channelId);
      case 'callJoin': return this.calls.join(command.channelId, command.vdec);
      case 'callLeave': return this.calls.leave(false, command.reason === 'lost');
      case 'callShare': return this.calls.share(command.on);
      case 'callRtt': return this.calls.rtt(command.ms);
      case 'callRefresh': return this.calls.refresh();
      case 'callStats': return this.calls.stats(command.report);
      case 'callNote': this.recordError(command.code); return Promise.resolve(null);
      case 'callDecline': return this.calls.decline(command.callId, command.ringTs);
      case 'renameCommunity': return this.admin.renameCommunity(command.communityId, command.name);
      case 'deleteCommunity': return this.admin.deleteCommunity(command.communityId);
      case 'leaveCommunity': return this.admin.leaveCommunity(command.communityId);
      case 'transferOwnership': return this.admin.transferOwnership(command.communityId, command.userId);
      case 'createCategory': return this.admin.createCategory(command.communityId, command.name);
      case 'updateChannel': return this.admin.updateChannel(command.channelId, command.name, command.topic, command.parent, command.slowmode);
      case 'moveChannel': return this.admin.moveChannel(command.channelId, command.direction);
      case 'deleteChannel': return this.admin.deleteChannel(command.channelId);
      case 'kickMember': return this.admin.kickMember(command.communityId, command.userId);
      case 'banMember': return this.admin.banMember(command.communityId, command.userId, command.reason, command.seconds);
      case 'unbanMember': return this.admin.unbanMember(command.communityId, command.userId);
      case 'getMemberTimeout': return this.getMemberTimeout(command.communityId, command.userId);
      case 'setMemberTimeout': return this.setMemberTimeout(command);
      case 'clearMemberTimeout': return this.clearMemberTimeout(command);
      case 'warnMember': return this.warnMember(command);
      case 'loadBans': return this.admin.loadBans(command.communityId);
      case 'loadInvites': return this.admin.loadInvites(command.communityId);
      case 'revokeInvite': return this.admin.revokeInvite(command.communityId, command.inviteId);
      case 'createRole': return this.rolesCtl.createRole(command.communityId, command.name, command.color, command.hoist, command.mentionable, command.allow);
      case 'updateRole': return this.rolesCtl.updateRole(command.communityId, command.roleId, command.name, command.color, command.hoist, command.mentionable, command.allow);
      case 'moveRole': return this.rolesCtl.moveRole(command.communityId, command.roleId, command.direction);
      case 'deleteRole': return this.rolesCtl.deleteRole(command.communityId, command.roleId);
      case 'grantRole': return this.rolesCtl.grantRole(command.communityId, command.userId, command.roleId);
      case 'revokeRole': return this.rolesCtl.revokeRole(command.communityId, command.userId, command.roleId);
      case 'setOverwrite': return this.rolesCtl.setOverwrite(command.channelId, command.kind, command.targetId, command.allow, command.deny);
      case 'setChannelPrivate': return this.rolesCtl.setChannelPrivate(command.channelId, command.private);
      case 'resign': return this.resign(command.nonce);
    }
  }

  private async getBotInstallation(communityId: string): Promise<ReturnType<typeof botInstallationView> | null> {
    if (!this.listed(communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const row = await this.routes.getBotInstallation(fromHex(communityId));
    return row === null ? null : botInstallationView(row);
  }

  private async getBotCommandStatus(communityId: string) {
    if (!this.listed(communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const row = await this.routes.getBotCommandStatus(fromHex(communityId));
    return row === null ? null : { installationId: toHex(row.installationId),
      botUserId: row.botUserId === null ? null : toHex(row.botUserId), state: row.state, revision: row.revision.toString() };
  }

  private async patchBotProfile(communityId: string, display: string): Promise<string> {
    if (!this.listed(communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const saved = await this.routes.patchBotProfile(fromHex(communityId), display);
    void this.reloadMembers(communityId).catch(() => undefined);
    return saved;
  }

  private async waitForActiveInteractionGroup(communityHex: string): Promise<ReturnType<CorePort['groups']>> {
    const core = this.requireCore();
    const sync = this.requireSync();
    const activeGroups = () => core.groups().filter((row) => row.kind === 3 && row.state === 2
      && row.communityId !== null && toHex(row.communityId) === communityHex);
    let rows = activeGroups();
    // Registration follows the daemon's 30-second reconciliation interval. Keep
    // the original request pending while its per-device Welcome is being issued.
    const welcomeAttempts = 90;
    const startedMs = this.deps.now();
    for (let attempt = 0; rows.length === 0 && attempt < welcomeAttempts; attempt++) {
      if (sync.stopped()) throw new SyncError('E_SYNC_STOPPED');
      await sync.pollWelcomes();
      rows = activeGroups();
      if (rows.length === 0 && attempt + 1 < welcomeAttempts) await this.deps.sleep(500);
    }
    // A Welcome refused for clock skew during this wait is refused again on every poll while the skew lasts, so the
    // request names that instead of the generic missing group.
    if (rows.length === 0 && [...this.welcomeClockSkew.values()]
      .some(seen => seen.communityHex === communityHex && seen.atMs >= startedMs)) {
      throw new Refusal('E_CORE_STATE', INTERACTION_CLOCK_SKEW);
    }
    if (rows.length === 0) throw new Refusal('E_CORE_STATE', NO_INTERACTION_GROUP);
    return rows;
  }

  /** Sends a revision-bound built-in Bot request. The result describes ciphertext upload only. */
  private async humanInteractionRequest(command: Extract<Command, { m: 'humanInteractionRequest' }>, reservedExpires?: bigint): Promise<{
    requestId: string; upload: 'accepted' | 'pending' | 'rejected' | 'unknown';
  }> {
    const { communityId: communityHex, channelId: channelHex, requestId: requestHex, operation } = command;
    if (!this.listed(communityHex)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const channelId = channelHex === null ? null : fromHex(channelHex);
    if (channelHex !== null) {
      const channel = this.channels.get(channelHex);
      if (channel === undefined || channel.communityId !== communityHex || channel.row.kind !== 1) {
        throw new Refusal('E_BAD_INPUT', 'music requests require a voice channel in this community');
      }
    }
    const acceptedKey = `interaction:${operation}:${communityHex}:${requestHex}`;
    if (this.acceptedHumanInteractionSends.has(acceptedKey)) return { requestId: requestHex, upload: 'accepted' };
    const communityId = fromHex(communityHex);
    const requestId = fromHex(requestHex);
    const groupRows = await this.waitForActiveInteractionGroup(communityHex);
    if (groupRows.length !== 1) throw new Refusal('E_CORE_STATE', AMBIGUOUS_INTERACTION_GROUP);
    const groupId = groupRows[0].groupId;
    const mutation = operation === 2 || operation >= 12 && operation <= 23 || operation === 25 || operation === 31 || operation === 51 || operation === 52
      || operation === 61 || operation === 63 || operation === 70 || operation === 73 || operation === 74 || operation === 80
      || operation === 82 || operation === 83;
    const idArgument = (index: number): boolean =>
      ((operation === 21 || operation === 25) && index === 0 || operation === 22 && (index === 0 || index === 1) || operation === 31 && index === 0
        || operation === 51 && index === 0 || operation === 52 && index === 0 || operation === 61 && index === 0
        || operation === 71 && index === 0 || operation === 72 && (index === 0 || index === 1) || operation === 73 && index === 0
        || operation === 74 && index === 0 || operation === 75 && index === 0 || (operation === 82 || operation === 83) && index === 0);
    const args: CborInput[] = command.args.map((value, index) =>
      idArgument(index) && typeof value === 'string'
        ? fromHex(value) : value);
    const sync = this.requireSync();
    return sync.runOnGroup(groupId, async () => {
      if (sync.stopped()) throw new SyncError('E_SYNC_STOPPED');
      if (this.acceptedHumanInteractionSends.has(acceptedKey)) return { requestId: requestHex, upload: 'accepted' };
      const now = BigInt(Math.floor(this.deps.now() / 1000));
      let requestExpires = mutation ? (reservedExpires ?? now + 30n) : null;
      const request = { requestId, communityId, channelId, operation, revision: mutation ? BigInt(command.revision!) : null,
        expires: requestExpires, args };
      if (operation === 63) {
        if (command.stepUp === undefined || requestExpires === null) throw new Refusal('E_BAD_INPUT', 'automatic moderation step-up is required');
        const document = command.args[1];
        if (!(document instanceof Uint8Array)) throw new Refusal('E_BAD_INPUT', 'automatic moderation document is invalid');
        const prior = this.autoModerationGrants.get(acceptedKey);
        let token: string;
        if (prior !== undefined) {
          if (toHex(prior.groupId) !== toHex(groupId) || prior.revision !== command.revision || prior.document.length !== document.length
            || prior.document.some((byte, index) => byte !== document[index])) throw new Refusal('E_BAD_INPUT', 'request id is already bound to another automatic moderation document');
          if (prior.requestExpires <= now) throw new Refusal('E_CONFLICT', 'automatic moderation request expired; reload and start a new save');
          token = prior.token; requestExpires = prior.requestExpires; request.expires = requestExpires;
        } else {
          const digestInput = new Uint8Array(document.length); digestInput.set(document);
          const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', digestInput.buffer));
          const grant = await this.routes.armAutoModerationGrant(groupId, requestId, communityId, BigInt(command.revision!),
            requestExpires, digest, command.stepUp);
          token = grant.token;
          this.autoModerationGrants.set(acceptedKey, { token, groupId, requestExpires, revision: command.revision!, document: document.slice() });
          while (this.autoModerationGrants.size > 64) {
            const oldest = this.autoModerationGrants.keys().next().value; if (oldest === undefined) break;
            this.autoModerationGrants.delete(oldest);
          }
        }
        request.args[0] = token;
      }
      validateBotContext(request, { community: communityId, now });
      const requestBody = encodeBotRequestBytes(request);
      const prepared = this.requireCore().humanInteractionSendPrepare(requestId, communityId, groupId, requestBody, now);
      if (toHex(prepared) !== requestHex) throw new Refusal('E_CORE_STATE', 'interaction request id changed during preparation');
      const core = this.requireCore();
      const outbox = core.outbox(groupId).find((row) => toHex(row.msgId) === requestHex);
      if (outbox === undefined) return { requestId: requestHex, upload: 'unknown' };
      if (outbox.state === 1) return { requestId: requestHex, upload: 'pending' };
      if (outbox.state === 2) core.sendRetry(prepared);
      const ciphertext = core.sendEncrypt(prepared);
      if (toHex(ciphertext.groupId) !== toHex(groupId)) throw new Refusal('E_CORE_STATE', 'prepared interaction upload changed groups');
      let receipt: Awaited<ReturnType<Routes['postMessage']>>;
      try {
        receipt = await this.routes.postMessage(groupId, ciphertext.messageBody);
      } catch (error) {
        if (error instanceof DillaHttpError && error.status > 0 && error.status < 500) {
          core.sendFail(prepared, error.code);
          return { requestId: requestHex, upload: 'rejected' };
        }
        return { requestId: requestHex, upload: 'pending' };
      }
      try { core.sendConfirm(prepared, receipt.raw); }
      catch { return { requestId: requestHex, upload: 'pending' }; }
      this.acceptedHumanInteractionSends.add(acceptedKey);
      if (this.acceptedHumanInteractionSends.size > 128) {
        const oldest = this.acceptedHumanInteractionSends.values().next().value;
        if (oldest !== undefined) this.acceptedHumanInteractionSends.delete(oldest);
      }
      return { requestId: requestHex, upload: 'accepted' };
    });
  }

  private async readBotInteractionResponses(communityHex: string, operations: readonly number[], requestIds?: readonly string[]): Promise<{
    responses: { deliveryId: string; requestId: string; communityId: string; operation: number; channelId: string | null;
      status: number; revision: string | null; data: Uint8Array; sourceSeq: string }[];
    through: string | null; blocked: boolean;
  }> {
    if (!this.listed(communityHex)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const allowed = new Set(operations);
    const requested = requestIds === undefined ? null : new Set(requestIds);
    const rows = this.readAllHumanInteractionEvents();
    const pendingEvents = rows.filter(event => toHex(event.communityId) === communityHex && allowed.has(event.operation)
      && (requested === null || requested.has(toHex(event.requestId)))).slice(0, 100);
    let archivedEvents: HumanInteractionEvent[] = [];
    if (requestIds !== undefined && requestIds.length > 0) {
      const status = await this.routes.getBotCommandStatus(fromHex(communityHex));
      if (status === null || status.state !== 3 || status.botUserId === null) {
        throw new Refusal('E_FORBIDDEN', 'current active Bot installation and community membership are required');
      }
      archivedEvents = this.requireCore().humanInteractionReceiptsRead(
        fromHex(communityHex), status.installationId, status.botUserId, requestIds.map(fromHex))
        .filter(event => toHex(event.communityId) === communityHex && allowed.has(event.operation)
          && requested?.has(toHex(event.requestId)));
    }
    const byRequest = new Map<string, HumanInteractionEvent>();
    for (const event of [...pendingEvents, ...archivedEvents]) {
      const requestId = toHex(event.requestId);
      const prior = byRequest.get(requestId);
      if (prior !== undefined) {
        if (prior.deliveryId !== event.deliveryId || prior.body !== event.body || prior.operation !== event.operation
          || toHex(prior.groupId) !== toHex(event.groupId)) {
          throw new Refusal('E_CORE_STATE', 'contradictory duplicate Bot interaction receipts');
        }
        continue;
      }
      byRequest.set(requestId, event);
    }
    const events = [...byRequest.values()].slice(0, 100);
    const mapResponse = (event: HumanInteractionEvent) => {
      const response = decodeBotResponse(event.body);
      validateBotResponseForRequest(response, fromHex(communityHex), event.requestId);
      return { deliveryId: String(event.deliveryId), requestId: toHex(event.requestId), communityId: communityHex,
        operation: event.operation, channelId: event.channelId === null ? null : toHex(event.channelId),
        status: response.status, revision: response.revision?.toString() ?? null, data: response.data,
        sourceSeq: String(event.sourceSeq) };
      };
    const responses = events.map(mapResponse);
    const through = this.issueInteractionAckLease(communityHex, pendingEvents
      .filter(event => byRequest.get(toHex(event.requestId))?.deliveryId === event.deliveryId)
      .map(event => event.deliveryId), this.pendingBotInteractionAcks);
    return { responses, through, blocked: false };
  }

  private ackBotInteractionResponses(communityHex: string, through: string): null {
    const key = `${communityHex}:${through}`;
    const deliveryIds = this.pendingBotInteractionAcks.get(key);
    if (!this.listed(communityHex) || deliveryIds === undefined) {
      throw new Refusal('E_BAD_INPUT', 'interaction response acknowledgement was not authorized');
    }
    const core = this.requireCore();
    core.humanInteractionEventsAckSelected(deliveryIds);
    this.pendingBotInteractionAcks.delete(key);
    return null;
  }

  private readAllHumanInteractionEvents(): HumanInteractionEvent[] {
    const events: HumanInteractionEvent[] = [];
    let cursor = 0n;
    let targetHighWater: bigint | null = null;
    for (let pageNo = 0; pageNo < 41; pageNo++) {
      const page = this.requireCore().humanInteractionEventsRead(cursor, 100);
      targetHighWater ??= page.highWater;
      events.push(...page.events.filter(event => event.deliveryId <= targetHighWater!));
      if (page.next <= cursor || page.next >= targetHighWater) break;
      cursor = page.next;
    }
    return events;
  }

  private issueInteractionAckLease(communityHex: string, deliveryIds: bigint[], leases: Map<string, bigint[]>): string | null {
    if (deliveryIds.length === 0) return null;
    let token: string;
    do {
      token = Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, '0')).join('');
    } while (leases.has(`${communityHex}:${token}`));
    const key = `${communityHex}:${token}`;
    leases.set(key, deliveryIds);
    while (leases.size > 128) {
      const oldest = leases.keys().next().value;
      if (oldest === undefined) break;
      leases.delete(oldest);
    }
    return token;
  }

  /** The first shipping human-side Bot request is a read-only moderation page. Its plaintext and
   *  ciphertext stay inside the worker/core boundary; the page receives only the stable request id
   *  and whether the upload was accepted, remains uncertain, was refused, or cannot be resolved. */
  private listModerationCases(command: Extract<Command, { m: 'listModerationCases' }>) {
    return this.sendModerationPage(command.communityId, command.requestId, 60, [null]);
  }

  private listManualModerationEvidence(command: Extract<Command, { m: 'listManualModerationEvidence' }>) {
    const cursor = command.cursor === null ? null : fromHex(command.cursor);
    return this.sendModerationPage(command.communityId, command.requestId, 64, [cursor]);
  }

  private async sendModerationPage(communityHex: string, requestHex: string, operation: 60 | 64,
    args: (Uint8Array | null)[]): Promise<{ requestId: string; status: 'accepted' | 'pending' | 'rejected' | 'unknown' }> {
    if (!this.listed(communityHex)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const acceptedKey = `${communityHex}:${requestHex}`;
    if (this.acceptedHumanInteractionSends.has(acceptedKey)) return { requestId: requestHex, status: 'accepted' };
    const core = this.requireCore();
    const sync = this.requireSync();
    const communityId = fromHex(communityHex);
    const groupRows = await this.waitForActiveInteractionGroup(communityHex);
    if (groupRows.length !== 1) throw new Refusal('E_CORE_STATE', AMBIGUOUS_INTERACTION_GROUP);
    const groupId = groupRows[0].groupId;
    const requestId = fromHex(requestHex);
    return sync.runOnGroup(groupId, async () => {
      if (sync.stopped()) throw new SyncError('E_SYNC_STOPPED');
      if (this.acceptedHumanInteractionSends.has(acceptedKey)) return { requestId: requestHex, status: 'accepted' };
      const now = BigInt(Math.floor(this.deps.now() / 1000));
      const request = { requestId, communityId, channelId: null, operation, revision: null, expires: null, args };
      // Structural encoding enforces operation 60, its sole null cursor, and read-only null
      // revision/expiry. Context validation binds it to this community and the trusted clock.
      validateBotContext(request, { community: communityId, now });
      const requestBody = encodeBotRequestBytes(request);
      const prepared = core.humanInteractionSendPrepare(requestId, communityId, groupId, requestBody, now);
      if (toHex(prepared) !== requestHex) throw new Refusal('E_CORE_STATE', 'interaction request id changed during preparation');

      const outbox = core.outbox(groupId).find((row) => toHex(row.msgId) === requestHex);
      if (outbox === undefined) return { requestId: requestHex, status: 'unknown' };
      if (outbox.state === 1) return { requestId: requestHex, status: 'pending' };
      if (outbox.state === 2) core.sendRetry(prepared);

      const ciphertext = core.sendEncrypt(prepared);
      if (toHex(ciphertext.groupId) !== toHex(groupId)) throw new Refusal('E_CORE_STATE', 'prepared interaction upload changed groups');
      let receipt: Awaited<ReturnType<Routes['postMessage']>>;
      try {
        receipt = await this.routes.postMessage(groupId, ciphertext.messageBody);
      } catch (error) {
        if (error instanceof DillaHttpError && error.status > 0 && error.status < 500) {
          core.sendFail(prepared, error.code);
          return { requestId: requestHex, status: 'rejected' };
        }
        // A timeout or server failure does not establish whether the upload committed. The durable
        // in-flight outbox is retained and the sync lane can resolve it after this POST has settled.
        return { requestId: requestHex, status: 'pending' };
      }
      try {
        core.sendConfirm(prepared, receipt.raw);
      } catch {
        // The server accepted the ciphertext, but the local receipt commit did not complete.
        return { requestId: requestHex, status: 'pending' };
      }
      this.acceptedHumanInteractionSends.add(acceptedKey);
      if (this.acceptedHumanInteractionSends.size > 128) {
        const oldest = this.acceptedHumanInteractionSends.values().next().value;
        if (oldest !== undefined) this.acceptedHumanInteractionSends.delete(oldest);
      }
      return { requestId: requestHex, status: 'accepted' };
    });
  }
  private async readHumanInteractionResponses(communityHex: string, requestIds?: readonly string[]): Promise<{
    authorized: boolean; responses: { deliveryId: string; requestId: string; status: number;
      cases: { key: string; channelId: string; actorId: string; messageId: string; revision: string;
        reason: string; action: string; excerpt: string; receivedAt: string }[]; hasMore: boolean; nextCursor: string | null }[];
    through: string | null; blocked: boolean;
  }> {
    if (!this.listed(communityHex)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const communityId = fromHex(communityHex);
    // This is deliberately independent of the cached permissions slice. The response can contain
    // private excerpts, so the authority decision must come from the instance on every read.
    const permissions = await this.routes.getPermissions(communityId);
    if ((permissions.bits & BigInt(PERM.viewAuditLog)) === 0n) {
      return { authorized: false, responses: [], through: null, blocked: false };
    }
    const requested = requestIds === undefined ? null : new Set(requestIds);
    const rows = this.readAllHumanInteractionEvents();
    const isModerationRead = (operation: number): boolean => operation === 60 || operation === 64;
    const pendingEvents = rows.filter(event => toHex(event.communityId) === communityHex && isModerationRead(event.operation)
      && (requested === null || requested.has(toHex(event.requestId)))).slice(0, 100);
    let archivedEvents: HumanInteractionEvent[] = [];
    if (requestIds !== undefined) {
      const status = await this.routes.getBotCommandStatus(communityId);
      if (status === null || status.state !== 3 || status.botUserId === null) {
        return { authorized: false, responses: [], through: null, blocked: false };
      }
      archivedEvents = this.requireCore().humanInteractionReceiptsRead(
        communityId, status.installationId, status.botUserId, requestIds.map(fromHex))
        .filter(event => toHex(event.communityId) === communityHex && isModerationRead(event.operation)
          && requested?.has(toHex(event.requestId)));
    }
    const eventsByRequest = new Map<string, HumanInteractionEvent>();
    for (const event of [...pendingEvents, ...archivedEvents]) {
      const requestId = toHex(event.requestId);
      const prior = eventsByRequest.get(requestId);
      if (prior !== undefined) {
        if (prior.deliveryId !== event.deliveryId || prior.body !== event.body || prior.operation !== event.operation) {
          throw new Refusal('E_CORE_STATE', 'contradictory duplicate moderation receipts');
        }
        continue;
      }
      eventsByRequest.set(requestId, event);
    }
    const visibleEvents = [...eventsByRequest.values()].slice(0, 100);
    const responses = visibleEvents.map(event => {
      const response = decodeBotResponse(event.body);
      validateBotResponseForRequest(response, communityId, event.requestId);
      const cases: { key: string; channelId: string; actorId: string; messageId: string; revision: string;
        reason: string; action: string; excerpt: string; receivedAt: string }[] = [];
      let hasMore = false;
      let nextCursor: string | null = null;
      if (response.status === 0) {
        const data = arr(decodeCbor(response.data));
        if (data.length !== 3 || data[0] !== 1n || !Array.isArray(data[1])
          || (data[2] !== null && !(data[2] instanceof Uint8Array)) || data[1].length > 8) {
          throw new Refusal('E_CORE_STATE', 'moderation response shape is invalid');
        }
        hasMore = data[2] !== null;
        if (event.operation === 64 && data[2] instanceof Uint8Array) nextCursor = toHex(data[2]);
        for (const value of data[1]) {
          const row = arr(value);
          if (event.operation === 64) {
            if (row.length !== 5 || !(row[0] instanceof Uint8Array) || row[0].length !== 16 || typeof row[1] !== 'string'
              || !['remove', 'warn'].includes(row[1]) || typeof row[2] !== 'string' || row[2].length < 1 || row[2].length > 1024
              || typeof row[3] !== 'string' || row[3].length > 512 || typeof row[4] !== 'bigint') {
              throw new Refusal('E_CORE_STATE', 'manual evidence response shape is invalid');
            }
            const evidenceId = toHex(row[0]);
            cases.push({ key: `manual:${evidenceId}`, channelId: '0'.repeat(32), actorId: '0'.repeat(32), messageId: evidenceId,
              revision: '1', reason: String(row[2]), action: String(row[1]), excerpt: String(row[3]), receivedAt: String(row[4]) });
            continue;
          }
          if (row.length !== 10 || row.slice(0, 5).some(v => typeof v !== 'string' || !/^[0-9a-f]{32}$/u.test(v))
            || typeof row[5] !== 'bigint' || typeof row[6] !== 'string'
            || !['log', 'remove', 'warn', 'timeout'].includes(String(row[7])) || typeof row[8] !== 'string'
            || typeof row[9] !== 'bigint' || row[8].length > 700) {
            throw new Refusal('E_CORE_STATE', 'moderation case shape is invalid');
          }
          const rowCommunity = String(row[0]);
          const channelId = String(row[2]);
          const actorId = String(row[3]);
          const messageId = String(row[4]);
          if (rowCommunity !== communityHex) throw new Refusal('E_CORE_STATE', 'moderation case community changed');
          cases.push({ key: `${messageId}:${String(row[5])}`, channelId, actorId, messageId,
            revision: String(row[5]), reason: String(row[6]), action: String(row[7]),
            excerpt: String(row[8]), receivedAt: String(row[9]) });
        }
      }
      return { deliveryId: String(event.deliveryId), requestId: toHex(event.requestId), status: response.status, cases, hasMore, nextCursor };
    });
    const ackable = pendingEvents.filter(event => eventsByRequest.get(toHex(event.requestId))?.deliveryId === event.deliveryId);
    const through = this.issueInteractionAckLease(communityHex, ackable.map(event => event.deliveryId), this.pendingHumanInteractionAcks);
    return { authorized: true, responses, through, blocked: false };
  }

  private async ackHumanInteractionResponses(communityHex: string, through: string): Promise<null> {
    const key = `${communityHex}:${through}`;
    const deliveryIds = this.pendingHumanInteractionAcks.get(key);
    if (!this.listed(communityHex) || deliveryIds === undefined) {
      throw new Refusal('E_BAD_INPUT', 'interaction response acknowledgement was not authorized');
    }
    const core = this.requireCore();
    const generation = this.interactionAckLeaseGeneration;
    const permissions = await this.routes.getPermissions(fromHex(communityHex));
    if (generation !== this.interactionAckLeaseGeneration || core !== this.core || this.pendingHumanInteractionAcks.get(key) !== deliveryIds) {
      throw new Refusal('E_BAD_INPUT', 'interaction response acknowledgement is stale');
    }
    if ((permissions.bits & BigInt(PERM.viewAuditLog)) === 0n) {
      throw new Refusal('E_FORBIDDEN', 'current view-audit-log permission is required');
    }
    core.humanInteractionEventsAckSelected(deliveryIds);
    this.pendingHumanInteractionAcks.delete(key);
    return null;
  }

  private async getBotJoinRole(communityId: string): Promise<{ roleId: string | null; installationId: string } | null> {
    if (!this.listed(communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const row = await this.routes.getBotJoinRole(fromHex(communityId));
    return row === null ? null : { roleId: row.roleId === null ? null : toHex(row.roleId), installationId: toHex(row.installationId) };
  }

  private async configureBotJoinRole(command: Extract<Command, { m: 'configureBotJoinRole' }>): Promise<{ installationId: string }> {
    if (!this.listed(command.communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const installationId = await this.routes.configureBotJoinRole(fromHex(command.communityId), fromHex(command.requestId), fromHex(command.roleId), fromHex(command.installationId));
    return { installationId: toHex(installationId) };
  }

  /** Task 10c: Glenn's Restart. Answers the restart counters as decimal strings. */
  private async restartBot(command: Extract<Command, { m: 'restartBot' }>): Promise<{ requested: string; handled: string }> {
    if (!this.listed(command.communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const row = await this.routes.restartBot(fromHex(command.communityId), fromHex(command.installationId));
    return { requested: row.requested.toString(), handled: row.handled.toString() };
  }

  /** Task 10c: the Music page's default volume and idle-leave minutes. */
  private async setBotMusicSettings(command: Extract<Command, { m: 'setBotMusicSettings' }>): Promise<ReturnType<typeof botInstallationView>> {
    if (!this.listed(command.communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    return botInstallationView(await this.routes.setBotMusicSettings(fromHex(command.communityId), fromHex(command.installationId),
      command.volume, command.idleMinutes));
  }

  private async listSelfRoles(communityId: string): Promise<{ id: string; name: string; position: number; selfSelected: boolean }[]> {
    if (!this.listed(communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    return (await this.routes.listSelfRoles(fromHex(communityId))).map(row => ({ id: toHex(row.id), name: row.name,
      position: row.position, selfSelected: row.selfSelected }));
  }

  private async configureSelfRoles(command: Extract<Command, { m: 'configureSelfRoles' }>): Promise<null> {
    if (!this.listed(command.communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    await this.routes.configureSelfRoles(fromHex(command.communityId), fromHex(command.requestId), command.roleIds.map(fromHex));
    return null;
  }

  private async selectSelfRole(command: Extract<Command, { m: 'selectSelfRole' }>): Promise<null> {
    if (!this.listed(command.communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    await this.routes.selectSelfRole(fromHex(command.communityId), fromHex(command.roleId), fromHex(command.requestId), command.enabled);
    return null;
  }

  private async registerRolePanel(command: Extract<Command, { m: 'registerRolePanel' }>): Promise<null> {
    await this.routes.registerRolePanel(fromHex(command.communityId), fromHex(command.requestId), fromHex(command.channelId),
      BigInt(command.messageSeq), command.options.map(option => ({ emoji: option.emoji, roleId: fromHex(option.roleId) })));
    return null;
  }

  private async getMemberTimeout(communityId: string, userId: string): Promise<string | null> {
    if (!this.listed(communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const expires = await this.routes.getMemberTimeout(fromHex(communityId), fromHex(userId));
    return expires === null ? null : expires.toString();
  }

  private async setMemberTimeout(command: Extract<Command, { m: 'setMemberTimeout' }>): Promise<null> {
    if (!this.listed(command.communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    await this.routes.putMemberTimeout(fromHex(command.communityId), fromHex(command.userId), fromHex(command.requestId), BigInt(command.expires));
    return null;
  }

  private async clearMemberTimeout(command: Extract<Command, { m: 'clearMemberTimeout' }>): Promise<null> {
    if (!this.listed(command.communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    await this.routes.deleteMemberTimeout(fromHex(command.communityId), fromHex(command.userId), fromHex(command.requestId));
    return null;
  }

  private async warnMember(command: Extract<Command, { m: 'warnMember' }>): Promise<{ requestId: string; actionId: string; revision: string; acceptedAt: number; evidence: 'accepted' | 'pending' | 'rejected' | 'unknown' }> {
    if (!this.listed(command.communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const core = this.requireCore();
    if (core.prepareManualWarningEvidence === undefined || core.markManualWarningAccepted === undefined
      || core.manualWarningEvidence === undefined || core.ensureManualWarningEvidenceRequest === undefined
      || core.completeManualWarningEvidence === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
    const requestId = fromHex(command.requestId);
    const now = BigInt(Math.floor(this.deps.now() / 1000));
    const current = core.manualWarningEvidence().find(row => toHex(row.requestId) === command.requestId);
    if (current !== undefined && (toHex(current.communityId) !== command.communityId || toHex(current.targetUserId) !== command.userId || current.reason !== command.reason)) {
      throw new Refusal('E_CONFLICT', 'warning request id is already bound to another reason or member');
    }
    if (current === undefined) core.prepareManualWarningEvidence(requestId, crypto.getRandomValues(new Uint8Array(16)), fromHex(command.communityId),
      fromHex(command.userId), command.reason, now, now);
    const receipt = await this.routes.warnMember(fromHex(command.communityId), fromHex(command.userId), requestId);
    core.markManualWarningAccepted(requestId, receipt.actionId, BigInt(receipt.revision), BigInt(receipt.acceptedAt));
    let evidence: 'accepted' | 'pending' | 'rejected' | 'unknown' = 'unknown';
    try {
      const row = core.manualWarningEvidence().find(item => toHex(item.requestId) === command.requestId);
      if (row === undefined) throw new CoreError('E_CORE_STATE', 'manual warning evidence was not staged');
      const reserved = core.ensureManualWarningEvidenceRequest(requestId, crypto.getRandomValues(new Uint8Array(16)), BigInt(Math.floor(this.deps.now() / 1000)));
      const sent = await this.humanInteractionRequest({ m: 'humanInteractionRequest', communityId: command.communityId, channelId: null,
        requestId: toHex(reserved.requestId), operation: 61, args: [command.requestId, row.reason, ''], revision: '0' }, reserved.expires);
      evidence = sent.upload;
      if (evidence === 'accepted') core.completeManualWarningEvidence(requestId, BigInt(Math.floor(this.deps.now() / 1000)));
    } catch { evidence = 'unknown'; }
    return { requestId: toHex(receipt.requestId), actionId: toHex(receipt.actionId), revision: String(receipt.revision), acceptedAt: receipt.acceptedAt, evidence };
  }

  private async configureBotInstallation(command: Extract<Command, { m: 'configureBotInstallation' }>): Promise<ReturnType<typeof botInstallationView>> {
    if (!this.listed(command.communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const row = await this.routes.configureBotInstallation(fromHex(command.communityId), fromHex(command.requestId),
      command.expectedRevision === null ? null : BigInt(command.expectedRevision), command.capabilities, command.stepUp);
    return botInstallationView(row);
  }

  private async disableBotInstallation(command: Extract<Command, { m: 'disableBotInstallation' }>): Promise<ReturnType<typeof botInstallationView>> {
    if (!this.listed(command.communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const row = await this.routes.disableBotInstallation(fromHex(command.communityId), fromHex(command.requestId), BigInt(command.expectedRevision));
    return botInstallationView(row);
  }

  // ---- slices ----

  private setAccount(patch: Partial<AccountState>): void {
    // The cached display name belongs to one signed-in user: any state that clears or revokes the user drops it.
    if (patch.user === null || patch.phase === 'revoked') { this.selfDisplay = null; this.profileSaved = null; }
    if (patch.error !== undefined && patch.error !== null) patch.error = sanitizePageError(patch.error);
    this.accountState = { ...this.accountState, ...patch };
    // The status choice belongs to the signed-in account too: with the user gone, the next account starts from the default
    // until its own read lands, and a read still in flight for the old one is not applied.
    if (patch.user === null && patch.presenceChoice === undefined) {
      delete this.accountState.presenceChoice;
      ++this.presenceChoiceReadSeq;
    }
    this.phase = this.accountState.phase;
    this.slices.set('account', this.accountState);
  }

  private setConnection(patch: Partial<ConnectionState>): void {
    this.connectionState = { ...this.connectionState, ...patch };
    this.slices.set('connection', this.connectionState);
    this.publishDiagnostics();
  }

  private onWelcomeClockSkew(groupId: Id, communityId: Id | null): void {
    const now = this.deps.now();
    // Bounded: one entry per kept interaction Welcome group, re-inserted on each report so the least recent goes first.
    const groupHex = toHex(groupId);
    this.welcomeClockSkew.delete(groupHex);
    this.welcomeClockSkew.set(groupHex, { communityHex: communityId === null ? '' : toHex(communityId), atMs: now });
    const oldest = this.welcomeClockSkew.size > 64 ? this.welcomeClockSkew.keys().next().value : undefined;
    if (oldest !== undefined) this.welcomeClockSkew.delete(oldest);
    if (this.welcomeClockSkewLoggedMs !== null && now - this.welcomeClockSkewLoggedMs < 60_000) return;
    this.welcomeClockSkewLoggedMs = now;
    this.recordError('E_CLOCK_SKEW');
  }

  private recordError(code: string): void {
    this.diagnosticsErrors.push({ code, at: this.deps.now() });
    if (this.diagnosticsErrors.length > 20) this.diagnosticsErrors.shift();
    this.publishDiagnostics();
  }

  private publishDiagnostics(): void {
    this.slices.set('diagnostics', { connection: this.connectionState.status, generation: this.connectionState.generation,
      instance: this.instance === null ? '' : toHex(this.instance.instanceId), devices: this.deviceCount,
      errors: [...this.diagnosticsErrors], keyPackages: { ...this.keyPackages } });
  }

  /**
   * Tops the device's KeyPackages up from the count `remaining` yields. One attempt runs at a
   * time. A failure is shown in Diagnostics and retried, 5 s doubling to 5 min, until it lands or the account is let go
   * of; a publish that vanished silently once left a device without any package for weeks.
   */
  private async topUpKeyPackages(core: CorePort, limits: Limits, remaining: () => Promise<number>, retry = false): Promise<void> {
    // A pending retry is in charge: a flapping gateway's ready must not publish more often than the backoff allows.
    if (this.keyPackagesBusy === core || this.keyPackagesRetry !== null) return;
    this.keyPackagesBusy = core;
    try {
      // A retry asks the server first: the failed attempt may have landed (a lost response), and the count says so.
      const known = (retry ? (await this.routes.getKeyPackageCount())?.ordinary : undefined) ?? await remaining();
      if (this.core !== core) return;
      const published = await refillKeyPackages(core, this.routes, known,
        { perDevice: limits.keypackagesPerDevice, threshold: limits.keypackageRefillThreshold });
      if (this.core !== core) return;
      this.keyPackagesDelayMs = 0;
      this.keyPackages = { count: known + published, lastAttempt: published > 0 ? this.deps.now() : this.keyPackages.lastAttempt, lastError: null };
      this.publishDiagnostics();
    } catch (e) {
      if (this.core !== core || this.revoked()) return;
      this.keyPackagesDelayMs = Math.min(Math.max(KEYPACKAGE_RETRY_MIN_MS, this.keyPackagesDelayMs * 2), KEYPACKAGE_RETRY_MAX_MS);
      this.keyPackages = { ...this.keyPackages, lastAttempt: this.deps.now(), lastError: errorOf(e).code };
      this.publishDiagnostics();
      this.keyPackagesRetry = this.deps.setTimeout(() => {
        this.keyPackagesRetry = null;
        if (this.core === core && !this.revoked()) void this.barrier.track(this.topUpKeyPackages(core, limits, remaining, true));
      }, this.keyPackagesDelayMs);
    } finally {
      if (this.keyPackagesBusy === core) this.keyPackagesBusy = null;
    }
  }

  private clearKeyPackageRetry(): void {
    if (this.keyPackagesRetry !== null) this.deps.clearTimeout(this.keyPackagesRetry);
    this.keyPackagesRetry = null;
  }

  private nowS(): bigint { return BigInt(Math.floor(this.deps.now() / 1000)); }

  /** The instance clock now: the last ticket's mint time plus the whole seconds elapsed here since, else this browser's
   *  clock (no socket has connected yet). A snapshot alone would stand still and hold every later frame as fresh. */
  private serverNowS(): bigint {
    if (this.ticketTime === null) return this.nowS();
    const elapsed = Math.max(0, Math.floor((this.deps.now() - this.ticketMintedMs) / 1000));
    return this.ticketTime + BigInt(elapsed);
  }

  // ---- start, leadership and boot ----

  private start(): Promise<null> {
    this.starting ??= this.startOnce();
    return this.starting;
  }

  private async startOnce(): Promise<null> {
    this.setAccount({ phase: 'loading', instance: null, user: null, deviceId: null, recoveryKey: null, error: null, signIn: null, rootMismatch: false, ownTrust: null,
      factorSetup: null });
    this.setConnection({ status: 'offline', generation: null });
    let instance: Instance;
    try {
      instance = await this.routes.getInstance();
      this.limits = await this.routes.getLimits();
    } catch (e) {
      this.setAccount({ phase: 'error', error: errorOf(e) });
      throw e;
    }
    this.instance = instance;
    // A tab that then waits for the lock offers hand-over by this instance (L-WEB-46), so diagnostics names it now.
    this.publishDiagnostics();
    this.setAccount({ instance: instanceStateOf(instance) });
    void this.lead(instance);
    return null;
  }

  private async lead(instance: Instance): Promise<void> {
    try {
      this.leadership = await acquireLeadership(this.deps.locks, lockName(toHex(instance.instanceId)), () => {
        // Pre-flight ruling 4: the blocking request is already queued; other-tab shows only after the grace.
        this.graceTimer = this.deps.setTimeout(() => {
          this.graceTimer = null;
          if (this.phase === 'loading') this.setAccount({ phase: 'other-tab' });
        }, LEADER.otherTabGraceMs);
      });
    } catch (e) {
      this.setAccount({ phase: 'error', error: errorOf(e) });
      return;
    }
    if (this.graceTimer !== null) { this.deps.clearTimeout(this.graceTimer); this.graceTimer = null; }
    if (this.phase === 'other-tab') this.setAccount({ phase: 'loading' });
    await this.bootStore(instance);
  }

  /** deps.boot, then the phase the store's identity calls for (requirement 10). Never throws. */
  private async bootStore(instance: Instance): Promise<void> {
    try {
      const paused = this.pausedCore;
      this.pausedCore = null;
      const outcome = paused === null ? await this.deps.boot(instance) : await this.resumePaused(paused, instance);
      if (outcome.kind === 'unsupported') {
        this.setAccount({ phase: 'unsupported', error: sanitizeWorkerError(new Refusal('E_UNSUPPORTED', outcome.reason)) });
        return;
      }
      if (outcome.kind === 'store-lost') { this.setAccount({ phase: 'store-lost' }); return; }
      const core = outcome.core;
      if (this.core !== core) this.clearInteractionAckLeases();
      this.core = core;
      this.seenTrustGeneration = core.trustGeneration();
      const now = (): number => this.deps.now();
      const session = this.parts.session({ core, routes: this.routes, now });
      this.session = session;
      this.signup = this.parts.signup({ core, routes: this.routes, session, now });
      const enrol = this.parts.enrol({ core, routes: this.routes, session, now });
      this.enrol = enrol;
      const identity = core.identity();
      if (identity.phase === 0) { this.setAccount({ phase: 'needs-signup' }); return; }
      if (identity.phase === 1) {
        const resumed = await this.requireSignup().resume();
        if (resumed === 0) this.setAccount({ phase: 'needs-signup' });
        else if (resumed === 2) { this.keyPackagesOwed = true; await this.enterReady('required'); }
        // A refused device whose account exists: nothing is deleted, signupReset is never called here.
        else this.enterRevoked();
        return;
      }
      if (identity.phase === 3) {
        // A reload mid-ceremony keeps its place: a registered enrolment resumes at the key step.
        if (identity.userId === null) { enrol.reset(); this.setAccount({ phase: 'needs-signup' }); return; }
        const signIn = signInOf(null, null);
        try {
          this.fetched = await enrol.fetch(identity.userId);
          this.setAccount({ phase: 'signin-key', signIn, error: null });
        } catch (e) {
          this.fetched = null;
          this.setAccount({ phase: 'signin-key', signIn, error: errorOf(e) });
        }
        return;
      }
      await this.enterReady('required');
    } catch (e) {
      // After a revocation the error is its consequence (a 401 that reauthenticate turned into revoked).
      if (!this.revoked()) this.setAccount({ phase: 'error', error: errorOf(e) });
    }
  }

  /**
   * A worker that released the lead keeps its paused store: the OPFS pool lives in this worker and stays paused, and a
   * fresh open here would find that paused pool and fail ("unable to open database file"). Resuming re-acquires the
   * pool and rebuilds the core from what the other tab wrote. If that fails, a fresh open is the fallback.
   */
  private async resumePaused(core: CorePort, instance: Instance): Promise<BootOutcome> {
    try {
      await core.resume();
      return { kind: 'opened', core, instance };
    } catch {
      try { core.close(); } catch { /* nothing left to release */ }
      return this.deps.boot(instance);
    }
  }

  private resetDevice(): Promise<null> {
    this.resetting ??= this.resetOnce().finally(() => { this.resetting = null; });
    return this.resetting;
  }

  private async resetOnce(): Promise<null> {
    const instance = this.requireInstance();
    this.dropTrays();
    await this.deps.resetDevice(instance);
    await this.bootStore(instance);
    return null;
  }

  // ---- ready and revoked ----

  /** Requirement 11, in order; after every await a revocation that happened meanwhile ends it silently. web3.6: past (d),
   *  an account that owes a second factor stops at factor-setup (`origin` says why) with no backup PUT, no KeyPackage and
   *  no socket: the instance refuses them all until a factor is added (protocol/09 scope F). */
  private async enterReady(origin: FactorSetupOrigin, known?: Factors): Promise<void> {
    try { await this.enterReadyOnce(origin, known); }
    catch (e) {
      // Final review M5: a reset (or a CLI sweep) landing after the gate read answers E_FACTOR_REQUIRED while the phase is
      // not yet ready, which the global hook ignores. The account owes a factor: stop the gateway and engine the steps
      // after the gate built, and take the forced set-up, as the hook does, rather than the error screen.
      const me = this.me;
      if (errorOf(e).code !== 'E_FACTOR_REQUIRED' || this.revoked() || this.wiping || this.storeCleared || me === null) throw e;
      this.onFactorRequired();
      await this.enterFactorSetup('required', me.userId, me.deviceId);
    }
  }

  private async enterReadyOnce(origin: FactorSetupOrigin, known?: Factors): Promise<void> {
    const core = this.requireCore();
    const session = this.session;
    if (session === null) throw new CoreError('E_CORE_STATE', 'no session');
    let userId: Id;
    let deviceId: Id;
    try {
      // (a) a list this device has not published yet: the session from before it may be pending.
      const unpublished = !core.identity().listPublished;
      // (b)
      const ok = await session.ensure();
      if (this.revoked()) return;
      if (!ok) { this.enterRevoked(); return; }
      await this.checkInstanceSignerPin();
      if (this.revoked()) return;
      // (c)
      await this.parts.publishDeviceList(core, this.routes);
      if (this.revoked()) return;
      // (d) the server enrols the session only once the list names this device; the gateway refuses a pending one.
      if (unpublished) {
        const fresh = await session.establish();
        if (this.revoked()) return;
        if (!fresh) { this.enterRevoked(); return; }
      }
      const identity = core.identity();
      if (identity.userId === null || identity.deviceId === null) throw new CoreError('E_CORE_NO_IDENTITY', '');
      userId = identity.userId;
      deviceId = identity.deviceId;
      this.me = { userId, deviceId };
      // The factor gate (web3.6), after (d) and before (e): ensureBackups' PUT is not an F route.
      // `known` is the read factorSetupDone just made: one read per set-up, a re-ready (contract 6) still reads.
      const factors = known ?? await this.routes.getFactors();
      if (this.revoked()) return;
      this.slices.set('factors', factorsState(factors));
      if (factors.owed) { await this.enterFactorSetup(origin, userId, deviceId); return; }
      // A signup's first KeyPackages, refused while owed (Signup.submit no longer sends them); before any join.
      if (this.keyPackagesOwed) {
        this.keyPackagesOwed = false;
        const limits = this.limits ?? await this.routes.getLimits();
        // A refusal is shown and retried in the background; the person still gets in.
        await this.topUpKeyPackages(core, limits, () => Promise.resolve(0));
        if (this.revoked()) return;
      }
      // (e) the next ready retries a failed upload; a root mismatch is recorded, never dropped.
      await this.ensureBackups(core);
      if (this.revoked()) return;
      // (f)
      const own = await this.parts.refreshOwnDeviceList(core, this.routes, userId);
      if (this.revoked()) return;
      if (!own.listed) { this.enterRevoked(); return; }
      // BACKUPS-RECOVERY-03: a stored state object behind the own list or junk is replaced; the next ready retries.
      await this.repairBackupState(core);
      if (this.revoked()) return;
    } catch (e) {
      // Requirement 12: a pending scope or an unlisted answer for a device the own list names is a revocation.
      if ((isCode(e, 'E_SESSION_SCOPE') || isCode(e, 'E_DEVICE_UNLISTED')) && core.identity().phase === 2) {
        this.enterRevoked();
        return;
      }
      throw e;
    }
    // (g) the engine subscribes before the gateway can report its first ready (pre-flight ruling 2).
    if (this.gateway === null) {
      const now = (): number => this.deps.now();
      const random = (): number => this.deps.random();
      const setTimeout = (fn: () => void, ms: number): number => this.deps.setTimeout(fn, ms);
      const clearTimeout = (handle: number): void => { this.deps.clearTimeout(handle); };
      const gateway = this.parts.gateway({
        url: this.deps.origin.replace(/^http/, 'ws') + '/gateway',
        mintTicket: async () => {
          // Gateway retries call mintTicket directly, without passing through the
          // controller's explicit ready/revocation paths. Revalidate the pinned
          // instance signer before every connection attempt.
          if (this.core?.identity().phase === 2) await this.checkInstanceSignerPin();
          const minted = await this.routes.postTicket();
          // The instance's clock at the mint (a ticket is valid TICKET_TTL_S seconds, protocol/09): the notice
          // floor compares server receive times with server time, never with this browser's clock.
          this.ticketTime = minted.expires - TICKET_TTL_S;
          this.ticketMintedMs = this.deps.now();
          return minted.ticket;
        },
        WebSocket: this.deps.WebSocket, now, random, setTimeout, clearTimeout,
      });
      this.gateway = gateway;
      gateway.subscribe((e) => { this.onGateway(e); });
      this.startEngine();
      this.wasDown = false;
      gateway.start();
    }
    // (h) every community's channels and members, then the DMs, so join-all expects every visible group.
    await this.refreshCommunities();
    if (this.revoked()) return;
    for (const community of this.slices.get('communities') ?? []) {
      try { await this.loadCommunity(community.id); } catch { /* the next ready retries */ }
      if (this.revoked()) return;
    }
    try { await this.reloadDms(); }
    catch {
      // Pre-flight row 2.14(d): a page waiting for the DM list must not wait for the next ready.
      if (this.slices.get('dms') === undefined) this.slices.set('dms', []);
    }
    if (this.revoked()) return;
    // (i)
    this.slices.set('settings', pageSettings(core.settings()));
    this.publishBadges();
    if (this.slices.get('notices') === undefined) this.slices.set('notices', this.noticesState);
    // C4 fix round 1: the removals slice is present from ready on, as notices is.
    if (this.slices.get('removals') === undefined) this.slices.set('removals', []);
    this.calls.publishInitial();
    // (j) after a reload mid-ceremony the store records no username (head ruling 42): the account names it.
    let username = core.identity().username;
    if (username === '') username = (await this.routes.getAccountMe()).username;
    if (this.revoked()) return;
    this.noticeFloor = this.serverNowS();
    this.setAccount({
      phase: 'ready', user: { id: toHex(userId), username, ...this.knownDisplay(toHex(userId)) }, deviceId: toHex(deviceId),
      recoveryKey: null, error: null, signIn: null, factorSetup: null,
    });
    this.publishDms();
    // The gateway's first ready can arrive before the phase is ready, when a purge run returns at once.
    this.runPurges();
    // I3: so can its trust refresh, which reads nothing before the phase is ready; this one does. It also republishes
    // the trust slice now that every member and DM slice has landed.
    if (this.trustRefresh !== null) this.trustRefreshAgain = true;
    void this.barrier.track(this.refreshTrust()).catch(() => undefined);
    // web3.7 3a-client: the call groups a page of this browser left behind are left now, before any join is served (a
    // join waits for it, bounded); it needs no gateway. Not tracked by the barrier: a resign stops it instead.
    this.calls.startSweep();
    void this.barrier.track(this.resumeManualRemovalEvidence()).catch(() => undefined);
    void this.barrier.track(this.resumeManualWarningEvidence()).catch(() => undefined);
  }

  /** The engine with the controller's report handlers; enterReady and resume() build it (requirement 23). */
  private startEngine(): void {
    const core = this.requireCore();
    const gateway = this.gateway;
    const me = this.me;
    if (gateway === null || me === null) throw new CoreError('E_CORE_STATE', 'no gateway');
    const sync = this.parts.sync({
      core, routes: this.routes, gateway, instance: this.requireInstance(), deviceId: me.deviceId,
      now: () => this.deps.now(),
      random: () => this.deps.random(),
      setTimeout: (fn, ms) => this.deps.setTimeout(fn, ms),
      clearTimeout: (handle) => { this.deps.clearTimeout(handle); },
      onGroupChanged: (g, result) => { this.onGroupChanged(g, result); },
      onTrustChanged: () => { this.safely(() => this.refreshTrustGeneration()); },
      onError: (code) => { this.recordError(code); },
      trackJob: (job) => { void this.barrier.track(job).catch(() => undefined); },
      // Ruling 8: the core records a delete's purge when the type-2 row is confirmed, which the engine reports here.
      onOutboxChanged: (g) => { this.safely(() => { this.touchGroup(g); }); this.runPurges(); },
      onMembership: (g, status) => { this.onMembership(g, status); },
      onJoinAll: () => {},
      // Head ruling 37: a Welcome for a group this device does not expect is a DM another participant opened.
      // The WB-walk belt beside op 54: the Welcome can be a new channel's group too, so the selected server's lists are
      // read again (debounced with op 54's reads).
      onUnexpectedWelcome: () => {
        void this.barrier.track(this.reloadDms().catch(() => {}));
        if (this.selected !== null) this.scheduleCommunityReload(this.selected);
      },
      onWelcomeClockSkew: (g, community) => { this.onWelcomeClockSkew(g, community); },
      beforeSend: (g, row) => this.beforeSend(g, row),
      onDiscarded: (g, row) => { this.onDiscarded(g, row); },
      refreshGroupTrust: (g) => this.refreshGroupTrust(g),
      onSlow: (g, untilMs, queued) => { this.safely(() => { this.onSlow(g, untilMs, queued); }); },
      onPosted: (g, type, row, seq) => { this.safely(() => { this.onPosted(g, type, row, seq); }); },
    });
    this.sync = sync;
    sync.start();
  }

  /** Requirements 11(e) and 13: a rejection is swallowed (the next ready retries, L-TS-21), except that a root
   *  mismatch (task 11 ruling (b), pre-flight row 1.5: the instance holds a root this device did not seal, so every
   *  later recovery on it fails as a wrong key) is published on the account slice, where the page shows its alert
   *  (BACKUPS-RECOVERY-04, protocol/06 and 09 "raises E_ROOT_MISMATCH with an alert"); a later success clears it. */
  private async ensureBackups(core: CorePort): Promise<void> {
    let mismatch: boolean;
    try {
      await this.parts.ensureBackups(core, this.routes);
      mismatch = false;
    } catch (e) {
      if (!isCode(e, 'E_ROOT_MISMATCH')) return;
      mismatch = true;
    }
    if (this.storeCleared || this.accountState.rootMismatch === mismatch) return;
    this.setAccount({ rootMismatch: mismatch });
  }

  /** BACKUPS-RECOVERY-03: swallowed like ensureBackups; the next ready tries again. */
  private async repairBackupState(core: CorePort): Promise<void> {
    try { await this.parts.repairBackupState(core, this.routes); } catch { /* the next ready retries */ }
  }

  /** Read through a call: tsc keeps a narrowing of this.phase across awaits, but a revocation can land in any of them. */
  private revoked(): boolean { return this.phase === 'revoked'; }

  /** The one revoked path (ruling 29): idempotent; inert while this browser erases itself (head ruling 27). */
  private enterRevoked(): void {
    if (this.wiping || this.storeCleared) return;
    if (this.revoked()) return;
    this.clearReauthTimer();
    this.clearHeldReads();
    this.clearPendingRolePanelActionRetries();
    this.clearKeyPackageRetry();
    this.typingOff();
    this.calls.reset();
    this.sync?.stop();
    this.gateway?.stop();
    this.setAccount({ phase: 'revoked', recoveryKey: null });
  }

  private onGateway(e: GatewayEvent): void {
    if (e.type === 'status') {
      let status: ConnectionState['status'];
      switch (e.status) {
        case 'ready':
          if (this.wasDown) this.keyPackagesRecheck = true;
          this.wasDown = false;
          status = 'online';
          // web3.7 3a-client: a boot sweep that found the instance unreachable runs at the first online; once it has
          // run this ready, this starts nothing (startSweep is memoised per ready).
          if (this.phase === 'ready') this.safely(() => { this.calls.startSweep(); });
          break;
        case 'waiting': this.wasDown = true; status = 'offline'; break;
        case 'connecting': status = this.wasDown ? 'offline' : 'connecting'; break;
        case 'idle': status = 'offline'; break;
      }
      // A version refusal leaves the gateway idle for good, so the page is told why (L-TS-08 reason);
      // every other status change drops the reason.
      const { generation } = this.connectionState;
      this.connectionState = e.status === 'idle' && e.closeCode === CLIENT_CLOSE.version
        ? { status, generation, reason: 'version' }
        : { status, generation, ...(status === 'online' && this.gateway?.acceptedVersions !== undefined && this.gateway.acceptedVersions !== null
          ? { wireVersion: Number(this.gateway.acceptedVersions.wire) } : {}) };
      this.slices.set('connection', this.connectionState);
      if (status === 'online') this.safely(() => { this.sendOfflineSelf(); });
      if (status === 'offline') for (const community of this.slices.get('communities') ?? []) {
        this.clearPresence(community.id);
        this.clearAFK(community.id);
      }
      this.stopTypingIfUnviewable();
    } else if (e.type === 'rtt') {
      // Task 11 (Jonas, Q3): the You tab's "Connected to {host} · {ping}"; published only while online and on a change.
      if (this.connectionState.status === 'online' && this.connectionState.pingMs !== e.ms) {
        this.connectionState = { ...this.connectionState, pingMs: e.ms };
        this.slices.set('connection', this.connectionState);
      }
    } else if (e.type === 'ready') {
      this.onReady(e.info);
    } else if (e.type === 'frame' && e.frame.op === Op.appBuild) {
      this.setConnection({ appBuild: e.frame.payload[0] as string });
    } else if (e.type === 'frame' && e.frame.op === Op.presence) {
      this.incomingPresence(e.frame.payload);
    } else if (e.type === 'frame' && e.frame.op === Op.typing) {
      this.incomingTypingFrame(e.frame.payload);
    } else if (e.type === 'frame' && (e.frame.op === Op.voiceState || e.frame.op === Op.callRing)) {
      if (e.frame.op === Op.voiceState) this.safely(() => { this.ownVoiceState(e.frame); });
      this.safely(() => { this.calls.onFrame(e.frame); });
    } else if (e.type === 'frame' && e.frame.op === Op.communityChanged) {
      const communityId = toHex(communityChangedOf(e.frame));
      const changeSlice = `communityChanges:${communityId}` as const;
      this.slices.set(changeSlice, (this.slices.get(changeSlice) ?? 0) + 1);
      this.safely(() => { this.scheduleCommunityReload(communityId); });
    } else if (e.type === 'frame' && e.frame.op === Op.afkAway) {
      this.safely(() => { this.incomingAFKAway(afkAwayOf(e.frame)); });
    } else if (e.type === 'revoked') {
      void this.barrier.track(this.onRevokedSocket());
    }
  }

  private onReady(info: ReadyInfo): void {
    this.fetchedLeaves.clear();
    if (this.trustRefresh !== null) this.trustRefreshAgain = true;
    void this.barrier.track(this.refreshTrust().catch(() => undefined));
    this.setConnection({ generation: info.generation.toString() });
    // R-AFK3 (a): the AFK snapshot is not cleared here; refreshAfterReady reads it again in place (an offline status
    // already cleared it, so a reconnect does start from null).
    for (const community of this.slices.get('communities') ?? []) this.clearPresence(community.id);
    void this.barrier.track(this.refreshPresenceChoice());
    // Pre-flight ruling (a): what this ready catches up was not delivered live.
    if (this.phase === 'ready') this.noticeFloor = this.serverNowS();
    const core = this.core;
    const limits = this.limits;
    if (core !== null && limits !== null) {
      // After a reconnect the server's own count is asked for; an older server without the route leaves ready's count.
      const recheck = this.keyPackagesRecheck;
      this.keyPackagesRecheck = false;
      void this.barrier.track(this.topUpKeyPackages(core, limits, async () =>
        (recheck ? (await this.routes.getKeyPackageCount())?.ordinary : undefined) ?? info.keypackagesRemaining));
    }
    // Requirement 13 (Q27: clients refetch on ready), not awaited in the ready path.
    const me = this.me;
    if (core !== null && me !== null) {
      this.retryPendingRolePanelActions(core, me);
      void this.barrier.track(this.ensureBackups(core));
      void this.barrier.track(this.parts.refreshOwnDeviceList(core, this.routes, me.userId)
        .then((own) => {
          if (!own.listed) return this.enterRevoked();
          // Contract 6: the device count is read once per ready (from the list this pass just refreshed), so Diagnostics never reads 0.
          void this.barrier.track(this.refreshDevices(own).then(() => undefined, () => undefined));
          return this.repairBackupState(core);
        }, () => undefined));
    }
    void this.barrier.track(this.refreshAfterReady());
    this.runPurges();
  }

  /** Every ready refreshes the community list, every listed community and the DMs; a failure waits for the next ready. */
  private async refreshAfterReady(): Promise<void> {
    try { await this.refreshCommunities(); } catch { /* the next ready retries */ }
    for (const community of this.slices.get('communities') ?? []) {
      if (this.revoked() || this.storeCleared) return;
      try { await this.loadCommunity(community.id); } catch { /* the next ready retries */ }
    }
    if (this.revoked() || this.storeCleared) return;
    try { await this.reloadDms(); } catch { /* the next ready retries */ }
  }

  private incomingTypingFrame(payload: readonly unknown[]): void {
    const [channel, user, , on, expires] = payload;
    if (!(channel instanceof Uint8Array) || channel.length !== 16 || !(user instanceof Uint8Array) || user.length !== 16
      || typeof on !== 'bigint' || typeof expires !== 'bigint' || this.me === null || toHex(user) === toHex(this.me.userId)) return;
    const channelId = toHex(channel);
    if (!this.open.has(channelId) || this.groupState(channelId) !== 'active' || this.connectionState.status !== 'online') return;
    // expires_ts is on the instance's clock (L-HTTP-96: its now + 10 s), read against that clock as estimated from this
    // connection's ticket; the row then lives on this browser's clock for what remains, never longer than the 10 s relay
    // window.
    const remaining = expires - this.serverNowS();
    if (remaining <= 0n) return;
    const expiry = this.deps.now() + Number(remaining < 10n ? remaining : 10n) * 1000;
    const rows = this.incomingTyping.get(channelId) ?? new Map<string, number>();
    const userId = toHex(user);
    if (on === 1n) rows.set(userId, expiry); else rows.delete(userId);
    this.incomingTyping.set(channelId, rows);
    this.publishTyping(channelId);
    if (on === 1n) this.armTypingSweep(channelId, expiry);          // a stop frame has nothing to expire
  }

  /** One sweep timer per channel, however many frames arrive: a frame moves it only when its deadline is earlier. */
  private armTypingSweep(channelId: string, at: number): void {
    const pending = this.typingSweeps.get(channelId);
    if (pending !== undefined) {
      if (pending.at <= at) return;
      this.deps.clearTimeout(pending.handle);
    }
    const handle = this.deps.setTimeout(() => this.sweepTyping(channelId), Math.max(0, at - this.deps.now()));
    this.typingSweeps.set(channelId, { handle, at });
  }

  private dropIncomingTyping(channelId: string): void {
    this.incomingTyping.delete(channelId);
    const pending = this.typingSweeps.get(channelId);
    if (pending !== undefined) this.deps.clearTimeout(pending.handle);
    this.typingSweeps.delete(channelId);
  }

  /** The expiry timer. It may fire a millisecond before the clock reaches the deadline it was set for (Firefox does, under
   *  load); the row is then still live and nothing else would sweep it, so while any row remains it looks again when the
   *  nearest one is due. */
  private sweepTyping(channelId: string): void {
    this.typingSweeps.delete(channelId);
    this.publishTyping(channelId);
    const rows = this.incomingTyping.get(channelId);
    if (rows === undefined) return;
    let nearest = Infinity;
    for (const expiry of rows.values()) if (expiry < nearest) nearest = expiry;
    if (nearest !== Infinity) this.armTypingSweep(channelId, nearest);
  }

  private publishTyping(channelId: string): void {
    const rows = this.incomingTyping.get(channelId);
    if (rows === undefined) return;
    const now = this.deps.now();
    for (const [user, expiry] of rows) if (expiry <= now) rows.delete(user);
    this.slices.set(`typing:${channelId}`, [...rows].map(([userId, expiresAt]) => ({ userId, name: this.nameOf(userId), expiresAt })));
  }

  private async onRevokedSocket(): Promise<void> {
    if (this.wiping || this.storeCleared) return;
    const session = this.session;
    if (session === null || this.revoked()) return;
    try {
      const ok = await session.establish();
      if (this.revoked() || this.wiping) return;
      if (ok) {
        if (this.core?.identity().phase === 2) await this.checkInstanceSignerPin();
        if (this.revoked() || this.wiping) return;
        this.gateway?.start();
      }
      else this.enterRevoked();
    } catch (e) {
      if (isCode(e, 'E_SESSION_SCOPE') && this.core?.identity().phase === 2) { this.enterRevoked(); return; }
      if (this.isInvalidInstancePin(e)) {
        this.failInstanceSignerPin(e);
        return;
      }
      // Not a refusal (a network failure, a 5xx): try the socket again later; a refused ticket comes back here.
      if (this.revoked() || this.wiping) return;
      // I4: the handle is kept and cleared by resign, quiesce and enterRevoked; a timer that fires anyway starts only the
      // socket of the leadership that armed it.
      this.scheduleSocketPinRetry(this.gateway);
    }
  }

  // ---- the second factor (web3.6, flow 10) ----

  /** enterReady's owed branch: the account is named (GET /v1/accounts/me is an F route) and nothing else runs. */
  private async enterFactorSetup(origin: FactorSetupOrigin, userId: Id, deviceId: Id): Promise<void> {
    let username = this.requireCore().identity().username;
    if (username === '') {
      try { username = (await this.routes.getAccountMe()).username; } catch { /* named at ready */ }
      if (this.revoked()) return;
    }
    // S2 review M5: under instance.discovery = "session" GET /v1/instance is E, F, so the set-up reads the methods it offers
    // with this session (the start's read had none). Only the same instance's answer is taken; a failure keeps the start's.
    let instance = this.accountState.instance;
    try {
      const read = await this.routes.getInstance();
      if (this.revoked()) return;
      if (toHex(read.instanceId) === toHex(this.requireInstance().instanceId)) instance = instanceStateOf(read);
    } catch { /* the start's read stands */ }
    this.setAccount({ phase: 'factor-setup', factorSetup: { origin }, user: { id: toHex(userId), username }, deviceId: toHex(deviceId),
      recoveryKey: null, error: null, signIn: null, instance });
  }

  /** The page's set-up is done: the factors are read again (still owed: E_FACTOR_REQUIRED, the phase stays), then ready,
   *  then the join a signup kept, else the invite given, as signupSubmit joins. */
  private async factorSetupDone(rawInvite: string | null): Promise<{ communityId: string | null; joinError: WorkerError | null }> {
    const factors = await this.routes.getFactors();
    this.slices.set('factors', factorsState(factors));
    if (factors.owed) throw new Refusal('E_FACTOR_REQUIRED', '');
    const origin = this.accountState.factorSetup?.origin ?? 'required';
    try {
      await this.enterReady(origin, factors);
    } catch (e) {
      if (!this.revoked()) this.setAccount({ phase: 'error', error: errorOf(e) });
      throw e;
    }
    if (this.phase !== 'ready') return { communityId: null, joinError: null };
    // The join a signup kept: in memory, else in the store (a page closed at the factor step and opened again).
    const pending = this.pendingJoin ?? this.storedPendingJoin();
    this.pendingJoin = null;
    let communityId: Id | null = null;
    let invite = '';
    if (pending !== null) ({ communityId, invite } = pending);
    else {
      this.dropStoredPendingJoin();
      if (rawInvite !== null && rawInvite.trim() !== '') {
        invite = rawInvite.trim();
        try { communityId = (await this.routes.getInvite(invite)).communityId; }
        catch (e) { return { communityId: null, joinError: errorOf(e) }; }
      }
    }
    if (communityId === null) { this.dropStoredPendingJoin(); return { communityId: null, joinError: null }; }
    const joined = await this.joinAfterReady(communityId, invite);
    // Spent once tried: a refused join is the signup's result, shown now, and is never tried again at a later set-up.
    this.dropStoredPendingJoin();
    return joined;
  }

  /** A signup stopped at factor-setup: the join waits in memory and in the account's store (one row, one invite), so a
   *  reload at the forced screen still joins it. Nothing reaches the instance until factorSetupDone finds the debt paid. */
  private keepPendingJoin(join: { communityId: Id | null; invite: string }): void {
    this.pendingJoin = join;
    const core = this.core;
    if (core === null) return;
    try {
      if (join.communityId === null || join.invite === '') core.settingDelete(PENDING_JOIN_KEY);
      else core.settingPut(PENDING_JOIN_KEY, JSON.stringify({ communityId: toHex(join.communityId), invite: join.invite }));
    } catch { /* too long for the row, or the store refused: the join is kept in memory only */ }
  }

  /** The stored join, or null when there is none or the row is not one this worker wrote. */
  private storedPendingJoin(): { communityId: Id; invite: string } | null {
    let raw: string | undefined;
    try { raw = this.core?.settings()[PENDING_JOIN_KEY]; } catch { return null; }
    if (raw === undefined) return null;
    try {
      const v = JSON.parse(raw) as unknown;
      if (typeof v !== 'object' || v === null) return null;
      const { communityId, invite } = v as Record<string, unknown>;
      if (typeof communityId !== 'string' || !/^[0-9a-f]{32}$/.test(communityId) || typeof invite !== 'string' || invite === '') return null;
      return { communityId: fromHex(communityId), invite };
    } catch { return null; }
  }

  private dropStoredPendingJoin(): void {
    try { this.core?.settingDelete(PENDING_JOIN_KEY); } catch { /* the next set-up finds no row it can read either way */ }
  }

  /** The global hook (HttpDeps.onRefusal): a ready account the instance now finds owing (an operator reset) stops its
   *  socket and engine, with no ticket retry, and shows the forced set-up. Once per transition (the hook's phase filter). */
  private onFactorRequired(): void {
    if (this.wiping || this.storeCleared) return;
    this.clearReauthTimer();
    this.typingOff();
    this.calls.reset();
    this.sync?.stop();
    this.gateway?.stop();
    // enterReady builds a fresh gateway and engine after factorSetupDone (a stopped engine cannot restart).
    this.gateway = null;
    this.sync = null;
    // Contract 6: what each ready reads once is read again at the next.
    this.fetchedLeaves.clear();
    // Web3.6 E3 finding: the factors the ready account held are gone (a reset removes the TOTP secret, the passkeys
    // and the backup codes, protocol/09 § Admin), so the slice says so before the phase moves. The page never shows the
    // forced set-up over the old factors, which it would take for one already in place; the read below replaces this.
    const held = this.slices.get('factors');
    this.slices.set('factors', { ...NO_FACTORS, owed: true, hasPassword: held?.hasPassword ?? true });
    this.setAccount({ phase: 'factor-setup', factorSetup: { origin: 'required' }, error: null });
    void this.barrier.track(this.routes.getFactors().then((f) => {
      if (this.phase === 'factor-setup') this.slices.set('factors', factorsState(f));
    }, () => undefined));
  }

  // ---- signup ----

  private signupBegin(): Promise<null> {
    if (this.storeCleared) return Promise.reject(new Refusal('E_NOT_READY', 'the store was cleared: reload'));
    const recoveryKey = this.requireSignup().begin(this.requireInstance().instanceId);
    this.setAccount({ phase: 'signup-keys', recoveryKey, error: null });
    return Promise.resolve(null);
  }

  private signupReset(): Promise<null> {
    this.requireCore().signupReset();
    this.setAccount({ phase: 'needs-signup', recoveryKey: null });
    return Promise.resolve(null);
  }

  /** Requirement 11 (ruling 34): the invite is read first, the account registered, ready entered, and the
   *  community the invite names joined; a refused join is a result, never a failed signup. */
  private async signupSubmit(c: Extract<Command, { m: 'signupSubmit' }>):
    Promise<{ communityId: string | null; joinError: WorkerError | null; factorOwed?: true }> {
    const signup = this.requireSignup();
    const core = this.requireCore();
    const invite = c.invite.trim();
    this.setAccount({ phase: 'registering', error: null });
    let communityId: Id | null = null;
    if (invite !== '') {
      try {
        communityId = (await this.routes.getInvite(invite)).communityId;
      } catch (e) {
        if (e instanceof DillaHttpError && e.code === 'E_INVITE_INVALID') {
          this.setAccount({ phase: 'signup-keys', error: errorOf(e) });
          throw e;
        }
        // Any other failure of this read: the server stays the authority at registration.
        communityId = null;
      }
    }
    try {
      await signup.submit({ invite, username: c.username, display: c.display, password: c.password });
    } catch (e) {
      if (core.identity().phase !== 2) {
        if (!this.revoked()) this.setAccount({ phase: 'signup-keys', error: errorOf(e) });
        throw e;
      }
      // The account exists; entering ready publishes what the submit could not.
    }
    if (this.phase === 'registering') this.setAccount({ recoveryKey: null });
    if (core.identity().phase === 2) this.keyPackagesOwed = true;
    try {
      await this.enterReady('signup');
    } catch (e) {
      if (!this.revoked()) this.setAccount({ phase: 'error', error: errorOf(e), recoveryKey: null });
      throw e;
    }
    // web3.6: the join waits for the factor; factorSetupDone makes it.
    if (this.phase === 'factor-setup') {
      this.keepPendingJoin({ communityId, invite });
      return { communityId: null, joinError: null, factorOwed: true };
    }
    if (this.phase !== 'ready' || communityId === null) return { communityId: null, joinError: null };
    return this.joinAfterReady(communityId, invite);
  }

  /** A refused join is a result, never a failed signup (ruling 34). */
  private async joinAfterReady(communityId: Id, invite: string): Promise<{ communityId: string | null; joinError: WorkerError | null }> {
    try {
      try {
        await this.routes.negotiateSession();
      } catch (e) {
        // Older instances have no capability endpoint. Only an explicit unsupported-route response falls back;
        // transient, authentication, and malformed negotiation failures must not race the gateway negotiation.
        if (!(e instanceof DillaHttpError) || ![404, 405, 501].includes(e.status)) throw e;
      }
      const joined = await this.joinPath(communityId, invite);
      return { communityId: joined.communityId, joinError: null };
    } catch (e) {
      return { communityId: null, joinError: errorOf(e) };
    }
  }

  // ---- sign-in (L-TS-23, requirements 3-9) ----

  /** One sign-in step at a time: the phase does not change while login or register run (requirement 3). */
  private step<T>(work: () => Promise<T>): Promise<T> {
    if (this.signInRunning) return Promise.reject(new Refusal('E_NOT_READY', 'a sign-in step is running'));
    this.signInRunning = true;
    return work().finally(() => { this.signInRunning = false; });
  }

  /** Elapsed time in ms (deps.monotonic, performance.now in the worker; the wall clock when a host gives none). */
  private elapsed(): number {
    return (this.deps.monotonic ?? this.deps.now)();
  }

  private signInBegin(): Promise<null> {
    if (this.storeCleared) return Promise.reject(new Refusal('E_NOT_READY', 'the store was cleared: reload'));
    this.fetched = null;
    this.setAccount({ phase: 'signin-login', signIn: signInOf(null, null), error: null });
    return Promise.resolve(null);
  }

  /** The key's form, in the core, before anything that needs it; a refusal returns to `phase` with the error. */
  private checkKey(recoveryKey: string, phase: BootPhase): void {
    try {
      this.requireCore().recoveryKeyCheck(recoveryKey);
    } catch (e) {
      this.setAccount({ phase, error: errorOf(e) });
      throw e;
    }
  }

  /** The password is passed to Enrol.login and held in no field (L-TS-21). web3.8 (R6): the login never registers.
   *  With no second factor owed, Enrol holds the settled assertion and the key step follows; signInKey registers it
   *  after the key's form check, back to back with the fetch, the enrolment and the list PUT (REGISTRATION-DEVICES-02). */
  private signInOIDC(assertion: string, needs: 0 | 1 | 2): Promise<{ needs: 0 | 1 | 2 }> {
    this.requireEnrol().oidc(assertion, needs);
    this.assertionAt = this.elapsed();
    this.setAccount({ phase: needs === 1 ? 'signin-totp' : needs === 2 ? 'signin-passkey' : 'signin-key',
      signIn: signInOf(null, needs === 1 ? 'totp' : needs === 2 ? 'passkey' : null), error: null });
    return Promise.resolve({ needs });
  }

  private async signInLogin(rawUsername: string, password: string): Promise<{ needs: 0 | 1 | 2 }> {
    const enrol = this.requireEnrol();
    const username = rawUsername.trim();
    let needs: 0 | 1 | 2;
    let passkey = false;
    try {
      const answer = await enrol.login(username, password);
      needs = answer.needs;
      passkey = answer.passkey === true;
    } catch (e) {
      this.setAccount({ phase: 'signin-login', error: errorOf(e) });
      throw e;
    }
    this.assertionAt = this.elapsed();
    // web3.6: needs 1 asks for the app's code, needs 2 (passkeys and no app) for a passkey; either takes a backup code.
    if (needs === 1) {
      this.setAccount({ phase: 'signin-totp', signIn: signInOf(username, 'totp', passkey), error: null });
      return { needs };
    }
    if (needs === 2) {
      this.setAccount({ phase: 'signin-passkey', signIn: signInOf(username, 'passkey'), error: null });
      return { needs };
    }
    this.setAccount({ phase: 'signin-key', signIn: signInOf(username, null), error: null });
    return { needs };
  }

  /** web3.6: the passkey ceremony's options for the page's navigator.credentials.get; nothing is held here. */
  private async signInPasskeyBegin(): Promise<{ ceremonyId: string; options: string }> {
    try {
      const ceremony = await this.routes.passkeyLoginBegin();
      return { ceremonyId: toHex(ceremony.ceremonyId), options: ceremony.options };
    } catch (e) {
      this.setAccount({ error: errorOf(e) });
      throw e;
    }
  }

  /** web3.6: a passkey that verified the person signs in on its own (needs 0); one that did not (needs 1) goes on to the
   *  code step with `factor 'passkey'` for its copy. The passkey names the account, so the typed username is not kept. A
   *  refused passkey stays at its step with the error, and an assertion held from a login stays usable. web3.8 (R6):
   *  needs 0 holds the settled assertion for the key step and never registers. */
  private async signInPasskey(ceremonyId: string, response: string): Promise<{ needs: 0 | 1 }> {
    const enrol = this.requireEnrol();
    const back = this.phase;
    let needs: 0 | 1;
    try {
      ({ needs } = await enrol.passkey(fromHex(ceremonyId), response));
    } catch (e) {
      this.setAccount({ phase: back, error: errorOf(e) });
      throw e;
    }
    this.assertionAt = this.elapsed();
    if (needs === 1) {
      this.setAccount({ phase: 'signin-totp', signIn: signInOf(null, 'passkey'), error: null });
      return { needs };
    }
    this.setAccount({ phase: 'signin-key', signIn: signInOf(null, null), error: null });
    return { needs };
  }

  /** web3.6: a backup code in place of the app's code or a passkey. A refused code spends the assertion: the person logs
   *  in again, the username kept (as signInTotp). web3.8 (R6): the page holds the code and sends it only with the key,
   *  so it is never spent early. dillad checks the assertion before the code, so a 401 once the held assertion is past
   *  its 5 minutes spent no code: it is the expiry (E_SIGNIN_EXPIRED, pre-flight C-16). A 401 inside them is the
   *  refused code, which dillad answers the same way. */
  private async signInBackupCode(code: string, recoveryKey: string): Promise<null> {
    const enrol = this.requireEnrol();
    const username = this.accountState.signIn?.username ?? null;
    this.checkKey(recoveryKey, this.phase);
    const heldAt = this.assertionAt;
    try {
      await enrol.backupCode(backupCode(code));
    } catch (e) {
      if (isStatus(e, 401) && heldAt !== null && this.elapsed() - heldAt >= ASSERTION_EXPIRED_MS) throw this.expired();
      this.setAccount({ phase: 'signin-login', signIn: signInOf(username, null), error: errorOf(e) });
      throw e;
    }
    await this.enrolWithKey(recoveryKey);
    return null;
  }

  /** A refused code spends the assertion (head ruling 39): the person logs in again, the username kept. web3.8 (R6):
   *  an accepted code holds the fresh settled assertion for the key step and never registers. */
  private async signInTotp(code: string): Promise<null> {
    const enrol = this.requireEnrol();
    const username = this.accountState.signIn?.username ?? null;
    try {
      await enrol.totp(code);
    } catch (e) {
      this.setAccount({ phase: 'signin-login', signIn: signInOf(username, null), error: errorOf(e) });
      throw e;
    }
    this.assertionAt = this.elapsed();
    this.setAccount({ phase: 'signin-key', signIn: signInOf(username, null), error: null });
    return null;
  }

  /** Requirement 7 as REGISTRATION-DEVICES-02 amends it: the registration runs inside signInKey, after the key's
   *  form passed. A refused registration keeps the phase-3 enrol record (its device id is reused) and returns to
   *  the login step. web3.8 (pre-flight C-15): its 401 is the held assertion that ran out during key entry. */
  private async register(): Promise<Id> {
    const enrol = this.requireEnrol();
    try {
      return (await enrol.register(this.requireInstance().instanceId)).userId;
    } catch (e) {
      if (isStatus(e, 401)) throw this.expired();
      const username = this.accountState.signIn?.username ?? null;
      this.setAccount({ phase: 'signin-login', signIn: signInOf(username, null), error: errorOf(e) });
      throw e;
    }
  }

  /** enrol.fetch inside signInKey; a failure stays at the key step with the error. A 401 is a registered row the
   *  instance revoked (replaced by another registration): the enrolment is dropped and step 1 says so. */
  private async fetchForKey(): Promise<EnrolFetched> {
    const userId = this.requireCore().identity().userId;
    try {
      if (userId === null) throw new CoreError('E_CORE_STATE', 'no user recorded');
      this.fetched = await this.requireEnrol().fetch(userId);
      return this.fetched;
    } catch (e) {
      this.fetched = null;
      if (isStatus(e, 401) && this.requireCore().identity().phase === 3) throw this.evicted();
      this.setAccount({ phase: 'signin-key', error: errorOf(e) });
      throw e;
    }
  }

  /** REGISTRATION-DEVICES-02 before the core wrote the enrolment: drop it and return to step 1 with the reason. */
  private evicted(): Error {
    this.requireEnrol().reset();
    const username = this.accountState.signIn?.username ?? null;
    this.setAccount({ phase: 'signin-login', signIn: signInOf(username, null),
      error: { code: 'E_SIGNIN_EVICTED', detail: '', status: 0, retryAfterMs: null } });
    return new Error('E_SIGNIN_EVICTED');
  }

  /** web3.8 (R6, pre-flight C-15/C-16): the held assertion ran out before it was spent, and nothing was spent with it.
   *  Back to step 1 with the reason; a kept enrol record (its device id) is reused by the next registration. */
  private expired(): Error {
    const username = this.accountState.signIn?.username ?? null;
    this.setAccount({ phase: 'signin-login', signIn: signInOf(username, null),
      error: { code: 'E_SIGNIN_EXPIRED', detail: '', status: 0, retryAfterMs: null } });
    return new Error('E_SIGNIN_EXPIRED');
  }

  /** Requirement 8 as REGISTRATION-DEVICES-02 amends it, for an enrolment already registered (a wrong key, a reload
   *  mid-ceremony): the key's form, then the fetch, the enrolment and the list PUT back to back. The recovery key is
   *  passed to the core and to Enrol.complete and held in no field, slice, ret or error. */
  private async signInKey(recoveryKey: string): Promise<null> {
    this.checkKey(recoveryKey, 'signin-key');
    await this.enrolWithKey(recoveryKey);
    return null;
  }

  /** The registration (when the enrolment holds none yet), the fetch, the enrolment and the list PUT, back to back. */
  private async enrolWithKey(recoveryKey: string): Promise<void> {
    const enrol = this.requireEnrol();
    const core = this.requireCore();
    this.setAccount({ phase: 'enrolling', error: null });
    // Phase 0, or an enrolment whose registration was refused (its record and device id are kept and reused).
    const before = core.identity();
    if (before.phase === 0 || (before.phase === 3 && before.userId === null)) await this.register();
    let fetched = this.fetched ?? await this.fetchForKey();
    const username = this.accountState.signIn?.username ?? '';
    let retried = false;
    for (;;) {
      try {
        await enrol.complete(recoveryKey, fetched, username);
        break;
      } catch (raw) {
        let e: unknown = raw;
        if (core.identity().phase === 3) {
          // Pre-flight ruling (c): an older served list may be a list PUT racing this read; read again once.
          if (coreInput(e, LIST_CONFLICT) && !retried) {
            retried = true;
            fetched = await this.fetchForKey();
            continue;
          }
          if (coreInput(e, LIST_CONFLICT)) e = new Error('E_LIST_RACE');
          // Pre-flight ruling (e): a state object missing or unreadable past list version 1 is a missing backup.
          else if (coreInput(e, STATE_UNUSABLE)) { this.fetched = null; e = new Error('E_NO_BACKUP'); }
        }
        if (isCode(e, 'E_LIST_RACE') || isCode(e, 'E_SIGNIN_EVICTED')) {
          // The core is in phase 2 with an unlisted identity (or the instance keeps serving a conflicting
          // list, or revoked the row before its list PUT): nothing returns it to phase 0 in this worker (head
          // ruling 26).
          if (isCode(e, 'E_SIGNIN_EVICTED') && core.identity().phase === 3) throw this.evicted();
          await this.wipe({ code: (e as Error).message, detail: '', status: 0, retryAfterMs: null });
          throw e;
        }
        if (core.identity().phase === 3) {
          this.setAccount({ phase: 'signin-key', error: errorOf(e) });
          throw e;
        }
        // The core wrote the enrolment and a later step failed: entering ready publishes and upgrades.
        break;
      }
    }
    this.fetched = null;
    // A new device has published nothing: its first batch goes up before it is ready, as a signup's does.
    if (core.identity().phase === 2) this.keyPackagesOwed = true;
    try {
      await this.enterReady('signin');
    } catch (e) {
      if (!this.revoked()) this.setAccount({ phase: 'error', error: errorOf(e) });
      throw e;
    }
  }

  /** Wave B task 3, the code and key steps' Back: the held assertion (and anything registered with it) is dropped exactly
   *  as by signInCancel, so no half-signed-in state survives, and the ceremony returns to the login with the username
   *  kept; the password is asked again. Refused while a sign-in step runs (fix round 1, security review): that step's
   *  own ending would otherwise land after the Back and undo it. */
  private signInBack(): Promise<null> {
    if (this.signInRunning) return Promise.reject(new Refusal('E_NOT_READY', 'a sign-in step is running'));
    const username = this.slices.get('account')?.signIn?.username ?? null;
    this.requireEnrol().reset();
    this.fetched = null;
    this.setAccount({ phase: 'signin-login', signIn: signInOf(username, null), error: null });
    return Promise.resolve(null);
  }

  /** Requirement 9; Enrol.reset also drops a held assertion, so it runs in every sign-in phase (it resets
   *  the core's enrolment only in phase 3). The pending session is left to expire. Refused while a sign-in step runs
   *  (wave B task 3 fix round 1), as signInBack is. */
  private signInCancel(): Promise<null> {
    if (this.signInRunning) return Promise.reject(new Refusal('E_NOT_READY', 'a sign-in step is running'));
    this.requireEnrol().reset();
    this.fetched = null;
    this.setAccount({ phase: 'needs-signup', signIn: null, error: null });
    return Promise.resolve(null);
  }

  // ---- communities ----

  private listed(communityId: string): boolean {
    return (this.slices.get('communities') ?? []).some((c) => c.id === communityId);
  }

  private async refreshCommunities(): Promise<void> {
    // C4 fix round 1: list reads overlap (op 54, a join, a transfer); one that began before the last applied is older
    // than what the page shows, and applying it would drop, or bring back, a server falsely.
    const seq = ++this.listSeq;
    const rows = await this.routes.listCommunities();
    if (this.storeCleared) return;
    if (seq < this.listApplied) return;
    this.listApplied = seq;
    // The list before this read; absent before the first list after a boot, which tells nothing (web3.4 C4).
    const before = this.slices.get('communities');
    const list = rows.map((r) => ({ id: toHex(r.id), name: r.name, owner: toHex(r.owner) }));
    this.slices.set('communities', list);
    // C4 fix round 1: a server still listed with no leave or delete of it in flight was not left (a 5xx that did not go
    // through): its later removal is told.
    for (const id of this.admin.selfRemoved) if (this.listed(id) && !this.admin.pendingSelf.has(id)) this.admin.selfRemoved.delete(id);
    // Pre-flight ruling 1(vi): a community that is no longer listed loses its selection and its slices.
    const gone = new Set<string>();
    const removed = (before ?? []).filter((c) => !this.listed(c.id));
    for (const c of removed) gone.add(c.id);
    for (const known of this.channels.values()) if (!this.listed(known.communityId)) gone.add(known.communityId);
    if (this.selected !== null && !this.listed(this.selected)) gone.add(this.selected);
    for (const communityId of gone) this.dropCommunity(communityId);
    // web3.4 C4: a server that was listed and is gone, and that this account did not leave or delete itself, was taken
    // from it (a kick, a ban, a delete by its owner, a leave on another device): the page says so.
    const notices = removed.filter((c) => !this.admin.selfRemoved.has(c.id))
      .map((c): RemovalNotice => ({ id: ++this.removalSeq, communityId: c.id, name: c.name }));
    if (notices.length > 0) this.slices.set('removals', [...(this.slices.get('removals') ?? []), ...notices].slice(-REMOVALS_MAX));
  }

  /** C4 fix round 2: a local write of the community list (a leave, a delete, a rename, a create's fallback) counts as the
   *  newest read applied, so a read begun before it is not applied over it. */
  private listWritten(): void {
    this.listApplied = ++this.listSeq;
  }

  private dropCommunity(communityId: string): void {
    if (this.selected === communityId) this.selected = null;
    const dropped: string[] = [];
    for (const [channelId, known] of this.channels) {
      if (known.communityId !== communityId) continue;
      this.channels.delete(channelId);
      this.dropOpen(channelId);
      dropped.push(channelId);
    }
    this.slices.set(`channels:${communityId}`, []);
    this.slices.set(`members:${communityId}`, []);
    this.clearPresence(communityId);
    this.clearAFK(communityId);
    this.admin.forget(communityId);
    // web3.5 C4: a server joined again is sent its views again, once its member list and access are read again.
    this.membersRead.delete(communityId);
    for (const [group, entry] of this.authority) {
      if (entry.communityId !== communityId) continue;
      this.authority.delete(group);
      this.authorityStates.delete(group);
    }
    // C4 fix round 1: a call in one of its channels ends here at once (its removed phase, its group forgotten), and the
    // voice book forgets the channels, so a rejoin starts from the instance's state, not from what was seen before.
    this.safely(() => { this.calls.channelsDropped(dropped); });
    // web3.4 C4: no voice row of the server's channels stays on the page (visibleVoice stays as the guard).
    const voice = this.slices.get('voice');
    if (voice !== undefined) {
      const visible = this.visibleVoice(voice);
      if (Object.keys(visible).length !== Object.keys(voice).length) this.slices.set('voice', visible);
    }
    this.updateExpected();
    this.publishBadges();
  }

  /** The voice slice as the page may see it: only the calls of channels and DMs this worker knows (a dropped server's
   *  channels are not known, though the call controller may still read or hear of their calls until its removal lands). */
  private visibleVoice(state: VoiceState): VoiceState {
    const known = (channel: string): boolean => this.channels.has(channel) || this.dms.has(channel);
    if (Object.keys(state).every(known)) return state;
    return Object.fromEntries(Object.entries(state).filter(([channel]) => known(channel)));
  }

  private async joinCommunity(rawInvite: string): Promise<{ communityId: string }> {
    const invite = rawInvite.trim();
    const info = await this.routes.getInvite(invite);
    if (info.communityId === null) throw new Refusal('E_INVITE_NOT_COMMUNITY', '');
    return this.joinPath(info.communityId, invite);
  }

  private async joinPath(communityId: Id, invite: string): Promise<{ communityId: string }> {
    await this.routes.joinCommunity(communityId, invite);
    // web3.4 C4: a server left earlier and joined again is told of when it is later taken away.
    this.admin.selfRemoved.delete(toHex(communityId));
    // Final fix I3: a notice of that server's removal is stale once it is joined again.
    const notices = this.slices.get('removals');
    if (notices !== undefined && notices.some((r) => r.communityId === toHex(communityId))) {
      this.slices.set('removals', notices.filter((r) => r.communityId !== toHex(communityId)));
    }
    // A server joined again in this tab (after a kick, a lifted ban or a leave): a resync refused while the account was out
    // stood as not-member until the next ready, so the channels stayed unwritable until a reload; the engine resyncs now.
    this.sync?.rejoined(communityId);
    // Pre-flight ruling 1(vii): the account joined; a failed refresh of the list does not undo that.
    try { await this.refreshCommunities(); } catch { /* the next ready refreshes it */ }
    return { communityId: toHex(communityId) };
  }

  private async selectCommunity(communityId: string): Promise<null> {
    if (!this.listed(communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    this.selected = communityId;
    await this.loadCommunity(communityId);
    return null;
  }

  // ---- the create flow (v0.1.0-web3.1) ----

  /** POST /v1/communities. The new server is listed before the answer, also when the list read after the create fails
   *  (as a server this account owns), so the page can select it and add its starter channels at once. */
  private async createCommunity(communityName: string): Promise<{ communityId: string }> {
    const created = await this.routes.createCommunity(communityName);
    const communityId = toHex(created.id);
    // As joinPath: the server exists; a failed refresh of the list does not undo that.
    try { await this.refreshCommunities(); } catch { /* listed below; the next ready refreshes it */ }
    if (!this.storeCleared && !this.listed(communityId) && this.me !== null) {
      this.listWritten();
      this.slices.set('communities', [...(this.slices.get('communities') ?? []),
        { id: communityId, name: communityName, owner: toHex(this.me.userId) }]);
    }
    return { communityId };
  }

  /** POST /v1/communities/{id}/channels at the position after the server's last channel. The channel is published as
   *  created before the answer, then the server's lists are read again; a text channel's group is registered by the
   *  first device that opens it (sync/channel.ts), a voice channel's call group at its first call. */
  private async createChannel(communityId: string, kind: NewChannelKind | 'announcement', channelName: string, priv?: { roleIds: string[] },
    announce?: { roleIds: string[] }, parentId?: string): Promise<{ channelId: string }> {
    if (!this.listed(communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    // R42: the built-in AFK channel sits at 2^30; the next position is taken from the other channels. R-AFK6: positions at
    // or above that reservation never count, so a create before the `afk:` slice is read still lands above AFK.
    const afkId = this.slices.get(`afk:${communityId}`)?.channelId ?? null;
    const position = nextPosition([...this.channels].filter(([knownId, known]) => known.communityId === communityId && knownId !== afkId)
      .map(([, known]) => known.row.position));
    // A private channel is private from birth: the overwrites go in the create (wave 2a).
    // An announcement channel is a text channel whose @everyone may not send, read-only from birth in the same way.
    const routeKind: NewChannelKind = kind === 'announcement' ? 'text' : kind;
    const overwrites = kind === 'announcement' ? await this.rolesCtl.announcementOverwrites(communityId, announce?.roleIds ?? [])
      : priv === undefined ? undefined : await this.rolesCtl.privateOverwrites(communityId, priv.roleIds);
    const parent = parentId === undefined ? {} : { parentId: fromHex(parentId) };
    const created = overwrites === undefined ? await this.routes.createChannel(fromHex(communityId), { kind: routeKind, name: channelName, position, ...parent })
      : await this.rolesCtl.guarded(communityId, () => this.routes.createChannel(fromHex(communityId), { kind: routeKind, name: channelName, position, ...parent, overwrites }));
    // A list read that began before this create answers without the channel: loadCommunity reads again (structureRev).
    this.structureRev.set(communityId, (this.structureRev.get(communityId) ?? 0) + 1);
    const channelId = toHex(created.id);
    if (this.storeCleared || !this.listed(communityId)) return { channelId };
    this.channels.set(channelId, { communityId, row: {
      id: created.id, kind: kind === 'voice' ? 1 : 0, mode: created.mode, visibility: created.visibility, parentId: parentId === undefined ? null : fromHex(parentId),
      name: channelName, topic: '', position, slowmode: 0, seq: 0n, textGroupId: null,
    } });
    this.publishChannels(communityId);
    // Not awaited (WB I1): the answer does not wait on a list read that may hang; the row is already published.
    void this.barrier.track(this.loadCommunity(communityId).catch(() => { /* the row stays as created; the next ready reads the list */ }));
    if (overwrites !== undefined) await this.admin.loadAccess(communityId);   // the page sees the overwrites with the ret
    return { channelId };
  }

  /** Op 54 community_changed, and the unexpected-Welcome belt: read the server list (a renamed server's new name) and a
   *  listed server's lists again after a short wait, once for a burst. An unlisted id is ignored; a failed read is left
   *  to the next notice, focus or ready (a failed list read keeps the list and still reads the server's lists). */
  private scheduleCommunityReload(communityId: string): void {
    if (!this.listed(communityId) || this.communityReloads.has(communityId)) return;
    const handle = this.deps.setTimeout(() => {
      this.communityReloads.delete(communityId);
      if (this.phase !== 'ready' || !this.listed(communityId)) return;
      void this.barrier.track((async () => {
        try { await this.refreshCommunities(); } catch { /* the list stays as it was */ }
        if (this.phase !== 'ready' || !this.listed(communityId)) return;
        await this.loadCommunity(communityId);
      })().catch(() => undefined));
    }, COMMUNITY_RELOAD_DEBOUNCE_MS);
    this.communityReloads.set(communityId, handle);
  }

  /** POST /v1/communities/{id}/invites: the instance's link (shown and copied by the page), its code and its expiry. */
  private async createInvite(communityId: string, maxUses: number, ttlSeconds: number): Promise<InviteCreatedResult> {
    if (!this.listed(communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    const invite = await this.routes.createInvite(fromHex(communityId), { maxUses, ttlSeconds });
    return { url: invite.url, code: invite.code, expires: Number(invite.expires) };
  }

  /** The channel and member lists of one community; join-all then expects its groups (requirement 18). A read that a
   *  createChannel of the same community overtook is read again, so it never replaces the list with one that lacks the
   *  created channel. */
  private async loadCommunity(communityId: string): Promise<void> {
    const id = fromHex(communityId);
    let rows: Awaited<ReturnType<Routes['listChannels']>>;
    let members: MemberRow[] | null;
    for (;;) {
      const rev = this.structureRev.get(communityId) ?? 0;
      rows = await this.routes.listChannels(id);
      members = await this.readMembers(communityId);
      if ((this.structureRev.get(communityId) ?? 0) === rev) break;
    }
    if (this.storeCleared || !this.listed(communityId)) return;
    for (const [channelId, known] of this.channels) if (known.communityId === communityId) this.channels.delete(channelId);
    for (const row of rows) this.channels.set(toHex(row.id), { communityId, row });
    // web3.9 VOICE-AFK fix round 3 (N-3): a held snapshot with no AFK pointer (a failed lazy ensure) cannot say whether a
    // newly listed channel is the built-in AFK channel, so it is unknown (null) until this re-read lands, and the page
    // holds a join of it instead of reaching for the microphone. Set before the channels are published.
    const held = this.slices.get(`afk:${communityId}`);
    if (held != null && held.channelId === null) this.slices.set(`afk:${communityId}`, null);
    this.publishChannels(communityId);
    // C4 fix round 1: the role view is pushed once, after the access read below, not from the member list as well.
    if (members !== null) this.publishMembers(communityId, members, false);
    this.clearPresence(communityId);
    void this.barrier.track(this.loadPresence(communityId));
    // R-AFK3 (a): read again in place; the previous snapshot stands until the new one lands.
    void this.barrier.track(this.loadAFK(communityId));
    this.updateExpected();
    this.publishBadges();
    this.publishDms();                               // a DM's name comes from the member lists
    // The access reads are tracked by the barrier, not awaited: a select never waits on them, and a failed read leaves them absent.
    // Once it settles, read or not, the role view is pushed (nothing while it is unknown).
    void this.barrier.track(this.admin.loadAccess(communityId).then(() => { this.safely(() => { this.pushAuthority(communityId); }); }))
      .catch(() => undefined);
  }

  /** The member list of one server, read again (a kick, a ban, a transfer). */
  private async reloadMembers(communityId: string): Promise<void> {
    const rows = await this.readMembers(communityId);
    if (rows === null || this.storeCleared || !this.listed(communityId)) return;
    this.publishMembers(communityId, rows);
  }

  /** Reads a server's member list; null when a read of it begun later has been started meanwhile (C4 fix round 1): only
   *  the newest read is published, so a slow one never puts an older list, and its role view, back. */
  private async readMembers(communityId: string): Promise<MemberRow[] | null> {
    const seq = (this.memberSeq.get(communityId) ?? 0) + 1;
    this.memberSeq.set(communityId, seq);
    const profileAt = this.profileSeq;
    const rows = await this.routes.listMembers(fromHex(communityId));
    if (this.memberSeq.get(communityId) !== seq) return null;
    // A profile saved while this read was in flight is newer than what the read saw (N6): the own row keeps the saved text.
    const saved = this.profileSeq !== profileAt ? this.profileSaved : null;
    if (saved === null) return rows;
    return rows.map((m) => (toHex(m.userId) === saved.userId ? { ...m, ...savedOf(saved) } : m));
  }

  private publishMembers(communityId: string, rows: readonly MemberRow[], push = true): void {
    this.slices.set(`members:${communityId}`, rows.map((m) => ({
      userId: toHex(m.userId), username: m.username, display: m.display, kind: m.kind, roleIds: m.roleIds.map(toHex),
      ...(m.statusMsg ? { statusMsg: m.statusMsg } : {}), ...(m.bio ? { bio: m.bio } : {}),
    })));
    // Product P1: the trust slice names each contact and says whether a group is shared from these member lists; a list
    // that lands after it (every reload: the first ready refreshes trust before the communities load) republishes it.
    this.publishTrust();
    // Web-3 task 10, concern 4: a voice participant the lists lacked is named now.
    this.calls.membersChanged();
    this.rolesCtl.membersChanged(communityId);
    this.membersRead.add(communityId);
    this.setOwnRoles(communityId, rows);
    if (push) this.safely(() => { this.pushAuthority(communityId); });
  }

  /** Ruling 12: the core's mention rule reads the own roles of the community (L-CORE-35); a change reaches it once. */
  private setOwnRoles(communityId: string, rows: readonly MemberRow[]): void {
    const core = this.core;
    const me = this.me;
    if (core === null || me === null) return;
    const ownUser = toHex(me.userId);
    const own = rows.find((m) => toHex(m.userId) === ownUser);
    if (own === undefined) return;
    const hexes = own.roleIds.map(toHex);
    const applied = this.roles.get(communityId);
    if (applied !== undefined && applied.length === hexes.length && applied.every((h, i) => h === hexes[i])) return;
    try {
      core.ownRolesSet(fromHex(communityId), own.roleIds);
      this.roles.set(communityId, hexes);
    } catch { /* the next member list tries again */ }
  }

  /**
   * web3.5 C4: hands the core each of the server's text groups' role view (who may pin, who may mention everyone, which
   * roles anyone may mention), so it counts only their pins, broadcast mentions and role mentions. Runs after a member
   * list and after an access read land.
   *
   * Nothing is sent while the view is unknown: until this attach has read the server's member list, and its roles,
   * @everyone and overwrites (`loaded` and `overwritesLoaded`), the core keeps its old rule (every pin counts). The member
   * list is whole (listMembers walks every page), so a user absent from it is no member and allowed nothing. Never for a
   * DM group (it has no roles; the core refuses one). A view equal to the one last sent for the group is not sent again;
   * a throw leaves the entry as it was, so the next load tries again. After a change the group's channel rows, its open
   * timeline and pins and the badges are published again, once for all the groups that changed.
   *
   * `groups` is the core's group list when the caller already read it; `only` limits the work to one group (a rejoin).
   */
  private pushAuthority(communityId: string, known?: readonly GroupInfo[], only?: string): void {
    const core = this.core;
    if (core === null || !this.membersRead.has(communityId) || !this.accessRead.has(communityId)) return;
    const access = this.slices.get(`permissions:${communityId}`);
    const roles = this.slices.get(`roles:${communityId}`);
    const members = this.slices.get(`members:${communityId}`);
    const overwrites = this.slices.get(`overwrites:${communityId}`);
    const owner = (this.slices.get('communities') ?? []).find((c) => c.id === communityId)?.owner;
    if (access === undefined || !access.loaded || !access.overwritesLoaded || access.everyoneId === '') return;
    if (roles === undefined || !roles.some((r) => r.id === access.everyoneId) || members === undefined || overwrites === undefined) return;
    if (owner === undefined || owner === '') return;
    const groups = known ?? core.groups();
    const channels: { id: string; kind: number; mode: number; textGroupId: string }[] = [];
    for (const g of groups) {
      if (g.kind !== 0 || g.communityId === null || toHex(g.communityId) !== communityId) continue;
      if (only !== undefined && toHex(g.groupId) !== only) continue;
      const channelId = toHex(g.targetId);
      const channel = this.channels.get(channelId);
      if (channel === undefined || channel.communityId !== communityId) continue;
      channels.push({ id: channelId, kind: channel.row.kind, mode: channel.row.mode, textGroupId: toHex(g.groupId) });
    }
    if (channels.length === 0) return;
    const changed = new Set<string>();
    // W2a review finding 1: the server's roles anyone may mention (never @everyone's own row, whose token is <@everyone>);
    // a role token from a member without mention_everyone counts only for these.
    const mentionable = roles.filter((r) => r.position > 0 && r.mentionable).map((r) => r.id).sort();
    for (const [groupHex, view] of authorityFor({ owner, everyoneId: access.everyoneId, roles, members, overwrites, channels })) {
      const key = `${view.pinners.join(',')}|${view.broadcasters.join(',')}|${mentionable.join(',')}`;
      const channelId = channels.find((c) => c.textGroupId === groupHex)?.id;
      if (channelId !== undefined) this.authorityStates.set(groupHex, this.groupState(channelId, groups));
      if (this.authority.get(groupHex)?.key === key) continue;
      try {
        core.authoritySet(fromHex(groupHex), view.pinners.map(fromHex), view.broadcasters.map(fromHex), mentionable.map(fromHex));
      } catch { continue; }
      this.authority.set(groupHex, { communityId, key });
      changed.add(groupHex);
    }
    if (changed.size === 0) return;
    this.safely(() => { this.touchGroups(changed, groups); });
    this.safely(() => { this.publishBadges(); });
  }

  /** web3.5 C4 re-arm: a text group of a server that comes back to active (from joining, resync or not-member) is sent its
   *  view again, also when it equals the last one sent (a rejoin may have discarded the group's view in the core). */
  private authorityGroupState(groupId: Id, groups: readonly GroupInfo[]): void {
    if (this.core === null) return;
    const hex = toHex(groupId);
    const g = groups.find((x) => toHex(x.groupId) === hex);
    if (g === undefined || g.kind !== 0 || g.communityId === null) return;
    const channelId = toHex(g.targetId);
    const known = this.channels.get(channelId);
    if (known === undefined || known.communityId !== toHex(g.communityId)) return;
    const current = channelGroup(g.targetId, groups);
    if (current !== undefined && toHex(current.groupId) !== hex) return;   // an older group of the channel
    const state = this.groupState(channelId, groups);
    const before = this.authorityStates.get(hex);
    this.authorityStates.set(hex, state);
    if (state !== 'active' || before === 'active') return;
    this.authority.delete(hex);
    this.pushAuthority(known.communityId, groups, hex);
  }

  /** ChannelSummary.announcement: a text channel whose @everyone overwrite denies send_messages, as far as the overwrites
   *  the page has read tell (false while they are unknown). */
  private isAnnouncement(communityId: string, channelId: string): boolean {
    const everyoneId = this.slices.get(`permissions:${communityId}`)?.everyoneId ?? '';
    if (everyoneId === '' || this.channels.get(channelId)?.row.kind !== 0) return false;
    return (this.slices.get(`overwrites:${communityId}`)?.[channelId] ?? [])
      .some((o) => o.kind === 0 && o.targetId === everyoneId && (o.deny & PERM.sendMessages) !== 0);
  }

  private publishChannels(communityId: string): void {
    const groups = this.core?.groups() ?? [];
    const list: ChannelSummary[] = [];
    for (const [channelId, known] of this.channels) {
      if (known.communityId !== communityId) continue;
      const row = known.row;
      list.push({
        id: channelId, communityId, kind: row.kind as ChannelSummary['kind'], mode: row.mode as ChannelSummary['mode'],
        name: row.name, topic: row.topic, parentId: row.parentId === null ? null : toHex(row.parentId), position: row.position,
        slowmode: row.slowmode,
        announcement: this.isAnnouncement(communityId, channelId),
        group: channelGroupState(row, groups, this.membership(channelId)),
      });
    }
    this.slices.set(`channels:${communityId}`, list);
    this.stopTypingIfUnviewable();
  }

  /** Requirement 18: every known text channel first, then every DM whose group is known (communityId null). */
  private updateExpected(): void {
    const policyVersion = this.instance?.policyVersion ?? 0n;
    const expected: ExpectedGroup[] = [];
    for (const [channelId, { communityId, row }] of this.channels) {
      if (row.kind !== 0 || row.mode !== 0 || row.textGroupId === null) continue;
      expected.push({ groupId: row.textGroupId, communityId: fromHex(communityId), channelId: fromHex(channelId), policyVersion });
    }
    for (const [channelId, dm] of this.dms) {
      if (dm.groupId === null) continue;
      expected.push({ groupId: dm.groupId, communityId: null, channelId: fromHex(channelId), policyVersion });
    }
    this.sync?.setExpected(expected);
  }

  // ---- DMs (requirements 15-16) ----

  /** Coalesced: any number of requests during one load cause exactly one further load. */
  private reloadDms(): Promise<void> {
    if (this.dmsLoading !== null) { this.dmsAgain = true; return this.dmsLoading; }
    const run = (async () => {
      let failure: { error: unknown } | null = null;
      try {
        do {
          this.dmsAgain = false;
          try { await this.loadDms(); failure = null; }
          catch (e) { failure = { error: e }; }
        } while (this.dmsAgain);
      } finally {
        this.dmsLoading = null;
      }
      if (failure !== null) throw failure.error;
    })();
    this.dmsLoading = run;
    return run;
  }

  private async loadDms(): Promise<void> {
    const rows = await this.routes.listDms();
    if (this.storeCleared) return;
    const asked = new Map<string, Id | null>();
    for (const row of rows) {
      const id = toHex(row.channelId);
      if ((this.dms.get(id)?.groupId ?? null) !== null || asked.has(id)) continue;
      // A failed read leaves the group unknown; the next load asks again.
      try { asked.set(id, (await this.routes.getChannel(row.channelId)).textGroupId); }
      catch { asked.set(id, null); }
      if (this.storeCleared) return;
    }
    const next = new Map<string, KnownDm>();
    for (const row of rows) {
      const id = toHex(row.channelId);
      next.set(id, { kind: row.kind, members: row.members, groupId: this.dms.get(id)?.groupId ?? asked.get(id) ?? null });
    }
    for (const id of this.dms.keys()) {
      if (next.has(id)) continue;
      this.dropOpen(id);
      if (this.slices.get(`timeline:${id}`) !== undefined) {
        this.slices.set(`timeline:${id}`, { channelId: id, group: 'none', items: [], hasEarlier: false });
      }
    }
    this.dms.clear();
    for (const [id, dm] of next) this.dms.set(id, dm);
    this.publishDms();
    // Product P1: a DM is a shared group for its members (sharedUser), so the trust slice is republished with it.
    this.publishTrust();
    this.updateExpected();
    this.publishBadges();
  }

  /** A member's name from the first loaded member list that has them, else the first 8 hex characters. */
  private nameOf(userHex: string): string {
    for (const community of this.slices.get('communities') ?? []) {
      const member = this.slices.get(`members:${community.id}`)?.find((m) => m.userId === userHex);
      if (member !== undefined) return member.display !== '' ? member.display : member.username;
    }
    return userHex.slice(0, 8);
  }

  private publishDms(): void {
    if (this.core === null) return;
    const groups = this.core.groups();
    const own = this.me === null ? null : toHex(this.me.userId);
    const list: DmSummary[] = [];
    for (const [id, dm] of this.dms) {
      const members = dm.members.map(toHex);
      const others = members.filter((m) => m !== own);
      const name = others.length === 0
        ? this.accountState.user?.username ?? this.core.identity().username
        : others.map((m) => this.nameOf(m)).join(', ');
      list.push({ id, kind: dm.kind, members, name, group: channelGroupState({ id: fromHex(id), kind: 0, mode: 0 }, groups, this.membership(id)) });
    }
    this.slices.set('dms', list);
    this.stopTypingIfUnviewable();
  }

  private async openDm(userId: string): Promise<{ channelId: string }> {
    const { channelId } = await this.routes.postDm([fromHex(userId)]);
    await this.reloadDms();
    return { channelId: toHex(channelId) };
  }

  // ---- badges and notices (requirements 19-21) ----

  /** group hex → channel hex, for every text group bound to a known channel or DM. */
  private channelMap(groups: readonly GroupInfo[]): Map<string, string> {
    const channelOf = new Map<string, string>();
    for (const g of groups) {
      if (g.kind !== 0) continue;
      const target = toHex(g.targetId);
      if (this.channels.has(target) || this.dms.has(target)) channelOf.set(toHex(g.groupId), target);
    }
    return channelOf;
  }

  private publishBadges(): void {
    const core = this.core;
    if (core === null) return;
    const channelOf = this.channelMap(core.groups());
    const activity = core.activity();
    this.lastActivity = new Map(activity.map((row) => [toHex(row.groupId), row]));
    this.slices.set('badges', buildBadges(activity, channelOf));
  }

  /** Badges after every change; a notice for each new message from another user that arrived live (ruling (a)).
   *  Both slices are published synchronously, badges first, before the handler returns to the engine. */
  private reportActivity(groupId: Id, result: ApplyResult): void {
    const core = this.core;
    const me = this.me;
    const hex = toHex(groupId);
    const channelId = core === null ? undefined : this.channelMap(core.groups()).get(hex);
    if (core === null || me === null || result.newSeqs.length === 0 || channelId === undefined) { this.publishBadges(); return; }
    this.publishBadges();
    const floor = this.noticeFloor;
    if (floor === null) return;
    const ownUser = toHex(me.userId);
    const fresh = new Set(result.newSeqs.map((s) => s.toString()));
    const dm = this.dms.has(channelId);
    const communityId = dm ? null : this.channels.get(channelId)?.communityId ?? null;
    const drafts: NoticeDraft[] = [];
    for (const { row, threadRoot } of this.freshRows(core, groupId, fresh, floor)) {
      if (fresh.has(row.seq.toString()) && heldForTrust(row) && row.recvTs >= floor) { this.holdLive(groupId, row.seq); continue; }
      if (!this.noticeable(row, fresh, ownUser, floor)) continue;
      const senderName = this.nameOf(toHex(row.senderUser as Id));
      // FACTS-SECURITY-07: the core's flag, set once at apply on the row's original body; never the shown body, which may
      // hold another member's edit folded in the same apply (ruling 4, ruling 34).
      drafts.push(noticeOf({ row, channelId, communityId, dm, mention: row.mention, senderName, threadRoot }));
    }
    this.noticesState = appendNotices(this.noticesState, drafts);
    this.slices.set('notices', this.noticesState);
  }

  /** CI1: remembers a live row the core holds for its sender's trust, at most HELD_LIVE_MAX, the oldest dropped first. */
  private holdLive(groupId: Id, seq: bigint): void {
    const hex = toHex(groupId);
    if (this.heldLive.some((h) => h.seq === seq && toHex(h.groupId) === hex)) return;
    this.heldLive.push({ groupId, seq, heldAt: this.elapsed() });
    if (this.heldLive.length > HELD_LIVE_MAX) this.heldLive.splice(0, this.heldLive.length - HELD_LIVE_MAX);
  }

  /** CI1 (controller ruling on ruling (a)): a row that arrived live, was held for its sender's trust and is now readable
   *  raises the notice it would have raised on arrival. A row still held stays remembered; any other is forgotten. */
  private noticeReleased(): void {
    const core = this.core;
    const me = this.me;
    const floor = this.noticeFloor;
    if (this.heldLive.length === 0) return;
    if (core === null || me === null || floor === null) { this.heldLive = []; this.clearHeldReads(); return; }
    const channelOf = this.channelMap(core.groups());
    const ownUser = toHex(me.userId);
    const drafts: NoticeDraft[] = [];
    const kept: HeldLive[] = [];
    for (const held of this.heldLive) {
      const channelId = channelOf.get(toHex(held.groupId));
      if (channelId === undefined) continue;
      const found = this.rowAt(core, held.groupId, held.seq);
      if (found === null) continue;
      if (heldForTrust(found.row)) { kept.push(held); continue; }
      const seqs = new Set([held.seq.toString()]);
      if (!this.noticeable(found.row, seqs, ownUser, floor)) continue;
      const dm = this.dms.has(channelId);
      const communityId = dm ? null : this.channels.get(channelId)?.communityId ?? null;
      const senderName = this.nameOf(toHex(found.row.senderUser as Id));
      drafts.push(noticeOf({ row: found.row, channelId, communityId, dm, mention: found.row.mention, senderName, threadRoot: found.threadRoot }));
    }
    // Fix round 2 (C1): a group that lost a held row drops its held-back read and timer, and re-applies nothing. The
    // released row is unread, as a live arrival would be (R8); the release raises the count, so the page marks an open,
    // visible channel itself.
    const keptRows = new Set(kept.map((h) => `${toHex(h.groupId)}:${h.seq.toString()}`));
    for (const held of this.heldLive) {
      if (keptRows.has(`${toHex(held.groupId)}:${held.seq.toString()}`)) continue;
      this.dropHeldRead(toHex(held.groupId));
    }
    this.heldLive = kept;
    if (drafts.length === 0) return;
    this.noticesState = appendNotices(this.noticesState, drafts);
    this.slices.set('notices', this.noticesState);
  }

  /** The row at `seq`, from the main timeline or else from one of the latest NOTICE_READ_MAX threads, or null. */
  private rowAt(core: CorePort, groupId: Id, seq: bigint): { row: TimelineRow; threadRoot: string | null } | null {
    const main = core.timeline(groupId, seq + 1n, 1).find((row) => row.seq === seq);
    if (main !== undefined) return { row: main, threadRoot: null };
    for (const t of core.threads(groupId).slice(0, NOTICE_READ_MAX)) {
      const reply = core.threadTimeline(groupId, t.rootMsgId, seq + 1n, 1).rows.find((row) => row.seq === seq);
      if (reply !== undefined) return { row: reply, threadRoot: toHex(t.rootMsgId) };
    }
    return null;
  }

  /** The apply's new rows, oldest first, at most NOTICE_READ_MAX, each with the thread root it shows under (hex, or null).
   *  FX-core (final review I1): since TB1 the main timeline leaves out a reply whose root is held, so a new seq it does not
   *  hold is looked for in the threads, which the core lists latest reply first: a new reply's thread is at the top.
   *  Only threads with a reply since the notice floor are read, at most NOTICE_READ_MAX of them, and never more rows than
   *  the main read left to find, so at most NOTICE_READ_MAX rows in all. */
  private freshRows(core: CorePort, groupId: Id, fresh: ReadonlySet<string>, floor: bigint): { row: TimelineRow; threadRoot: string | null }[] {
    const limit = Math.min(fresh.size, NOTICE_READ_MAX);
    const found = core.timeline(groupId, 0n, limit).filter((row) => fresh.has(row.seq.toString()))
      .map((row) => ({ row, threadRoot: null as string | null }));
    const missing = (): number => limit - found.length;
    if (missing() > 0) {
      for (const t of core.threads(groupId).slice(0, NOTICE_READ_MAX)) {
        if (missing() <= 0 || t.lastTs < floor) break;
        const threadRoot = toHex(t.rootMsgId);
        for (const row of core.threadTimeline(groupId, t.rootMsgId, 0n, missing()).rows) {
          if (fresh.has(row.seq.toString())) found.push({ row, threadRoot });
        }
      }
    }
    return found.sort((a, b) => (a.row.seq < b.row.seq ? -1 : a.row.seq > b.row.seq ? 1 : 0));
  }

  /** A new, readable message from another user (on any device: head ruling 29), received live. */
  private noticeable(row: TimelineRow, fresh: ReadonlySet<string>, ownUser: string, floor: bigint): boolean {
    return fresh.has(row.seq.toString()) && row.status === 0 && row.type === 0
      && row.senderUser !== null && toHex(row.senderUser) !== ownUser && row.recvTs >= floor;
  }

  private markRead(channelId: string): Promise<null> {
    const core = this.requireCore();
    if (this.target(channelId) === undefined) throw new Refusal('E_BAD_INPUT', 'unknown channel');
    const g = this.timelineGroup(channelId, core.groups());
    if (g === null) return Promise.resolve(null);
    const row = core.groupRow(g);
    if (row === null || row.state === 4) return Promise.resolve(null);
    const rows = core.timeline(g, 0n, 1);
    if (rows.length === 0) return Promise.resolve(null);
    const newest = rows.reduce((max, r) => (r.seq > max ? r.seq : max), rows[0]?.seq ?? 0n);
    // web3.7: there is no read state per thread, so reading the channel marks its thread replies read too; they are not in
    // the main timeline, so the marker goes to the group's head (the core clamps it there as well).
    const head = row.nextSeq - 1n;
    this.applyRead(core, g, head > newest ? head : newest);
    return Promise.resolve(null);
  }

  private static readonly PRESENCE_CHOICES: readonly PresenceChoice[] = ['online', 'idle', 'dnd', 'invisible'];
  private readonly afkReadSeq = new Map<string, number>();

  /** web3.9 R-AFK3 (b): a failed AFK read is read again after 1 s, 2 s, 4 s … (at most 30 s) while the community stays listed. */
  private readonly afkRetry = new Map<string, { timer: number; attempt: number }>();
  /** AW4 fix round 1 (I1): per community, the op 55 changes received while its newest read is in flight. Each read owns
   *  its buffer; a newer read or clearAFK replaces or drops it, so a superseded read's frames are never replayed. */
  private readonly afkReadBuffer = new Map<string, { frames: { userId: string; since: number }[] }>();
  /** R-AFK4: the seq of the newest landed AFK read, across every community; it only rises. */
  private afkLandedSeq = 0;
  private static readonly AFK_RETRY_MAX_MS = 30_000;

  private stopAFKRetry(communityId: string): void {
    const retry = this.afkRetry.get(communityId);
    if (retry === undefined) return;
    this.deps.clearTimeout(retry.timer);
    this.afkRetry.delete(communityId);
  }

  /** Unloads the snapshot (offline, a left or wiped community): null, any read in flight ignored, the retry stopped. */
  private clearAFK(communityId: string): void {
    this.afkReadSeq.set(communityId, (this.afkReadSeq.get(communityId) ?? 0) + 1);
    this.stopAFKRetry(communityId);
    this.afkReadBuffer.delete(communityId);
    this.slices.set(`afk:${communityId}`, null);
  }

  /** AW4: op 55 patches the away roster of a landed snapshot in place; `seq` stays, so no move is announced. Fix round 1
   *  (I1): while a read is in flight the frame is also kept in that read's buffer, and replayed in order onto the read's
   *  roster when it lands, since the instance may have served the read before the change. With no read in flight and no
   *  snapshot (offline, unlisted) the frame is dropped: the next read is served after it. */
  private incomingAFKAway(frame: AFKAwayFrame): void {
    const communityId = toHex(frame.communityId);
    const change = { userId: toHex(frame.userId), since: frame.since };
    this.afkReadBuffer.get(communityId)?.frames.push(change);
    const key = `afk:${communityId}` as const;
    const held = this.slices.get(key);
    if (held == null) return;
    const next = awayPatched(held.away ?? [], change);
    if (next !== null) this.slices.set(key, { ...held, away: next });
  }

  /** AW5 fix round 2 (B-M1): the newest read of each community's snapshot while it is in flight; afkRead waits for it. */
  private readonly afkLoads = new Map<string, Promise<void>>();
  /** Starts a read of the snapshot (readAFK) and keeps it as the community's newest. It never rejects. */
  private loadAFK(communityId: string): Promise<void> {
    const load = this.readAFK(communityId);
    this.afkLoads.set(communityId, load);
    void load.finally(() => { if (this.afkLoads.get(communityId) === load) this.afkLoads.delete(communityId); });
    return load;
  }

  /** Reads the snapshot. R-AFK3 (a): the slice keeps its previous value until this read lands (a re-read never passes
   *  through null), so a voice join that needs it is never held by a re-read. */
  private async readAFK(communityId: string): Promise<void> {
    const seq = (this.afkReadSeq.get(communityId) ?? 0) + 1;
    this.afkReadSeq.set(communityId, seq);
    const buffer = { frames: [] as { userId: string; since: number }[] };
    this.afkReadBuffer.set(communityId, buffer);
    try {
      const row = await this.routes.getAFK(fromHex(communityId));
      if (!this.listed(communityId) || this.afkReadSeq.get(communityId) !== seq) return;
      this.stopAFKRetry(communityId);
      // Fix round 1 (I1): the op 55 frames received while this read was in flight, replayed in order.
      let away = row.away.map((r) => ({ userId: toHex(r.userId), since: r.since })).sort(byUserId);
      for (const change of buffer.frames) away = awayPatched(away, change) ?? away;
      // R-AFK4: every landed read gets the next seq, so a read that says pending again (a second sweep) is a new move.
      this.slices.set(`afk:${communityId}`, { channelId: row.channelId === null ? null : toHex(row.channelId),
        timeoutSeconds: row.timeoutSeconds, pending: row.pending, pendingReason: row.reason ?? (row.pending ? 1 : 0),
        seq: ++this.afkLandedSeq, away });
    } catch {
      // A newer read, an unload or a cleared store owns the snapshot now.
      if (this.storeCleared || !this.listed(communityId) || this.afkReadSeq.get(communityId) !== seq) return;
      const previous = this.afkRetry.get(communityId);
      if (previous !== undefined) this.deps.clearTimeout(previous.timer);
      const attempt = previous?.attempt ?? 0;
      const delay = Math.min(1_000 * 2 ** attempt, Controller.AFK_RETRY_MAX_MS);
      const timer: number = this.deps.setTimeout(() => {
        if (this.afkRetry.get(communityId)?.timer !== timer || this.storeCleared || !this.listed(communityId)) return;
        void this.barrier.track(this.loadAFK(communityId));
      }, delay);
      this.afkRetry.set(communityId, { timer, attempt: attempt + 1 });
    } finally {
      if (this.afkReadBuffer.get(communityId) === buffer) this.afkReadBuffer.delete(communityId);
    }
  }

  /** AW5 fix round 1 (I1): a fresh read of the community's AFK snapshot, answered once one landed after this call; the
   *  page's restore and recovery act only on such a read, never on a slice left from before a sleep or a reload.
   *  Fix round 2 (B-M1): a newer read of the same community that supersedes this one's (a reload, setAFK, another
   *  afkRead) is waited for and answers for it; E_UNAVAILABLE only when no read landed (a failed read, offline) or
   *  `timeoutMs` (the page's own wait; at most 60 s) passed first. */
  private async afkRead(communityId: string, timeoutMs: number | undefined): Promise<null> {
    if (!this.listed(communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    if (timeoutMs !== undefined && !(Number.isInteger(timeoutMs) && timeoutMs > 0 && timeoutMs <= 60_000)) {
      throw new Refusal('E_BAD_INPUT', 'invalid timeout');
    }
    const before = this.afkLandedSeq;
    const settled = (async () => {
      let load = this.loadAFK(communityId);
      for (;;) {
        await load;
        const newer = this.afkLoads.get(communityId);
        if (newer === undefined || newer === load) return;
        load = newer;
      }
    })();
    let timer: number | undefined;
    const inTime = timeoutMs === undefined ? settled.then(() => true)
      : Promise.race([settled.then(() => true), new Promise<boolean>((r) => { timer = this.deps.setTimeout(() => r(false), timeoutMs); })]);
    const landed = await inTime;
    if (timer !== undefined) this.deps.clearTimeout(timer);
    const now = this.slices.get(`afk:${communityId}`);
    if (!landed || now == null || (now.seq ?? 0) <= before) throw new Refusal('E_UNAVAILABLE', 'the AFK state could not be read');
    return null;
  }
  /** AW4 fix round 1 (M1): an unlisted community is refused here, as setAFK refuses it; the instance's 403 or 404 reaches
   *  the page unchanged. */
  private async afkAway(communityId: string): Promise<null> {
    if (!this.listed(communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    await this.routes.putAFKAway(fromHex(communityId));
    return null;
  }
  private async setAFK(communityId: string, timeoutSeconds: number): Promise<null> {
    if (!this.listed(communityId)) throw new Refusal('E_BAD_INPUT', 'unknown community');
    if (![60, 300, 900, 1800, 3600].includes(timeoutSeconds)) throw new Refusal('E_BAD_INPUT', 'invalid AFK timeout');
    await this.routes.setAFKSettings(fromHex(communityId), timeoutSeconds);
    await this.loadAFK(communityId);
    return null;
  }
  private static readonly PRESENCE_STATUSES: readonly PresenceStatus[] = ['offline', 'online', 'idle', 'dnd'];
  private presenceSeq = new Map<string, number>();
  private presenceLoads = new Map<string, { seq: number; updates: Record<string, PresenceStatus>; promise: Promise<void> }>();
  private presenceChoiceReadSeq = 0;
  private presenceChoiceWriteSeq = 0;
  private presenceChoiceAppliedRead = 0;

  private clearPresence(communityId: string): void {
    this.presenceSeq.set(communityId, (this.presenceSeq.get(communityId) ?? 0) + 1);
    this.presenceLoads.delete(communityId);
    this.slices.set(`presence:${communityId}`, null);
  }

  /** Bumped when a profile save lands; a read that began before it does not undo it (N6). */
  private profileSeq = 0;
  /** What the person saved this session (the trimmed text), per field, for the reads that were already in flight. */
  private profileSaved: { userId: string; display?: string; statusMsg?: string; bio?: string } | null = null;
  /** The account's display name as last read, so the user entry written at ready carries it before the next read. */
  private selfDisplay: { userId: string; display: string } | null = null;
  /** The cached display name, only for the user it was read for; another account never inherits it. */
  private knownDisplay(userId: string): { display?: string } {
    return this.selfDisplay !== null && this.selfDisplay.userId === userId ? { display: this.selfDisplay.display } : {};
  }

  private async refreshPresenceChoice(): Promise<void> {
    const seq = ++this.presenceChoiceReadSeq;
    const write = this.presenceChoiceWriteSeq;
    const profileAt = this.profileSeq;
    try {
      const me = await this.routes.getAccountMe();
      if (this.revoked() || this.presenceChoiceReadSeq !== seq || this.presenceChoiceWriteSeq !== write) return;
      const choice = Controller.PRESENCE_CHOICES[(me.presenceChoice ?? 1) - 1] ?? 'online';
      this.presenceChoiceAppliedRead = seq;
      // Only the current user's own read counts: a read for another account (a switch while it was in flight) is not applied.
      const user = this.accountState.user;
      const mine = user !== null && toHex(me.userId) === user.id;
      // A profile saved while this read was in flight is newer than what the read saw (N6): keep the saved text.
      const saved = this.profileSeq !== profileAt && mine && this.profileSaved?.userId === user.id ? this.profileSaved : null;
      const seen = saved === null ? me : { display: saved.display ?? me.display, statusMsg: saved.statusMsg ?? me.statusMsg, bio: saved.bio ?? me.bio };
      if (mine) this.selfDisplay = { userId: user.id, display: seen.display };
      this.setAccount({ presenceChoice: choice, ...(mine ? { user: { ...profileOf(user, seen) } } : {}) });
    } catch { /* the next ready retries */ }
  }

  /** EOD E6: saves the given profile fields (a field left out is left alone, '' clears one), then shows the saved text on the
   *  account and on the own row of every loaded member list, with no read: the server stored what was sent, trimmed. Other
   *  people's edits trigger a member-list re-read through community_changed. */
  private async updateProfile(command: { display?: string; statusMsg?: string; bio?: string }): Promise<null> {
    const saved = {
      ...(command.display === undefined ? {} : { display: profileTextValue(command.display) }),
      ...(command.statusMsg === undefined ? {} : { statusMsg: profileTextValue(command.statusMsg) }),
      ...(command.bio === undefined ? {} : { bio: profileTextValue(command.bio, true) }),
    };
    if (Object.keys(saved).length === 0) return null;
    await this.routes.patchMe(saved);
    const user = this.accountState.user;
    if (user === null) return null;
    this.profileSaved = { ...(this.profileSaved?.userId === user.id ? this.profileSaved : {}), ...saved, userId: user.id };
    this.profileSeq++;
    const apply = <T extends { display?: string; statusMsg?: string; bio?: string }>(row: T): T => {
      const next = { ...row, ...saved };
      if (next.statusMsg === '') delete next.statusMsg;
      if (next.bio === '') delete next.bio;
      return next;
    };
    if (saved.display !== undefined) this.selfDisplay = { userId: user.id, display: saved.display };
    this.setAccount({ user: apply(user) });
    for (const community of this.slices.get('communities') ?? []) {
      const key = `members:${community.id}` as const;
      const rows = this.slices.get(key);
      if (rows == null) continue;
      this.slices.set(key, rows.map((m) => (m.userId === user.id ? apply(m) : m)));
    }
    return null;
  }

  private async setPresenceChoice(choice: PresenceChoice): Promise<null> {
    const write = ++this.presenceChoiceWriteSeq;
    ++this.presenceChoiceReadSeq;
    const appliedRead = this.presenceChoiceAppliedRead;
    await this.routes.setPresenceChoice(Controller.PRESENCE_CHOICES.indexOf(choice) + 1);
    if (this.presenceChoiceWriteSeq === write && this.presenceChoiceAppliedRead === appliedRead) this.setAccount({ presenceChoice: choice });
    return null;
  }

  private loadPresence(communityId: string): Promise<void> {
    const seq = this.presenceSeq.get(communityId) ?? 0;
    const current = this.presenceLoads.get(communityId);
    if (current?.seq === seq) return current.promise;
    const pending = { seq, updates: {} as Record<string, PresenceStatus>, promise: Promise.resolve() };
    const work = (async () => {
      let failed = false;
      try {
        const rows = await this.routes.getPresenceSnapshot(fromHex(communityId));
        if (!this.listed(communityId) || this.connectionState.status !== 'online' || this.presenceSeq.get(communityId) !== seq) return;
        const statuses: Record<string, PresenceStatus> = {};
        for (const row of rows) statuses[toHex(row.userId)] = Controller.PRESENCE_STATUSES[row.status] ?? 'offline';
        this.slices.set(`presence:${communityId}`, { ...statuses, ...pending.updates });
      } catch { failed = true; }
      finally {
        if (this.presenceLoads.get(communityId) === pending) this.presenceLoads.delete(communityId);
        if (failed) this.deps.setTimeout(() => {
          if (this.listed(communityId) && this.connectionState.status === 'online' && this.presenceSeq.get(communityId) === seq
            && this.slices.get(`presence:${communityId}`) === null) void this.barrier.track(this.loadPresence(communityId));
        }, 10_000);
      }
    })();
    pending.promise = work;
    this.presenceLoads.set(communityId, pending);
    return work;
  }

  private incomingPresence(payload: readonly unknown[]): void {
    const [rawUser, rawStatus] = payload;
    if (!(rawUser instanceof Uint8Array) || rawUser.length !== 16 || typeof rawStatus !== 'bigint' || rawStatus < 0n || rawStatus > 3n) return;
    const user = toHex(rawUser);
    const status = Controller.PRESENCE_STATUSES[Number(rawStatus)];
    for (const community of this.slices.get('communities') ?? []) {
      if (!this.slices.get(`members:${community.id}`)?.some(m => m.userId === user)) continue;
      const current = this.slices.get(`presence:${community.id}`);
      if (current === undefined || current === null) {
        void this.barrier.track(this.loadPresence(community.id));
        const pending = this.presenceLoads.get(community.id);
        if (pending !== undefined) pending.updates[user] = status;
      }
      else this.slices.set(`presence:${community.id}`, { ...current, [user]: status });
    }
    if (this.accountState.user?.id === user) void this.barrier.track(this.refreshPresenceChoice());
  }

  /** web3.8 R8: marks `target` read, but stops one below the oldest row of this group held live less than
   *  HELD_READ_HOLD_MS ago, so a row released later is unread, as a live arrival would be. A row held longer no longer
   *  holds it. Fix I2: the page sends markRead only when the unread count changes, and a held row counts as nothing, so
   *  a held-back read is remembered (one entry and at most one timer per group) and applied again, never past `target`,
   *  when the oldest young held row it stopped at reaches HELD_READ_HOLD_MS while still held. A release drops it
   *  instead (fix round 2, C1: noticeReleased), so the released row stays unread. */
  private applyRead(core: CorePort, g: Id, target: bigint): void {
    const hex = toHex(g);
    const now = this.elapsed();
    let marker = target;
    let wait: number | null = null;
    for (const held of this.heldLive) {
      if (toHex(held.groupId) !== hex || held.seq > target) continue;
      const age = now - held.heldAt;
      if (age >= HELD_READ_HOLD_MS) continue;
      if (held.seq - 1n < marker) marker = held.seq - 1n;
      if (wait === null || HELD_READ_HOLD_MS - age < wait) wait = HELD_READ_HOLD_MS - age;
    }
    const before = this.heldReads.get(hex);
    if (before !== undefined) { this.deps.clearTimeout(before.timer); this.heldReads.delete(hex); }
    core.markRead(g, marker, this.nowS());
    if (marker < target && wait !== null) {
      const timer = this.deps.setTimeout(() => { this.reapplyRead(hex); }, wait);
      this.heldReads.set(hex, { groupId: g, target, timer });
    }
    this.publishBadges();
  }

  /** Fix I2: the age-out timer applies a remembered held-back read again, only while a row of that group at or below its
   *  target is still held live; a group gone or left, or one with no such row, drops it. */
  private reapplyRead(hex: string): void {
    const read = this.heldReads.get(hex);
    if (read === undefined) return;
    this.heldReads.delete(hex);
    const core = this.core;
    if (core === null) return;
    if (!this.heldLive.some((h) => toHex(h.groupId) === hex && h.seq <= read.target)) return;
    try {
      const row = core.groupRow(read.groupId);
      if (row === null || row.state === 4) return;
      this.applyRead(core, read.groupId, read.target);
    } catch { /* the page's next markRead marks it */ }
  }

  /** Fix round 2 (C1): drops one group's held-back read and its timer. */
  private dropHeldRead(hex: string): void {
    const read = this.heldReads.get(hex);
    if (read === undefined) return;
    this.deps.clearTimeout(read.timer);
    this.heldReads.delete(hex);
  }

  /** Fix I2: drops every remembered held-back read and its timer (a reset, a sign-out, a revocation). */
  private clearHeldReads(): void {
    for (const read of this.heldReads.values()) this.deps.clearTimeout(read.timer);
    this.heldReads.clear();
  }

  // ---- settings (requirement 27) ----

  private setSetting(key: string, value: string | null): Promise<null> {
    if (!isSettingKey(key)) throw new Refusal('E_SETTING_KEY', '');
    const core = this.requireCore();
    if (value === null) core.settingDelete(key);
    else {
      if (!isSettingValue(key, value)) throw new Refusal('E_BAD_INPUT', `value is not allowed for ${key}`);
      core.settingPut(key, value);
    }
    this.slices.set('settings', pageSettings(core.settings()));
    return Promise.resolve(null);
  }

  // ---- devices (requirements 23-26) ----

  /** Requirement 25: the own list first, so a just-enrolled device shows as listed. */
  private devicesReadSeq = 0;
  private async refreshDevices(refreshed?: { listed: boolean }): Promise<null> {
    const core = this.requireCore();
    const me = this.requireMe();
    const own = refreshed ?? await this.parts.refreshOwnDeviceList(core, this.routes, me.userId);
    if (!own.listed) { this.enterRevoked(); return null; }
    // A read that began before a newer one must not publish over it (a ready-path read racing a revoke's).
    const seq = ++this.devicesReadSeq;
    const rows = await this.routes.listDevices();
    if (seq !== this.devicesReadSeq) return null;
    const entries = core.ownDeviceList().entries;
    const ownHex = toHex(me.deviceId);
    const devices: DeviceSummary[] = rows.map((r) => {
      const id = toHex(r.id);
      return {
        id, tier: r.tier, signerTier: r.signerTier, lastSeen: Number(r.lastSeen), label: r.label,
        revokedAt: r.revokedAt === null ? null : Number(r.revokedAt),
        listed: entries.some((e) => toHex(e.deviceId) === id && e.revokedAt === null),
        own: id === ownHex,
      };
    });
    devices.sort((a, b) => (a.own !== b.own ? (a.own ? -1 : 1)
      : a.lastSeen !== b.lastSeen ? b.lastSeen - a.lastSeen : a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
    this.slices.set('devices', devices);
    this.deviceCount = devices.filter((d) => d.revokedAt === null).length;
    this.publishDiagnostics();
    return null;
  }

  /** The shared part of a revocation (requirement 24): read the root, the state object and the list, and sign the
   *  revoking list and the re-sealed state object. The caller writes them, in its own order (BACKUPS-RECOVERY-01).
   *  An older served list is refreshed and the reads retried once (pre-flight ruling (c)); a second is the list
   *  conflict. */
  private async revoke(ids: Id[], recoveryKey: string): Promise<SignedLists & { served: Uint8Array | null }> {
    const core = this.requireCore();
    const me = this.requireMe();
    for (let attempt = 0; ; attempt += 1) {
      const root = await this.routes.getBackup(0);
      if (root === null) throw new Error('E_NO_BACKUP');
      // A missing state object is not a refusal here (head ruling 28): the re-sealed one repairs it.
      const state = await this.routes.getBackup(1);
      const list = await this.routes.getDeviceList(me.userId);
      if (list === null) throw new Error('E_NO_BACKUP');
      const now = this.nowS();
      const sign = (stateSealed: Uint8Array): SignedLists =>
        core.deviceListRevoke({ recoveryKey, rootSealed: root.object, stateSealed, listBody: chainRow(list.raw), deviceIds: ids, now });
      let signed: SignedLists;
      try {
        try {
          signed = sign(state?.object ?? new Uint8Array(0));
        } catch (e) {
          // The core accepts a missing or unopenable served state only at list version 1 (core-block security
          // ruling 1): past it, a session that deleted or spoiled the object would block every revocation with
          // the key. This device's own sealed state is authentic, and its list is not newer than the list this
          // device stores, so the floor still holds; the re-sealed object then repairs the instance's copy.
          if (!coreInput(e, STATE_UNUSABLE)) throw e;
          const own = core.sealedObjects().state;
          if (own === null) throw new Error('E_NO_BACKUP');
          try { signed = sign(own); }
          catch (again) { throw coreInput(again, STATE_UNUSABLE) ? new Error('E_NO_BACKUP') : again; }
        }
      } catch (e) {
        if (!coreInput(e, LIST_CONFLICT)) throw e;
        if (attempt > 0) throw new Error('E_LIST_RACE');
        try { await this.parts.refreshOwnDeviceList(core, this.routes, me.userId); } catch { /* the reads below decide */ }
        continue;
      }
      // The state object the instance held before this revocation: a failed sign-out puts it back.
      return { ...signed, served: state?.object ?? null };
    }
  }

  /** BACKUPS-RECOVERY-02: the list another device signed and could not publish, which the core found in the state
   *  object and signed on, goes to the instance first. A 409 is a list there already (published meanwhile, or a
   *  fork the next PUT meets); any other failure drops the new candidate back to it, so a later ready publishes it. */
  private async publishInterrupted(signed: SignedLists): Promise<void> {
    if (signed.interrupted === null) return;
    try {
      await this.routes.putDeviceList(this.requireMe().userId, signed.interrupted);
    } catch (e) {
      if (isStatus(e, 409)) return;
      this.requireCore().deviceListDrop();
      throw e;
    }
  }

  private async listRace(): Promise<never> {
    // The core drops its candidate when the newer list is adopted.
    try { await this.parts.refreshOwnDeviceList(this.requireCore(), this.routes, this.requireMe().userId); }
    catch { /* the next ready refreshes it */ }
    throw new Error('E_LIST_RACE');
  }

  private async revokeDevice(deviceId: string, recoveryKey: string | null): Promise<null> {
    const core = this.requireCore();
    const me = this.requireMe();
    if (deviceId === toHex(me.deviceId)) throw new Refusal('E_BAD_INPUT', 'use signOutRevoke for this browser');
    // Pre-flight ruling (b): decide listed or unlisted from a fresh own list, never a stale one; the server's
    // 409 for a listed device on the key-less DELETE is the backstop and crosses unchanged.
    const own = await this.parts.refreshOwnDeviceList(core, this.routes, me.userId);
    if (!own.listed) { this.enterRevoked(); return null; }
    const listed = core.ownDeviceList().entries.some((e) => toHex(e.deviceId) === deviceId && e.revokedAt === null);
    if (!listed) {
      // Head ruling 30: a row no signed list names needs no list change and no key.
      await this.routes.deleteDevice(fromHex(deviceId));
      await this.refreshDevices();
      return null;
    }
    if (recoveryKey === null || recoveryKey.trim() === '') throw new Refusal('E_BAD_INPUT', 'the recovery key is empty');
    const signed = await this.revoke([fromHex(deviceId)], recoveryKey);
    await this.publishInterrupted(signed);
    // BACKUPS-RECOVERY-01: the revoking list first. The state object's PUT shares the user's upload meter and quota
    // with every session of the account, the stolen one included, so it must not be able to hold the list back.
    try {
      await this.routes.putDeviceList(me.userId, signed.deviceListBody);
    } catch (e) {
      if (isStatus(e, 409)) return this.listRace();
      throw e;
    }
    core.deviceListPublished();
    // A refused state object stays unmarked (state_uploaded 0): ensureBackups uploads it at the next ready, and
    // until then the instance holds a state behind the list, which the rollback floor accepts.
    try {
      await this.routes.putBackup(1, signed.stateSealed);
      core.stateSealedUploaded();
    } catch { /* the next ready uploads it */ }
    await this.refreshDevices();
    return null;
  }

  /** Names (or, with null, unnames) one of the account's devices. The name is not in the signed device list, so no key
   *  and no list change: the server stores it, and the list is read again so the slice shows what it holds. */
  private async renameDevice(deviceId: string, label: string | null): Promise<null> {
    this.requireCore();
    await this.routes.patchDevice(fromHex(deviceId), label === null ? null : deviceLabel(label));
    await this.refreshDevices();
    return null;
  }

  private async signOutRevoke(recoveryKey: string): Promise<null> {
    await this.calls.shutdown();
    const me = this.requireMe();
    const core = this.requireCore();
    const signed = await this.revoke([me.deviceId], recoveryKey);
    // An interrupted publication first, then the state object (architect ruling 17: before the list, so the
    // self-revoking PUT is the last request this device makes); a refusal of either sends nothing more and drops
    // the self-revoking candidate, so no later ready publishes it without the wipe the person asked for
    // (BACKUPS-RECOVERY-02).
    await this.publishInterrupted(signed);
    try {
      await this.routes.putBackup(1, signed.stateSealed);
    } catch (e) {
      core.deviceListDrop();
      throw e;
    }
    // The socket and the engine stop before the self-revoking PUT (head ruling 27).
    this.quiesce();
    try {
      await this.routes.putDeviceList(me.userId, signed.deviceListBody);
    } catch (e) {
      if (isStatus(e, 409)) { this.resume(); return this.listRace(); }
      // Fix-wave review NEW-2: the drop restores this device's state from before the sign-out, and the instance's
      // copy is put back too. Its self-revoking state object is the served list's successor, so the next recovery or
      // revocation (this browser's own included) would publish it as an interrupted publication.
      core.deviceListDrop();
      const previous = signed.served ?? core.sealedObjects().state;
      if (previous !== null) {
        try { await this.routes.putBackup(1, previous); }
        catch { /* the repair at the next ready re-uploads this browser's state when it carries the newest list */ }
      }
      this.resume();
      throw e;
    }
    await this.wipe(null);
    return null;
  }

  private async forgetBrowser(): Promise<null> {
    // L-TS-56 step 9: a wipe leaves the call first, while the session still works.
    await this.calls.shutdown();
    const core = this.requireCore();
    const identity = core.identity();
    const deviceId = this.me?.deviceId ?? identity.deviceId;
    this.quiesce();
    // BACKUPS-RECOVERY-02: a candidate this device signed and did not publish may already have its state object at
    // the instance; it is published before the store goes. A failure leaves it to the wipe, and the next device that
    // recovers finds it as an interrupted publication.
    if (identity.phase === 2 && !identity.listPublished) {
      try { await this.parts.publishDeviceList(core, this.routes); } catch { /* the wipe drops it */ }
    }
    if (deviceId !== null) {
      try {
        await this.routes.deleteSessions(deviceId);
      } catch (e) {
        // A 401: the sessions are gone already.
        if (!isStatus(e, 401)) { this.resume(); throw e; }
      }
    }
    await this.wipe(null);
    return null;
  }

  /** Stops the engine and the gateway before a server call that ends this device's sessions (head ruling 27). */
  private quiesce(): void {
    if (this.wiping) return;
    this.wiping = true;
    this.clearReauthTimer();
    this.clearPendingRolePanelActionRetries();
    this.typingOff();
    this.sync?.stop();
    this.gateway?.stop();
  }

  /** A refused server call: a fresh engine (a stopped one cannot restart), its expected set, then the socket. */
  private resume(): void {
    this.wiping = false;
    if (this.phase !== 'ready') return;               // a revoked browser had nothing running
    this.startEngine();
    this.updateExpected();
    this.gateway?.start();
  }

  /** Requirement 23: the store is erased and the worker stays inert until the page reloads (phase cleared). */
  private async wipe(error: WorkerError | null): Promise<void> {
    this.quiesce();
    const instance = this.requireInstance();
    try {
      this.core?.pause();                             // releases the OPFS handles; close() would not
      await this.deps.resetDevice(instance);
    } catch (e) {
      this.setAccount({ phase: 'error', error: errorOf(e) });
      throw e;
    }
    this.forgetAccount();
    this.storeCleared = true;
    this.setAccount({ phase: 'cleared', user: null, deviceId: null, recoveryKey: null, signIn: null, error, rootMismatch: false, ownTrust: null,
      factorSetup: null });
  }

  /**
   * Drops everything this worker holds about the store's account and empties every slice the page shows from it: the
   * core and its session parts, the socket and engine, the trust annotations, channels, DMs, timelines, notices, search,
   * typing and trays. A wipe ends with it; so does a release, after which another tab may sign out, sign in as someone
   * else or reset the store before this one leads again, so the next lead rebuilds from the store alone.
   */
  private forgetAccount(): void {
    this.clearInteractionAckLeases();
    this.selfDisplay = null;
    this.clearReauthTimer();
    for (const handle of this.communityReloads.values()) this.deps.clearTimeout(handle);
    this.communityReloads.clear();
    const communities = new Set<string>((this.slices.get('communities') ?? []).map((c) => c.id));
    for (const known of this.channels.values()) communities.add(known.communityId);
    const timelines = new Set([...this.open.keys(), ...this.pinsOpen, ...this.incomingTyping.keys(), ...this.typingOn]);
    this.unlistedUsers.clear(); this.changedLeafGroups = new Map(); this.removedGroups.clear();
    this.historyFailedUsers.clear(); this.pendingUsers = new Set(); this.ownRefusedLeaves.clear();
    this.fetchedLeaves.clear(); this.trustRefreshAgain = false;
    this.core = null; this.seenTrustGeneration = null; this.session = null; this.signup = null; this.enrol = null; this.gateway = null; this.sync = null;
    this.me = null; this.selected = null; this.fetched = null; this.noticeFloor = null; this.heldLive = []; this.clearHeldReads(); this.ticketTime = null;
    this.pendingJoin = null; this.keyPackagesOwed = false;
    this.clearKeyPackageRetry();
    this.keyPackages = { count: null, lastAttempt: null, lastError: null }; this.keyPackagesDelayMs = 0; this.keyPackagesRecheck = false; this.keyPackagesBusy = null;
    this.channels.clear(); this.dms.clear(); this.open.clear(); this.opening.clear(); this.refusedChannels.clear();
    // CB2 fix round 1: the next account in this tab must not see this one's wait.
    for (const channelId of this.slowUntil.keys()) this.slices.set(`slow:${channelId}`, { until: 0, queued: false });
    this.slowUntil.clear();
    this.notMember.clear(); this.resyncing.clear(); this.lookedUp.clear(); this.lastActivity = new Map();
    this.memberReadEnded.clear(); this.voiceLookups.clear();
    this.dropTrays(); this.pinsOpen.clear(); this.roles.clear(); this.purgeAgain = false;
    for (const channelId of this.threadOpen.keys()) this.slices.set(`thread:${channelId}`, null);
    for (const channelId of this.threadsOpen) this.slices.set(`threads:${channelId}`, []);
    this.threadOpen.clear(); this.threadsOpen.clear(); this.threadSends.clear();
    // web3.5 C4: the next attach sends every role view again, once its own reads have landed.
    this.authority.clear(); this.authorityStates.clear(); this.membersRead.clear(); this.accessRead.clear();
    this.typingOn.clear(); this.incomingTyping.clear(); this.deviceCount = 0;
    for (const { handle } of this.typingSweeps.values()) this.deps.clearTimeout(handle);
    this.typingSweeps.clear();
    this.searchGate.invalidate(); this.searchCursor = null;
    this.noticesState = { nextId: this.noticesState.nextId, items: [] };
    this.slices.set('communities', []);
    for (const communityId of communities) {
      this.slices.set(`channels:${communityId}`, []);
      this.slices.set(`members:${communityId}`, []);
      this.clearPresence(communityId);
      this.clearAFK(communityId);
    }
    for (const channelId of timelines) {
      this.slices.set(`timeline:${channelId}`, { channelId, group: 'none', items: [], hasEarlier: false });
      this.slices.set(`typing:${channelId}`, []);
      if (this.slices.get(`pins:${channelId}`) !== undefined) this.slices.set(`pins:${channelId}`, []);
    }
    this.slices.set('dms', []);
    this.slices.set('devices', []);
    this.slices.set('badges', {});
    this.slices.set('settings', {});
    this.slices.set('notices', this.noticesState);
    // C4 fix round 1: the removal notices and the own leaves of this account go with it; the next account's removals are
    // told whatever this one left.
    if (this.slices.get('removals') !== undefined) this.slices.set('removals', []);
    this.admin.selfRemoved.clear();
    this.slices.set('trust', []);
    if (this.slices.get('factors') !== undefined) this.slices.set('factors', NO_FACTORS);
    this.slices.set('sendBlocks', {});
    this.slices.set('search', { requestId: 0, hits: [], cursor: null });
    this.calls.reset();
    this.lastVoiceSelf = null; this.ownInCall.clear(); this.selfResends = null; this.selfOffline = null;
    this.setConnection({ status: 'offline', generation: null });
  }

  private clearInteractionAckLeases(): void {
    this.interactionAckLeaseGeneration++;
    this.pendingBotInteractionAcks.clear();
    this.pendingHumanInteractionAcks.clear();
  }

  // ---- channels and timelines ----

  /** A channel lookup that serves community channels and DMs alike (requirement 17). */
  private target(channelId: string): Target | undefined {
    const known = this.channels.get(channelId);
    if (known !== undefined) return { communityId: known.communityId, dm: false, channel: known.row, textGroupId: known.row.textGroupId };
    const dm = this.dms.get(channelId);
    if (dm !== undefined) return { communityId: null, dm: true, channel: { id: fromHex(channelId), kind: 0, mode: 0 }, textGroupId: dm.groupId };
    return undefined;
  }

  private membership(channelId: string): GroupMembership {
    return { refusedChannel: this.refusedChannels.has(channelId), notMember: this.notMember, resyncing: this.resyncing };
  }

  private groupState(channelId: string, groups: readonly GroupInfo[] = this.core?.groups() ?? []): ChannelGroupState {
    const target = this.target(channelId);
    if (target === undefined) return 'none';
    return channelGroupState(target.channel, groups, this.membership(channelId));
  }

  private openChannel(channelId: string): Promise<null> {
    // Pre-flight ruling 1(i): one open per channel in flight; a later call awaits it. A closeChannel
    // between the two (ruling 1(ii)) only cleared the open mark, so the later call marks the channel
    // open again with the in-flight open's own entry: the join's success then stores its group id in
    // it, and a failed join removes it as it would have without the close.
    const inFlight = this.opening.get(channelId);
    if (inFlight !== undefined) {
      if (inFlight.entry !== null && !this.open.has(channelId)) this.open.set(channelId, inFlight.entry);
      return inFlight.run;
    }
    const opening: { run: Promise<null>; entry: OpenChannel | null } = { run: Promise.resolve(null), entry: null };
    opening.run = this.openOnce(channelId, (entry) => { opening.entry = entry; })
      .finally(() => { this.opening.delete(channelId); });
    this.opening.set(channelId, opening);
    return opening.run;
  }

  private async openOnce(channelId: string, marked: (entry: OpenChannel) => void): Promise<null> {
    const target = this.target(channelId);
    if (target === undefined) throw new Refusal('E_BAD_INPUT', 'unknown channel');
    const entry: OpenChannel = { groupId: this.open.get(channelId)?.groupId ?? null, limit: TIMELINE_PAGE };
    this.open.set(channelId, entry);
    marked(entry);
    if (this.groupState(channelId) === 'unsupported') {
      this.slices.set(`timeline:${channelId}`, { channelId, group: 'unsupported', items: [], hasEarlier: false });
      return null;
    }
    this.refreshTimeline(channelId);
    try {
      const result = await this.requireSync().openChannel({
        communityId: target.communityId === null ? null : fromHex(target.communityId), channelId: fromHex(channelId),
        textGroupId: target.textGroupId,
      });
      this.refusedChannels.delete(channelId);
      // Pre-flight ruling 1(ii): a channel closed meanwhile gets fresh slices but is not opened again.
      const current = this.open.get(channelId);
      if (current !== undefined) current.groupId = result.groupId;
      const dm = this.dms.get(channelId);
      if (dm !== undefined && dm.groupId === null) {
        dm.groupId = result.groupId;
        this.updateExpected();
      }
      this.refreshChannel(channelId);
      this.refreshTimeline(channelId, result.groupId);
      return null;
    } catch (e) {
      if (e instanceof DillaHttpError && e.status === 403) {
        this.refusedChannels.add(channelId);
        this.refreshChannel(channelId);
        this.refreshTimeline(channelId);
        return null;
      }
      if (this.open.get(channelId) === entry) this.open.delete(channelId);
      throw e;
    }
  }

  private closeChannel(channelId: string): Promise<null> {
    const wasOpen = this.open.has(channelId);
    if (this.typingOn.delete(channelId)) this.gateway?.sendTyping(fromHex(channelId), false);
    this.dropIncomingTyping(channelId);
    if (wasOpen) this.slices.set(`typing:${channelId}`, []);
    this.pinsOpen.delete(channelId);
    this.dropOpen(channelId);
    return Promise.resolve(null);
  }

  /** A channel stops being open (closed, removed, its community dropped, its DM gone): its thread and threads list go
   *  with it, so nothing of a channel that is not open stays on display or is resurrected by a later open. */
  private dropOpen(channelId: string): void {
    this.open.delete(channelId);
    if (this.threadOpen.delete(channelId) || this.slices.get(`thread:${channelId}`) !== undefined) this.slices.set(`thread:${channelId}`, null);
    if (this.threadsOpen.delete(channelId) || this.slices.get(`threads:${channelId}`) !== undefined) this.slices.set(`threads:${channelId}`, []);
  }

  private loadEarlier(channelId: string): Promise<null> {
    const entry = this.open.get(channelId);
    if (entry !== undefined) {
      entry.limit += TIMELINE_PAGE;
      this.refreshTimeline(channelId);
    }
    return Promise.resolve(null);
  }

  private refreshChannel(channelId: string): void {
    const target = this.target(channelId);
    if (target === undefined) return;
    if (target.communityId === null) this.publishDms();
    else this.publishChannels(target.communityId);
  }

  /** The group id a channel's timeline is read from: the one its open resolved to, else the local group. */
  private timelineGroup(channelId: string, groups: readonly GroupInfo[], hint?: Id): Id | null {
    return this.open.get(channelId)?.groupId ?? hint ?? channelGroup(fromHex(channelId), groups)?.groupId ?? null;
  }

  /** Answers whether the timeline was read again and set (false: the channel is not known, or nothing is open). */
  private refreshTimeline(channelId: string, hint?: Id): boolean {
    const target = this.target(channelId);
    const core = this.core;
    const me = this.me;
    if (target === undefined || core === null || me === null) return false;
    const groups = core.groups();
    const group = this.groupState(channelId, groups);
    if (group === 'unsupported') {
      this.slices.set(`timeline:${channelId}`, { channelId, group, items: [], hasEarlier: false });
      return true;
    }
    const limit = this.open.get(channelId)?.limit ?? TIMELINE_PAGE;
    const groupId = this.timelineGroup(channelId, groups, hint);
    const outbox = groupId === null ? [] : this.settleThreadSends(groupId, core.outbox(groupId));
    const timeline = buildTimeline({
      channelId, group, rows: groupId === null ? [] : core.timeline(groupId, 0n, limit),
      outbox: outbox.filter((row) => groupId === null || this.threadOf(groupId, row) === null), ownUser: me.userId, ownDevice: me.deviceId, limit,
    });
    this.slices.set(`timeline:${channelId}`, timeline);
    // A DM has no community member list to look senders up in.
    if (target.communityId !== null) this.lookUpMembers(target.communityId, timeline.items);
    this.refreshThread(channelId, groupId, group, outbox);
    if (this.threadsOpen.has(channelId)) this.publishThreads(channelId);
    return true;
  }

  /** The root hex of the thread an own outbox row of the group is a reply in (null: a channel row or a fold). */
  private threadOf(groupId: Id, row: OutboxRow): string | null {
    if (row.type !== 0) return null;
    const msgId = toHex(row.msgId);
    const sent = this.threadSends.get(msgId);
    if (sent !== undefined) return sent.root;
    const intent = this.threadIntent;
    return intent !== null && intent.group === toHex(groupId) && !intent.known.has(msgId) ? intent.root : null;
  }

  /** Forgets the thread sends of the group that the outbox no longer holds (stored, or discarded); answers the outbox. */
  private settleThreadSends(groupId: Id, outbox: OutboxRow[]): OutboxRow[] {
    if (this.threadSends.size === 0) return outbox;
    const group = toHex(groupId);
    const held = new Set(outbox.map((row) => toHex(row.msgId)));
    for (const [msgId, sent] of this.threadSends) if (sent.group === group && !held.has(msgId)) this.threadSends.delete(msgId);
    return outbox;
  }

  /** Replies read per open thread: the core's cap. There is no paging command (the brief names three), so `hasEarlier` is
   *  true at exactly this many replies, though the thread may hold no more. */
  private static readonly THREAD_PAGE = 200;

  /** The open thread of the channel, read again from the core (nothing when none is open). */
  private refreshThread(channelId: string, groupId: Id | null, group: ChannelGroupState, outbox: readonly OutboxRow[]): void {
    const root = this.threadOpen.get(channelId);
    const core = this.core;
    const me = this.me;
    if (root === undefined || core === null || me === null) return;
    const empty: ThreadState = { channelId, rootMsgId: root, root: null, items: [], hasEarlier: false };
    if (groupId === null) { this.slices.set(`thread:${channelId}`, empty); return; }
    const read = core.threadTimeline(groupId, fromHex(root), 0n, Controller.THREAD_PAGE);
    const replies = [...read.rows].reverse();   // the core answers newest first; the page reads oldest first, newest last
    const timeline = buildTimeline({
      channelId, group, rows: read.root === null ? replies : [read.root, ...replies],
      outbox: outbox.filter((row) => row.type !== 0 || this.threadOf(groupId, row) === root),
      ownUser: me.userId, ownDevice: me.deviceId, limit: Controller.THREAD_PAGE, threadRoot: fromHex(root),
    });
    const items = read.root === null ? timeline.items : timeline.items.slice(1);
    this.slices.set(`thread:${channelId}`, {
      channelId, rootMsgId: root, root: read.root === null ? null : timeline.items[0] ?? null, items, hasEarlier: read.rows.length >= Controller.THREAD_PAGE,
    });
    const communityId = this.target(channelId)?.communityId ?? null;
    if (communityId !== null) this.lookUpMembers(communityId, timeline.items);
  }

  private openThread(channelId: string, rootMsgId: string): Promise<null> {
    if (this.target(channelId) === undefined) throw new Refusal('E_BAD_INPUT', 'unknown channel');
    if (!this.open.has(channelId)) throw new Refusal('E_NOT_READY', 'the channel is not open');
    this.threadOpen.set(channelId, rootMsgId);
    const core = this.requireCore();
    const groups = core.groups();
    const groupId = this.timelineGroup(channelId, groups);
    this.refreshThread(channelId, groupId, this.groupState(channelId, groups), groupId === null ? [] : core.outbox(groupId));
    return Promise.resolve(null);
  }

  private closeThread(channelId: string): Promise<null> {
    this.threadOpen.delete(channelId);
    this.slices.set(`thread:${channelId}`, null);
    return Promise.resolve(null);
  }

  private loadThreads(channelId: string): Promise<null> {
    if (this.target(channelId) === undefined) throw new Refusal('E_BAD_INPUT', 'unknown channel');
    if (!this.open.has(channelId)) throw new Refusal('E_NOT_READY', 'the channel is not open');
    this.threadsOpen.add(channelId);
    this.publishThreads(channelId);
    return Promise.resolve(null);
  }

  private publishThreads(channelId: string): void {
    const core = this.core;
    if (core === null) return;
    const g = this.timelineGroup(channelId, core.groups());
    this.slices.set(`threads:${channelId}`, g === null ? [] : core.threads(g).map((t) => ({
      rootMsgId: toHex(t.rootMsgId), rootSeq: t.rootSeq === null ? null : t.rootSeq.toString(), count: t.count,
      lastTs: Number(t.lastTs), excerpt: t.excerpt,
    })));
  }

  /** A sender who is not in the loaded member list re-runs listMembers once per unknown user (L-TS-09). */
  private lookUpMembers(communityId: string, items: readonly TimelineItem[]): void {
    this.lookUpUsers(communityId, items.map((item) => item.senderUser).filter((u): u is string => u !== null), 'timeline');
  }

  /** Users the community's loaded member list lacks re-run listMembers, once per unknown user (L-TS-09): a timeline's
   *  senders, and a voice call's participants (web-3 task 10, concern 4).
   *
   *  The two sources differ (f10app m3). A timeline sender is authenticated by its group, and also reads a list that has not
   *  loaded (its first read failed) as before. A voice participant comes from the instance's unauthenticated voice_state
   *  frames, so a flood of ever-new user ids is bounded: a list not loaded yet is left to its load, at most
   *  VOICE_LOOKUPS_MAX users per community ever enter `lookedUp` from voice, and a voice-driven read starts no sooner
   *  than MEMBER_REREAD_MIN_MS after the last one ended. Reads are coalesced per community either way: one running, at
   *  most one more owed. */
  private lookUpUsers(communityId: string, users: readonly string[], from: 'timeline' | 'voice'): void {
    const loaded = this.slices.get(`members:${communityId}`);
    if (loaded === undefined && from === 'voice') return;
    const known = new Set((loaded ?? []).map((m) => m.userId));
    let unknown = false;
    for (const user of users) {
      if (known.has(user) || this.lookedUp.has(user)) continue;
      if (from === 'voice') {
        const n = this.voiceLookups.get(communityId) ?? 0;
        if (n >= VOICE_LOOKUPS_MAX) continue;
        this.voiceLookups.set(communityId, n + 1);
      }
      this.lookedUp.add(user);
      unknown = true;
    }
    if (!unknown) return;
    const running = this.memberReads.get(communityId);
    if (running !== undefined) { running.again = true; return; }
    const read = { again: false };
    this.memberReads.set(communityId, read);
    // WB client m5: only the read itself is tracked by the barrier; the pacing sleep is not, so a hand-over's close never
    // waits for it, and nothing is read once the barrier closed.
    void (async () => {
      try {
        let paced = from === 'voice';
        do {
          read.again = false;
          if (paced) {
            const wait = (this.memberReadEnded.get(communityId) ?? -Infinity) + MEMBER_REREAD_MIN_MS - this.deps.now();
            if (wait > 0) await new Promise<void>((resolve) => { this.deps.setTimeout(resolve, wait); });
          }
          if (this.barrier.isClosed()) return;
          await this.barrier.track((async () => {
            const rows = await this.readMembers(communityId);
            this.memberReadEnded.set(communityId, this.deps.now());
            if (rows !== null && this.listed(communityId)) this.publishMembers(communityId, rows);
          })());
          paced = true;
        } while (read.again);
      } finally {
        this.memberReads.delete(communityId);
      }
    })().catch(() => undefined);
  }

  // ---- the sync engine's reports ----

  /** A report from the engine must never throw back into it. */
  private safely(fn: () => void): void {
    try { fn(); } catch { /* the next report or command refreshes the slices */ }
  }

  private onGroupChanged(groupId: Id, result: ApplyResult): void {
    const hex = toHex(groupId);
    if (result.epochChanged) {
      const prefix = `${hex}:`;
      for (const key of this.fetchedLeaves) if (key.startsWith(prefix)) this.fetchedLeaves.delete(key);
      if (this.trustRefresh !== null) this.trustRefreshAgain = true;
    }
    // A state-2 apply ends a resync, never a not-member report: a frame applied to the group after a refused resync (the
    // docs shoot's removal) proves nothing about this device's leaf. Not-member ends with a rejoin ('resyncing') or a
    // catch-up the DS served ('member').
    if (result.state === 2) this.resyncing.delete(hex);
    const seen: { groups: readonly GroupInfo[] | null } = { groups: null };
    this.safely(() => { this.refreshTrustGeneration(); seen.groups = this.touchGroup(groupId); });
    this.safely(() => { this.authorityGroupState(groupId, seen.groups ?? this.core?.groups() ?? []); });
    this.stopTypingIfUnviewable();
    void this.barrier.track(this.refreshTrust().catch(() => undefined));
    this.safely(() => { this.reportActivity(groupId, result); });
    // web-3 (L-TS-56 step 2): a call group's new epoch is keyed through the core's gate, or pauses, blocks or removes.
    this.safely(() => { if (this.calls.isCallGroup(groupId)) this.calls.onGroupChanged(groupId, result); });
    // Pre-flight ruling F6: an apply can adopt the own type-2 row (a catch-up) and so record a purge.
    this.runPurges();
  }

  private onMembership(groupId: Id, status: 'resyncing' | 'not-member' | 'member'): void {
    const hex = toHex(groupId);
    if (status === 'member') {
      if (!this.notMember.delete(hex) && !this.resyncing.delete(hex)) return;
      this.touchAndRearm(groupId);
      return;
    }
    if (status === 'resyncing') { this.resyncing.add(hex); this.notMember.delete(hex); }
    else { this.notMember.add(hex); this.resyncing.delete(hex); }
    this.touchAndRearm(groupId);
    this.stopTypingIfUnviewable();
    // Review M6: a call group's lost membership ends the call here (the engine never resyncs a call group).
    if (status === 'not-member') this.safely(() => { if (this.calls.isCallGroup(groupId)) this.calls.onMembershipLost(groupId); });
  }

  /** A membership report: the group's rows and timelines, then the role view's re-arm, from one read of the groups. */
  private touchAndRearm(groupId: Id): void {
    const seen: { groups: readonly GroupInfo[] | null } = { groups: null };
    this.safely(() => { seen.groups = this.touchGroup(groupId); });
    this.safely(() => { this.authorityGroupState(groupId, seen.groups ?? this.core?.groups() ?? []); });
  }

  /** Refreshes the row of every known channel or DM bound to the group and the timeline of every such open one; answers
   *  the groups it read. */
  private touchGroup(groupId: Id): readonly GroupInfo[] {
    const groups = this.core?.groups() ?? [];
    this.touchGroups(new Set([toHex(groupId)]), groups);
    return groups;
  }

  /** touchGroup for several groups at once: each server's channel rows are published once (C4 fix round 1). */
  private touchGroups(hexes: ReadonlySet<string>, groups: readonly GroupInfo[]): void {
    const bound = (channelId: string, textGroupId: Id | null): boolean => {
      const openGroup = this.open.get(channelId)?.groupId ?? null;
      const local = channelGroup(fromHex(channelId), groups);
      return (textGroupId !== null && hexes.has(toHex(textGroupId)))
        || (openGroup !== null && hexes.has(toHex(openGroup)))
        || (local !== undefined && hexes.has(toHex(local.groupId)));
    };
    const communities = new Set<string>();
    const timelines: string[] = [];
    for (const [channelId, { communityId, row }] of this.channels) {
      if (!bound(channelId, row.textGroupId)) continue;
      communities.add(communityId);
      if (this.open.has(channelId)) timelines.push(channelId);
    }
    let dms = false;
    for (const [channelId, dm] of this.dms) {
      if (!bound(channelId, dm.groupId)) continue;
      dms = true;
      if (this.open.has(channelId)) timelines.push(channelId);
    }
    for (const communityId of communities) this.publishChannels(communityId);
    if (dms) this.publishDms();
    for (const channelId of timelines) this.refreshTimeline(channelId);
    for (const channelId of this.pinsOpen) if (bound(channelId, this.target(channelId)?.textGroupId ?? null)) this.publishPins(channelId);
  }

  /** A trust write may retract rows in groups other than the one that reported activity. */
  private refreshTrustGeneration(): void {
    const generation = this.requireCore().trustGeneration();
    if (this.seenTrustGeneration === generation) return;
    this.seenTrustGeneration = generation;
    this.searchGate.invalidate();
    this.searchCursor = null;
    this.slices.set('search', { requestId: 0, hits: [], cursor: null });
    // L-CORE-43: no slice of the old generation stays on display. The refreshed timeline replaces it in one set (M4: an
    // empty one first made a second render, lost the scroll position and could re-announce the log); only a timeline
    // that is not re-read now (it throws, or returns without a set: a channel no longer listed) is emptied.
    for (const channelId of this.open.keys()) {
      let read = false;
      try { read = this.refreshTimeline(channelId); } catch { /* emptied below */ }
      if (read) continue;
      let group: ChannelGroupState = 'none';
      try { group = this.groupState(channelId); } catch { /* none */ }
      this.slices.set(`timeline:${channelId}`, { channelId, group, items: [], hasEarlier: false });
      if (this.threadOpen.has(channelId)) this.slices.set(`thread:${channelId}`, null);
    }
    // CI1: a row the write released counts as unread whether or not its channel is open, and raises its notice.
    this.publishBadges();
    this.noticeReleased();
    this.publishTrust();
  }

  /** Coalesce concurrent triggers while keeping their whole refresh inside the tracked promise. */
  private refreshTrust(): Promise<null> {
    if (this.trustRefresh !== null) return this.trustRefresh;
    const run = (async (): Promise<null> => {
      do {
        this.trustRefreshAgain = false;
        await this.refreshTrustOnce();
      } while (this.trustRefreshAgain && this.phase === 'ready');
      // web-3 (L-TS-56 step 2): after every trust refresh the call's epoch is read through the gate again.
      this.safely(() => { this.calls.onTrust(); });
      return null;
    })().finally(() => { this.trustRefresh = null; });
    this.trustRefresh = run;
    return run;
  }

  private async refreshTrustOnce(): Promise<null> {
    const core = this.requireCore();
    const leaves = core.pendingLeaves();
    this.noteChangedLeaves(leaves);
    let keyChanged = false;
    // web3.8 C3: an own leaf whose ingest lands a newer own list (a device of this account joined) refreshes the device
    // count below, once per pass. Fix I1: the core never answers `changed` for the own user (a new own device keeps the
    // key; a new key throws E_UMK_CHANGED), so the version is compared with the own list's at the pass start. A core with
    // no own list to read (refreshDevices would read it too) triggers nothing, and never fails the pass.
    let ownVersion: bigint | null = null;
    try { ownVersion = core.ownDeviceList().version; } catch { /* no re-read this pass */ }
    let ownChanged = false;
    // A user the core no longer reports in any active group has no history left to wait for.
    const present = new Set(leaves.map((leaf) => toHex(leaf.userId)));
    for (const user of this.historyFailedUsers) if (!present.has(user)) this.historyFailedUsers.delete(user);
    // A refused own leaf the core no longer reports has left the group (a validated Remove).
    const current = new Set(leaves.map((leaf) => `${toHex(leaf.groupId)}:${leaf.leafIndex}`));
    for (const leaf of this.ownRefusedLeaves) if (!current.has(leaf)) this.ownRefusedLeaves.delete(leaf);
    // Users with a leaf whose signed history the core has never accepted; a successful ingest below caches their list.
    const unknownUsers = new Set(leaves.filter((leaf) => leaf.reason === 'unknown').map((leaf) => toHex(leaf.userId)));
    const historyByUser = new Map<string, { head: Uint8Array; history: Uint8Array } | null>();
    // Each user's history is read from the oldest version any of their leaves caches; one pass (M5: not once per user).
    const oldestByUser = new Map<string, bigint>();
    for (const leaf of leaves) {
      const user = toHex(leaf.userId);
      const known = oldestByUser.get(user);
      if (known === undefined || leaf.cachedVersion < known) oldestByUser.set(user, leaf.cachedVersion);
    }
    for (const leaf of leaves) {
      if (this.phase !== 'ready') break;
      const key = `${toHex(leaf.groupId)}:${leaf.leafIndex}:${toHex(leaf.userId)}:${leaf.cachedVersion}:${leaf.reason}`;
      if (this.fetchedLeaves.has(key)) continue;
      this.fetchedLeaves.add(key);
      const user = toHex(leaf.userId);
      try {
        if (!historyByUser.has(user)) {
          const oldest = oldestByUser.get(user) ?? leaf.cachedVersion;
          try { historyByUser.set(user, await readDeviceListHistory(this.routes, leaf.userId, oldest)); this.historyFailedUsers.delete(user); }
          catch { historyByUser.set(user, null); this.historyFailedUsers.add(user); }
        }
        const signed = historyByUser.get(user);
        if (signed === null || signed === undefined) continue;
        const { head, history } = signed;
        let verdict: { version: bigint; changed: boolean; quarantined: number };
        const own = this.me !== null && toHex(this.me.userId) === user;
        try {
          ingestAccountKind(core, leaf.userId, head, code => this.recordError(code));
          verdict = core.deviceListIngest(leaf.groupId, leaf.leafIndex, leaf.userId, history, signedHeadList(head), this.nowS());
        }
        catch (e) {
          // Only the core's own leaf verdicts say the device is off the list; a stale read or limit says nothing about it.
          // The refusal writes nothing in the core, so the worker keeps it. An own-user history under another key is an
          // own-trust signal (fixA), never an unlisted contact; the leaf is not fetched again before the next ready.
          if (e instanceof CoreError && LEAF_VERDICTS.has(e.code)) this.unlistedUsers.add(user);
          if (e instanceof CoreError && e.code === 'E_UMK_CHANGED' && own) this.ownRefusedLeaves.add(`${toHex(leaf.groupId)}:${leaf.leafIndex}`);
          throw e;
        }
        // The core caches verdict.version now and anchors every later ingest of this user on it: the user's other leaves
        // in this pass are ingested from that version (a full history from below it is refused E_DEVICE_LIST_STALE, which
        // left their held rows unreadable until some later pass). The head is the history's last row, so the slice holds it.
        const rest = historyFrom(history, verdict.version);
        if (rest !== null) historyByUser.set(user, { head, history: rest });
        // The quarantine count is not read: what it counts the core stores, and trust_state's `blocked` reports it.
        if (verdict.changed) keyChanged = true;
        if (own && ownVersion !== null && verdict.version > ownVersion) ownChanged = true;
        this.unlistedUsers.delete(user);
        if (own) this.ownRefusedLeaves.delete(`${toHex(leaf.groupId)}:${leaf.leafIndex}`);
        unknownUsers.delete(user);
        core.revalidatePending(leaf.groupId, leaf.leafIndex, 256);
        this.refreshTrustGeneration();
      } catch { /* Keep the leaf untrusted; the next ready retries. */ }
    }
    if (keyChanged) this.noteChangedLeaves(core.pendingLeaves());
    this.pendingUsers = new Set([...this.historyFailedUsers, ...unknownUsers]);
    this.publishTrust();
    // The devices and diagnostics slices follow (refreshDevices); a failed read keeps the last count until the next one.
    if (ownChanged && this.phase === 'ready') {
      try { await this.refreshDevices(); } catch { /* the next read refreshes it */ }
    }
    return null;
  }

  /**
   * Remembers where each user's changed leaves are. A group that had one, is still active here and no longer has any
   * changed leaf of that user lost it to a validated Remove (the core then cleared only that group's send block).
   */
  private noteChangedLeaves(leaves: readonly { groupId: Id; userId: Id; reason: string }[]): void {
    const now = new Map<string, Set<string>>();
    for (const leaf of leaves) {
      if (leaf.reason !== 'changed') continue;
      const user = toHex(leaf.userId);
      let groups = now.get(user);
      if (groups === undefined) { groups = new Set(); now.set(user, groups); }
      groups.add(toHex(leaf.groupId));
    }
    const active = new Set(this.requireCore().groups().filter((g) => g.state === 2).map((g) => toHex(g.groupId)));
    for (const [user, before] of this.changedLeafGroups) {
      for (const group of before) {
        if (now.get(user)?.has(group) === true || !active.has(group)) continue;
        let removed = this.removedGroups.get(user);
        if (removed === undefined) { removed = new Set(); this.removedGroups.set(user, removed); }
        removed.add(group);
      }
    }
    for (const [user, removed] of this.removedGroups) {
      for (const group of removed) if (!active.has(group) || now.get(user)?.has(group) === true) removed.delete(group);
      if (removed.size === 0) this.removedGroups.delete(user);
    }
    this.changedLeafGroups = now;
  }

  private sharedUser(userHex: string): boolean {
    const groups = this.core?.groups() ?? [];
    if ([...this.dms].some(([channelId, dm]) => dm.members.some((u) => toHex(u) === userHex)
      && groups.some((g) => g.state === 2 && toHex(g.targetId) === channelId))) return true;
    for (const community of this.slices.get('communities') ?? []) {
      if (this.slices.get(`members:${community.id}`)?.some((member) => member.userId === userHex)
        && groups.some((g) => g.state === 2 && g.communityId !== null && toHex(g.communityId) === community.id)) return true;
    }
    return false;
  }

  private publishTrust(): void {
    if (this.core === null) return;
    // The page learns the channel a Remove unblocked, never the group id.
    const channelOf = new Map<string, string>();
    for (const group of this.core.groups()) {
      if (group.kind !== 0) continue;
      const channelId = toHex(group.targetId);
      if (this.dms.has(channelId) || this.channels.has(channelId)) channelOf.set(toHex(group.groupId), channelId);
    }
    // The core pins the own key as well; nobody compares a safety number with themselves, so the own row is no contact.
    // What the core says about it is not dropped: ABI 8 keeps the own row verified and never changed, and its `blocked`
    // says an own-user leaf is under another key (E_UMK_CHANGED) or an own device is unlisted. That stands on the account
    // slice, where the page alerts on it (ruling M6 revised: no own-key compare; a Remove of the leaf ends it).
    const own = this.me === null ? null : toHex(this.me.userId);
    let rows: TrustRecord[];
    try {
      rows = this.core.trustState();
    } catch (e) {
      // R3: a trust state the core cannot read never leaves an earlier verified view standing. Every contact reads as
      // history pending (not verified, no Verify) until the core answers again; the gate is read as it is.
      this.recordError(errorOf(e).code);
      this.slices.set('trust', (this.slices.get('trust') ?? []).map((row) => ({ ...row, verified: false, unlisted: true as const })));
      this.publishSendBlocks();
      return;
    }
    const ownRow = own === null ? undefined : rows.find((row) => toHex(row.userId) === own);
    const ownTrust: AccountState['ownTrust'] = ownRow?.blocked === 'E_UMK_CHANGED' || this.ownRefusedLeaves.size > 0 ? 'changed'
      : (ownRow?.blocked ?? null) !== null || (own !== null && this.unlistedUsers.has(own)) ? 'unlisted' : null;
    if (!this.storeCleared && this.accountState.ownTrust !== ownTrust) this.setAccount({ ownTrust });
    this.slices.set('trust', rows.filter((row) => toHex(row.userId) !== own).map((row): TrustSummary => {
      const userId = toHex(row.userId);
      // An approved key ends the changed state everywhere, so no group is left to call removed.
      if (row.changedUmk === null) this.removedGroups.delete(userId);
      const removedIn = [...new Set([...this.removedGroups.get(userId) ?? []].flatMap((g) => channelOf.get(g) ?? []))].sort();
      const blocked = row.blocked;
      // A failed leaf (not a changed key) reads as unlisted; so do a refusal the core stored nothing for and Flow 05's
      // "history pending" (the list read has not completed): the unlisted announcement and no Verify.
      const unlisted = (blocked !== null && blocked !== 'E_UMK_CHANGED') || this.unlistedUsers.has(userId) || this.pendingUsers.has(userId);
      return { userId, name: this.nameOf(userId), shared: this.sharedUser(userId), umk: toHex(row.umk),
        // F4: never verified while the core holds a failed or changed verdict for one of the contact's leaves.
        firstSeen: Number(row.firstSeen), verified: row.verified && blocked === null, changedUmk: row.changedUmk === null ? null : toHex(row.changedUmk),
        ...(unlisted ? { unlisted: true as const } : {}), ...(removedIn.length > 0 ? { removedIn } : {}),
        ...(blocked !== null ? { blocked } : {}) };
    }));
    this.publishSendBlocks();
  }

  /** The composer's send gate (W-01): send_block_code of each active channel group, by channel; open channels are absent. */
  private publishSendBlocks(): void {
    if (this.core === null) return;
    const blocks: SendBlocksState = {};
    for (const group of this.core.groups()) {
      if (group.kind !== 0 || group.state !== 2) continue;
      const channelId = toHex(group.targetId);
      if (!this.dms.has(channelId) && !this.channels.has(channelId)) continue;
      const code = this.core.sendBlockCode(group.groupId);
      if (code !== null) blocks[channelId] = { code, own: this.core.sendBlockOwn(group.groupId) };
    }
    this.slices.set('sendBlocks', blocks);
    // Integration cards: a ring of a DM blocked now ends; a blocked call whose channel block cleared goes idle.
    this.safely(() => { this.calls.onSendBlocks(); });
  }

  private verifyContact(userId: string, umk: string): Promise<null> {
    if (!this.sharedUser(userId)) throw new Refusal('E_CORE_NOT_FOUND', '');
    this.requireCore().trustVerify(fromHex(userId), fromHex(umk));
    // The approval re-judged this user's leaves in the core and released held messages (the generation moved): the trust
    // slice and every open timeline are read again from the core below, and the user's leaves are fetched again.
    for (const key of this.fetchedLeaves) if (key.split(':')[2] === userId) this.fetchedLeaves.delete(key);
    if (this.trustRefresh !== null) this.trustRefreshAgain = true;
    void this.barrier.track(this.refreshTrust()).catch(() => undefined);
    this.refreshTrustGeneration();
    this.publishTrust();
    return Promise.resolve(null);
  }

  /** The digits for exactly the key the page compares (ABI 8); the core refuses a key that is not its comparison key. */
  private safetyNumber(userId: string, umk: string): string {
    if (!this.sharedUser(userId)) throw new Refusal('E_CORE_NOT_FOUND', '');
    return this.requireCore().safetyNumberFor(fromHex(userId), fromHex(umk));
  }

  private search(command: Extract<Command, { m: 'search' }>, requestId: number): Promise<null> {
    const previous = this.searchCursor;
    this.searchCursor = null;
    const scope = { kind: command.scope, id: command.id === null ? null : fromHex(command.id) };
    if (command.cursor !== null && (previous?.token !== command.cursor.token
      || previous.scope.scope !== command.scope || previous.scope.id !== command.id || previous.scope.query !== command.query))
      throw new CoreError('E_CORE_INPUT', 'stale search cursor');
    const cursor = command.cursor === null ? null : previous!.position;
    const hits = this.requireCore().search(scope, command.query, cursor, 50);
    if (this.searchGate.current(requestId)) {
      const destinations = new Map<string, { channelId: string; communityId: string | null }>();
      for (const group of this.requireCore().groups()) {
        if (group.kind !== 0) continue;
        const channelId = toHex(group.targetId);
        if (this.dms.has(channelId)) destinations.set(toHex(group.groupId), { channelId, communityId: null });
        else {
          const known = this.channels.get(channelId);
          if (known !== undefined && group.communityId !== null && known.communityId === toHex(group.communityId))
            destinations.set(toHex(group.groupId), { channelId, communityId: known.communityId });
        }
      }
      const rows = hits.slice(0, 50).flatMap((h) => {
        const destination = destinations.get(toHex(h.groupId));
        if (destination === undefined) return [];
        // J1: the row's sender and time, so the palette can show them; a row that cannot be read leaves both out.
        const found = this.rowAt(this.requireCore(), h.groupId, h.seq);
        const who = found?.row.senderUser ?? null;
        return [{ ...destination, seq: h.seq.toString(),
          snippet: Array.from(h.snippet).slice(0, 160).join(''), rank: h.rank.toString(),
          threadRoot: h.threadRoot === null ? null : toHex(h.threadRoot),
          ...(found === null ? {} : { ts: Number(found.row.recvTs), ...(who === null ? {} : { author: this.nameOf(toHex(who)) }) }) }];
      });
      const last = hits.at(-1);
      if (last !== undefined) this.searchCursor = { token: String(requestId), scope: command,
        position: { rank: last.rank, groupId: last.groupId, seq: last.seq } };
      this.slices.set('search', { requestId, hits: rows,
        cursor: last === undefined ? null : { token: String(requestId) } });
    }
    return Promise.resolve(null);
  }

  private typing(channelId: string, on: boolean): Promise<null> {
    // WB product walk I1: an off with no signal on is a no-op. The page's off for the channel it just left arrives after
    // closeChannel (which sent the off already), as do the offs after a membership loss or the link's end.
    if (!on && !this.typingOn.has(channelId)) return Promise.resolve(null);
    if (!this.open.has(channelId) || this.groupState(channelId) !== 'active'
      || this.phase !== 'ready' || this.connectionState.status !== 'online')
      throw new Refusal('E_NOT_READY', '');
    this.gateway?.sendTyping(fromHex(channelId), on);
    if (on) this.typingOn.add(channelId); else this.typingOn.delete(channelId);
    return Promise.resolve(null);
  }

  private voiceSelf(callId: string, flags: number): Promise<null> {
    // web3.8 MUTE: kept before the online check, so a mute made while offline is sent again once the own voice_state
    // after the reconnect lacks it (the re-send rule below), and the others never keep "unmuted" for a muted session.
    this.lastVoiceSelf = { callId, flags: flags as 0 | 1 | 2 | 3 };
    if (this.connectionState.status !== 'online') {
      // Owed to the instance: the reconnect sends it itself, whether or not an own voice_state follows.
      this.selfOffline = { callId, left: VOICE_SELF_RECONNECT_SENDS };
      throw new Refusal('E_NOT_READY', '');
    }
    this.selfOffline = null;
    this.gateway?.sendVoiceSelf(fromHex(callId), flags as 0 | 1 | 2 | 3);
    return Promise.resolve(null);
  }

  /** web3.8 MUTE: the reconnect sends a voice_self made while offline (the last one, for a call that has not ended), at most
   *  VOICE_SELF_RECONNECT_SENDS times; the own voice_state re-send rule below keeps its own budget. */
  private sendOfflineSelf(): void {
    const owed = this.selfOffline;
    const last = this.lastVoiceSelf;
    if (owed === null || last === null || last.callId !== owed.callId || owed.left <= 0) { this.selfOffline = null; return; }
    owed.left -= 1;
    if (owed.left <= 0) this.selfOffline = null;
    this.gateway?.sendVoiceSelf(fromHex(last.callId), last.flags);
  }

  /** Task 3 review (controller ruling): the instance keeps a device's own mute and deafen flags only once the device is in
   *  the call's room, so one sent before the join webhook is lost. web3.8 VM: the SFU may list the device only after the
   *  join transition, so a voice_self is refused then too. While this device's own voice_state for the call lacks the self
   *  bits of the last voice_self for that call, it is sent again: online only, at most VOICE_SELF_RESENDS times per join.
   *  web3.8 MUTE: only what the page said is sent. The worker never infers a voice_self from voice.self: the page applies a
   *  press made while the join started (its start reads it live, and its attach reconcile sends the voice_self), so page
   *  and worker never disagree for a call. */
  private ownVoiceState(frame: Frame): void {
    let f;
    try { f = voiceStateOf(frame); } catch { return; }
    if (this.me === null || toHex(f.deviceId) !== toHex(this.me.deviceId)) return;
    const call = toHex(f.callId);
    if ((f.flags & 1) === 0) { this.ownInCall.delete(call); return; }
    if (!this.ownInCall.has(call)) {
      this.ownInCall.add(call);
      this.selfResends = { callId: call, left: VOICE_SELF_RESENDS };
    }
    const last = this.lastVoiceSelf;
    if (last === null || last.callId !== call || this.connectionState.status !== 'online') return;
    if (((f.flags >> 1) & 3) === last.flags) return;
    const budget = this.selfResends;
    if (budget === null || budget.callId !== call || budget.left <= 0) return;
    budget.left -= 1;
    this.gateway?.sendVoiceSelf(f.callId, last.flags);
  }

  private typingOff(): void {
    for (const channelId of this.typingOn) this.gateway?.sendTyping(fromHex(channelId), false);
    this.typingOn.clear();
  }

  private stopTypingIfUnviewable(): void {
    for (const channelId of new Set([...this.typingOn, ...this.incomingTyping.keys()])) {
      if (this.open.has(channelId) && this.groupState(channelId) === 'active'
        && this.phase === 'ready' && this.connectionState.status === 'online') continue;
      if (this.typingOn.delete(channelId)) this.gateway?.sendTyping(fromHex(channelId), false);
      this.dropIncomingTyping(channelId);
      this.slices.set(`typing:${channelId}`, []);
    }
  }

  private clearReauthTimer(): void {
    if (this.reauthTimer !== null) { this.deps.clearTimeout(this.reauthTimer); this.reauthTimer = null; }
  }

  private async resign(nonce: string): Promise<{ released: true }> {
    // WB client m5: the new tab waits 5 s for this release (handover.ts). A join in flight (its rejoin wait lasts up to
    // 30 s) is cancelled first, so the barrier does not wait for it: its teardown sends the self-Remove.
    this.calls.cancelJoin();
    this.clearPendingRolePanelActionRetries();
    await this.barrier.close();
    this.clearReauthTimer();
    // L-TS-56 step 9: the call is left and the rings cleared before the store is released; the leave's round trips are
    // bounded, since the instance removes the leaf when the call ends anyway.
    let bound: number | null = null;
    await Promise.race([
      this.calls.shutdown(),
      new Promise<void>((resolve) => { bound = this.deps.setTimeout(resolve, RESIGN_LEAVE_MS); }),
    ]);
    if (bound !== null) this.deps.clearTimeout(bound);
    try {
      this.typingOff();
      this.gateway?.stop();
      this.sync?.stop();
      this.requireCore().pause();
    } catch (e) {
      try {
        await this.requireCore().resume();
        // M2: resume()'s sequence, a fresh engine and its expected groups before the socket.
        this.startEngine();
        this.updateExpected();
        this.gateway?.start();
        this.barrier.reopen();
      } catch {
        this.setAccount({ phase: 'paused-error', error: errorOf(e) });
        // M1: a store this worker can neither pause nor resume is no lead to keep. The lock goes to the tab that asked,
        // and this worker waits behind it as a released one does (it boots from the store again when it leads).
        const instance = this.requireInstance();
        this.dropStore();
        await this.leadership?.release();
        this.leadership = null;
        this.awaitLeadership(instance);
        this.setAccount({ error: errorOf(e) });
      }
      throw e;
    }
    const instance = this.requireInstance();
    this.dropStore();
    await this.leadership?.release();
    this.leadership = null;
    this.setAccount({ phase: 'other-tab' });
    this.deps.post({ t: 'released', instance: toHex(instance.instanceId), nonce });
    this.awaitLeadership(instance);
    return { released: true };
  }

  /**
   * Drops what this worker knew of the account it led with, before the lock goes. Meanwhile the other tab may sign out,
   * sign in as someone else or reset the store: nothing of this account is kept (the page's views went with the shell
   * and open their channels again once this tab leads). The paused store is kept to be resumed, never opened again.
   */
  private dropStore(): void {
    const core = this.core;
    this.forgetAccount();
    this.pausedCore = core;
    this.setAccount({ user: null, deviceId: null, recoveryKey: null, signIn: null, error: null, rootMismatch: false, ownTrust: null, factorSetup: null });
  }

  /**
   * After a release this worker waits for the lock again, queued behind the new leader, as a tab that started while
   * another one led does: a hand-back (or the leader's tab closing) boots it from the store the other tab wrote. The
   * paused core, the stopped engine and the socket are dropped, so enterReady builds a fresh gateway and engine.
   */
  private awaitLeadership(instance: Instance): void {
    this.starting = Promise.resolve(null);
    this.barrier.reopen();
    void this.lead(instance);
  }

  // ---- sending ----

  /** The group a command of an open channel sends into: open, and its group active, else E_NOT_READY. */
  private activeGroup(channelId: string): Id {
    const entry = this.open.get(channelId);
    if (entry === undefined) throw new Refusal('E_NOT_READY', 'the channel is not open');
    const groups = this.requireCore().groups();
    const state = this.groupState(channelId, groups);
    const groupId = this.timelineGroup(channelId, groups);
    if (state !== 'active' || groupId === null) throw new Refusal('E_NOT_READY', `the channel's group is ${state}`);
    return groupId;
  }

  /** Requirement 9: the page has already encoded its mentions; the descriptors never enter a slice. */
  private async send(c: Extract<Command, { m: 'send' }>): Promise<{ msgId: string }> {
    const groupId = this.activeGroup(c.channelId);
    const ids = c.attachments ?? [];
    let descriptors: AttachmentDescriptor[] = [];
    if (ids.length > 0) {
      const tray = this.trays.get(c.channelId);
      if (tray === undefined) throw new Refusal('E_TRAY_NOT_READY', '');
      try { descriptors = tray.take(ids); }
      catch (e) {
        if (e instanceof Error && e.message === 'E_TRAY_NOT_READY') throw new Refusal('E_TRAY_NOT_READY', '');
        throw e;
      }
      // take reported through onChange already; the store drops the equal value (ruled: worker-ui 8).
      this.publishTray(c.channelId, tray.entries());
    }
    const request: SendRequest = {
      type: 0, replyTo: c.replyTo === undefined || c.replyTo === null ? null : fromHex(c.replyTo), body: c.text, attachments: descriptors,
      threadId: c.threadId === undefined || c.threadId === null ? null : fromHex(c.threadId),
    };
    let msgId: Id;
    // The core enforces the 4000-byte limit (E_ENVELOPE_LIMIT); the controller does not measure the text.
    try {
      msgId = await this.sendWithTrustRetry(groupId, request);
    } catch (e) {
      // The taken references were never sent; each is deleted now, or expires after pending_ttl (L-HTTP-83).
      const channel = fromHex(c.channelId);
      for (const d of descriptors) void this.barrier.track(this.routes.deleteBlob(channel, d.blobId).catch(() => undefined));
      throw e;
    }
    if (c.threadId !== undefined && c.threadId !== null) this.threadSends.set(toHex(msgId), { root: c.threadId, group: toHex(groupId) });
    // The thread is read again now that the msg id is recorded.
    this.refreshTimeline(c.channelId);
    return { msgId: toHex(msgId) };
  }

  /**
   * Queues any envelope type. Controller ruling 2: the core refuses a group holding a failed leaf. A device that joined
   * before the list naming it was ingested fails against the cached list until a refresh ingests the newer one, so the
   * group's leaves are read again and the request is tried once more; the core alone decides the second answer. A changed
   * key (E_UMK_CHANGED) is not retried.
   */
  private async sendWithTrustRetry(groupId: Id, request: SendRequest): Promise<Id> {
    try { return this.sendIntoThread(groupId, request); }
    catch (e) {
      if (!(e instanceof CoreError) || !LEAF_VERDICTS.has(e.code)) throw e;
      await this.refreshGroupTrust(groupId);
      return this.sendIntoThread(groupId, request);
    }
  }

  /** The synchronous core send. A thread reply names its thread first, so the publishes the outbox change makes inside
   *  the call already keep it out of the channel and show it in the thread. */
  private sendIntoThread(groupId: Id, request: SendRequest): Id {
    const sync = this.requireSync();
    if (request.threadId === null) return sync.sendRequest(groupId, request);
    const core = this.requireCore();
    this.threadIntent = { root: toHex(request.threadId), group: toHex(groupId), known: new Set(core.outbox(groupId).map((r) => toHex(r.msgId))) };
    try { return sync.sendRequest(groupId, request); }
    finally { this.threadIntent = null; }
  }

  /** Forgets which leaves of the group were already fetched and waits for a trust refresh that reads them again. */
  private async refreshGroupTrust(groupId: Id): Promise<void> {
    const prefix = `${toHex(groupId)}:`;
    for (const key of this.fetchedLeaves) if (key.startsWith(prefix)) this.fetchedLeaves.delete(key);
    if (this.trustRefresh !== null) this.trustRefreshAgain = true;
    await this.barrier.track(this.refreshTrust()).catch(() => undefined);
  }

  /** Pre-flight ruling 1(iv): a message id that no outbox holds is an idempotent no-op. */
  private inOutbox(msgId: string): boolean {
    const core = this.requireCore();
    return core.groups().some((g) => core.outbox(g.groupId).some((row) => toHex(row.msgId) === msgId));
  }

  private retrySend(msgId: string): Promise<null> {
    if (this.inOutbox(msgId)) this.requireSync().retry(fromHex(msgId));
    return Promise.resolve(null);
  }

  private discardSend(msgId: string): Promise<null> {
    if (this.inOutbox(msgId)) this.requireSync().discard(fromHex(msgId));
    return Promise.resolve(null);
  }

  // ---- say more: folds, purges, pins and files (L-TS-35) ----

  /** Requirement 10: an edit, delete, reaction or pin is queued as its fold type aimed at the message; the page's state
   *  changes only when the core folds it (a pending fold shows as a PendingAction). The core's refusals cross as they are. */
  private async fold(channelId: string, msgId: string, type: 1 | 2 | 3 | 4 | 5 | 6, body: string): Promise<null> {
    if (type === 1 || type === 2) {
      this.searchGate.invalidate();
      this.searchCursor = null;
      this.slices.set('search', { requestId: 0, hits: [], cursor: null });
    }
    const groupId = this.activeGroup(channelId);
    await this.sendWithTrustRetry(groupId, { type, replyTo: fromHex(msgId), body, attachments: [], threadId: null });
    this.refreshTimeline(channelId);
    return null;
  }

  private editMessage(channelId: string, msgId: string, text: string): Promise<null> { return this.fold(channelId, msgId, 1, text); }

  /** A timed-out member's owner-only DELETE path stays available. The server checks the uploader and emits the same
   * replayable tombstone to every reader; ordinary deletes remain type-2 folds. */
  private async deleteMessage(channelId: string, msgId: string, seq: string): Promise<null> {
    const groupId = this.activeGroup(channelId);
    const known = this.channels.get(channelId);
    const me = this.me;
    if (known !== undefined && me !== null) {
      const expires = await this.routes.getMemberTimeout(fromHex(known.communityId), me.userId);
      if (expires !== null) {
        if (!SEQ.test(seq)) throw new Refusal('E_BAD_INPUT', 'invalid message sequence');
        await this.routes.deleteGroupMessage(groupId, BigInt(seq));
        return null;
      }
    }
    return this.fold(channelId, msgId, 2, '');
  }

  /** A distinct human moderation path: validate local target/group and loaded authority, then let the instance recheck. */
  private async moderateRemoveMessage(channelId: string, msgId: string, rawSeq: string, reason: string, excerpt: string): Promise<{
    removal: 'accepted' | 'unknown' | 'rejected'; proofRequestId: string; evidenceRequestId: string | null;
    evidenceUpload: 'accepted' | 'pending' | 'rejected' | 'unknown';
  }> {
    const core = this.requireCore();
    if (!SEQ.test(rawSeq)) throw new Refusal('E_BAD_INPUT', 'invalid target sequence');
    const reasonBytes = new TextEncoder().encode(reason);
    const excerptBytes = new TextEncoder().encode(excerpt);
    if (reasonBytes.length === 0 || reasonBytes.length > 1024 || reason.trim().length === 0 || excerptBytes.length > 512) {
      throw new Refusal('E_BAD_INPUT', 'moderation reason or excerpt is outside its allowed bounds');
    }
    const seq = BigInt(rawSeq);
    const target = this.target(channelId);
    if (target?.communityId === null || target === undefined) throw new Refusal('E_BAD_INPUT', 'moderation removal needs a community channel');
    const communityId = target.communityId;
    const groups = core.groups();
    const groupId = this.timelineGroup(channelId, groups);
    const group = groupId === null ? undefined : groups.find(g => toHex(g.groupId) === toHex(groupId));
    if (group === undefined || group.kind !== 0 || group.state !== 2 || toHex(group.targetId) !== channelId ||
        group.communityId === null || toHex(group.communityId) !== communityId) {
      throw new Refusal('E_NOT_READY', 'the active community text group is unavailable');
    }
    const row = core.timeline(group.groupId, seq + 1n, 1).find(r => r.seq === seq);
    if (row?.msgId === null || row === undefined || toHex(row.msgId) !== msgId || row.status !== 0) {
      throw new Refusal('E_BAD_INPUT', 'target message is no longer available');
    }
    const access = this.slices.get(`permissions:${communityId}`);
    const roles = this.slices.get(`roles:${communityId}`);
    const members = this.slices.get(`members:${communityId}`);
    const overwrites = this.slices.get(`overwrites:${communityId}`);
    const owner = (this.slices.get('communities') ?? []).find(c => c.id === communityId)?.owner;
    const me = this.me === null ? null : toHex(this.me.userId);
    if (access?.loaded !== true || access.overwritesLoaded !== true || access.everyoneId === '' || roles === undefined ||
        members === undefined || overwrites === undefined || owner === undefined || me === null) {
      throw new Refusal('E_NOT_READY', 'current channel permissions are unavailable');
    }
    const bits = channelBitsOf({ owner, everyoneId: access.everyoneId, roles, members, overwrites: overwrites[channelId] ?? [] }, me);
    if (bits === null || (bits & PERM.manageMessages) === 0) throw new Refusal('E_FORBIDDEN', 'manage messages permission required');
    const pendingTarget = core.manualRemovalEvidence?.().find((entry) => toHex(entry.groupId) === toHex(group.groupId)
      && toHex(entry.messageId) === msgId && entry.targetSequence === seq);
    if (pendingTarget !== undefined) return this.resolveManualRemoval(toHex(pendingTarget.proofRequestId));
    const requestId = crypto.getRandomValues(new Uint8Array(16));
    const evidenceRequestId = crypto.getRandomValues(new Uint8Array(16));
    const now = BigInt(Math.floor(this.deps.now() / 1000));
    if (core.prepareManualRemovalEvidence === undefined || core.markManualRemovalSubmitted === undefined
      || core.markManualRemovalAccepted === undefined || core.completeManualRemovalEvidence === undefined
      || core.rejectManualRemoval === undefined || core.manualRemovalEvidence === undefined) {
      throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
    }
    const signed = core.prepareManualRemovalEvidence(requestId, evidenceRequestId, fromHex(communityId), group.groupId,
      // Reserve five seconds of the server's 30-second acceptance window for client/server clock skew.
      fromHex(channelId), fromHex(msgId), seq, reason, excerpt, now, now + 25n, now);
    // This durable transition precedes the request. A restart can now distinguish "never submitted"
    // from "POST may have succeeded" and only ever retries the same proof ID and signed bytes.
    core.markManualRemovalSubmitted(requestId);
    try {
      await this.routes.postModerationRemoval(group.groupId, signed);
    } catch (error) {
      if (this.definiteManualRemovalRefusal(error)) {
        core.rejectManualRemoval(requestId, BigInt(Math.floor(this.deps.now() / 1000)));
        return { removal: 'rejected', proofRequestId: toHex(requestId), evidenceRequestId: null, evidenceUpload: 'rejected' };
      }
      return { removal: 'unknown', proofRequestId: toHex(requestId), evidenceRequestId: toHex(evidenceRequestId), evidenceUpload: 'unknown' };
    }
    core.markManualRemovalAccepted(requestId);
    this.refreshTimeline(channelId, group.groupId);
    const proofRequestId = toHex(requestId);
    const evidenceRequestIdHex = toHex(evidenceRequestId);
    let evidenceUpload: 'accepted' | 'pending' | 'rejected' | 'unknown' = 'unknown';
    try {
      const evidence = await this.uploadManualRemovalEvidence({ proofRequestId, evidenceRequestId: evidenceRequestIdHex,
        communityId, reason, excerpt, now: BigInt(Math.floor(this.deps.now() / 1000)) });
      evidenceUpload = evidence.upload;
      if (evidenceUpload === 'accepted') core.completeManualRemovalEvidence(requestId, BigInt(Math.floor(this.deps.now() / 1000)));
    } catch {
      // The removal is already accepted. Return its status separately so the page
      // can see its evidence state; the durable outbox resumes it without another removal.
      evidenceUpload = 'unknown';
    }
    return { removal: 'accepted', proofRequestId, evidenceRequestId: evidenceRequestIdHex, evidenceUpload };
  }

  private definiteManualRemovalRefusal(error: unknown): boolean {
    // A server 4xx other than Request Timeout is an authoritative refusal; 408 and every
    // transport/5xx error can race with persistence and therefore remain unknown.
    return error instanceof DillaHttpError && error.status >= 400 && error.status < 500 && error.status !== 408;
  }

  private async resolveManualRemoval(proofRequestHex: string): Promise<{
    removal: 'accepted' | 'unknown' | 'rejected'; proofRequestId: string; evidenceRequestId: string | null;
    evidenceUpload: 'accepted' | 'pending' | 'rejected' | 'unknown';
  }> {
    const core = this.requireCore();
    if (core.manualRemovalEvidence === undefined || core.markManualRemovalSubmitted === undefined
      || core.markManualRemovalAccepted === undefined || core.completeManualRemovalEvidence === undefined) {
      throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
    }
    const proofRequestId = fromHex(proofRequestHex);
    let row = core.manualRemovalEvidence().find((entry) => toHex(entry.proofRequestId) === proofRequestHex);
    if (row === undefined) throw new Refusal('E_NOT_FOUND', 'manual removal request is no longer pending');
    if (row.state === 1) {
      try { await recoverModerationRemovals(core, this.routes, row.groupId, code => this.recordError(code)); } catch { /* absence or outage is not proof of rejection */ }
      row = core.manualRemovalEvidence().find((entry) => toHex(entry.proofRequestId) === proofRequestHex);
      if (row === undefined) throw new Refusal('E_NOT_FOUND', 'manual removal request is no longer pending');
    }
    if (row.state < 2) {
      const now = BigInt(Math.floor(this.deps.now() / 1000));
      if (row.expires <= now) {
        if (row.state === 0) core.rejectUnsubmittedManualRemoval?.(proofRequestId, now);
        return { removal: row.state === 0 ? 'rejected' : 'unknown', proofRequestId: proofRequestHex,
          evidenceRequestId: toHex(row.evidenceRequestId), evidenceUpload: 'unknown' };
      }
      try {
        core.markManualRemovalSubmitted(proofRequestId);
        await this.routes.postModerationRemoval(row.groupId, row.signedBody);
        core.markManualRemovalAccepted(proofRequestId);
      } catch (error) {
        if (this.definiteManualRemovalRefusal(error)) {
          core.rejectManualRemoval?.(proofRequestId, now);
          return { removal: 'rejected', proofRequestId: proofRequestHex, evidenceRequestId: null, evidenceUpload: 'rejected' };
        }
        return { removal: 'unknown', proofRequestId: proofRequestHex, evidenceRequestId: toHex(row.evidenceRequestId), evidenceUpload: 'unknown' };
      }
      row = core.manualRemovalEvidence().find((entry) => toHex(entry.proofRequestId) === proofRequestHex);
      if (row === undefined || row.state !== 2) return { removal: 'unknown', proofRequestId: proofRequestHex,
        evidenceRequestId: toHex(row?.evidenceRequestId ?? new Uint8Array(16)), evidenceUpload: 'unknown' };
    }
    let evidenceUpload: 'accepted' | 'pending' | 'rejected' | 'unknown' = 'unknown';
    try {
      const sent = await this.uploadManualRemovalEvidence({ proofRequestId: proofRequestHex,
        evidenceRequestId: toHex(row.evidenceRequestId), communityId: toHex(row.communityId), reason: row.reason,
        excerpt: row.excerpt, now: BigInt(Math.floor(this.deps.now() / 1000)) });
      evidenceUpload = sent.upload;
      if (evidenceUpload === 'accepted') core.completeManualRemovalEvidence(proofRequestId, BigInt(Math.floor(this.deps.now() / 1000)));
    } catch { evidenceUpload = 'unknown'; }
    return { removal: 'accepted', proofRequestId: proofRequestHex, evidenceRequestId: toHex(row.evidenceRequestId), evidenceUpload };
  }

  /** Reconcile crash-interrupted removals against the verified feed, then resume their same-ID op61 evidence. */
  private async resumeManualRemovalEvidence(): Promise<void> {
    const core = this.core;
    if (core?.manualRemovalEvidence === undefined || core.markManualRemovalSubmitted === undefined
      || core.markManualRemovalAccepted === undefined || core.completeManualRemovalEvidence === undefined) return;
    const initial = core.manualRemovalEvidence();
    for (const saved of initial) {
      if (this.revoked() || this.wiping || this.storeCleared) return;
      const group = core.groups().find((row) => toHex(row.groupId) === toHex(saved.groupId));
      if (group?.kind !== 0 || group.state !== 2 || group.communityId === null || toHex(group.communityId) !== toHex(saved.communityId)) continue;

      if (saved.state === 1) {
        // Feed ingestion promotes only an authenticated proof matching this exact actor, device,
        // group, community and sequence. A failed lookup leaves the outcome unknown.
        try { await recoverModerationRemovals(core, this.routes, saved.groupId, code => this.recordError(code)); } catch { /* retry next ready */ }
      }
      let current = core.manualRemovalEvidence().find((entry) => toHex(entry.proofRequestId) === toHex(saved.proofRequestId));
      if (current === undefined) continue;
      if (current.state === 0 || current.state === 1) {
        const now = BigInt(Math.floor(this.deps.now() / 1000));
        if (current.expires <= now) {
          if (current.state === 0) core.rejectUnsubmittedManualRemoval?.(current.proofRequestId, now);
          continue; // A submitted proof stays unknown unless the verified feed resolves it.
        }
        try {
          core.markManualRemovalSubmitted(current.proofRequestId);
          await this.routes.postModerationRemoval(current.groupId, current.signedBody);
          core.markManualRemovalAccepted(current.proofRequestId);
        } catch { continue; } // exact replay is server-idempotent; state 1 remains an explicit unknown.
        current = core.manualRemovalEvidence().find((entry) => toHex(entry.proofRequestId) === toHex(saved.proofRequestId));
      }
      if (current?.state !== 2) continue;
      try {
        const result = await this.uploadManualRemovalEvidence({ proofRequestId: toHex(current.proofRequestId),
          evidenceRequestId: toHex(current.evidenceRequestId), communityId: toHex(current.communityId),
          reason: current.reason, excerpt: current.excerpt, now: BigInt(Math.floor(this.deps.now() / 1000)) });
        if (result.upload === 'accepted') core.completeManualRemovalEvidence(current.proofRequestId, BigInt(Math.floor(this.deps.now() / 1000)));
      } catch { /* same durable evidence remains for the next ready */ }
    }
  }

  private uploadManualRemovalEvidence(input: { proofRequestId: string; evidenceRequestId: string; communityId: string; reason: string; excerpt: string; now: bigint }) {
    const core = this.requireCore();
    if (core.ensureManualRemovalEvidenceRequest === undefined) throw new CoreError('E_CORE_STATE', 'durable moderation evidence is not available in this WASM build');
    const reserved = core.ensureManualRemovalEvidenceRequest(fromHex(input.proofRequestId), crypto.getRandomValues(new Uint8Array(16)), input.now);
    return this.humanInteractionRequest({ m: 'humanInteractionRequest', communityId: input.communityId, channelId: null,
      requestId: toHex(reserved.requestId), operation: 61, args: [input.proofRequestId, input.reason, input.excerpt], revision: '0' }, reserved.expires);
  }

  /** Replays the idempotent accepted warning and resumes its op61 case from the protected local outbox. */
  private async resumeManualWarningEvidence(): Promise<void> {
    const core = this.core;
    if (core?.manualWarningEvidence === undefined || core.markManualWarningAccepted === undefined
      || core.ensureManualWarningEvidenceRequest === undefined || core.completeManualWarningEvidence === undefined) return;
    for (const saved of core.manualWarningEvidence()) {
      if (this.revoked() || this.wiping || this.storeCleared) return;
      if (!this.listed(toHex(saved.communityId))) continue;
      try {
        let row = saved;
        if (row.state === 0) {
          const receipt = await this.routes.warnMember(row.communityId, row.targetUserId, row.requestId);
          core.markManualWarningAccepted(row.requestId, receipt.actionId, BigInt(receipt.revision), BigInt(receipt.acceptedAt));
          row = core.manualWarningEvidence().find(item => toHex(item.requestId) === toHex(saved.requestId)) ?? row;
        }
        if (row.state !== 1) continue;
        const now = BigInt(Math.floor(this.deps.now() / 1000));
        const reserved = core.ensureManualWarningEvidenceRequest(row.requestId, crypto.getRandomValues(new Uint8Array(16)), now);
        const result = await this.humanInteractionRequest({ m: 'humanInteractionRequest', communityId: toHex(row.communityId), channelId: null,
          requestId: toHex(reserved.requestId), operation: 61,
          args: [toHex(row.requestId), 'warn', row.reason, ''], revision: '0' }, reserved.expires);
        if (result.upload === 'accepted') core.completeManualWarningEvidence(row.requestId, BigInt(Math.floor(this.deps.now() / 1000)));
      } catch { /* retain the protected outbox row for the next ready */ }
    }
  }

  /** Ruling 28: the worker does not de-duplicate reactions; the core folds them per user. */
  private react(channelId: string, msgId: string, emoji: string, on: boolean): Promise<null> {
    return this.fold(channelId, msgId, on ? 3 : 4, emoji);
  }

  private pin(channelId: string, msgId: string, on: boolean): Promise<null> { return this.fold(channelId, msgId, on ? 5 : 6, ''); }

  /** CB2: the community channel whose text group g is (the channels table's textGroupId, else the group's target when that
   *  is a listed channel); a DM group has none. */
  private slowChannel(g: Id): { channelId: string; slowmode: number } | null {
    const hex = toHex(g);
    for (const [channelId, { row }] of this.channels) {
      if (row.textGroupId !== null && toHex(row.textGroupId) === hex) return { channelId, slowmode: row.slowmode };
    }
    const target = this.core?.groupRow(g)?.targetId;
    const known = target === undefined ? undefined : this.channels.get(toHex(target));
    return target === undefined || known === undefined ? null : { channelId: toHex(target), slowmode: known.row.slowmode };
  }

  /** CB2 (R6): the engine requeued a post after a 429 (queued) or confirmed it after the wait (untilMs 0, queued false).
   *  A wait is published only for a channel with slow mode on; a clear only where a wait was published. The countdown
   *  is published whether or not the person holds bypass_slowmode: the page hides it for them (WEB-B). */
  private onSlow(g: Id, untilMs: number, queued: boolean): void {
    const channel = this.slowChannel(g);
    if (channel === null) return;
    if (untilMs === 0 && !queued) { if (!this.slowUntil.has(channel.channelId)) return; }
    else if (channel.slowmode <= 0) return;
    this.publishSlow(channel.channelId, untilMs, queued);
  }

  /** CB2: a confirmed own post (type 0) starts the channel's countdown of slowmode seconds; a fold never does. */
  private onPosted(g: Id, type: number, posted?: OutboxRow, confirmedSeq?: bigint): void {
    if ((type === 3 || type === 4) && posted?.replyTo !== null && posted?.replyTo !== undefined) {
      const core = this.core;
      const group = core?.groupRow(g);
      const me = this.me;
      if (core !== null && core !== undefined && group !== null && group !== undefined && me !== null) {
        const channelId = toHex(group.targetId);
        const channel = this.target(channelId);
        if (channel?.communityId !== null && channel?.communityId !== undefined) {
          const targetTimeline = this.slices.get(`timeline:${channelId}`);
          const target = targetTimeline?.items.find(item => item.msgId === toHex(posted.replyTo!));
          if (confirmedSeq !== undefined && target?.state === 'ok' && target.seq !== null) {
            const key = PENDING_ROLE_PANEL_ACTION_PREFIX + toHex(posted.msgId);
            const action: PendingRolePanelAction = {
              communityId: channel.communityId, channelId, messageSeq: target.seq, groupId: toHex(g), eventSeq: confirmedSeq.toString(),
              emoji: posted.body, enabled: type === 3, userId: toHex(me.userId), deviceId: toHex(me.deviceId),
            };
            try {
              core.settingPut(key, JSON.stringify(action));
              void this.barrier.track(this.submitPendingRolePanelAction(core, key, action));
            } catch { /* Durable retry is unavailable until the next confirmed event. */ }
          }
        }
      }
    }
    if (type !== 0) return;
    const channel = this.slowChannel(g);
    if (channel === null || channel.slowmode <= 0) return;
    this.publishSlow(channel.channelId, this.deps.now() + channel.slowmode * 1000, false);
  }

  private retryPendingRolePanelActions(core: CorePort, me: { userId: Id; deviceId: Id }): void {
    this.clearPendingRolePanelActionRetries();
    let settings: Record<string, string>;
    try { settings = core.settings(); } catch { return; }
    for (const [key, value] of Object.entries(settings)) {
      if (!key.startsWith(PENDING_ROLE_PANEL_ACTION_PREFIX)) continue;
      let action: PendingRolePanelAction;
      try { action = JSON.parse(value) as PendingRolePanelAction; } catch { core.settingDelete(key); continue; }
      const valid = [action.communityId, action.channelId, action.groupId, action.userId, action.deviceId]
        .every(value => /^[0-9a-f]{32}$/.test(value))
        && /^\d+$/.test(action.messageSeq) && /^\d+$/.test(action.eventSeq)
        && typeof action.emoji === 'string' && new TextEncoder().encode(action.emoji).length >= 1
        && new TextEncoder().encode(action.emoji).length <= 32 && typeof action.enabled === 'boolean';
      if (!valid || action.userId !== toHex(me.userId) || action.deviceId !== toHex(me.deviceId)) {
        core.settingDelete(key);
        continue;
      }
      void this.barrier.track(this.submitPendingRolePanelAction(core, key, action));
    }
  }

  private schedulePendingRolePanelActionRetry(core: CorePort, key: string, action: PendingRolePanelAction, attempt: number): void {
    if (attempt > ROLE_PANEL_ACTION_RETRY_LIMIT || this.phase !== 'ready' || this.connectionState.status !== 'online'
      || this.pendingRolePanelActionRetries.has(key)) return;
    const delay = Math.min(ROLE_PANEL_ACTION_RETRY_BASE_MS * 2 ** (attempt - 1), ROLE_PANEL_ACTION_RETRY_MAX_MS);
    const timer = this.deps.setTimeout(() => {
      this.pendingRolePanelActionRetries.delete(key);
      if (this.phase !== 'ready' || this.connectionState.status !== 'online' || this.core !== core) return;
      void this.barrier.track(this.submitPendingRolePanelAction(core, key, action, attempt));
    }, delay);
    this.pendingRolePanelActionRetries.set(key, timer);
  }

  private clearPendingRolePanelActionRetries(): void {
    for (const timer of this.pendingRolePanelActionRetries.values()) this.deps.clearTimeout(timer);
    this.pendingRolePanelActionRetries.clear();
  }

  private async submitPendingRolePanelAction(core: CorePort, key: string, action: PendingRolePanelAction, attempt = 0): Promise<void> {
    if (this.pendingRolePanelActionJobs.has(key)) return;
    this.pendingRolePanelActionJobs.add(key);
    try {
      await this.routes.applyRolePanelReaction(fromHex(action.communityId), fromHex(action.channelId), BigInt(action.messageSeq),
        fromHex(action.groupId), BigInt(action.eventSeq), action.emoji, action.enabled);
      core.settingDelete(key);
      // The action endpoint is an authenticated state change; read the canonical member roles back
      // now so this device reflects its grant even if its community_changed frame is delayed/lost.
      try { await this.reloadMembers(action.communityId); } catch { /* the next community_changed or gateway ready refreshes it */ }
    } catch (e) {
      // Preserve only outcomes that may succeed on retry. A definitive refusal is terminal and must not become a later
      // grant after policy or permissions change. Replays reuse the exact confirmed event and server source key.
      const retryable = e instanceof DillaHttpError && (e.status === 0 || e.status === 408 || e.status === 425 || e.status === 429 || e.status >= 500);
      if (retryable) this.schedulePendingRolePanelActionRetry(core, key, action, attempt + 1);
      else if (e instanceof DillaHttpError) core.settingDelete(key);
    } finally {
      this.pendingRolePanelActionJobs.delete(key);
    }
  }

  private publishSlow(channelId: string, until: number, queued: boolean): void {
    this.slowUntil.set(channelId, until);
    this.slices.set(`slow:${channelId}`, { until, queued });
  }

  /** A text group targets its channel or DM (web-2a). */
  private channelOfGroup(g: Id): Id {
    const row = this.requireCore().groupRow(g);
    if (row === null) throw new CoreError('E_CORE_STATE', 'unknown group');
    return row.targetId;
  }

  /** Requirement 11: each reference of the row is confirmed, in order, before every upload attempt; the first refusal
   *  propagates to the engine. Only the uploading user can confirm (L-HTTP-82), so a refusal answers this user's own
   *  upload: a 404 means the reference expired or this user deleted it, and the person attaches again. */
  private async beforeSend(g: Id, row: OutboxRow): Promise<void> {
    const channel = this.channelOfGroup(g);
    for (const a of row.attachments) await this.routes.confirmBlob(channel, a.blobId);
  }

  /** A discarded row's references are deleted, not awaited; a failure is left to the pending_ttl sweep (L-HTTP-83). */
  private onDiscarded(g: Id, row: OutboxRow): void {
    let channel: Id;
    try { channel = this.channelOfGroup(g); } catch { return; }
    for (const a of row.attachments) void this.barrier.track(this.routes.deleteBlob(channel, a.blobId).catch(() => undefined));
  }

  /** Requirement 12, single flight: a call during a run makes the run go once more after it ends. */
  private runPurges(): void {
    if (this.purging !== null) { this.purgeAgain = true; return; }
    const run = (async () => {
      try {
        do { this.purgeAgain = false; await this.purgeOnce(); } while (this.purgeAgain);
      } finally {
        this.purging = null;
      }
    })();
    this.purging = run;
    void this.barrier.track(run).catch(() => undefined);
  }

  /** Read through a call: a wipe or a revocation can land in any await of a purge run. */
  private purgeable(core: CorePort): boolean {
    return this.core === core && this.me !== null && this.sync !== null && this.phase === 'ready' && !this.wiping && !this.storeCleared;
  }

  /** Carries out this device's own record of its own deletes, in order. A row of a group this device is not an active
   *  member of is kept: the delivery service's 404 to a non-member is its membership refusal, not completion (L-HTTP-80,
   *  FACTS-SECURITY-08), so the row runs once the group is back in state 2. Any other failure keeps the row for the next
   *  trigger and the run moves on. Residual (ruling 8): a device that never rejoins keeps its row until retention. */
  private async purgeOnce(): Promise<void> {
    const core = this.core;
    if (core === null || !this.purgeable(core)) return;
    let rows: ReturnType<CorePort['purges']>;
    try { rows = core.purges(); } catch { return; }
    for (const row of rows) {
      if (!this.purgeable(core)) return;
      try {
        const group = core.groupRow(row.groupId);
        if (group === null || group.state !== 2) continue;
        await this.routes.deleteGroupMessage(row.groupId, row.seq);          // 'deleted' and 'gone' are both done
        if (!this.purgeable(core)) return;
        for (const blobId of row.blobIds) {
          try { await this.routes.deleteBlob(row.channelId, blobId); }
          catch (e) {
            // 403: the reference is not this user's (another uploader's file); nothing of ours is left to delete.
            if (!(e instanceof DillaHttpError && e.status === 403)) throw e;
          }
          if (!this.purgeable(core)) return;
        }
        if (!this.purgeable(core)) return;
        core.purgeDone(row.groupId, row.seq);
      } catch { /* this row waits for the next trigger */ }
    }
  }

  /** Requirement 14. */
  private loadPins(channelId: string): Promise<null> {
    if (this.target(channelId) === undefined) throw new Refusal('E_BAD_INPUT', 'unknown channel');
    this.pinsOpen.add(channelId);
    this.publishPins(channelId);
    return Promise.resolve(null);
  }

  /** The pins slice keeps its last value. */
  private closePins(channelId: string): Promise<null> {
    this.pinsOpen.delete(channelId);
    return Promise.resolve(null);
  }

  private publishPins(channelId: string): void {
    const core = this.core;
    if (core === null) return;
    const g = this.timelineGroup(channelId, core.groups());
    const items: PinnedItem[] = g === null ? [] : core.pins(g).map((p) => ({
      msgId: toHex(p.msgId), seq: p.targetSeq.toString(), senderUser: p.author === null ? null : toHex(p.author), excerpt: p.excerpt,
      pinnedBy: toHex(p.byUser), pinnedSeq: p.pinnedSeq.toString(), ts: Number(p.targetTs),
      threadRoot: p.threadRoot === null ? null : toHex(p.threadRoot),
    }));
    this.slices.set(`pins:${channelId}`, items);
  }

  /** The thumbnail seam: a unit test passes its own; else the worker global, when it can draw off screen. */
  private thumbDeps(): ThumbDeps | null {
    if (this.deps.thumbs !== undefined) return this.deps.thumbs;
    const scope = globalThis as { createImageBitmap?: unknown; OffscreenCanvas?: unknown };
    if (typeof scope.createImageBitmap !== 'function' || typeof scope.OffscreenCanvas !== 'function') return null;
    return { createImageBitmap: (b) => createImageBitmap(b), offscreen: (w, h) => new OffscreenCanvas(w, h) };
  }

  /** The slice entry names the file and its phase only: never the descriptor (Global Constraints "Never written"). */
  private publishTray(channelId: string, entries: readonly TrayEntry[]): void {
    const item = (e: TrayEntry): TrayItem => ({ id: e.id, name: e.name, size: e.size, mime: e.mime, image: e.image, phase: e.phase, reason: e.reason });
    this.slices.set(`tray:${channelId}`, entries.map(item));
  }

  private trayFor(channelId: string): Tray {
    const held = this.trays.get(channelId);
    if (held !== undefined) return held;
    const tray: Tray = new Tray({
      channelId: fromHex(channelId), routes: this.routes, thumb: this.thumbDeps(),
      random: (n) => crypto.getRandomValues(new Uint8Array(n)),
      // A tray dropped by a wipe or a reset publishes nothing more.
      onChange: (entries) => { if (this.trays.get(channelId) === tray) this.publishTray(channelId, entries); },
    });
    this.trays.set(channelId, tray);
    return tray;
  }

  /** Requirement 15: wipe and resetOnce drop every tray without a request; the pending references expire (L-HTTP-83). */
  private dropTrays(): void {
    for (const channelId of this.trays.keys()) this.slices.set(`tray:${channelId}`, []);
    this.trays.clear();
  }

  private async attachFiles(channelId: string, files: readonly File[]): Promise<{ trayIds: string[] }> {
    if (this.target(channelId) === undefined) throw new Refusal('E_BAD_INPUT', 'unknown channel');
    return { trayIds: await this.trayFor(channelId).add(files) };
  }

  private async discardAttachment(channelId: string, trayId: string): Promise<null> {
    await this.trays.get(channelId)?.discard(trayId);
    return null;
  }

  /** Requirement 16: the decrypted Blob crosses in the ret (structured clone); the worker keeps no copy and mints no URL. */
  private async openAttachment(channelId: string, seq: string, index: number, thumb: boolean): Promise<{ blob: Blob; name: string; mime: string }> {
    const core = this.requireCore();
    const g = this.timelineGroup(channelId, core.groups());
    if (g === null) throw new Refusal('E_NOT_READY', 'the channel has no group');
    const d = core.attachmentGet(g, BigInt(seq), index);
    if (thumb && d.thumb === null) throw new AttachmentError('E_ATTACHMENT_MISSING', 'no thumbnail');
    const blob = thumb ? await thumbnailBlob(d) : await fetchAttachment(this.routes, fromHex(channelId), d);
    return { blob, name: safeName(d.name, ''), mime: d.mime };
  }

  // ---- invariants ----

  private requireInstance(): Instance {
    if (this.instance === null) throw new CoreError('E_CORE_STATE', 'no instance');
    return this.instance;
  }

  private requireCore(): CorePort {
    if (this.core === null) throw new CoreError('E_CORE_STATE', 'no store');
    return this.core;
  }

  private requireSignup(): Signup {
    if (this.signup === null) throw new CoreError('E_CORE_STATE', 'no signup');
    return this.signup;
  }

  private requireEnrol(): Enrol {
    if (this.enrol === null) throw new CoreError('E_CORE_STATE', 'no enrolment');
    return this.enrol;
  }

  private requireMe(): { userId: Id; deviceId: Id } {
    if (this.me === null) throw new CoreError('E_CORE_NO_IDENTITY', '');
    return this.me;
  }

  private requireSync(): SyncEngine {
    if (this.sync === null) throw new CoreError('E_CORE_STATE', 'no sync');
    return this.sync;
  }
}
