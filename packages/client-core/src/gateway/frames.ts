import { decode, encode, type CborInput, type CborValue } from '../cbor';

export interface Frame {
  op: number;
  n: bigint;
  groupId: Uint8Array | null;
  payload: CborValue[];
}

export const Op = {
  hello: 0, identify: 1, ready: 3, invalidSession: 5, heartbeat: 6, heartbeatAck: 7,
  reconnect: 8, error: 9, commitAck: 12, handshake: 16, commitNeeded: 17,
  epochChanged: 18, messageCt: 19, welcome: 20, messageDeleted: 21,
  presence: 48, typing: 49, voiceState: 50, typingSet: 51, voiceSelf: 52, callRing: 53, communityChanged: 54, afkAway: 55, appBuild: 56, presenceActivity: 57, callStats: 58,
  request: 59, response: 60,
} as const;

const SPECS: ReadonlyMap<number, { elements: number; grouped: boolean }> = new Map([
  [0, 9], [1, 5], [2, 4], [3, 10], [4, 3], [5, 2], [6, 2], [7, 1], [8, 2],
  [9, 3], [10, 1], [11, 1], [12, 1], [16, 5], [17, 4], [18, 2], [19, 7],
  [20, 6], [21, 2], [32, 7], [33, 4], [48, 4], [49, 5], [50, 4], [51, 2], [52, 2], [53, 5], [54, 1], [55, 3], [56, 1], [57, 0], [58, 2], [59, 5], [60, 3],
].map(([op, elements]) => [op, { elements, grouped: op === 12 || (op >= 16 && op <= 21) }]));

function shape(): never {
  throw new Error('E_FRAME_SHAPE');
}

export function encodeFrame(op: number, n: number, groupId: Uint8Array | null, payload: readonly CborInput[]): Uint8Array {
  if (!Number.isInteger(op) || op < 0 || op > 255 || !Number.isSafeInteger(n) || n < 0
    || (groupId !== null && (!(groupId instanceof Uint8Array) || groupId.length !== 16))) shape();
  if (op === Op.typing && (typeof payload[4] === 'number' || typeof payload[4] === 'bigint') && payload[4] < 0) shape();
  return encode([op, n, groupId, payload]);
}

export function decodeFrame(bytes: Uint8Array): Frame {
  const value = decode(bytes);
  if (!Array.isArray(value) || value.length !== 4) shape();
  const [op, n, groupId, payload] = value;
  if (typeof op !== 'bigint' || op > 255n || typeof n !== 'bigint'
    || (groupId !== null && (!(groupId instanceof Uint8Array) || groupId.length !== 16))
    || !Array.isArray(payload)) shape();
  const spec = SPECS.get(Number(op));
  // op 19 message.ct (web3.7): seven elements, the 7th the upload class (0, 1 or null); six from a web3.6 host that
  // predates the class (web3.7 only). Checked before the generic arity rule.
  if (op === 19n && !(payload.length === 6 || (payload.length === 7 && classOrNull(payload[6])))) shape();
  if (spec !== undefined && ((op !== 19n && payload.length !== spec.elements) || spec.grouped !== (groupId !== null))) shape();
  if (op === 49n && (!isId(payload[0]) || !isId(payload[1]) || !isId(payload[2]) || !bit(payload[3]) || !uint(payload[4]))) shape();
  // op 50 voice_state: its flags are the five bits of 02 § voice_state (0..31); op 52's own flags stay 0..3.
  if (op === 50n && (!isId(payload[0]) || !isId(payload[1]) || !isId(payload[2]) || !voiceFlags(payload[3]))) shape();
  if (op === 51n && (n !== 0n || !isId(payload[0]) || !bit(payload[1]))) shape();
  if (op === 52n && (n !== 0n || !isId(payload[0]) || !flags(payload[1]))) shape();
  // op 53 call_ring (L-GW-33): [call_id, channel_id, user_id, state 0..2, expires_ts].
  if (op === 53n && (n !== 0n || !isId(payload[0]) || !isId(payload[1]) || !isId(payload[2]) || !ringState(payload[3]) || !uint(payload[4]))) shape();
  // op 54 community_changed (dilla-web-3.1): [community_id].
  if (op === 54n && (n !== 0n || !isId(payload[0]))) shape();
  // op 55 afk_away (AW4): [community_id, user_id, since_ts], since_ts 0 = cleared.
  if (op === 55n && (n !== 0n || !isId(payload[0]) || !isId(payload[1]) || !uint(payload[2]))) shape();
  if (op === 56n && (n !== 0n || !validAppEntry(payload[0]))) shape();
  // op 59 request (Bot v2): [id, method, route, body, idem | null]; op 60 response: [id, status 100..599, body].
  if (op === 59n && !validRequestPayload(payload)) shape();
  if (op === 60n && (n !== 0n || !uint(payload[0]) || !httpStatus(payload[1]) || !(payload[2] instanceof Uint8Array))) shape();
  return { op: Number(op), n, groupId, payload };
}

/** The socket request shim (Bot v2): one HTTP-shaped request over the gateway, answered by op 60 with its id. */
export interface SocketRequestFrame { id: number; method: string; route: string; body: Uint8Array; idem: Uint8Array | null }
export interface SocketResponseFrame { id: number; status: number; body: Uint8Array }

/** The instance's bounds on op 59 (internal/gateway/request.go): a method of at most 16 bytes, a route of at most
 *  8192, an idem of 1..64 bytes or null. */
const MAX_METHOD = 16;
const MAX_ROUTE = 8192;
const MAX_IDEM = 64;
/** The instance's request body cap (gateway.MaxRequestBodyBytes): a larger body is refused E_FRAME_LIMIT and closes the
 *  socket, so a client that queued it would re-send it after every resume. */
export const MAX_REQUEST_BODY = 16 * 1024 * 1024;
/** The largest response frame a client reads: a route's answer is capped at MAX_REQUEST_BODY by default, plus the frame's
 *  own bytes (the instance's maxRequestFrameBytes is the same bound). It applies to op 60 only; every other frame keeps the
 *  hello's max_frame_bytes. */
export const MAX_RESPONSE_FRAME = MAX_REQUEST_BODY + 16 * 1024;

/** True when the bytes begin a four-element frame whose op is 60 (0x84, then uint8 60), read without decoding the body. */
export function isResponseFrameHead(bytes: Uint8Array): boolean {
  return bytes.length >= 3 && bytes[0] === 0x84 && bytes[1] === 0x18 && bytes[2] === Op.response;
}

function validRequestPayload(payload: CborValue[]): boolean {
  const [id, method, route, body, idem] = payload;
  return uint(id) && typeof method === 'string' && method.length > 0 && utf8Length(method) <= MAX_METHOD
    && typeof route === 'string' && route.length > 0 && utf8Length(route) <= MAX_ROUTE && body instanceof Uint8Array
    && (idem === null || (idem instanceof Uint8Array && idem.length > 0 && idem.length <= MAX_IDEM));
}

const utf8Length = (s: string): number => new TextEncoder().encode(s).length;
const httpStatus = (value: CborValue | undefined): boolean => typeof value === 'bigint' && value >= 100n && value <= 599n;

/** op 59 [id, method, route, body, idem | null], n = 0 and no group: neither frame is replayable. A request the instance
 *  would refuse as E_FRAME_SHAPE is refused here, before it is sent. */
export function encodeRequestFrame(r: SocketRequestFrame): Uint8Array {
  if (!Number.isSafeInteger(r.id) || r.id < 0) shape();
  if (r.body.length > MAX_REQUEST_BODY) throw new Error('E_FRAME_LIMIT');
  const payload = [r.id, r.method, r.route, r.body, r.idem];
  if (!validRequestPayload(payload.map((v) => (typeof v === 'number' ? BigInt(v) : v)))) shape();
  return encodeFrame(Op.request, 0, null, payload);
}

/** op 60 [id, status, body], checked as decodeFrame checks it; any other frame, or an id past the safe integers, is
 *  E_FRAME_SHAPE. */
export function decodeResponseFrame(bytes: Uint8Array): SocketResponseFrame {
  const frame = decodeFrame(bytes);
  const [id, status, body] = frame.payload;
  if (frame.op !== Op.response || frame.groupId !== null || frame.payload.length !== 3 || !uint(id)
    || (id as bigint) > BigInt(Number.MAX_SAFE_INTEGER) || !httpStatus(status) || !(body instanceof Uint8Array)) shape();
  return { id: Number(id), status: Number(status), body };
}

const isId = (value: CborValue | undefined): boolean => value instanceof Uint8Array && value.length === 16;
const uint = (value: CborValue | undefined): boolean => typeof value === 'bigint' && value >= 0n;
const bit = (value: CborValue | undefined): boolean => value === 0n || value === 1n;
const classOrNull = (value: CborValue | undefined): boolean => value === null || bit(value);
const ringState = (value: CborValue | undefined): boolean => value === 0n || value === 1n || value === 2n;
const flags = (value: CborValue | undefined): boolean => typeof value === 'bigint' && value >= 0n && value <= 3n;
const voiceFlags = (value: CborValue | undefined): boolean => typeof value === 'bigint' && value >= 0n && value <= 31n;

/** The ops whose malformed frame the gateway drops without closing the socket: the server's ephemeral voice signals
 *  (L-GW-32, L-GW-33), the structure notice (op 54) and the away roster notice (op 55, AW4). A bad frame of any other op
 *  is still a protocol failure that closes it. */
export const DROPPABLE_OPS: ReadonlySet<number> = new Set([Op.voiceState, Op.callRing, Op.communityChanged, Op.afkAway, Op.appBuild]);

/** The op of a frame whose envelope is a four-element array with an op of at most 255, else null; nothing else is read. */
export function frameOp(bytes: Uint8Array): number | null {
  try {
    const value = decode(bytes);
    if (!Array.isArray(value) || value.length !== 4 || typeof value[0] !== 'bigint' || value[0] > 255n) return null;
    return Number(value[0]);
  } catch {
    return null;
  }
}

export interface VoiceStateFrame { userId: Uint8Array; deviceId: Uint8Array; callId: Uint8Array; flags: number }
export interface CallRingFrame { callId: Uint8Array; channelId: Uint8Array; userId: Uint8Array; state: 0 | 1 | 2; expiresTs: number }

/** op 50 [user_id, device_id, call_id, flags], as decodeFrame checked it; any other frame is E_FRAME_SHAPE. */
export function voiceStateOf(frame: Frame): VoiceStateFrame {
  const [user, device, call, bits] = frame.payload;
  if (frame.op !== Op.voiceState || frame.payload.length !== 4 || !isId(user) || !isId(device) || !isId(call) || !voiceFlags(bits)) shape();
  return { userId: user as Uint8Array, deviceId: device as Uint8Array, callId: call as Uint8Array, flags: Number(bits) };
}

/** op 53 [call_id, channel_id, user_id, state, expires_ts] (L-GW-33); an expiry past the safe integers is a shape error. */
export function callRingOf(frame: Frame): CallRingFrame {
  const [call, channel, user, state, expires] = frame.payload;
  if (frame.op !== Op.callRing || frame.payload.length !== 5 || !isId(call) || !isId(channel) || !isId(user) || !ringState(state)
    || !uint(expires) || (expires as bigint) > BigInt(Number.MAX_SAFE_INTEGER)) shape();
  return { callId: call as Uint8Array, channelId: channel as Uint8Array, userId: user as Uint8Array, state: Number(state) as 0 | 1 | 2,
    expiresTs: Number(expires) };
}

/** op 54 [community_id], as decodeFrame checked it; any other frame is E_FRAME_SHAPE. */
export function communityChangedOf(frame: Frame): Uint8Array {
  const [community] = frame.payload;
  if (frame.op !== Op.communityChanged || frame.payload.length !== 1 || !isId(community)) shape();
  return community as Uint8Array;
}

export interface AFKAwayFrame { communityId: Uint8Array; userId: Uint8Array; since: number }

/** op 55 [community_id, user_id, since_ts] (AW4), since 0 = cleared; a since past the safe integers, or any other frame,
 *  is E_FRAME_SHAPE. */
export function afkAwayOf(frame: Frame): AFKAwayFrame {
  const [community, user, since] = frame.payload;
  if (frame.op !== Op.afkAway || frame.payload.length !== 3 || !isId(community) || !isId(user) || !uint(since)
    || (since as bigint) > BigInt(Number.MAX_SAFE_INTEGER)) shape();
  return { communityId: community as Uint8Array, userId: user as Uint8Array, since: Number(since) };
}

/** Same bounded entry grammar as the verified web handler; never a URL or parent traversal. */
export const validAppEntry = (value: unknown): value is string => typeof value === 'string' && value.length <= 512
  && /^\/assets\/[A-Za-z0-9_./-]+\.js$/.test(value) && !value.includes('..');
