import type { CallStats } from '../http/routes';
import { arr, bin, u53, u64, type CborValue } from '../cbor';
import { DillaHttpError } from '../http/errors';
import { DROPPABLE_OPS, encodeFrame, decodeFrame, frameOp, Op, type Frame } from './frames';
import { toHex } from '../hex';

export type GatewayStatus = 'idle' | 'connecting' | 'ready' | 'waiting';

export interface ReadyInfo {
  deviceId: Uint8Array;
  userId: Uint8Array;
  generation: bigint;
  versions: { wire: bigint; e2ee: bigint; media: bigint };
  keypackagesRemaining: number;
  groups: { groupId: Uint8Array; epoch: bigint; lastSeq: bigint; proposalsOutstanding: number }[];
  heartbeatMs: number;
  maxFrameBytes: number;
  backoffMs: number;
  backoffJitterMs: number;
}

export type GatewayEvent =
  | { type: 'status'; status: GatewayStatus; closeCode: number | null }
  | { type: 'ready'; info: ReadyInfo }
  | { type: 'frame'; frame: Frame }
  | { type: 'revoked' }
  /** Task 11 (Jonas, Q3): a heartbeat's round trip, in ms (the phone's You tab: "Connected to {host} · {ping}"). */
  | { type: 'rtt'; ms: number };

export interface GatewayDeps {
  url: string;
  mintTicket(): Promise<string>;
  WebSocket: typeof WebSocket;
  now(): number;
  random(): number;
  setTimeout(fn: () => void, ms: number): number;
  clearTimeout(id: number): void;
}

export const GATEWAY = {
  heartbeatMaxMs: 15000, ackTimeoutMs: 10000, helloTimeoutMs: 15000,
  reconnectBaseMs: 500, reconnectCapMs: 30000, reconnectJitterMs: 500,
} as const;

export const CLIENT_CLOSE = {
  normal: 1000, version: 4006, helloTimeout: 4100, heartbeatTimeout: 4101,
  undecodable: 4102, tooLarge: 4109,
} as const;

const DEFAULT_MAX_FRAME_BYTES = 131584;
const WIRE_PREFERENCE = [5n, 4n, 3n, 2n] as const;
type TimerName = 'reconnectTimer' | 'handshakeTimer' | 'heartbeatTimer' | 'ackTimer';
type HelloInfo = Pick<ReadyInfo, 'heartbeatMs' | 'maxFrameBytes' | 'backoffMs' | 'backoffJitterMs'> & {
  versions: bigint[][]; selectedVersions: { wire: bigint; e2ee: bigint; media: bigint };
};

function readHello(payload: CborValue[]): HelloInfo {
  const p = arr(payload, 9);
  const versions = [arr(p[0]).map(u64), arr(p[1]).map(u64), arr(p[2]).map(u64)];
  const heartbeatMs = u53(p[3]);
  const maxFrameBytes = u53(p[4]);
  bin(p[5], 16);
  u64(p[6]);
  return { versions, selectedVersions: { wire: 0n, e2ee: 1n, media: 1n }, heartbeatMs, maxFrameBytes,
    backoffMs: u53(p[7]), backoffJitterMs: u53(p[8]) };
}

function highestCommon(offered: bigint[], preference: readonly bigint[]): bigint | null {
  return preference.find((version) => offered.includes(version)) ?? null;
}

function readReady(payload: CborValue[], hello: HelloInfo): ReadyInfo {
  const p = arr(payload, 9);
  const deviceId = bin(p[0], 16);
  const userId = bin(p[1], 16);
  const generation = u64(p[2]);
  bin(p[3], 32);
  const versions = { wire: u64(p[4]), e2ee: u64(p[5]), media: u64(p[6]) };
  if (versions.wire !== hello.selectedVersions.wire || versions.e2ee !== hello.selectedVersions.e2ee
    || versions.media !== hello.selectedVersions.media) throw new Error('ready version mismatch');
  const keypackagesRemaining = u53(p[7]);
  const groups = arr(p[8]).map((entry) => {
    const group = arr(entry, 4);
    return { groupId: bin(group[0], 16), epoch: u64(group[1]), lastSeq: u64(group[2]), proposalsOutstanding: u53(group[3]) };
  });
  return { deviceId, userId, generation, versions, keypackagesRemaining, groups,
    heartbeatMs: hello.heartbeatMs, maxFrameBytes: hello.maxFrameBytes,
    backoffMs: hello.backoffMs, backoffJitterMs: hello.backoffJitterMs };
}

export class Gateway {
  private readonly deps: GatewayDeps;
  private readonly listeners = new Set<(e: GatewayEvent) => void>();
  private current: GatewayStatus = 'idle';
  private attempt = 0;
  private socket: WebSocket | null = null;
  private phase: 'minting' | 'hello' | 'ready-wait' | 'ready' = 'minting';
  private hello: HelloInfo | null = null;
  private lastN = 0n;
  private failures = 0;
  private afterMs = 0;
  private reconnectTimer: number | null = null;
  private handshakeTimer: number | null = null;
  private heartbeatTimer: number | null = null;
  private ackTimer: number | null = null;
  /** When the heartbeat now awaiting its ack was sent (deps.now); null while none is outstanding. */
  private beatSentAt: number | null = null;
  private readonly lastTyping = new Map<string, number>();
  private accepted: ReadyInfo['versions'] | null = null;

  constructor(deps: GatewayDeps) { this.deps = deps; }
  get status(): GatewayStatus { return this.current; }
  /** Accepted capabilities are available only for the current ready connection. */
  get acceptedVersions(): ReadyInfo['versions'] | null { return this.current === 'ready' ? this.accepted : null; }

  subscribe(listener: (e: GatewayEvent) => void): () => void {
    this.listeners.add(listener);
    return () => { this.listeners.delete(listener); };
  }

  private emit(e: GatewayEvent): void {
    for (const listener of this.listeners) {
      try { listener(e); }
      catch (err) { queueMicrotask(() => { throw err; }); }
    }
  }

  private setStatus(status: GatewayStatus, closeCode: number | null): void {
    this.current = status;
    this.emit({ type: 'status', status, closeCode });
  }

  private cancel(name: TimerName): void {
    const id = this[name];
    if (id !== null) this.deps.clearTimeout(id);
    this[name] = null;
  }

  private arm(name: TimerName, ms: number, fn: () => void): void {
    this.cancel(name);
    const id = this.attempt;
    const timer = this.deps.setTimeout(() => {
      if (this[name] !== timer || id !== this.attempt) return;
      this[name] = null;
      fn();
    }, ms);
    this[name] = timer;
  }

  start(): void {
    if (this.current !== 'idle') return;
    this.failures = 0;
    this.afterMs = 0;
    this.setStatus('connecting', null);
    this.connect();
  }

  private connect(): void {
    this.accepted = null;
    this.phase = 'minting';
    const id = ++this.attempt;
    // The mint is the first step of the handshake and is bounded like the others: a ticket request stalled
    // on a dead connection fails the attempt (the late ticket is then ignored by the attempt check).
    this.arm('handshakeTimer', GATEWAY.helloTimeoutMs, () => { this.fail(null); });
    void this.open(id);
  }

  private async open(id: number): Promise<void> {
    let ticket: string;
    try { ticket = await this.deps.mintTicket(); }
    catch (err) {
      if (id !== this.attempt) return;
      if (err instanceof DillaHttpError && err.status === 401) {
        this.goIdle();
        this.setStatus('idle', null);
        this.emit({ type: 'revoked' });
        return;
      }
      this.fail(null);
      return;
    }
    if (id !== this.attempt) return;
    let ws: WebSocket;
    try { ws = new this.deps.WebSocket(this.deps.url, ['dilla.v1', `dilla.ticket.${ticket}`]); }
    catch { this.fail(null); return; }
    this.socket = ws;
    ws.binaryType = 'arraybuffer';
    this.lastN = 0n;
    this.hello = null;
    // Fix round 2: a beat of an earlier socket (closed by the server or torn down here) is never timed against this one's ack.
    this.beatSentAt = null;
    this.phase = 'hello';
    ws.addEventListener('message', (event) => { if (id === this.attempt) this.onMessage(event); });
    ws.addEventListener('close', (event) => { if (id === this.attempt) this.onClose(event); });
    ws.addEventListener('error', () => { /* A close follows in a browser. */ });
    this.arm('handshakeTimer', GATEWAY.helloTimeoutMs, () => this.teardown(CLIENT_CLOSE.helloTimeout, true));
  }

  private onMessage(event: MessageEvent): void {
    if (!(event.data instanceof ArrayBuffer)) { this.teardown(CLIENT_CLOSE.undecodable, true); return; }
    if (event.data.byteLength > (this.hello?.maxFrameBytes ?? DEFAULT_MAX_FRAME_BYTES)) {
      this.teardown(CLIENT_CLOSE.tooLarge, true);
      return;
    }
    let frame: Frame;
    const bytes = new Uint8Array(event.data);
    try { frame = decodeFrame(bytes); }
    catch {
      // web-3 (L-GW-33): a malformed voice_state or call_ring of a ready session is dropped, never acted on, and the
      // socket stays open; any other undecodable frame is a protocol failure.
      const op = this.phase === 'ready' ? frameOp(bytes) : null;
      if (op !== null && DROPPABLE_OPS.has(op)) return;
      this.teardown(CLIENT_CLOSE.undecodable, true);
      return;
    }
    if (frame.n > this.lastN) this.lastN = frame.n;
    if (this.phase === 'hello') {
      if (frame.op !== Op.hello) { this.teardown(CLIENT_CLOSE.undecodable, true); return; }
      let hello: HelloInfo;
      try { hello = readHello(frame.payload); }
      catch { this.teardown(CLIENT_CLOSE.undecodable, true); return; }
      const versions = hello.versions;
      const wire = highestCommon(versions[0], WIRE_PREFERENCE);
      const e2ee = highestCommon(versions[1], [1n]);
      const media = highestCommon(versions[2], [1n]);
      if (wire === null || e2ee === null || media === null) {
        this.teardown(CLIENT_CLOSE.version, false);
        this.setStatus('idle', CLIENT_CLOSE.version);
        return;
      }
      hello.selectedVersions = { wire, e2ee, media };
      this.hello = hello;
      this.send(encodeFrame(Op.identify, 1, null, ['', wire, e2ee, media, 0]));
      this.phase = 'ready-wait';
      this.arm('handshakeTimer', GATEWAY.helloTimeoutMs, () => this.teardown(CLIENT_CLOSE.helloTimeout, true));
      return;
    }
    if (this.phase === 'ready-wait') {
      if (frame.op === Op.ready) {
        let info: ReadyInfo;
        try { info = readReady(frame.payload, this.hello!); }
        catch { this.teardown(CLIENT_CLOSE.undecodable, true); return; }
        this.cancel('handshakeTimer');
        this.failures = 0;
        this.phase = 'ready';
        this.accepted = info.versions;
        this.setStatus('ready', null);
        this.emit({ type: 'ready', info });
        this.scheduleBeat();
      } else if (frame.op === Op.invalidSession || frame.op === Op.reconnect) this.reconnectFrame(frame);
      return;
    }
    if (this.phase === 'ready') {
      if (frame.op === Op.heartbeatAck) {
        this.cancel('ackTimer');
        if (this.beatSentAt !== null) {
          const ms = Math.max(0, Math.round(this.deps.now() - this.beatSentAt));
          this.beatSentAt = null;
          this.emit({ type: 'rtt', ms });
        }
      }
      // The group frames, the relayed typing signal the worker publishes as typing:<channel> (L-HTTP-96 op 49), and the
      // voice presence and DM ring signals of web-3 (ops 50 and 53, L-GW-32, L-GW-33), the structure notice of
      // web-3.1 (op 54), and the away roster notice (op 55, AW4).
      else if ((frame.op >= Op.handshake && frame.op <= Op.messageDeleted) || frame.op === Op.presence || frame.op === Op.typing
        || frame.op === Op.voiceState || frame.op === Op.callRing || frame.op === Op.communityChanged || frame.op === Op.afkAway
        || (frame.op === Op.appBuild && (this.acceptedVersions?.wire ?? 0) >= 5)) {
        this.emit({ type: 'frame', frame });
      }
      else if (frame.op === Op.invalidSession || frame.op === Op.reconnect) this.reconnectFrame(frame);
    }
  }

  private reconnectFrame(frame: Frame): void {
    if (frame.op === Op.reconnect) {
      try { this.afterMs = u53(frame.payload[1]); }
      catch { this.afterMs = 0; }
    } else this.afterMs = 0;
    this.teardown(CLIENT_CLOSE.normal, true);
  }

  private scheduleBeat(): void {
    if (this.current !== 'ready' || this.hello === null) return;
    const interval = Math.min(Math.floor(this.hello.heartbeatMs / 2), GATEWAY.heartbeatMaxMs);
    this.arm('heartbeatTimer', interval, () => {
      this.send(encodeFrame(Op.heartbeat, 0, null, [this.lastN, 0]));
      if (this.ackTimer === null) {
        this.beatSentAt = this.deps.now();
        this.arm('ackTimer', GATEWAY.ackTimeoutMs, () => this.teardown(CLIENT_CLOSE.heartbeatTimeout, true));
      }
      this.scheduleBeat();
    });
  }

  private send(bytes: Uint8Array): void {
    if (this.socket?.readyState === 1) this.socket.send(bytes);
  }

  private onClose(event: CloseEvent): void {
    if (event.code === 4004) {
      this.goIdle();
      this.setStatus('idle', 4004);
      this.emit({ type: 'revoked' });
      return;
    }
    // C2 review finding 1: the instance refused this client's wire (E_VERSION, close 4006). As with a hello without
    // WIRE, the refusal stands until the page is reloaded: no reconnect, no new ticket.
    if (event.code === CLIENT_CLOSE.version) {
      this.goIdle();
      this.setStatus('idle', CLIENT_CLOSE.version);
      return;
    }
    this.fail(event.code);
  }

  private teardown(code: number, reconnect: boolean): void {
    this.accepted = null;
    this.beatSentAt = null;
    const ws = this.socket;
    this.attempt += 1;
    this.cancel('handshakeTimer');
    this.cancel('heartbeatTimer');
    this.cancel('ackTimer');
    this.socket = null;
    try { ws?.close(code); }
    catch { /* A socket can already be gone. */ }
    if (reconnect) this.fail(code);
  }

  private fail(closeCode: number | null): void {
    this.accepted = null;
    this.attempt += 1;
    this.cancel('handshakeTimer');
    this.cancel('heartbeatTimer');
    this.cancel('ackTimer');
    this.socket = null;
    this.setStatus('waiting', closeCode);
    const delay = Math.floor(Math.max(this.afterMs,
      Math.min(GATEWAY.reconnectCapMs, GATEWAY.reconnectBaseMs * 2 ** this.failures)
      + this.deps.random() * GATEWAY.reconnectJitterMs));
    this.failures += 1;
    this.afterMs = 0;
    this.arm('reconnectTimer', delay, () => { this.setStatus('connecting', null); this.connect(); });
  }

  private goIdle(): void {
    this.accepted = null;
    this.attempt += 1;
    this.cancel('reconnectTimer');
    this.cancel('handshakeTimer');
    this.cancel('heartbeatTimer');
    this.cancel('ackTimer');
    this.socket = null;
    this.phase = 'minting';
  }

  stop(): void {
    if (this.current === 'idle') return;
    this.teardown(CLIENT_CLOSE.normal, false);
    this.goIdle();
    this.setStatus('idle', null);
    this.lastTyping.clear();
  }

  private lastStatsAt = -Infinity;

  /** The boolean selects transport once. True also covers a throttled, congested or ambiguously
   * failed send: callers must never repeat that same sample through HTTP. */
  sendCallStats(callId: Uint8Array, report: CallStats): boolean {
    if (this.phase !== 'ready' || (this.acceptedVersions?.wire ?? 0n) < 5n || this.socket?.readyState !== 1) return false;
    const now = this.deps.now();
    if (now - this.lastStatsAt < 10_000) return true;
    this.lastStatsAt = now;
    if (this.socket.bufferedAmount > 65536) return true;
    try { this.send(encodeFrame(Op.callStats, 0, null, [callId, report])); } catch { /* A coarse sample is disposable. */ }
    return true;
  }

  private lastActivityAt = -Infinity;

  /** True means the wire-5 path owns this sample, including a coalesced/congested drop.
   * False lets the caller choose the compatibility HTTP path; nothing is queued for reconnect. */
  sendPresenceActivity(): boolean {
    if (this.phase !== 'ready' || (this.acceptedVersions?.wire ?? 0n) < 5n || this.socket?.readyState !== 1) return false;
    const now = this.deps.now();
    if (now - this.lastActivityAt < 5000) return true;
    this.lastActivityAt = now;
    if (this.socket.bufferedAmount <= 65536) this.send(encodeFrame(Op.presenceActivity, 0, null, []));
    return true;
  }

  sendTyping(channelId: Uint8Array, on: boolean): void {
    if (!(channelId instanceof Uint8Array) || channelId.length !== 16 || typeof on !== 'boolean') throw new Error('E_FRAME_SHAPE');
    if (this.current !== 'ready' || this.socket?.readyState !== 1) return;
    const key = toHex(channelId);
    const now = this.deps.now();
    const last = this.lastTyping.get(key);
    if (on && last !== undefined && now - last < 3000) return;
    if (on) this.lastTyping.set(key, now);
    else this.lastTyping.delete(key);
    this.send(encodeFrame(Op.typingSet, 0, null, [channelId, on ? 1 : 0]));
  }

  sendVoiceSelf(callId: Uint8Array, flags: 0 | 1 | 2 | 3): void {
    if (!(callId instanceof Uint8Array) || callId.length !== 16 || !Number.isInteger(flags) || flags < 0 || flags > 3)
      throw new Error('E_FRAME_SHAPE');
    if (this.current === 'ready' && this.socket?.readyState === 1)
      this.send(encodeFrame(Op.voiceSelf, 0, null, [callId, flags]));
  }

  commitAck(groupId: Uint8Array, round: bigint): void {
    if (this.current === 'ready' && this.socket?.readyState === 1) {
      this.send(encodeFrame(Op.commitAck, 0, groupId, [round]));
    }
  }
}
