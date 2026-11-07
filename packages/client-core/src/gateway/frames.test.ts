import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { arr, CborError, decode, encode } from '../cbor';
import { fromHex, toHex } from '../hex';
import {
  afkAwayOf, botCatalogOf, botReplyOf, botStateChangedOf, callRingOf, communityChangedOf, decodeFrame, decodeResponseFrame, DROPPABLE_OPS,
  encodeFrame, encodeRequestFrame, frameOp, Op, voiceStateOf,
} from './frames';

interface FrameCase { name: string; op: number; n: number; group_id: string; payload: string; frame: string }
interface FrameReject { name: string; frame: string; code: string }
const vectors = JSON.parse(
  readFileSync(fileURLToPath(new URL('../../../../protocol/vectors/frames.json', import.meta.url)), 'utf8'),
) as { cases: FrameCase[]; rejects: FrameReject[] };

/** What this client does with each server-side reject vector: an error code, or null for "accepted". */
const CLIENT_VERDICT: Record<string, string | null> = {
  'three elements': 'E_FRAME_SHAPE',
  'five elements': 'E_FRAME_SHAPE',
  'unknown opcode': null,
  'server opcode from a client': 'E_FRAME_SHAPE',
  'payload is not an array': 'E_FRAME_SHAPE',
  'subscribe with the wrong element count': 'E_FRAME_SHAPE',
  'group_id on a control frame': 'E_FRAME_SHAPE',
  'group_id of the wrong length': 'E_FRAME_SHAPE',
  'commit_ack with no group_id': 'E_FRAME_SHAPE',
  'non-minimal op': 'E_CBOR_NONCANONICAL',
  'map instead of an array': 'E_CBOR_TYPE',
  'old typing has four elements': 'E_FRAME_SHAPE',
  'typing_set channel id has 17 bytes': 'E_FRAME_SHAPE',
  'voice_self flags 4': 'E_FRAME_SHAPE',
  'typing_set has group_id': 'E_FRAME_SHAPE',
  // The instance answers both E_FRAME_TYPE (op 53 is S->C); this client decodes op 53 and refuses
  // the shape: the 5-element spec and the state <= 2 check (L-GW-33).
  'call_ring state 3': 'E_FRAME_SHAPE',
  'call_ring with 4 elements': 'E_FRAME_SHAPE',
  // The instance answers E_FRAME_TYPE (op 54 is S->C); this client decodes op 54 and refuses two elements, not one.
  'community_changed with 2 elements': 'E_FRAME_SHAPE',
  // The instance answers E_FRAME_TYPE (op 19 is S->C); this client decodes op 19 and refuses eight elements (web3.7).
  'message.ct with 8 elements': 'E_FRAME_SHAPE',
  // The instance answers E_FRAME_TYPE (op 55 is S->C, AW3); this client decodes op 55 and refuses four elements, not three (AW4).
  'afk_away with 4 elements': 'E_FRAME_SHAPE',
  // Bot v2 socket requests: this client checks op 59's payload before it sends one, and refuses a status outside
  // 100..599 in op 60 (the instance answers E_FRAME_TYPE: op 60 is S->C).
  'request with a text body': 'E_FRAME_SHAPE',
  'response with status 600': 'E_FRAME_SHAPE',
};

function verdict(bytes: Uint8Array): string | null {
  try {
    decodeFrame(bytes);
    return null;
  } catch (err) {
    if (err instanceof CborError) return err.code;
    if (err instanceof Error) return err.message;
    throw err;
  }
}

describe('frames.json', () => {
  it('holds the 44 cases and 22 rejects this codec was written against', () => {
    expect(vectors.cases).toHaveLength(44);
    expect(vectors.rejects).toHaveLength(22);
    expect(Object.keys(CLIENT_VERDICT).sort()).toEqual(vectors.rejects.map((r) => r.name).sort());
  });

  for (const c of vectors.cases) {
    it(`encodes ${c.name} byte for byte`, () => {
      const payload = arr(decode(fromHex(c.payload)));
      const groupId = c.group_id === '' ? null : fromHex(c.group_id);
      expect(toHex(encodeFrame(c.op, c.n, groupId, payload))).toBe(c.frame);
    });

    it(`decodes ${c.name}`, () => {
      const frame = decodeFrame(fromHex(c.frame));
      expect(frame.op).toBe(c.op);
      expect(frame.n).toBe(BigInt(c.n));
      expect(frame.groupId === null ? '' : toHex(frame.groupId)).toBe(c.group_id);
      expect(toHex(encode(frame.payload))).toBe(c.payload);
    });
  }

  for (const r of vectors.rejects) {
    it(`gives the client verdict on the reject "${r.name}"`, () => {
      expect(verdict(fromHex(r.frame))).toBe(CLIENT_VERDICT[r.name]);
    });
  }
});

describe('frame shape', () => {
  it('accepts an unknown opcode with any payload', () => {
    expect(decodeFrame(encode([99, 1, null, []]))).toEqual({ op: 99, n: 1n, groupId: null, payload: [] });
  });

  it('refuses an opcode above 255, a text group id and a wrong ready length', () => {
    expect(verdict(encode([256, 0, null, []]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encode([19, 1, 'g', [1, 2, 3, 4, 5, 6]]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encode([Op.ready, 1, null, [1, 2, 3]]))).toBe('E_FRAME_SHAPE');
  });

  it('refuses to encode a group id that is not 16 bytes, or a bad op or n', () => {
    expect(() => encodeFrame(Op.commitAck, 0, new Uint8Array(15), [1])).toThrow('E_FRAME_SHAPE');
    expect(() => encodeFrame(256, 0, null, [])).toThrow('E_FRAME_SHAPE');
    expect(() => encodeFrame(Op.heartbeat, -1, null, [0, 1])).toThrow('E_FRAME_SHAPE');
    expect(() => encodeFrame(Op.heartbeat, 1.5, null, [0, 1])).toThrow('E_FRAME_SHAPE');
  });

  // web3.7 (R29): message.ct carries the upload class as its 7th element (0, 1 or null). Six elements is a web3.6
  // host, kept for one release as a unit case only; the corpus holds the 7-element shape.
  it('reads message.ct with or without its class, and refuses another count or class', () => {
    const ct = [4130, 42, fid(8), new Uint8Array([1, 2, 3]), new Uint8Array(32).fill(10), 1758659700];
    const g = fid(7);
    expect(decodeFrame(encode([19, 9, g, [...ct, 0]])).payload[6]).toBe(0n);
    expect(decodeFrame(encode([19, 9, g, [...ct, 1]])).payload[6]).toBe(1n);
    expect(decodeFrame(encode([19, 9, g, [...ct, null]])).payload[6]).toBeNull();
    expect(decodeFrame(encode([19, 9, g, ct])).payload).toHaveLength(6);
    expect(verdict(encode([19, 9, g, [...ct, 0, 0]]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encode([19, 9, g, [...ct, 2n]]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encode([19, 9, g, [...ct, 'post']]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encode([19, 9, null, [...ct, 0]]))).toBe('E_FRAME_SHAPE');
  });

  it('pins the opcode numbers', () => {
    expect(Op).toEqual({ hello: 0, identify: 1, ready: 3, invalidSession: 5, heartbeat: 6, heartbeatAck: 7, reconnect: 8,
      error: 9, commitAck: 12, handshake: 16, commitNeeded: 17, epochChanged: 18, messageCt: 19, welcome: 20, messageDeleted: 21,
      presence: 48, typing: 49, voiceState: 50, typingSet: 51, voiceSelf: 52, callRing: 53, communityChanged: 54, afkAway: 55, appBuild: 56, presenceActivity: 57, callStats: 58,
      request: 59, response: 60, botCatalog: 61, botReply: 62, botStateChanged: 63 });
  });
});

describe('socket request and response frames (ops 59, 60)', () => {
  const byName = (name: string): FrameCase => {
    const c = vectors.cases.find((x) => x.name === name);
    if (c === undefined) throw new Error(`no vector ${name}`);
    return c;
  };

  it('encodes the request vectors byte for byte', () => {
    expect(toHex(encodeRequestFrame({ id: 7, method: 'POST', route: '/v1/communities', body: new Uint8Array([0x80]),
      idem: new Uint8Array(16).fill(0x0f) }))).toBe(byName('request').frame);
    expect(toHex(encodeRequestFrame({ id: 8, method: 'GET', route: '/v1/communities?limit=50', body: new Uint8Array(0),
      idem: null }))).toBe(byName('request without idem').frame);
  });

  it('decodes the response vector', () => {
    expect(decodeResponseFrame(fromHex(byName('response').frame))).toEqual({ id: 7, status: 201, body: new Uint8Array([0x80]) });
  });

  it('refuses to encode a request the instance would refuse', () => {
    const ok = { id: 1, method: 'GET', route: '/v1/x', body: new Uint8Array(0), idem: null };
    expect(() => encodeRequestFrame({ ...ok, id: -1 })).toThrow('E_FRAME_SHAPE');
    expect(() => encodeRequestFrame({ ...ok, id: 1.5 })).toThrow('E_FRAME_SHAPE');
    expect(() => encodeRequestFrame({ ...ok, method: '' })).toThrow('E_FRAME_SHAPE');
    expect(() => encodeRequestFrame({ ...ok, route: '' })).toThrow('E_FRAME_SHAPE');
    expect(() => encodeRequestFrame({ ...ok, route: '/'.repeat(8193) })).toThrow('E_FRAME_SHAPE');
    expect(() => encodeRequestFrame({ ...ok, idem: new Uint8Array(0) })).toThrow('E_FRAME_SHAPE');
    expect(() => encodeRequestFrame({ ...ok, idem: new Uint8Array(65) })).toThrow('E_FRAME_SHAPE');
    // M4: a body over the instance's 16 MiB request cap would be refused (E_FRAME_LIMIT) and re-sent after every resume.
    expect(() => encodeRequestFrame({ ...ok, body: new Uint8Array(16 * 1024 * 1024 + 1) })).toThrow('E_FRAME_LIMIT');
    expect(encodeRequestFrame({ ...ok, body: new Uint8Array(16 * 1024 * 1024) }).length).toBeGreaterThan(16 * 1024 * 1024);
  });

  it('refuses a response of another op, shape or status', () => {
    expect(() => decodeResponseFrame(encode([60, 0, null, [7, 99, new Uint8Array(0)]]))).toThrow('E_FRAME_SHAPE');
    expect(() => decodeResponseFrame(encode([60, 0, null, [7, 200, 'body']]))).toThrow('E_FRAME_SHAPE');
    expect(() => decodeResponseFrame(encode([60, 0, null, [7, 200]]))).toThrow('E_FRAME_SHAPE');
    expect(() => decodeResponseFrame(encode([60, 0, fid(1), [7, 200, new Uint8Array(0)]]))).toThrow('E_FRAME_SHAPE');
    expect(() => decodeResponseFrame(encode([59, 0, null, [7, 'GET', '/v1/x', new Uint8Array(0), null]]))).toThrow('E_FRAME_SHAPE');
  });
});

const fid = (byte: number): Uint8Array => new Uint8Array(16).fill(byte);
it('requires all five typed fields of server typing', () => {
  const [channel, user, device] = [fid(1), fid(2), fid(3)];
  expect(decodeFrame(encodeFrame(Op.typing, 0, null, [channel, user, device, 1, 10])).payload).toHaveLength(5);
  expect(() => decodeFrame(encodeFrame(Op.typing, 0, null, [user, device, 1, 10]))).toThrow('E_FRAME_SHAPE');
  expect(() => decodeFrame(encodeFrame(Op.typing, 0, null, [fid(1).slice(1), user, device, 1, 10]))).toThrow('E_FRAME_SHAPE');
  expect(() => decodeFrame(encodeFrame(Op.typing, 0, null, [channel, user, device, 2, 10]))).toThrow('E_FRAME_SHAPE');
  expect(() => decodeFrame(encodeFrame(Op.typing, 0, null, [channel, user, device, 1, -1]))).toThrow('E_FRAME_SHAPE');
});
it('encodes ungrouped client signals', () => {
  const channel = fid(4);
  expect(decodeFrame(encodeFrame(Op.typingSet, 0, null, [channel, 1]))).toMatchObject({ op: 51, n: 0n, groupId: null });
  expect(decodeFrame(encodeFrame(Op.voiceSelf, 0, null, [channel, 3]))).toMatchObject({ op: 52, n: 0n, groupId: null });
  expect(() => decodeFrame(encodeFrame(Op.voiceSelf, 0, null, [channel, 4]))).toThrow('E_FRAME_SHAPE');
});
// Attacker: a hostile or buggy instance sending a ring the client cannot interpret; the decoder
// refuses it as E_FRAME_SHAPE so the gateway drops it without acting on it (L-GW-33).
it('accepts call_ring states 0..2 and refuses state 3, a short id and a missing element', () => {
  const [call, channel, user] = [fid(5), fid(6), fid(7)];
  for (const state of [0, 1, 2]) {
    expect(decodeFrame(encodeFrame(Op.callRing, 0, null, [call, channel, user, state, 1758659030]))).toMatchObject({ op: 53, n: 0n, groupId: null });
  }
  expect(() => decodeFrame(encodeFrame(Op.callRing, 0, null, [call, channel, user, 3, 1758659030]))).toThrow('E_FRAME_SHAPE');
  expect(() => decodeFrame(encodeFrame(Op.callRing, 0, null, [call, channel.slice(1), user, 1, 1758659030]))).toThrow('E_FRAME_SHAPE');
  expect(() => decodeFrame(encodeFrame(Op.callRing, 0, null, [call, channel, user, 1]))).toThrow('E_FRAME_SHAPE');
  expect(() => decodeFrame(encodeFrame(Op.callRing, 0, null, [call, channel, user, 1, 'soon']))).toThrow('E_FRAME_SHAPE');
  // L-GW-33: an ephemeral frame has n = 0.
  expect(() => decodeFrame(encodeFrame(Op.callRing, 1, null, [call, channel, user, 1, 1758659030]))).toThrow('E_FRAME_SHAPE');
});

// web-3 task 6 (L-TS-55): the typed reads of ops 50 and 53 the worker acts on.
describe('typed voice and ring frames', () => {
  const [call, channel, user, device] = [fid(5), fid(6), fid(7), fid(8)];

  it('op 50 decodes with every flag bit of 02 § voice_state (in_call, mute, deaf, video, screen)', () => {
    for (const flags of [0, 1, 3, 7, 9, 17, 31]) {
      const frame = decodeFrame(encodeFrame(Op.voiceState, 0, null, [user, device, call, flags]));
      expect(voiceStateOf(frame)).toEqual({ userId: user, deviceId: device, callId: call, flags });
    }
  });

  // Attacker: an instance sending a reserved flag bit; the decoder refuses it, and the gateway drops it (below).
  it('op 50 refuses flags above 31 and a short id', () => {
    expect(() => decodeFrame(encodeFrame(Op.voiceState, 0, null, [user, device, call, 32]))).toThrow('E_FRAME_SHAPE');
    expect(() => decodeFrame(encodeFrame(Op.voiceState, 0, null, [user, device.slice(1), call, 1]))).toThrow('E_FRAME_SHAPE');
  });

  it('op 53 decodes to { callId, channelId, userId, state, expiresTs }', () => {
    for (const state of [0, 1, 2] as const) {
      const frame = decodeFrame(encodeFrame(Op.callRing, 0, null, [call, channel, user, state, 1_700_000_030]));
      expect(callRingOf(frame)).toEqual({ callId: call, channelId: channel, userId: user, state, expiresTs: 1_700_000_030 });
    }
  });

  it('a typed read of a frame of another op, or an expiry past the safe integers, is a shape error', () => {
    expect(() => callRingOf({ op: Op.voiceState, n: 0n, groupId: null, payload: [user, device, call, 1n] })).toThrow('E_FRAME_SHAPE');
    expect(() => voiceStateOf({ op: Op.callRing, n: 0n, groupId: null, payload: [call, channel, user, 1n, 5n] })).toThrow('E_FRAME_SHAPE');
    expect(() => callRingOf(decodeFrame(encodeFrame(Op.callRing, 0, null, [call, channel, user, 1, 2n ** 60n]))))
      .toThrow('E_FRAME_SHAPE');
  });

  it('frameOp names the op of a frame whose payload does not decode, and null for a broken envelope', () => {
    expect(frameOp(encodeFrame(Op.callRing, 0, null, [call, channel, user, 3, 1]))).toBe(53);
    expect(frameOp(encodeFrame(Op.voiceState, 0, null, [user, device, call, 99]))).toBe(50);
    expect(frameOp(encode([53, 0, null]))).toBeNull();
    expect(frameOp(new Uint8Array([0xff]))).toBeNull();
  });
});

// dilla-web-3.1: op 54 community_changed [community_id]; a hostile or buggy instance's misshapen one is a shape error the
// gateway drops (DROPPABLE_OPS), and communityChangedOf reads the id of a well-formed one.
it('accepts community_changed with one id and refuses a short id, a second element and a group', () => {
  const community = fid(9);
  const frame = decodeFrame(encodeFrame(Op.communityChanged, 0, null, [community]));
  expect(frame).toMatchObject({ op: 54, n: 0n, groupId: null });
  expect(communityChangedOf(frame)).toEqual(community);
  expect(() => decodeFrame(encodeFrame(Op.communityChanged, 0, null, [community.slice(1)]))).toThrow('E_FRAME_SHAPE');
  expect(() => decodeFrame(encodeFrame(Op.communityChanged, 0, null, [community, 1]))).toThrow('E_FRAME_SHAPE');
  expect(() => decodeFrame(encodeFrame(Op.communityChanged, 0, fid(1), [community]))).toThrow('E_FRAME_SHAPE');
  expect(() => communityChangedOf({ op: Op.callRing, n: 0n, groupId: null, payload: [community] })).toThrow('E_FRAME_SHAPE');
  expect(DROPPABLE_OPS.has(Op.communityChanged)).toBe(true);
});

// AW4: op 55 afk_away [community_id, user_id, since_ts], since_ts 0 = cleared. A hostile or buggy instance's misshapen
// one is a shape error the gateway drops (DROPPABLE_OPS), and afkAwayOf reads a well-formed one.
describe('afk_away (op 55)', () => {
  const [community, user] = [fid(9), fid(10)];

  it('decodes a set and a clear to { communityId, userId, since }', () => {
    for (const since of [1758659040, 0]) {
      const frame = decodeFrame(encodeFrame(Op.afkAway, 0, null, [community, user, since]));
      expect(frame).toMatchObject({ op: 55, n: 0n, groupId: null });
      expect(afkAwayOf(frame)).toEqual({ communityId: community, userId: user, since });
    }
    expect(DROPPABLE_OPS.has(Op.afkAway)).toBe(true);
  });

  it('refuses a short id, a missing or extra element, a non-uint since, an n and a group', () => {
    expect(verdict(encodeFrame(Op.afkAway, 0, null, [community.slice(1), user, 1]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.afkAway, 0, null, [community, user.slice(1), 1]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.afkAway, 0, null, [community, user]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.afkAway, 0, null, [community, user, 1, 1]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.afkAway, 0, null, [community, user, 'now']))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.afkAway, 0, null, [community, user, fid(1)]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.afkAway, 1, null, [community, user, 1]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.afkAway, 0, fid(1), [community, user, 1]))).toBe('E_FRAME_SHAPE');
  });

  it('a typed read of another op, or a since past the safe integers, is a shape error', () => {
    expect(() => afkAwayOf({ op: Op.communityChanged, n: 0n, groupId: null, payload: [community, user, 1n] })).toThrow('E_FRAME_SHAPE');
    expect(() => afkAwayOf(decodeFrame(encodeFrame(Op.afkAway, 0, null, [community, user, 2n ** 60n])))).toThrow('E_FRAME_SHAPE');
  });
});


it('validates an ephemeral app-build hint and makes malformed hints droppable', () => {
  expect(decodeFrame(encode([56, 0, null, ['/assets/index-new.js']])).op).toBe(56);
  expect(DROPPABLE_OPS.has(56)).toBe(true);
  for (const entry of ['https://other.test/a.js', '/assets/../a.js', '/assets/a.css', 'x'.repeat(513), 3]) {
    expect(() => decodeFrame(encode([56, 0, null, [entry]]))).toThrow('E_FRAME_SHAPE');
  }
  expect(() => decodeFrame(encode([56, 1, null, ['/assets/a.js']]))).toThrow('E_FRAME_SHAPE');
});

// Bot v2 task 20: ops 61 bot_catalog, 62 bot_reply and 63 bot_state_changed. A misshapen one is a shape error the gateway
// drops (DROPPABLE_OPS): a bad Bot frame never closes the socket.
describe('Bot frames (ops 61, 62, 63)', () => {
  const vector = (name: string): FrameCase => vectors.cases.find((x) => x.name === name)!;

  it('reads the bot_catalog vector', () => {
    const catalog = botCatalogOf(decodeFrame(fromHex(vector('bot_catalog').frame)));
    expect(catalog.communityId).toEqual(fid(0x0e));
    expect(catalog.revision).toBe(3n);
    expect(catalog.commands).toHaveLength(1);
    expect(catalog.publication).toBeNull();
    expect(DROPPABLE_OPS.has(Op.botCatalog)).toBe(true);
  });

  it('reads the bot_reply vector, and a reply with n = 0 (over the ring budget)', () => {
    expect(botReplyOf(decodeFrame(fromHex(vector('bot_reply').frame))))
      .toEqual({ requestId: fid(0x10), seq: 1, sealed: fromHex('830141004100'), final: true });
    const unringed = decodeFrame(encodeFrame(Op.botReply, 0, null, [fid(1), 0, new Uint8Array([1]), 0]));
    expect(botReplyOf(unringed)).toEqual({ requestId: fid(1), seq: 0, sealed: new Uint8Array([1]), final: false });
    expect(DROPPABLE_OPS.has(Op.botReply)).toBe(true);
  });

  it('reads the bot_state_changed vector', () => {
    expect(botStateChangedOf(decodeFrame(fromHex(vector('bot_state_changed').frame)))).toEqual({ scopeKind: 1, scopeId: fid(0x0c), revision: 4n });
    expect(DROPPABLE_OPS.has(Op.botStateChanged)).toBe(true);
  });

  it('refuses misshapen Bot frames', () => {
    const ok61 = [fid(1), 2, [], null];
    expect(verdict(encodeFrame(Op.botCatalog, 0, null, ok61))).toBeNull();
    expect(verdict(encodeFrame(Op.botCatalog, 0, null, [fid(1), 2, [], new Uint8Array([1])]))).toBeNull();
    expect(verdict(encodeFrame(Op.botCatalog, 1, null, ok61))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.botCatalog, 0, null, [fid(1).slice(1), 2, [], null]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.botCatalog, 0, null, [fid(1), 'two', [], null]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.botCatalog, 0, null, [fid(1), 2, 'x', null]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.botCatalog, 0, null, [fid(1), 2, [], 'key']))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.botCatalog, 0, fid(2), ok61))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.botReply, 3, null, [fid(1), 0, new Uint8Array([1]), 2]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.botReply, 3, null, [fid(1), 2 ** 32, new Uint8Array([1]), 1]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.botReply, 3, null, [fid(1), 0, 'sealed', 1]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.botReply, 3, null, [fid(1), 0, new Uint8Array([1])]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.botStateChanged, 0, null, [3, fid(1), 1]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.botStateChanged, 1, null, [0, fid(1), 1]))).toBe('E_FRAME_SHAPE');
    expect(verdict(encodeFrame(Op.botStateChanged, 0, null, [0, fid(1)]))).toBe('E_FRAME_SHAPE');
    expect(() => botReplyOf({ op: Op.botCatalog, n: 0n, groupId: null, payload: [fid(1), 0n, new Uint8Array(1), 0n] })).toThrow('E_FRAME_SHAPE');
  });
});
