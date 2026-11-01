import { arr, decode, encode, u64, type CborInput, type CborValue } from '../cbor/index.ts';

const MAX = (1n << 64n) - 1n;
const enc = new TextEncoder();
const unicodeWhitespaceOnly = /^\p{White_Space}*$/u;
const PREFIX = 'dilla.bot/1:';
const HELPER_MAX = 1_048_576;

export class BotError extends Error {
  constructor(readonly code: string) { super(code); this.name = 'BotError'; }
}
const fail = (code = 'E_BOT_INVALID'): never => { throw new BotError(code); };
function guarded<T>(run: () => T): T {
  try { return run(); } catch (e) { if (e instanceof BotError) throw e; return fail('E_BOT_CBOR'); }
}
const bytes = (v: unknown, n?: number): Uint8Array => {
  if (!(v instanceof Uint8Array) || (n !== undefined && v.length !== n)) fail('E_BOT_SHAPE');
  return v as Uint8Array;
};
const id = (v: unknown): Uint8Array => { const b = bytes(v, 16); if (b.every(x => x === 0)) fail('E_BOT_ID'); return b; };
const uint = (v: unknown, min = 0n, max = MAX): bigint => {
  if (typeof v === 'number' && Number.isSafeInteger(v) && v >= 0) v = BigInt(v);
  if (typeof v !== 'bigint' || v < min || v > max) fail('E_BOT_RANGE');
  return v as bigint;
};
const nullable = (v: unknown, read: (v: unknown) => unknown) => v === null ? null : read(v);
function text(v: unknown, min: number, max: number, controls = false): string {
  if (typeof v !== 'string') fail('E_BOT_SHAPE');
  const s = v as string; const n = enc.encode(s).length;
  if (n < min || n > max || (controls && /[\u0000-\u001f\u007f]/u.test(s))) fail('E_BOT_RANGE');
  return s;
}
const cborBytes = (v: unknown, min: number, max: number): Uint8Array => {
  const b = bytes(v); if (b.length < min || b.length > max) fail('E_BOT_RANGE');
  const value = decode(b); if (!Array.isArray(value)) fail('E_BOT_SHAPE'); return b;
};
const readId = (v: CborValue) => id(v);
const readOptId = (v: CborValue) => nullable(v, x => readId(x as CborValue)) as Uint8Array | null;
const readOptU = (v: CborValue) => nullable(v, x => u64(x as CborValue)) as bigint | null;
const base64url = (b: Uint8Array) => {
  let s = ''; for (const x of b) s += String.fromCharCode(x);
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/u, '');
};
function unbase64url(s: string): Uint8Array {
  if (!/^[A-Za-z0-9_-]+$/u.test(s)) fail('E_BOT_BASE64');
  const padded = s.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - s.length % 4) % 4);
  let raw: string; try { raw = atob(padded); } catch { return fail('E_BOT_BASE64'); }
  const out = Uint8Array.from(raw, c => c.charCodeAt(0));
  if (base64url(out) !== s) fail('E_BOT_BASE64'); return out;
}
function bodyEncode(value: CborInput, max = 4000): string {
  const b = encode(value); const s = PREFIX + base64url(b);
  if (enc.encode(s).length > max) fail('E_BOT_SIZE'); return s;
}
function bodyDecode(body: string, max = 4000): CborValue {
  if (typeof body !== 'string' || enc.encode(body).length > max || !body.startsWith(PREFIX)) fail('E_BOT_BODY');
  try { return decode(unbase64url(body.slice(PREFIX.length))); } catch (e) { if (e instanceof BotError) throw e; return fail('E_BOT_CBOR'); }
}

export interface BotRequest { requestId: Uint8Array; communityId: Uint8Array; channelId: Uint8Array | null; operation: number; revision: bigint | null; expires: bigint | null; args: CborInput[] }
export interface BotContext {
  community: Uint8Array;
  group?: Uint8Array;
  actorUser?: Uint8Array;
  actorDevice?: Uint8Array;
  now: bigint;
}
const ops: Record<number, { channel: boolean; mutation: boolean; count: number }> = {
  0:{channel:false,mutation:false,count:0},3:{channel:false,mutation:false,count:0},1:{channel:false,mutation:false,count:1},2:{channel:false,mutation:true,count:2},
  10:{channel:true,mutation:false,count:0},11:{channel:true,mutation:false,count:1},12:{channel:true,mutation:true,count:1},
  13:{channel:true,mutation:true,count:0},14:{channel:true,mutation:true,count:0},15:{channel:true,mutation:true,count:0},16:{channel:true,mutation:true,count:0},
  17:{channel:true,mutation:true,count:1},18:{channel:true,mutation:true,count:1},19:{channel:true,mutation:true,count:0},20:{channel:true,mutation:true,count:1},
  21:{channel:true,mutation:true,count:1},22:{channel:true,mutation:true,count:2},23:{channel:true,mutation:true,count:0},24:{channel:false,mutation:false,count:0},25:{channel:true,mutation:true,count:1},
  30:{channel:false,mutation:false,count:1},31:{channel:false,mutation:true,count:2},40:{channel:false,mutation:false,count:1},50:{channel:false,mutation:false,count:1},
  51:{channel:false,mutation:true,count:2},52:{channel:false,mutation:true,count:2},60:{channel:false,mutation:false,count:1},61:{channel:false,mutation:true,count:3},
  62:{channel:false,mutation:false,count:0},63:{channel:false,mutation:true,count:2},64:{channel:false,mutation:false,count:1},
  70:{channel:false,mutation:true,count:1},71:{channel:false,mutation:false,count:1},72:{channel:false,mutation:false,count:3},73:{channel:false,mutation:true,count:2},74:{channel:false,mutation:true,count:1},75:{channel:false,mutation:false,count:3},
  80:{channel:false,mutation:true,count:1},81:{channel:false,mutation:false,count:1},82:{channel:false,mutation:true,count:1},83:{channel:false,mutation:true,count:1},84:{channel:false,mutation:false,count:1},
};
function cursor(v: unknown): Uint8Array | null {
  if (v === null) return null;
  const b = bytes(v); if (b.length < 1 || b.length > 256) fail('E_BOT_RANGE'); return b;
}
function canonicalArray(raw: unknown, max: number): CborValue[] {
  const b = bytes(raw); if (b.length < 1 || b.length > max) fail('E_BOT_RANGE');
  const decoded = decode(b); if (!Array.isArray(decoded)) fail('E_BOT_SHAPE');
  const values = decoded as CborValue[]; if (!sameBytes(encode(values), b)) fail('E_BOT_SHAPE'); return values;
}
function sameBytes(a: Uint8Array,b: Uint8Array): boolean { return a.length===b.length && a.every((v,i)=>v===b[i]); }
function validatePollSelection(raw: unknown): Uint8Array {
  const b=bytes(raw); const values=canonicalArray(b,2200); if(values.length>10) fail('E_BOT_SHAPE');
  const seen=new Set<bigint>(); for(const v of values) { const n=u64(v); if(n>9n || seen.has(n)) fail('E_BOT_RANGE'); seen.add(n); }
  return b;
}
function validateUtilityPayload(raw: unknown, poll: boolean): Uint8Array {
  const b=bytes(raw); const f=canonicalArray(b,poll?1800:2200); if(f.length!==7 || u64(f[0])!==1n) fail('E_BOT_SHAPE');
  if(poll) {
    id(f[1]); nullable(f[2],id);
    if(typeof f[3]!=='string' || unicodeWhitespaceOnly.test(f[3]) || Array.from(f[3]).length>300) fail('E_BOT_RANGE');
    const answerValues=f[4]; if(!Array.isArray(answerValues) || answerValues.length<2 || answerValues.length>10) fail('E_BOT_SHAPE');
    const answers=new Set<string>(); for(const x of answerValues as CborValue[]) { if(typeof x!=='string') fail('E_BOT_SHAPE'); const answer=x as string; if(unicodeWhitespaceOnly.test(answer) || Array.from(answer).length>100 || answers.has(answer)) fail('E_BOT_RANGE'); answers.add(answer); }
    if(u64(f[5])>1n || ![3600n,14400n,28800n,86400n,259200n,604800n].includes(u64(f[6]))) fail('E_BOT_RANGE');
  } else {
    const kind=u64(f[1]); if(kind>1n) fail('E_BOT_RANGE');
    const when=u64(f[2]); if((kind===0n && (when<60n || when>31536000n)) || (kind===1n && (when<1n || when>253402300799n))) fail('E_BOT_RANGE');
    if(typeof f[3]!=='string' || enc.encode(f[3]).length<1 || enc.encode(f[3]).length>1500 || typeof f[4]!=='string' || enc.encode(f[4]).length<1 || enc.encode(f[4]).length>100) fail('E_BOT_RANGE');
    const channel=nullable(f[5],id) as Uint8Array|null; const thread=nullable(f[6],id) as Uint8Array|null; if(thread!==null && channel===null) fail('E_BOT_SHAPE');
  }
  return b;
}
function validateArgs(op: number, a: unknown[]): void {
  const spec = ops[op]; if (!spec) fail('E_BOT_OPERATION'); if (a.length !== spec.count && !(op === 15 && a.length === 1)) fail('E_BOT_SHAPE');
  const u = (i:number,min=0n,max=MAX) => { a[i]=uint(a[i],min,max); };
  const t = (i:number,min:number,max:number,ctrl=false) => { a[i]=text(a[i],min,max,ctrl); };
  const ident=(i:number)=>{a[i]=id(a[i]);};
  switch(op) {
    case 1: case 2: u(0,0n,5n); if(op===2) a[1]=cborBytes(a[1],1,2400); break;
    case 11: t(0,1,200,true); break; case 12: t(0,1,2048,true); break;
    case 15:
      if (a.length === 1 && (typeof a[0] !== 'string' || a[0] !== ''
        && (!/^[0-9a-f]{32}$/u.test(a[0]) || a[0] === '0'.repeat(32)))) fail('E_BOT_SHAPE');
      break;
    case 17: u(0); break; case 18: u(0,0n,100n); break; case 20: u(0,0n,2n); break;
    case 21: case 25: ident(0); break; case 22: ident(0); a[1]=nullable(a[1],id); break;
    case 30: t(0,1,2000); break;
    case 31: {
      ident(0); const payload = cborBytes(a[1], 1, 2200); const rows = arr(decode(payload));
      if (rows.length < 1 || rows.length > 100) fail('E_BOT_SHAPE');
      const emojis = new Set<string>(); const roles = new Set<string>();
      for (const value of rows) {
        const row = arr(value, 3); const emoji = text(row[0], 1, 32, true); const role = id(row[1]); text(row[2], 1, 100, true);
        const roleHex = [...role].map(x => x.toString(16).padStart(2, '0')).join('');
        if (emojis.has(emoji) || roles.has(roleHex)) fail('E_BOT_SHAPE');
        emojis.add(emoji); roles.add(roleHex);
      }
      a[1] = payload; break;
    }
    case 40: case 50: u(0); break;
    case 60: if (a[0] !== null) { const cursor = bytes(a[0]); if (cursor.length < 1 || cursor.length > 256) fail('E_BOT_RANGE'); } break;
    case 64: if (a[0] !== null) { const cursor = bytes(a[0]); if (cursor.length < 1 || cursor.length > 256) fail('E_BOT_RANGE'); } break;
    case 51: a[0]=nullable(a[0],id); a[1]=cborBytes(a[1],1,2200); break;
    case 52: ident(0); u(1,1n,(1n<<63n)-1n); break;
    case 61:
      ident(0);
      if (a.length === 3) { t(1,0,1024); t(2,0,512); }
      else if (a.length === 4 && a[1] === 'warn') { t(2,1,1024); t(3,0,0); }
      else fail('E_BOT_SHAPE');
      break;
    case 63: t(0,1,512,true); a[1]=cborBytes(a[1],1,2400); break;
    case 70: a[0]=validateUtilityPayload(a[0], true); break;
    case 80: a[0]=validateUtilityPayload(a[0], false); break;
    case 71: case 74: case 82: case 83: ident(0); break;
    case 72: ident(0); a[1]=nullable(a[1],id); a[2]=cursor(a[2]); break;
    case 73: ident(0); a[1]=validatePollSelection(a[1]); break;
    case 75: ident(0); u(1,0n,9n); a[2]=cursor(a[2]); break;
    case 81: case 84: a[0]=cursor(a[0]); break;
  }
}
function requestValue(x: BotRequest): CborInput[] {
  id(x.requestId); id(x.communityId); nullable(x.channelId,id); uint(x.operation,0n,MAX);
  if (typeof x.operation !== 'number' || !Number.isInteger(x.operation)) fail('E_BOT_OPERATION');
  const spec=ops[x.operation]; if(!spec) fail('E_BOT_OPERATION');
  if (spec.channel !== (x.channelId !== null)) fail('E_BOT_CHANNEL');
  if (spec.mutation ? x.revision === null || x.expires === null : x.revision !== null || x.expires !== null) fail('E_BOT_CONTEXT');
  if(x.revision!==null)uint(x.revision); if(x.expires!==null)uint(x.expires);
  if ((x.operation === 25 || x.operation === 31 || x.operation === 70 || x.operation === 80) && x.revision !== 0n) fail('E_BOT_CONTEXT');
  if (!Array.isArray(x.args)) fail('E_BOT_SHAPE');
  const args=[...x.args]; validateArgs(x.operation,args);
  return [1n,x.requestId,x.communityId,x.channelId,BigInt(x.operation),x.revision,x.expires,args];
}
function readRequest(v: CborValue): BotRequest {
  const a=arr(v,8); if(u64(a[0])!==1n)fail('E_BOT_VERSION');
  const op=Number(u64(a[4])); if(!Number.isSafeInteger(op))fail('E_BOT_OPERATION');
  const out:BotRequest={requestId:readId(a[1]),communityId:readId(a[2]),channelId:readOptId(a[3]),operation:op,
    revision:readOptU(a[5]),expires:readOptU(a[6]),args:arr(a[7])};
  requestValue(out); return out;
}
export function encodeBotRequestBytes(request: BotRequest): Uint8Array { return guarded(() => encode(requestValue(request))); }
export function encodeBotRequest(request: BotRequest): string { return guarded(() => bodyEncode(requestValue(request))); }
export function decodeBotRequest(body: string): BotRequest { return guarded(() => readRequest(bodyDecode(body))); }
export function validateBotContext(request: BotRequest, context: BotContext): void {
  if (!equalId(request.communityId, context.community)) fail('E_BOT_COMMUNITY');
  if (request.revision !== null) {
    const now=uint(context.now); if (request.expires === null || request.expires <= now || now > MAX-30n || request.expires > now+30n) fail('E_BOT_EXPIRED');
  }
}
function equalId(a:Uint8Array,b:Uint8Array):boolean { return a.length===b.length && a.every((v,i)=>v===b[i]); }

export interface BotResponse {
  requestId: Uint8Array;
  communityId: Uint8Array;
  status: number;
  revision: bigint | null;
  data: Uint8Array;
}

function responseValue(response: BotResponse): CborInput[] {
  id(response.requestId);
  id(response.communityId);
  uint(response.status, 0n, 9n);
  if (!Number.isInteger(response.status)) fail('E_BOT_RANGE');
  if (response.revision !== null) uint(response.revision);

  const data = bytes(response.data);
  if (data.length > 2400) fail('E_BOT_RANGE');
  if (data.length > 0 && !Array.isArray(decode(data))) fail('E_BOT_SHAPE');
  return [1n, response.requestId, response.communityId, BigInt(response.status), response.revision, data];
}

function readResponse(value: CborValue): BotResponse {
  const fields = arr(value, 6);
  if (u64(fields[0]) !== 1n) fail('E_BOT_VERSION');

  const response: BotResponse = {
    requestId: readId(fields[1]),
    communityId: readId(fields[2]),
    status: Number(u64(fields[3])),
    revision: readOptU(fields[4]),
    data: bytes(fields[5]!),
  };
  responseValue(response);
  return response;
}

export const encodeBotResponse = (response: BotResponse) =>
  guarded(() => bodyEncode(responseValue(response)));
export const decodeBotResponse = (body: string) =>
  guarded(() => readResponse(bodyDecode(body)));

const GROUP_REGISTRATION_BODY_MAX = 65_505;
const STORED_MAX = (1n << 63n) - 1n;

export interface BotGroupRegistrationRequest {
  operationId: Uint8Array;
  expectedGeneration: bigint;
  registrationBody: Uint8Array;
}

export interface BotGroupRegistrationReceipt {
  operationId: Uint8Array;
  groupId: Uint8Array;
  registrationSHA256: Uint8Array;
  acceptedGeneration: bigint;
  initialEpoch: bigint;
  nextSequence: bigint;
}

function registrationGroup(body: Uint8Array): Uint8Array {
  if (body.length === 0 || body.length > GROUP_REGISTRATION_BODY_MAX) fail('E_BOT_RANGE');
  const fields = arr(decode(body), 4);
  const group = id(fields[0]);
  bytes(fields[1]); bytes(fields[2]); bytes(fields[3]);
  return group;
}

function groupRegistrationRequestValue(request: BotGroupRegistrationRequest): CborInput[] {
  id(request.operationId);
  uint(request.expectedGeneration, 0n, STORED_MAX);
  registrationGroup(bytes(request.registrationBody));
  return [request.operationId, request.expectedGeneration, request.registrationBody];
}

export function encodeBotGroupRegistrationRequest(request: BotGroupRegistrationRequest): Uint8Array {
  return guarded(() => encode(groupRegistrationRequestValue(request)));
}

export function decodeBotGroupRegistrationRequest(raw: Uint8Array): BotGroupRegistrationRequest {
  return guarded(() => {
    if (raw.length > GROUP_REGISTRATION_BODY_MAX + 64) fail('E_BOT_SIZE');
    const fields = arr(decode(raw), 3);
    const request = {
      operationId: id(fields[0]),
      expectedGeneration: uint(fields[1], 0n, STORED_MAX),
      registrationBody: bytes(fields[2]),
    };
    groupRegistrationRequestValue(request);
    return request;
  });
}

function groupRegistrationReceiptValue(receipt: BotGroupRegistrationReceipt): CborInput[] {
  id(receipt.operationId);
  id(receipt.groupId);
  bytes(receipt.registrationSHA256, 32);
  uint(receipt.acceptedGeneration, 0n, STORED_MAX);
  uint(receipt.initialEpoch, 0n, STORED_MAX);
  if (uint(receipt.nextSequence, 1n, STORED_MAX) !== 1n) fail('E_BOT_RANGE');
  return [receipt.operationId, receipt.groupId, receipt.registrationSHA256,
    receipt.acceptedGeneration, receipt.initialEpoch, receipt.nextSequence];
}

export function encodeBotGroupRegistrationReceipt(receipt: BotGroupRegistrationReceipt): Uint8Array {
  return guarded(() => encode(groupRegistrationReceiptValue(receipt)));
}

export function decodeBotGroupRegistrationReceipt(raw: Uint8Array): BotGroupRegistrationReceipt {
  return guarded(() => {
    const fields = arr(decode(raw), 6);
    const receipt = {
      operationId: id(fields[0]), groupId: id(fields[1]), registrationSHA256: bytes(fields[2], 32),
      acceptedGeneration: uint(fields[3], 0n, STORED_MAX), initialEpoch: uint(fields[4], 0n, STORED_MAX),
      nextSequence: uint(fields[5], 1n, STORED_MAX),
    };
    groupRegistrationReceiptValue(receipt);
    return receipt;
  });
}

export async function validateBotGroupRegistrationReceipt(
  receipt: BotGroupRegistrationReceipt,
  request: BotGroupRegistrationRequest,
): Promise<void> {
  groupRegistrationReceiptValue(receipt);
  const group = registrationGroup(request.registrationBody);
  const domain = enc.encode('dilla bot group registration v1\0');
  const preimage = new Uint8Array(domain.length + request.registrationBody.length);
  preimage.set(domain); preimage.set(request.registrationBody, domain.length);
  const hash = new Uint8Array(await crypto.subtle.digest('SHA-256', preimage));
  if (!equalId(receipt.operationId, request.operationId) || !equalId(receipt.groupId, group) ||
      receipt.acceptedGeneration !== request.expectedGeneration || !equalId(receipt.registrationSHA256, hash)) {
    fail('E_BOT_RECEIPT');
  }
}

export function validateBotResponseContext(response: BotResponse, community: Uint8Array): void {
  if (!equalId(response.communityId, community)) fail('E_BOT_COMMUNITY');
}

/** Binds a decoded response to the exact local request that is awaiting it. */
export function validateBotResponseForRequest(response: BotResponse, community: Uint8Array, requestId: Uint8Array): void {
  validateBotResponseContext(response, community);
  if (!equalId(response.requestId, requestId)) fail('E_BOT_REQUEST');
}

export interface RemovalProof {
  requestId: Uint8Array;
  actorUser: Uint8Array;
  actorDevice: Uint8Array;
  communityId: Uint8Array;
  groupId: Uint8Array;
  targetSeq: bigint;
  expires: bigint;
}

function removalValue(proof: RemovalProof): CborInput[] {
  id(proof.requestId);
  id(proof.actorUser);
  id(proof.actorDevice);
  id(proof.communityId);
  id(proof.groupId);
  uint(proof.targetSeq, 1n);
  uint(proof.expires, 1n);
  return [1n, proof.requestId, proof.actorUser, proof.actorDevice, proof.communityId,
    proof.groupId, proof.targetSeq, proof.expires];
}

function readRemoval(value: CborValue): RemovalProof {
  const fields = arr(value, 8);
  if (u64(fields[0]) !== 1n) fail('E_BOT_VERSION');
  const proof: RemovalProof = {
    requestId: readId(fields[1]), actorUser: readId(fields[2]), actorDevice: readId(fields[3]),
    communityId: readId(fields[4]), groupId: readId(fields[5]), targetSeq: u64(fields[6]),
    expires: u64(fields[7]),
  };
  removalValue(proof);
  return proof;
}

export const encodeRemovalProof = (proof: RemovalProof) =>
  guarded(() => encode(removalValue(proof)));
export const decodeRemovalProof = (bytes: Uint8Array) =>
  guarded(() => readRemoval(decode(bytes)));

export function removalPreimage(proof: RemovalProof): Uint8Array {
  const domain = enc.encode('dilla moderation removal v1');
  const encodedProof = encodeRemovalProof(proof);
  const preimage = new Uint8Array(domain.length + encodedProof.length);
  preimage.set(domain);
  preimage.set(encodedProof, domain.length);
  return preimage;
}

export function validateRemovalContext(proof: RemovalProof, context: BotContext): void {
  if (!equalId(proof.communityId, context.community) ||
      !equalId(proof.groupId, context.group ?? new Uint8Array())) {
    fail('E_BOT_COMMUNITY');
  }
  if (context.actorUser !== undefined && !equalId(proof.actorUser, context.actorUser)) fail('E_BOT_ACTOR');
  if (context.actorDevice !== undefined && !equalId(proof.actorDevice, context.actorDevice)) fail('E_BOT_ACTOR');

  const now = uint(context.now);
  if (proof.expires <= now || now > MAX - 30n || proof.expires > now + 30n) fail('E_BOT_EXPIRED');
}

export interface DurableEvent {
  eventId: Uint8Array;
  sequence: bigint;
  communityId: Uint8Array;
  timestamp: bigint;
  kind: number;
  data: CborInput[];
}

function eventData(kind: number, data: unknown[]): void {
  const ident = (index: number) => { data[index] = id(data[index]); };
  const uintAt = (index: number, min = 0n, max = MAX) => { data[index] = uint(data[index], min, max); };
  const lengths: Record<number, number> = { 1: 2, 2: 2, 3: 3, 4: 3, 5: 3, 6: 9, 7: 3, 8: 2 };
  const expectedLength = lengths[kind];
  if (expectedLength === undefined) fail('E_BOT_OPERATION');
  if (data.length !== expectedLength) fail('E_BOT_SHAPE');

  switch (kind) {
    case 1:
    case 2:
      ident(0);
      ident(1);
      break;
    case 3:
      ident(0);
      uintAt(1, 0n, 5n);
      uintAt(2);
      break;
    case 4:
      uintAt(0, 0n, 3n);
      ident(1);
      uintAt(2);
      break;
    case 5:
      ident(0);
      uintAt(1, 0n, 2n);
      uintAt(2);
      break;
    case 6:
      ident(0);
      ident(1);
      ident(2);
      ident(3);
      data[4] = nullable(data[4], id);
      data[5] = nullable(data[5], id);
      data[6] = nullable(data[6], value => uint(value, 1n));
      uintAt(7, 0n, 6n);
      data[8] = nullable(data[8], value => uint(value, 1n));
      if (data[7] === 4n) {
        if (data[5] === null || data[6] === null) fail('E_BOT_SHAPE');
      } else if (data[4] === null || data[5] !== null || data[6] !== null) {
        fail('E_BOT_SHAPE');
      }
      break;
    case 7:
      ident(0);
      uintAt(1);
      uintAt(2, 0n, 1n);
      break;
    case 8:
      ident(0);
      uintAt(1, 1n, MAX);
      break;
  }
}

function eventValue(event: DurableEvent): CborInput[] {
  id(event.eventId);
  uint(event.sequence, 1n);
  id(event.communityId);
  uint(event.timestamp);
  uint(event.kind, 1n, 8n);
  if (!Number.isInteger(event.kind)) fail('E_BOT_RANGE');
  if (!Array.isArray(event.data)) fail('E_BOT_SHAPE');

  const data = [...event.data];
  eventData(event.kind, data);
  if (event.kind === 6) {
    const action = data[7] as bigint;
    const expires = data[8] as bigint | null;
    if (action === 1n && (expires === null || expires <= event.timestamp)) fail('E_BOT_RANGE');
    if (action === 3n && expires !== null && expires <= event.timestamp) fail('E_BOT_RANGE');
    if (action !== 1n && action !== 3n && expires !== null) fail('E_BOT_SHAPE');
  }
  return [1n, event.eventId, event.sequence, event.communityId, event.timestamp, BigInt(event.kind), data];
}

function readEvent(value: CborValue): DurableEvent {
  const fields = arr(value, 7);
  if (u64(fields[0]) !== 1n) fail('E_BOT_VERSION');
  const event: DurableEvent = {
    eventId: readId(fields[1]),
    sequence: u64(fields[2]),
    communityId: readId(fields[3]),
    timestamp: u64(fields[4]),
    kind: Number(u64(fields[5])),
    data: arr(fields[6]),
  };
  eventValue(event);
  return event;
}

export const encodeDurableEvent = (event: DurableEvent) => guarded(() => {
  const encoded = encode(eventValue(event));
  if (encoded.length > 4096) fail('E_BOT_SIZE');
  return encoded;
});
export const decodeDurableEvent = (bytes: Uint8Array) => guarded(() => {
  if (bytes.length > 4096) fail('E_BOT_SIZE');
  return readEvent(decode(bytes));
});

export function validateEventContext(event: DurableEvent, community: Uint8Array): void {
  if (!equalId(event.communityId, community)) fail('E_BOT_COMMUNITY');
}

export enum SendOutcome {
  ClosedNotAccepted = 0,
  Accepted = 1,
  UnknownAfterRestore = 2,
}

export interface OriginalSendResponse {
  sequence: bigint;
  frankingTag: Uint8Array;
  receiveTimestamp: bigint;
  /** Exact original canonical response bytes. */
  body: Uint8Array;
}

export interface SendReceipt {
  operationId: Uint8Array;
  attemptId: Uint8Array;
  predecessorAttemptId: Uint8Array | null;
  communityId: Uint8Array;
  groupId: Uint8Array;
  expectedGeneration: bigint;
  ciphertextSha256: Uint8Array;
  class: number;
  outcome: SendOutcome;
  epoch: bigint;
  sendResponse: OriginalSendResponse | null;
}

const MAX_I64 = (1n << 63n) - 1n;

function readOriginalSendResponse(body: Uint8Array): OriginalSendResponse {
  const fields = arr(decode(body), 3);
  const sequence = u64(fields[0]);
  const frankingTag = bytes(fields[1], 32);
  const receiveTimestamp = u64(fields[2]);
  if (sequence === 0n || sequence > MAX_I64 || receiveTimestamp > MAX_I64) fail('E_BOT_RANGE');
  return { sequence, frankingTag, receiveTimestamp, body };
}

function receiptValue(receipt: SendReceipt): CborInput[] {
  id(receipt.operationId);
  id(receipt.attemptId);
  nullable(receipt.predecessorAttemptId, id);
  id(receipt.communityId);
  id(receipt.groupId);
  uint(receipt.expectedGeneration, 0n, MAX_I64);
  bytes(receipt.ciphertextSha256, 32);
  uint(receipt.class, 0n, 1n);
  uint(receipt.outcome, 0n, 2n);
  if (!Number.isInteger(receipt.class) || !Number.isInteger(receipt.outcome)) fail('E_BOT_RANGE');
  uint(receipt.epoch, 0n, MAX_I64);
  let sendResponseBody: Uint8Array | null = null;
  if (receipt.outcome === SendOutcome.Accepted) {
    const originalResponse = receipt.sendResponse;
    if (originalResponse === null) return fail('E_BOT_SHAPE');
    const parsed = readOriginalSendResponse(originalResponse.body);
    if (parsed.sequence !== originalResponse.sequence || parsed.receiveTimestamp !== originalResponse.receiveTimestamp ||
        !equalId(parsed.frankingTag, originalResponse.frankingTag)) fail('E_BOT_SHAPE');
    sendResponseBody = originalResponse.body;
  } else if (receipt.sendResponse !== null) {
    fail('E_BOT_SHAPE');
  }
  return [1n, receipt.operationId, receipt.attemptId, receipt.predecessorAttemptId,
    receipt.communityId, receipt.groupId, receipt.expectedGeneration, receipt.ciphertextSha256,
    BigInt(receipt.class), BigInt(receipt.outcome), receipt.epoch, sendResponseBody];
}

function readReceipt(value: CborValue): SendReceipt {
  const fields = arr(value, 12);
  if (u64(fields[0]) !== 1n) fail('E_BOT_VERSION');
  const receipt: SendReceipt = {
    operationId: readId(fields[1]),
    attemptId: readId(fields[2]),
    predecessorAttemptId: readOptId(fields[3]),
    communityId: readId(fields[4]),
    groupId: readId(fields[5]),
    expectedGeneration: u64(fields[6]),
    ciphertextSha256: bytes(fields[7]!, 32),
    class: Number(u64(fields[8])),
    outcome: Number(u64(fields[9])),
    epoch: u64(fields[10]),
    sendResponse: fields[11] === null ? null : readOriginalSendResponse(bytes(fields[11])),
  };
  receiptValue(receipt);
  return receipt;
}

export const encodeSendReceipt = (receipt: SendReceipt) => guarded(() => {
  const encoded = encode(receiptValue(receipt));
  if (encoded.length > 512) fail('E_BOT_SIZE');
  return encoded;
});
export const decodeSendReceipt = (bytes: Uint8Array) => guarded(() => {
  if (bytes.length > 512) fail('E_BOT_SIZE');
  return readReceipt(decode(bytes));
});

export function validateReceiptContext(
  receipt: SendReceipt,
  community: Uint8Array,
  group: Uint8Array,
): void {
  if (!equalId(receipt.communityId, community) || !equalId(receipt.groupId, group)) {
    fail('E_BOT_COMMUNITY');
  }
}

export interface HelperEnvelope { requestId: Uint8Array; operation: number; payload: Uint8Array }
export interface HelperResponse { requestId: Uint8Array; status: number; payload: Uint8Array }

function helperValue(
  envelope: HelperEnvelope | HelperResponse,
  isResponse: boolean,
): CborInput[] {
  id(envelope.requestId);
  const operation = isResponse
    ? (envelope as HelperResponse).status
    : (envelope as HelperEnvelope).operation;
  uint(operation, isResponse ? 0n : 1n, isResponse ? 9n : 20n);
  if (!Number.isInteger(operation)) fail('E_BOT_RANGE');

  const payload = bytes(envelope.payload);
  if (payload.length === 0 || payload.length > 1_048_480 || !Array.isArray(decode(payload))) {
    fail('E_BOT_SHAPE');
  }
  return [1n, envelope.requestId, BigInt(operation), payload];
}

function readHelper(value: CborValue, isResponse: boolean): HelperEnvelope | HelperResponse {
  const fields = arr(value, 4);
  if (u64(fields[0]) !== 1n) fail('E_BOT_VERSION');
  const requestId = readId(fields[1]);
  const operation = Number(u64(fields[2]));
  const payload = bytes(fields[3]!);
  const envelope = isResponse
    ? { requestId, status: operation, payload }
    : { requestId, operation, payload };
  helperValue(envelope, isResponse);
  return envelope;
}

function helperBody(value: CborInput[]): Uint8Array {
  const encoded = encode(value);
  if (encoded.length > HELPER_MAX) fail('E_BOT_SIZE');
  return encoded;
}

export const encodeHelperRequest = (envelope: HelperEnvelope) =>
  guarded(() => helperBody(helperValue(envelope, false)));
export const decodeHelperRequest = (bytes: Uint8Array) => guarded(() => {
  if (bytes.length > HELPER_MAX) fail('E_BOT_SIZE');
  return readHelper(decode(bytes), false) as HelperEnvelope;
});
export const encodeHelperResponse = (response: HelperResponse) =>
  guarded(() => helperBody(helperValue(response, true)));
export const decodeHelperResponse = (bytes: Uint8Array) => guarded(() => {
  if (bytes.length > HELPER_MAX) fail('E_BOT_SIZE');
  return readHelper(decode(bytes), true) as HelperResponse;
});

export function frameHelper(bytes: Uint8Array): Uint8Array {
  if (bytes.length < 1 || bytes.length > HELPER_MAX) fail('E_BOT_SIZE');
  const framed = new Uint8Array(bytes.length + 4);
  new DataView(framed.buffer).setUint32(0, bytes.length, false);
  framed.set(bytes, 4);
  return framed;
}

export function parseHelperFrame(bytes: Uint8Array): Uint8Array {
  if (bytes.length < 4) fail('E_BOT_FRAME');
  const length = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getUint32(0, false);
  if (length < 1 || length > HELPER_MAX || length !== bytes.length - 4) fail('E_BOT_FRAME');
  return bytes.slice(4);
}
