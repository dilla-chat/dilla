import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { decode, encode, type CborInput, type CborValue } from '../cbor';
import { fromHex, toHex } from '../hex';
import { DillaHttpError } from '../http/errors';
import { decodeFrame, encodeFrame, Op, type Frame } from './frames';
import { CLIENT_CLOSE, GATEWAY, Gateway, type GatewayDeps, type GatewayEvent } from './gateway';

it('sends bounded client signals and throttles typing per channel', async () => {
  // Parity authority: internal/gateway/ephemeral_test.go::TestTypingSetThrottlesPerDeviceChannelAndExpires
  // and TestVoiceSelfRequiresLiveCallAndValidFlags; the Go tests also enforce audience ACLs.
  const h = harness();
  const socket = await connected(h);
  const a = new Uint8Array(16).fill(0xa1);
  const b = new Uint8Array(16).fill(0xb1);
  h.gw.sendTyping(a, true);
  h.gw.sendTyping(a, true);
  h.gw.sendTyping(b, true);
  h.clock.advance(3000);
  h.gw.sendTyping(a, true);
  h.gw.sendTyping(a, false);
  h.gw.sendVoiceSelf(b, 3);
  const frames = socket.sentFrames().filter((f) => f.op >= 49);
  expect(frames.map((f) => [f.op, f.n, f.groupId, f.payload])).toEqual([
    [51, 0n, null, [a, 1n]], [51, 0n, null, [b, 1n]],
    [51, 0n, null, [a, 1n]], [51, 0n, null, [a, 0n]], [52, 0n, null, [b, 3n]],
  ]);
  expect(frames.some((f) => f.op === 50)).toBe(false);
});

const INSTANCE = new Uint8Array(16).fill(0x01);
const DEVICE = new Uint8Array(16).fill(0x08);
const USER = new Uint8Array(16).fill(0x09);
const GROUP = new Uint8Array(16).fill(0x07);

function hello(over: { wire?: number[]; e2ee?: number[]; media?: number[]; heartbeatMs?: number; maxFrameBytes?: number } = {}): Uint8Array {
  return encodeFrame(Op.hello, 0, null, [over.wire ?? [2], over.e2ee ?? [1], over.media ?? [1], over.heartbeatMs ?? 30000,
    over.maxFrameBytes ?? 131584, INSTANCE, 1, 300, 300]);
}

function ready(over: { wire?: number; e2ee?: number; media?: number } = {}): Uint8Array {
  return encodeFrame(Op.ready, 1, null, [DEVICE, USER, 7, new Uint8Array(32).fill(0x0b), over.wire ?? 2, over.e2ee ?? 1,
    over.media ?? 1, 32, [[GROUP, 6, 4127, 2]]]);
}

/** A manual clock: timers run only inside advance(), in deadline order, ties in creation order. */
class FakeClock {
  now = 0;
  private nextId = 1;
  private readonly timers = new Map<number, { at: number; fn: () => void }>();
  readonly setTimeout = (fn: () => void, ms: number): number => {
    const id = this.nextId;
    this.nextId += 1;
    this.timers.set(id, { at: this.now + ms, fn });
    return id;
  };
  readonly clearTimeout = (id: number): void => {
    this.timers.delete(id);
  };
  advance(ms: number): void {
    const end = this.now + ms;
    for (;;) {
      let due: [number, { at: number; fn: () => void }] | undefined;
      for (const entry of this.timers) {
        if (entry[1].at <= end && (due === undefined || entry[1].at < due[1].at)) due = entry;
      }
      if (due === undefined) break;
      this.timers.delete(due[0]);
      this.now = due[1].at;
      due[1].fn();
    }
    this.now = end;
  }
  /** Delays of the pending timers from now, ascending. */
  pending(): number[] {
    return [...this.timers.values()].map((t) => t.at - this.now).sort((a, b) => a - b);
  }
}

type Listener = (event: unknown) => void;

/** A WebSocket double with the browser's close-code rule. */
class FakeSocket {
  static instances: FakeSocket[] = [];
  readonly url: string;
  readonly protocols: string[];
  binaryType = 'blob';
  bufferedAmount = 0;
  readyState = 0;
  readonly sent: Uint8Array[] = [];
  closedWith: { code: number; reason: string } | null = null;
  private readonly listeners = new Map<string, Listener[]>();

  constructor(url: string, protocols: string | string[]) {
    this.url = url;
    this.protocols = typeof protocols === 'string' ? [protocols] : [...protocols];
    FakeSocket.instances.push(this);
  }
  addEventListener(type: string, fn: Listener): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), fn]);
  }
  removeEventListener(type: string, fn: Listener): void {
    this.listeners.set(type, (this.listeners.get(type) ?? []).filter((l) => l !== fn));
  }
  send(data: Uint8Array | ArrayBuffer): void {
    if (this.readyState !== 1) throw new Error('send on a socket that is not open');
    this.sent.push(data instanceof Uint8Array ? data.slice() : new Uint8Array(data));
  }
  close(code?: number, reason = ''): void {
    if (code !== undefined && code !== 1000 && (code < 3000 || code > 4999)) {
      throw new DOMException(`close code ${code} is not allowed`, 'InvalidAccessError');
    }
    if (this.readyState === 3) return;
    this.closedWith = { code: code ?? 1005, reason };
    this.readyState = 3;
    this.emit('close', { code: code ?? 1005, reason, wasClean: true });
  }
  open(): void {
    this.readyState = 1;
    this.emit('open', {});
  }
  receive(data: Uint8Array | string): void {
    this.emit('message', { data: typeof data === 'string' ? data : data.slice().buffer });
  }
  serverClose(code: number): void {
    this.readyState = 3;
    this.emit('close', { code, reason: '', wasClean: true });
  }
  sentFrames(): Frame[] {
    return this.sent.map((b) => decodeFrame(b));
  }
  private emit(type: string, event: unknown): void {
    for (const l of this.listeners.get(type) ?? []) l(event);
  }
}

const flush = (): Promise<void> => new Promise<void>((resolve) => setImmediate(resolve));

function harness(opts: { random?: number; mint?: () => Promise<string> } = {}) {
  const clock = new FakeClock();
  const events: GatewayEvent[] = [];
  let minted = 0;
  const deps: GatewayDeps = {
    url: 'ws://127.0.0.1:8453/gateway',
    mintTicket: opts.mint ?? (() => {
      minted += 1;
      return Promise.resolve(`t${minted}`);
    }),
    WebSocket: FakeSocket as unknown as typeof WebSocket,
    now: () => clock.now,
    random: () => opts.random ?? 0,
    setTimeout: clock.setTimeout,
    clearTimeout: clock.clearTimeout,
  };
  const gw = new Gateway(deps);
  gw.subscribe((e) => events.push(e));
  const last = (): FakeSocket => {
    const s = FakeSocket.instances.at(-1);
    if (s === undefined) throw new Error('no socket was opened');
    return s;
  };
  return { gw, clock, events, last, minted: () => minted };
}

async function connected(h: ReturnType<typeof harness>, helloFrame = hello()): Promise<FakeSocket> {
  h.gw.start();
  await flush();
  const s = h.last();
  s.open();
  s.receive(helloFrame);
  s.receive(ready());
  return s;
}

const statuses = (events: GatewayEvent[]) =>
  events.flatMap((e) => (e.type === 'status' ? [`${e.status}:${String(e.closeCode)}`] : []));

beforeEach(() => {
  FakeSocket.instances = [];
});

describe('Gateway connect', () => {
  it('pins the timing constants', () => {
    expect(GATEWAY).toEqual({ heartbeatMaxMs: 15000, ackTimeoutMs: 10000, helloTimeoutMs: 15000,
      reconnectBaseMs: 500, reconnectCapMs: 30000, reconnectJitterMs: 500 });
    expect(CLIENT_CLOSE).toEqual({ normal: 1000, version: 4006, helloTimeout: 4100, heartbeatTimeout: 4101,
      undecodable: 4102, tooLarge: 4109 });
  });

  it('offers dilla.v1 and the ticket as subprotocols and reads binary frames', async () => {
    const h = harness();
    h.gw.start();
    expect(h.gw.status).toBe('connecting');
    await flush();
    expect(FakeSocket.instances).toHaveLength(1);
    const s = h.last();
    expect(s.url).toBe('ws://127.0.0.1:8453/gateway');
    expect(s.protocols).toEqual(['dilla.v1', 'dilla.ticket.t1']);
    expect(s.binaryType).toBe('arraybuffer');
    expect(h.events[0]).toEqual({ type: 'status', status: 'connecting', closeCode: null });
  });

  it('answers hello with a fresh identify and reports ready', async () => {
    const h = harness();
    h.gw.start();
    await flush();
    const s = h.last();
    s.open();
    expect(s.sent).toHaveLength(0);
    expect(h.gw.acceptedVersions).toBeNull();
    s.receive(hello());
    expect(s.sent).toEqual([encodeFrame(Op.identify, 1, null, ['', 2, 1, 1, 0])]);
    s.receive(ready());
    expect(h.gw.status).toBe('ready');
    expect(h.gw.acceptedVersions).toEqual({ wire: 2n, e2ee: 1n, media: 1n });
    expect(h.events.find((e) => e.type === 'ready')).toEqual({
      type: 'ready',
      info: {
        deviceId: DEVICE, userId: USER, generation: 7n, keypackagesRemaining: 32,
        groups: [{ groupId: GROUP, epoch: 6n, lastSeq: 4127n, proposalsOutstanding: 2 }],
        versions: { wire: 2n, e2ee: 1n, media: 1n },
        heartbeatMs: 30000, maxFrameBytes: 131584, backoffMs: 300, backoffJitterMs: 300,
      },
    });
    expect(statuses(h.events)).toEqual(['connecting:null', 'ready:null']);
  });

  it('selects the highest common wire regardless of server order and exposes the echo only after ready', async () => {
    const h = harness();
    h.gw.start();
    await flush();
    const s = h.last();
    s.open();
    s.receive(hello({ wire: [2, 3] }));
    expect(s.sent).toEqual([encodeFrame(Op.identify, 1, null, ['', 3, 1, 1, 0])]);
    expect(h.events.some((e) => e.type === 'ready')).toBe(false);
    s.receive(ready({ wire: 3 }));
    expect(h.events.find((e) => e.type === 'ready')).toMatchObject({ info: { versions: { wire: 3n, e2ee: 1n, media: 1n } } });
  });

  it('falls back to wire 2 and refuses a hello without wire version 2', async () => {
    const h = harness();
    h.gw.start();
    await flush();
    const s = h.last();
    s.open();
    s.receive(hello({ wire: [1] }));
    expect(s.closedWith?.code).toBe(4006);
    expect(s.sent).toHaveLength(0);
    expect(h.gw.status).toBe('idle');
    expect(h.events.at(-1)).toEqual({ type: 'status', status: 'idle', closeCode: 4006 });
    expect(h.clock.pending()).toEqual([]);
    h.clock.advance(600_000);
    await flush();
    expect(FakeSocket.instances).toHaveLength(1);
    expect(h.minted()).toBe(1);

    const fallback = harness();
    fallback.gw.start();
    await flush();
    const f = fallback.last();
    f.open();
    f.receive(hello({ wire: [2] }));
    expect(f.sent).toEqual([encodeFrame(Op.identify, 1, null, ['', 2, 1, 1, 0])]);
  });

  it('rejects ready when the server echoes a different selected version', async () => {
    for (const echo of [{ wire: 2 }, { e2ee: 2 }, { media: 2 }]) {
      const h = harness();
      h.gw.start();
      await flush();
      const s = h.last();
      s.open();
      s.receive(hello({ wire: [3, 2] }));
      s.receive(ready(echo));
      expect(s.closedWith?.code).toBe(CLIENT_CLOSE.undecodable);
      expect(h.events.some((e) => e.type === 'ready')).toBe(false);
    }
  });

  it('identifies with the selected wire when the hello offers 2 and 3, and refuses one without e2ee or media version 1', async () => {
    const h = harness();
    h.gw.start();
    await flush();
    const s = h.last();
    s.open();
    s.receive(hello({ wire: [1, 2, 3] }));
    expect(s.sent).toEqual([encodeFrame(Op.identify, 1, null, ['', 3, 1, 1, 0])]);
    for (const bad of [[[2, 3], [2], [1]], [[2, 3], [1], [2]]]) {
      const g = harness();
      g.gw.start();
      await flush();
      const t = g.last();
      t.open();
      t.receive(encodeFrame(Op.hello, 0, null, [...bad, 30000, 131584, INSTANCE, 1, 300, 300]));
      expect(t.closedWith?.code).toBe(4006);
      expect(t.sent).toHaveLength(0);
    }
  });

  it('gives up on a socket that sends no hello, and on one that sends no ready', async () => {
    const h = harness();
    h.gw.start();
    await flush();
    const first = h.last();
    first.open();
    h.clock.advance(14_999);
    expect(first.closedWith).toBeNull();
    h.clock.advance(1);
    expect(first.closedWith?.code).toBe(CLIENT_CLOSE.helloTimeout);
    expect(h.gw.status).toBe('waiting');
    h.clock.advance(h.clock.pending()[0] ?? 0);
    await flush();
    const second = h.last();
    second.open();
    second.receive(hello());
    h.clock.advance(15_000);
    expect(second.closedWith?.code).toBe(CLIENT_CLOSE.helloTimeout);
  });
});

describe('Gateway heartbeat', () => {
  it('beats every min(heartbeat_ms / 2, 15 s) with the highest n seen', async () => {
    const h = harness();
    const s = await connected(h);
    const beats = () => s.sentFrames().filter((f) => f.op === Op.heartbeat);
    h.clock.advance(14_999);
    expect(beats()).toHaveLength(0);
    h.clock.advance(1);
    expect(beats()).toEqual([{ op: Op.heartbeat, n: 0n, groupId: null, payload: [1n, 0n] }]);
    s.receive(encodeFrame(Op.heartbeatAck, 0, null, [1_700_000_000]));
    s.receive(encodeFrame(Op.messageCt, 5, GROUP, [9, 6, DEVICE, new Uint8Array([1]), new Uint8Array(32), 1_700_000_000]));
    h.clock.advance(15_000);
    expect(beats()[1]?.payload).toEqual([5n, 0n]);
  });

  // Task 11 (Jonas, Q3): the ack's delay after its beat is the gateway's round trip.
  it('reports each acknowledged beat\'s round trip as an rtt event', async () => {
    const h = harness();
    const s = await connected(h);
    const rtts = () => h.events.flatMap((e) => (e.type === 'rtt' ? [e.ms] : []));
    h.clock.advance(15_000);
    h.clock.advance(24);
    s.receive(encodeFrame(Op.heartbeatAck, 0, null, [1_700_000_000]));
    expect(rtts()).toEqual([24]);
    s.receive(encodeFrame(Op.heartbeatAck, 0, null, [1_700_000_000]));
    expect(rtts()).toEqual([24]); // an ack with no beat outstanding reports nothing
  });

  it('forgets an unacknowledged beat when its socket goes, so the next socket reports no stale round trip', async () => {
    const h = harness();
    const s = await connected(h);
    const rtts = () => h.events.flatMap((e) => (e.type === 'rtt' ? [e.ms] : []));
    h.clock.advance(15_000);
    s.serverClose(1006);
    h.clock.advance(h.clock.pending()[0] ?? 0);
    await flush();
    const next = h.last();
    next.open();
    next.receive(hello());
    next.receive(ready());
    next.receive(encodeFrame(Op.heartbeatAck, 0, null, [1_700_000_000]));
    expect(rtts()).toEqual([]);
  });

  it('uses half of a short heartbeat_ms', async () => {
    const h = harness();
    const s = await connected(h, hello({ heartbeatMs: 10_000 }));
    h.clock.advance(4_999);
    expect(s.sentFrames().filter((f) => f.op === Op.heartbeat)).toHaveLength(0);
    h.clock.advance(1);
    expect(s.sentFrames().filter((f) => f.op === Op.heartbeat)).toHaveLength(1);
  });

  it('keeps a socket whose beats are acknowledged', async () => {
    const h = harness();
    const s = await connected(h);
    for (let i = 0; i < 4; i += 1) {
      h.clock.advance(15_000);
      s.receive(encodeFrame(Op.heartbeatAck, 0, null, [1_700_000_000 + i]));
    }
    expect(s.closedWith).toBeNull();
    expect(h.gw.status).toBe('ready');
  });

  it('closes a socket whose beat is not acknowledged within 10 s', async () => {
    const h = harness();
    const s = await connected(h);
    h.clock.advance(15_000);
    h.clock.advance(9_999);
    expect(s.closedWith).toBeNull();
    h.clock.advance(1);
    expect(s.closedWith?.code).toBe(CLIENT_CLOSE.heartbeatTimeout);
    expect(h.gw.status).toBe('waiting');
    expect(h.events.at(-1)).toEqual({ type: 'status', status: 'waiting', closeCode: CLIENT_CLOSE.heartbeatTimeout });
    expect(h.clock.pending()).toEqual([500]);
  });

  it('closes at the first unanswered beat deadline when beats are 5 s apart', async () => {
    const h = harness();
    const s = await connected(h, hello({ heartbeatMs: 10_000 }));
    h.clock.advance(14_999);
    expect(s.sentFrames().filter((f) => f.op === Op.heartbeat)).toHaveLength(2);
    expect(s.closedWith).toBeNull();
    h.clock.advance(1);
    expect(s.closedWith?.code).toBe(CLIENT_CLOSE.heartbeatTimeout);
    expect(h.gw.status).toBe('waiting');
  });
});

describe('Gateway input checks', () => {
  it('closes on a frame larger than max_frame_bytes, and accepts one of exactly that size', async () => {
    const big = encodeFrame(Op.messageCt, 2, GROUP, [9, 6, DEVICE, new Uint8Array(200), new Uint8Array(32), 1]);
    const h1 = harness();
    const s1 = await connected(h1, hello({ maxFrameBytes: big.length }));
    s1.receive(big);
    expect(s1.closedWith).toBeNull();
    FakeSocket.instances = [];
    const h2 = harness();
    const s2 = await connected(h2, hello({ maxFrameBytes: big.length - 1 }));
    s2.receive(big);
    expect(s2.closedWith?.code).toBe(CLIENT_CLOSE.tooLarge);
    expect(h2.events.at(-1)).toEqual({ type: 'status', status: 'waiting', closeCode: CLIENT_CLOSE.tooLarge });
    expect(h2.clock.pending()).toEqual([500]);
  });

  it('closes on undecodable bytes and on a text frame', async () => {
    const h1 = harness();
    const s1 = await connected(h1);
    s1.receive(new Uint8Array([0xa1, 0x00, 0xf6]));
    expect(s1.closedWith?.code).toBe(CLIENT_CLOSE.undecodable);
    expect(h1.clock.pending()).toEqual([500]);
    FakeSocket.instances = [];
    const h2 = harness();
    const s2 = await connected(h2);
    s2.receive('{"op":0}');
    expect(s2.closedWith?.code).toBe(CLIENT_CLOSE.undecodable);
  });

  it('closes when the first frame is not hello', async () => {
    const h = harness();
    h.gw.start();
    await flush();
    const s = h.last();
    s.open();
    s.receive(ready());
    expect(s.closedWith?.code).toBe(CLIENT_CLOSE.undecodable);
  });

  // web-3 task 6: op 50 is emitted now (the test below it); the rest stays ignored.
  it('emits ops 16 to 21 and nothing else', async () => {
    const h = harness();
    const s = await connected(h);
    const wanted = [
      encodeFrame(Op.handshake, 2, GROUP, [10, 6, 1, 3, new Uint8Array([0xde])]),
      encodeFrame(Op.commitNeeded, 0, GROUP, [6, [new Uint8Array(32)], 2000, 1]),
      encodeFrame(Op.epochChanged, 3, GROUP, [7, 10]),
      encodeFrame(Op.messageCt, 4, GROUP, [11, 7, DEVICE, new Uint8Array([1]), new Uint8Array(32), 1_700_000_000]),
      encodeFrame(Op.welcome, 5, GROUP, [4, 7, 10, new Uint8Array([2]), new Uint8Array([3]), new Uint8Array(32)]),
      encodeFrame(Op.messageDeleted, 6, GROUP, [11, 1_700_000_100]),
    ];
    const ignored = [
      encodeFrame(32, 7, null, [GROUP, 1, USER, new Uint8Array(0), new Uint8Array(32), 0, 0]),
      encodeFrame(Op.error, 0, null, [0, 'E_FRAME_SHAPE', 'x']),
      encodeFrame(63, 8, null, []),
    ];
    for (const f of [...wanted, ...ignored]) s.receive(f);
    const got = h.events.flatMap((e) => (e.type === 'frame' ? [e.frame] : []));
    expect(got).toEqual(wanted.map((b) => decodeFrame(b)));
    expect(h.gw.status).toBe('ready');
    expect(s.closedWith).toBeNull();
  });

  // L-HTTP-96's S→C op 49 is the only way a peer's typing reaches the worker (task 10: no browser ever showed one).
  it('emits the relayed typing frame, op 49, once ready', async () => {
    const h = harness();
    const s = await connected(h);
    const typing = encodeFrame(Op.typing, 0, null, [GROUP, USER, DEVICE, 1, 1_700_000_010]);
    s.receive(typing);
    expect(h.events.flatMap((e) => (e.type === 'frame' ? [e.frame] : []))).toEqual([decodeFrame(typing)]);
    expect(s.closedWith).toBeNull();
  });

  // web-3 task 6 (L-GW-32, L-GW-33): voice_state and call_ring reach the worker once ready.
  it('emits voice_state (op 50) and call_ring (op 53) once ready', async () => {
    const h = harness();
    const s = await connected(h);
    const voice = encodeFrame(Op.voiceState, 0, null, [USER, DEVICE, GROUP, 17]);
    const ring = encodeFrame(Op.callRing, 0, null, [GROUP, GROUP, USER, 1, 1_700_000_030]);
    s.receive(voice);
    s.receive(ring);
    expect(h.events.flatMap((e) => (e.type === 'frame' ? [e.frame] : []))).toEqual([decodeFrame(voice), decodeFrame(ring)]);
    expect(s.closedWith).toBeNull();
  });

  // Attacker: a hostile or buggy instance sending a ring of state 3, a four-element ring or a voice_state with a reserved
  // flag bit. Each is dropped without acting on it, and without closing the socket (a malformed ephemeral frame must not
  // cost the person their connection; L-GW-33).
  it('drops a malformed op 53 or op 50 without disconnecting', async () => {
    const h = harness();
    const s = await connected(h);
    const before = h.events.length;
    s.receive(encodeFrame(Op.callRing, 0, null, [GROUP, GROUP, USER, 3, 1_700_000_030]));
    s.receive(encodeFrame(Op.callRing, 0, null, [GROUP, GROUP, USER, 1]));
    s.receive(encodeFrame(Op.voiceState, 0, null, [USER, DEVICE, GROUP, 32]));
    expect(h.events.slice(before)).toEqual([]);
    expect(s.closedWith).toBeNull();
    expect(h.gw.status).toBe('ready');
    // A malformed frame of any other op still closes the socket as undecodable.
    s.receive(encodeFrame(Op.typing, 0, null, [GROUP, USER, DEVICE, 2, 1]));
    expect(s.closedWith?.code).toBe(CLIENT_CLOSE.undecodable);
  });

  // dilla-web-3.1: community_changed reaches the worker once ready; a malformed one is dropped as an ephemeral
  // signal is, without closing the socket.
  it('emits community_changed (op 54) once ready and drops a malformed one', async () => {
    const h = harness();
    const s = await connected(h);
    const changed = encodeFrame(Op.communityChanged, 0, null, [GROUP]);
    s.receive(changed);
    expect(h.events.flatMap((e) => (e.type === 'frame' ? [e.frame] : []))).toEqual([decodeFrame(changed)]);
    const before = h.events.length;
    s.receive(encodeFrame(Op.communityChanged, 0, null, [GROUP, 1]));
    s.receive(encodeFrame(Op.communityChanged, 0, null, [GROUP.slice(1)]));
    expect(h.events.slice(before)).toEqual([]);
    expect(s.closedWith).toBeNull();
  });

  // AW4: afk_away reaches the worker once ready; a malformed one is dropped without closing the socket.
  it('emits afk_away (op 55) once ready and drops a malformed one', async () => {
    const h = harness();
    const s = await connected(h);
    const away = encodeFrame(Op.afkAway, 0, null, [GROUP, USER, 1758659040]);
    s.receive(away);
    expect(h.events.flatMap((e) => (e.type === 'frame' ? [e.frame] : []))).toEqual([decodeFrame(away)]);
    const before = h.events.length;
    s.receive(encodeFrame(Op.afkAway, 0, null, [GROUP, USER, 1758659040, 1]));
    s.receive(encodeFrame(Op.afkAway, 0, null, [GROUP, USER.slice(1), 0]));
    expect(h.events.slice(before)).toEqual([]);
    expect(s.closedWith).toBeNull();
  });
});

describe('Gateway reconnect', () => {
  it('backs off 500 ms doubling to a 30 s cap, without jitter when random() is 0', async () => {
    const h = harness();
    const s = await connected(h);
    s.serverClose(1006);
    expect(h.events.at(-1)).toEqual({ type: 'status', status: 'waiting', closeCode: 1006 });
    const delays: number[] = [];
    for (let i = 0; i < 8; i += 1) {
      const delay = h.clock.pending()[0] ?? -1;
      delays.push(delay);
      h.clock.advance(delay);
      await flush();
      h.last().serverClose(1006);
    }
    expect(delays).toEqual([500, 1000, 2000, 4000, 8000, 16000, 30000, 30000]);
  });

  it('adds random() * 500 ms of jitter', async () => {
    const h = harness({ random: 0.5 });
    const s = await connected(h);
    s.serverClose(1006);
    expect(h.clock.pending()).toEqual([750]);
  });

  it('resets the back-off on ready', async () => {
    const h = harness();
    h.gw.start();
    await flush();
    h.last().serverClose(1006);
    h.clock.advance(500);
    await flush();
    h.last().serverClose(1006);
    expect(h.clock.pending()).toEqual([1000]);
    h.clock.advance(1000);
    await flush();
    const s = h.last();
    s.open();
    s.receive(hello());
    s.receive(ready());
    s.serverClose(1006);
    expect(h.clock.pending()).toEqual([500]);
  });

  it('honours reconnect.after_ms and treats invalid_session as a reconnect', async () => {
    const h = harness();
    const s1 = await connected(h);
    expect(h.gw.acceptedVersions).toEqual({ wire: 2n, e2ee: 1n, media: 1n });
    s1.receive(encodeFrame(Op.reconnect, 0, null, ['going away', 2500]));
    expect(h.gw.acceptedVersions).toBeNull();
    expect(s1.closedWith?.code).toBe(1000);
    expect(h.events.at(-1)).toEqual({ type: 'status', status: 'waiting', closeCode: 1000 });
    expect(h.clock.pending()).toEqual([2500]);
    h.clock.advance(2500);
    await flush();
    const s2 = h.last();
    s2.open();
    s2.receive(hello({ wire: [3, 2] }));
    expect(h.gw.acceptedVersions).toBeNull();
    s2.receive(ready({ wire: 3 }));
    expect(h.gw.acceptedVersions).toEqual({ wire: 3n, e2ee: 1n, media: 1n });
    s2.receive(encodeFrame(Op.invalidSession, 0, null, [0, 'unknown resume token']));
    expect(s2.closedWith?.code).toBe(1000);
    expect(h.clock.pending()).toEqual([500]);
  });

  it('treats a failed ticket mint as a failure and retries it', async () => {
    let calls = 0;
    const h = harness({
      mint: () => {
        calls += 1;
        return calls <= 2 ? Promise.reject(new Error('E_NETWORK')) : Promise.resolve('t');
      },
    });
    h.gw.start();
    await flush();
    expect(h.gw.status).toBe('waiting');
    expect(h.events.at(-1)).toEqual({ type: 'status', status: 'waiting', closeCode: null });
    expect(h.clock.pending()).toEqual([500]);
    h.clock.advance(500);
    await flush();
    expect(h.clock.pending()).toEqual([1000]);
    h.clock.advance(1000);
    await flush();
    expect(calls).toBe(3);
    expect(FakeSocket.instances).toHaveLength(1);
    expect(h.last().protocols).toEqual(['dilla.v1', 'dilla.ticket.t']);
  });

  it('bounds a ticket mint that never settles by the hello timeout, then reconnects and ignores the late ticket (GATEWAY-RESEED-02)', async () => {
    const releases: ((ticket: string) => void)[] = [];
    const h = harness({ mint: () => new Promise<string>((resolve) => { releases.push(resolve); }) });
    h.gw.start();
    await flush();
    expect(h.gw.status).toBe('connecting');
    h.clock.advance(GATEWAY.helloTimeoutMs - 1);
    await flush();
    expect(h.gw.status).toBe('connecting');
    h.clock.advance(1);
    await flush();
    expect(h.gw.status).toBe('waiting');
    expect(h.events.at(-1)).toEqual({ type: 'status', status: 'waiting', closeCode: null });
    expect(h.clock.pending()).toEqual([500]);
    releases[0]?.('late');
    await flush();
    expect(FakeSocket.instances).toHaveLength(0);
    h.clock.advance(500);
    await flush();
    expect(releases).toHaveLength(2);
    releases[1]?.('fresh');
    await flush();
    expect(FakeSocket.instances).toHaveLength(1);
    expect(h.last().protocols).toEqual(['dilla.v1', 'dilla.ticket.fresh']);
  });

  it('never sends resume, subscribe or unsubscribe, and identifies afresh on every connect', async () => {
    const h = harness();
    const s1 = await connected(h);
    h.clock.advance(15_000);
    s1.serverClose(4010);
    h.clock.advance(h.clock.pending()[0] ?? 0);
    await flush();
    const s2 = h.last();
    s2.open();
    s2.receive(hello());
    s2.receive(ready());
    h.gw.commitAck(GROUP, 3n);
    const ops = [...s1.sentFrames(), ...s2.sentFrames()].map((f) => f.op);
    expect(ops).toEqual([Op.identify, Op.heartbeat, Op.identify, Op.commitAck]);
    expect(s2.sent[0]).toEqual(encodeFrame(Op.identify, 1, null, ['', 2, 1, 1, 0]));
    expect(s2.protocols).toEqual(['dilla.v1', 'dilla.ticket.t2']);
  });
});

// C2 review finding 2: the identify this gateway sends is the corpus identify (protocol/vectors/frames.json), the token
// aside (the corpus carries 'tok', the gateway '' because the ticket rides the subprotocol). A skew between the vector
// and the client's wire version fails here.
it('identifies with the frames.json identify vector, the token aside', async () => {
  const corpus = JSON.parse(readFileSync(fileURLToPath(new URL('../../../../protocol/vectors/frames.json', import.meta.url)), 'utf8')) as
    { cases: { name: string; op: number; n: number; payload: string }[] };
  const vector = corpus.cases.find((c) => c.name === 'identify');
  expect(vector).toBeDefined();
  const want = decode(fromHex(vector!.payload)) as CborValue[];
  expect(want[0]).toBe('tok');
  const h = harness();
  h.gw.start();
  await flush();
  const s = h.last();
  s.open();
  s.receive(hello());
  const sent = s.sentFrames()[0];
  expect([sent.op, sent.n]).toEqual([vector!.op, BigInt(vector!.n)]);
  expect(toHex(encode(['tok', ...sent.payload.slice(1)] as CborInput))).toBe(vector!.payload);
  expect(sent.payload[0]).toBe('');
});

// C2 review finding 1: the instance refuses a wire it does not speak with an error frame and close 4006 (S1). A tab open
// across an upgrade lands in the version state, as a hello without 2 does: idle 4006, no reconnect, no new ticket.
describe('Gateway server version close', () => {
  it('a server close 4006 after identify goes idle with 4006, arms no timer and opens no second socket', async () => {
    const h = harness();
    h.gw.start();
    await flush();
    const s = h.last();
    s.open();
    s.receive(hello());
    s.serverClose(4006);
    expect(h.gw.status).toBe('idle');
    expect(h.events.at(-1)).toEqual({ type: 'status', status: 'idle', closeCode: 4006 });
    expect(h.clock.pending()).toEqual([]);
    h.clock.advance(600_000);
    await flush();
    expect(FakeSocket.instances).toHaveLength(1);
    expect(h.minted()).toBe(1);
  });

  it('a server close 4006 on a ready socket goes idle with 4006 and stays down', async () => {
    const h = harness();
    const s = await connected(h);
    s.serverClose(4006);
    expect(statuses(h.events)).toEqual(['connecting:null', 'ready:null', 'idle:4006']);
    expect(h.clock.pending()).toEqual([]);
    h.clock.advance(600_000);
    await flush();
    expect(FakeSocket.instances).toHaveLength(1);
    expect(h.minted()).toBe(1);
  });

  it('control: an ordinary close still waits and reconnects', async () => {
    const h = harness();
    const s = await connected(h);
    s.serverClose(1006);
    expect(h.events.at(-1)).toEqual({ type: 'status', status: 'waiting', closeCode: 1006 });
    expect(h.clock.pending()).toHaveLength(1);
    h.clock.advance(h.clock.pending()[0] ?? 0);
    await flush();
    expect(FakeSocket.instances).toHaveLength(2);
    expect(h.minted()).toBe(2);
  });
});

describe('Gateway revocation and stop', () => {
  it('reports revoked on close 4004 and does not reconnect by itself', async () => {
    const h = harness();
    const s = await connected(h);
    s.serverClose(4004);
    expect(h.events.slice(-2)).toEqual([{ type: 'status', status: 'idle', closeCode: 4004 }, { type: 'revoked' }]);
    expect(h.gw.status).toBe('idle');
    expect(h.clock.pending()).toEqual([]);
    h.clock.advance(120_000);
    await flush();
    expect(FakeSocket.instances).toHaveLength(1);
    expect(h.minted()).toBe(1);
    h.gw.start();
    await flush();
    expect(FakeSocket.instances).toHaveLength(2);
  });

  /** A mint that succeeds until `refuseWith` is set, then rejects with a DillaHttpError of that status. */
  function refusingMint() {
    const m = { calls: 0, refuseWith: null as number | null };
    const mint = (): Promise<string> => {
      m.calls += 1;
      if (m.refuseWith === null) return Promise.resolve(`t${m.calls}`);
      return Promise.reject(new DillaHttpError({
        status: m.refuseWith, code: m.refuseWith === 401 ? 'E_UNAUTHENTICATED' : 'E_HTTP', detail: '', retryAfterMs: null, extra: [],
      }));
    };
    return { m, mint };
  }

  it('a revoked device with a dead socket reaches revoked', async () => {
    const { m, mint } = refusingMint();
    const h = harness({ mint });
    const s = await connected(h);
    expect(h.gw.status).toBe('ready');
    s.serverClose(1006);
    expect(h.events.at(-1)).toEqual({ type: 'status', status: 'waiting', closeCode: 1006 });
    expect(h.clock.pending()).toEqual([500]);
    m.refuseWith = 401;
    const seen = h.events.length;
    h.clock.advance(500);
    await flush();
    expect(h.events.slice(seen)).toEqual([
      { type: 'status', status: 'connecting', closeCode: null },
      { type: 'status', status: 'idle', closeCode: null },
      { type: 'revoked' },
    ]);
    expect(h.gw.status).toBe('idle');
    expect(m.calls).toBe(2);
    expect(h.clock.pending()).toEqual([]);
    h.clock.advance(120_000);
    await flush();
    expect(m.calls).toBe(2);
    expect(FakeSocket.instances).toHaveLength(1);
  });

  it('a mint that fails for another reason is retried', async () => {
    const { m, mint } = refusingMint();
    const h = harness({ mint });
    const s = await connected(h);
    s.serverClose(1006);
    m.refuseWith = 503;
    const seen = h.events.length;
    h.clock.advance(500);
    await flush();
    expect(h.events.slice(seen)).toEqual([
      { type: 'status', status: 'connecting', closeCode: null },
      { type: 'status', status: 'waiting', closeCode: null },
    ]);
    expect(h.gw.status).toBe('waiting');
    expect(h.events.some((e) => e.type === 'revoked')).toBe(false);
    expect(m.calls).toBe(2);
    expect(h.clock.pending()).toEqual([1000]);
    h.clock.advance(1000);
    await flush();
    expect(m.calls).toBe(3);
  });

  it('stop closes with 1000, cancels every timer and reports idle', async () => {
    const h = harness();
    const s = await connected(h);
    h.gw.stop();
    expect(s.closedWith?.code).toBe(1000);
    expect(h.gw.status).toBe('idle');
    expect(h.clock.pending()).toEqual([]);
    expect(h.events.at(-1)).toEqual({ type: 'status', status: 'idle', closeCode: null });
    h.gw.stop();
    expect(statuses(h.events).filter((x) => x === 'idle:null')).toHaveLength(1);
  });

  it('opens no socket for a ticket that arrives after stop', async () => {
    let release: (ticket: string) => void = () => undefined;
    const h = harness({ mint: () => new Promise<string>((resolve) => { release = resolve; }) });
    h.gw.start();
    h.gw.stop();
    release('late');
    await flush();
    expect(FakeSocket.instances).toHaveLength(0);
    expect(h.gw.status).toBe('idle');
  });

  it('drops commitAck unless ready', async () => {
    const h = harness();
    h.gw.commitAck(GROUP, 1n);
    h.gw.start();
    await flush();
    const s = h.last();
    s.open();
    s.receive(hello());
    h.gw.commitAck(GROUP, 2n);
    expect(s.sentFrames().map((f) => f.op)).toEqual([Op.identify]);
    s.receive(ready());
    h.gw.commitAck(GROUP, 9n);
    expect(s.sent.at(-1)).toEqual(encodeFrame(Op.commitAck, 0, GROUP, [9n]));
  });

  it('keeps running when a listener throws, and rethrows the error from a microtask', async () => {
    const deferred: (() => void)[] = [];
    const spy = vi.spyOn(globalThis, 'queueMicrotask').mockImplementation((cb: () => void) => {
      deferred.push(cb);
    });
    try {
      const h = harness();
      h.gw.subscribe(() => {
        throw new Error('listener failed');
      });
      const s = await connected(h);
      expect(h.gw.status).toBe('ready');
      expect(s.closedWith).toBeNull();
      expect(h.events.map((e) => e.type)).toEqual(['status', 'status', 'ready']);
      expect(deferred.length).toBe(3);
      expect(() => deferred[0]?.()).toThrow('listener failed');
    } finally {
      spy.mockRestore();
    }
  });
});


it.each([2, 3, 4, 5])('uses the selected wire %i to gate build notices', async wire => {
  const h = harness(); h.gw.start(); await flush();
  const s = h.last(); s.open(); s.receive(hello({ wire: [wire] })); s.receive(ready({ wire }));
  expect(h.gw.acceptedVersions?.wire).toBe(BigInt(wire));
  s.receive(encode([56, 0, null, ['/assets/../bad.js']]));
  expect(h.gw.status).toBe('ready');
  s.receive(encode([56, 0, null, ['/assets/new.js']]));
  expect(h.events.filter(e => e.type === 'frame' && e.frame.op === 56)).toHaveLength(wire === 5 ? 1 : 0);
  h.gw.stop(); expect(h.gw.acceptedVersions).toBeNull();
});

it('coalesces real activity on wire 5, drops congestion and never replays it', async () => {
  const h = harness(); h.gw.start(); await flush(); const s = h.last(); s.open();
  s.receive(hello({ wire: [5,4,3,2] })); s.receive(ready({ wire: 5 }));
  expect(h.gw.sendPresenceActivity()).toBe(true);
  h.gw.sendPresenceActivity(); h.clock.advance(4999); h.gw.sendPresenceActivity();
  expect(s.sentFrames().filter(f => f.op === 57)).toHaveLength(1);
  h.clock.advance(1); h.gw.sendPresenceActivity();
  expect(s.sentFrames().filter(f => f.op === 57)).toHaveLength(2);
  s.bufferedAmount = 65537; h.clock.advance(5000); h.gw.sendPresenceActivity();
  expect(s.sentFrames().filter(f => f.op === 57)).toHaveLength(2);
  s.serverClose(1006); expect(h.gw.sendPresenceActivity()).toBe(false);
});

it('sends ninety current call samples over wire 5 in fifteen minutes, bounded and never replayed', async () => {
  const h = harness(); h.gw.start(); await flush(); const s = h.last(); s.open();
  s.receive(hello({ wire: [5,4,3,2] })); s.receive(ready({ wire: 5 }));
  for (let i = 0; i < 90; i++) {
    expect(h.gw.sendCallStats(GROUP, [1,null,25,0,0,20])).toBe(true);
    h.gw.sendCallStats(GROUP, [1,null,26,0,0,20]);
    h.clock.advance(10_000); s.receive(encodeFrame(Op.heartbeatAck,0,null,[1_700_000_000+i*10]));
  }
  const samples = s.sentFrames().filter(f => f.op === 58);
  expect(samples).toHaveLength(90);
  expect(samples.every(f => f.n === 0n && f.groupId === null)).toBe(true);
  s.bufferedAmount = 65537;
  expect(h.gw.sendCallStats(GROUP,[1,null,25,0,0,20])).toBe(true);
  expect(s.sentFrames().filter(f => f.op === 58)).toHaveLength(90);
  s.serverClose(1006);
  expect(h.gw.sendCallStats(GROUP,[1,null,25,0,0,20])).toBe(false);
});
