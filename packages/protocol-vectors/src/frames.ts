// Gateway frame vectors (02-delivery-service.md, "Gateway frames"). One accept case per opcode
// and one reject case per refusal rule, so the Go gateway, a future TypeScript client and the
// Rust testkit all agree byte for byte on the four-element frame.
// Every module in this package imports with the `.ts` extension: `tsconfig.json` sets
// `allowImportingTsExtensions: true` and `npm run vectors` is
// `node --experimental-strip-types src/generate.ts`, so a `.js` specifier resolves to a file that
// does not exist (`generate.ts:4` and `envelope.test.ts:4-7` are the existing examples).
import { encode } from './cbor.ts';
import { hex } from './bytes.ts';

const GROUP = new Uint8Array(16).fill(0x07);
const DEVICE = new Uint8Array(16).fill(0x08);
const USER = new Uint8Array(16).fill(0x09);
const CHANNEL = new Uint8Array(16).fill(0x0c);
const CALL = new Uint8Array(16).fill(0x0d);
const COMMUNITY = new Uint8Array(16).fill(0x0e);
const TAG = new Uint8Array(32).fill(0x0a);
const TOKEN = new Uint8Array(32).fill(0x0b);
const IDEM = new Uint8Array(16).fill(0x0f);
const REQUEST = new Uint8Array(16).fill(0x10);

type Case = { name: string; op: number; n: number; group_id: string; payload: string; frame: string };

function frame(name: string, op: number, n: number, group: Uint8Array | null, payload: unknown[]): Case {
  const payloadBytes = encode(payload as never);
  const frameBytes = encode([op, n, group, payload] as never);
  return {
    name,
    op,
    n,
    group_id: group ? hex(group) : '',
    payload: hex(payloadBytes),
    frame: hex(frameBytes),
  };
}

export function frameVectors() {
  const cases: Case[] = [
    // The last two elements are invariant 7's back-off window, backoff_ms and backoff_jitter_ms.
    // Bot v2: the instance offers wire version 6 alone (the only wire), the client identifies with it,
    // and ready echoes it (web3.8 S1 fix round 1 ruling).
    frame('hello', 0, 0, null, [[6], [1], [1], 30000, 131584, new Uint8Array(16).fill(0x01), 1, 300, 300]),
    frame('identify', 1, 1, null, ['tok', 6, 1, 1, 0]),
    frame('resume', 2, 2, null, ['tok', TOKEN, 1, 41]),
    frame('ready', 3, 1, null, [DEVICE, USER, 1, TOKEN, 6, 1, 1, 32, [[GROUP, 6, 4127, 0]], new Uint8Array(16).fill(0x0c)]),
    // `resumed` carries the ROTATED resume token as element 2: the instance discards the token the
    // client just spent, so without it on the wire a client could resume exactly once per identify.
    frame('resumed', 4, 2, null, [42, 47, TOKEN]),
    frame('invalid_session', 5, 0, null, [0, 'unknown resume token']),
    frame('heartbeat', 6, 3, null, [47, 1]),
    frame('heartbeat_ack', 7, 0, null, [1758659640]),
    frame('reconnect', 8, 0, null, ['going away', 2500]),
    frame('error', 9, 0, null, [3, 'E_FRAME_SHAPE', 'payload has 2 elements, want 4']),
    frame('subscribe', 10, 4, null, [[GROUP]]),
    frame('unsubscribe', 11, 5, null, [[GROUP]]),
    // The one group-scoped client opcode: invariant 7's acknowledgement.
    frame('commit_ack', 12, 6, GROUP, [1]),
    frame('mls.handshake', 16, 6, GROUP, [4127, 41, 1, 3, new Uint8Array([0xde, 0xad])]),
    frame('mls.handshake from the instance', 16, 7, GROUP, [4128, 41, 0, null, new Uint8Array([0xbe])]),
    frame('mls.commit_needed', 17, 0, GROUP, [41, [new Uint8Array(32).fill(0x0c)], 2000, 1]),
    frame('mls.epoch_changed', 18, 8, GROUP, [42, 4129]),
    // web3.7 (R29): the 7th element is the upload class the host stored, 0 for a post and 1 for
    // any other type, or null for a legacy upload that carried none.
    frame('message.ct', 19, 9, GROUP, [4130, 42, DEVICE, new Uint8Array([1, 2, 3]), TAG, 1758659700, 0]),
    frame('message.ct with a null class', 19, 9, GROUP, [4130, 42, DEVICE, new Uint8Array([1, 2, 3]), TAG, 1758659700, null]),
    frame('mls.welcome', 20, 10, GROUP, [7, 42, 4129, new Uint8Array([4, 5]), new Uint8Array([6, 7]), TAG]),
    frame('message.deleted', 21, 11, GROUP, [4130, 1758659800]),
    frame('message.plain', 32, 12, null, [GROUP, 12, USER, new Uint8Array([8]), TAG, 0, 0]),
    frame('interaction', 33, 13, null, [GROUP, USER, 1, new Uint8Array([9])]),
    frame('presence', 48, 0, null, [USER, 1, 1758659000, 'at the forge']),
    frame('typing channel', 49, 0, null, [CHANNEL, USER, DEVICE, 1, 1758659010]),
    frame('voice_state', 50, 0, null, [USER, DEVICE, GROUP, 3]),
    frame('typing_set on', 51, 0, null, [CHANNEL, 1]),
    frame('voice_self muted', 52, 0, null, [CALL, 1]),
    // op 53 call_ring (S->C, web-3): [call_id, channel_id, user_id, state 0..2, expires_ts].
    // State 1 rings (user_id is the caller), 0 ends a ring, 2 tells the caller who declined.
    frame('call_ring state 1', 53, 0, null, [CALL, CHANNEL, USER, 1, 1758659030]),
    frame('call_ring state 0', 53, 0, null, [CALL, CHANNEL, USER, 0, 1758659030]),
    frame('call_ring state 2', 53, 0, null, [CALL, CHANNEL, USER, 2, 1758659030]),
    // op 54 community_changed (S->C, dilla-web-3.1): [community_id]. A member re-reads the
    // community's channels and settings.
    frame('community_changed', 54, 0, null, [COMMUNITY]),
    // op 55 afk_away (S->C, AW3): [community_id, user_id, since_ts]. since_ts is the unix second
    // the member became away in the community; 0 says the member is no longer away.
    frame('afk_away', 55, 0, null, [COMMUNITY, USER, 1758659040]),
    frame('afk_away cleared', 55, 0, null, [COMMUNITY, USER, 0]),
    // op 59 request (C->S, Bot v2): [id, method, route, body, idem | null]. n = 0: neither request
    // nor response is replayable; an unanswered request is re-sent after a reconnect with its idem.
    frame('request', 59, 0, null, [7, 'POST', '/v1/communities', new Uint8Array([0x80]), IDEM]),
    frame('request without idem', 59, 0, null, [8, 'GET', '/v1/communities?limit=50', new Uint8Array(0), null]),
    // op 60 response (S->C, Bot v2): [id, status, body], in any order.
    frame('response', 60, 0, null, [7, 201, new Uint8Array([0x80])]),
    // op 61 bot_catalog (S->C, Bot v2): [community_id, revision, filtered_catalog, publication | null].
    // The catalog is the commands this one member may use, each [name, description, operation,
    // args_shape, permission_bit, needs_bot_channel(uint 0|1), handler_kind]; args_shape is
    // [[name, type_code 0..5, required(uint 0|1)]...]. The publication is the Bot's command-key
    // publication, null before the Bot publishes one. n = 0: a member who missed it compares
    // revisions at connect sync.
    frame('bot_catalog', 61, 0, null, [COMMUNITY, 3, [['remind', 'Set a reminder', 20, [['when', 4, 1], ['text', 0, 1]], 0, 0, 0]], null]),
    // op 62 bot_reply (S->C, Bot v2): [request_id, seq, sealed, final(uint 0|1)], to the one device
    // that sent the command. The ephemeral range's one replayable frame: n != 0, held two minutes for
    // a suspended connection; a reply over the connection's reply ring budget is sent unringed with
    // n = 0, so a decoder accepts both for op 62.
    frame('bot_reply', 62, 5, null, [REQUEST, 1, new Uint8Array([0x83, 0x01, 0x41, 0x00, 0x41, 0x00]), 1]),
    // op 63 bot_state_changed (S->C, Bot v2): [scope_kind(0 user, 1 channel, 2 call), scope_id,
    // revision]. Content-free, n = 0: a client that missed it compares revisions at connect sync.
    frame('bot_state_changed', 63, 0, null, [1, CHANNEL, 4]),
    // op 64 bot_command (S->C, Bot runtime connection only, wire 6): [request_id, community_id,
    // user_id, device_id, sealed]. The identity is the sending connection's session. n != 0: since
    // Bot v2 task 12 it is sequenced in the Bot connection's ring, so a resume replays it.
    frame('bot_command', 64, 7, null, [REQUEST, COMMUNITY, USER, DEVICE, new Uint8Array([0x85, 0x01])]),
    // op 65 bot_event (S->C, Bot runtime connection only, wire 6, Bot v2 task 12): [kind, payload].
    // Kind 2 member is [community_id, user_id, joined(uint 0|1), role_ids]. A live event is ringed
    // (n != 0); a cold-start snapshot's body frames are sent with n = 0.
    frame('bot_event', 65, 8, null, [2, [COMMUNITY, USER, 1, [GROUP]]]),
    // op 66 bot_webhook (S->C, Bot runtime connection only, wire 6, Bot v2 task 13): [delivery_id,
    // webhook_id, community_id, channel_id, display_name, avatar_id(bstr 32) | null, text,
    // allow_everyone(uint 0|1)]. avatar_id names the avatar the instance fetched and re-encoded, never the
    // sender's URL. Never ringed (n = 0): sent only to a live Bot connection.
    frame('bot_webhook', 66, 0, null, [REQUEST, IDEM, COMMUNITY, CHANNEL, 'Terraria', TAG, 'The boss was defeated', 0]),
    // op 67 bot_webhook_ack (C->S, the Bot runtime connection's answer to one op 66): [delivery_id,
    // status], status 0 posted. n = 0.
    frame('bot_webhook_ack', 67, 0, null, [REQUEST, 0]),
  ];

  const rejects = [
    { name: 'three elements', frame: hex(encode([10, 1, null])), code: 'E_FRAME_SHAPE' },
    { name: 'five elements', frame: hex(encode([10, 1, null, [[GROUP]], 0])), code: 'E_FRAME_SHAPE' },
    { name: 'unknown opcode', frame: hex(encode([100, 1, null, []])), code: 'E_FRAME_TYPE' },
    { name: 'server opcode from a client', frame: hex(encode([3, 1, null, []])), code: 'E_FRAME_TYPE' },
    { name: 'payload is not an array', frame: hex(encode([10, 1, null, 'x'])), code: 'E_FRAME_SHAPE' },
    { name: 'subscribe with the wrong element count', frame: hex(encode([10, 1, null, [[GROUP], 1]])), code: 'E_FRAME_SHAPE' },
    { name: 'group_id on a control frame', frame: hex(encode([10, 1, GROUP, [[GROUP]]])), code: 'E_FRAME_SHAPE' },
    // A client opcode, deliberately: op 16 is `mls.handshake`, whose spec has `fromClient: false`,
    // so `Decode` would return E_FRAME_TYPE at the opcode check before it ever looked at
    // element 2. With op 10 the decoder reaches the `group_id` unmarshal, which refuses a
    // 15-byte bstr for a 16-byte id.ID — a different branch from the 'group_id on a control
    // frame' case above, which trips `gid != nil && !spec.grouped`.
    { name: 'group_id of the wrong length', frame: hex(encode([10, 1, new Uint8Array(15), [[GROUP]]])), code: 'E_FRAME_SHAPE' },
    // The mirror rule, reachable because opcode 12 `commit_ack` is the one group-scoped CLIENT
    // opcode: a grouped frame with a null group_id. `Decode`'s `gid == nil && spec.grouped`
    // branch answers it; without that branch the frame would reach `handle` with a nil GroupID
    // and invariant 7's acknowledgement would be silently dropped rather than refused.
    { name: 'commit_ack with no group_id', frame: hex(encode([12, 6, null, [1]])), code: 'E_FRAME_SHAPE' },
    // Deterministic-CBOR failures: a non-minimal integer head and a map where an array belongs.
    { name: 'non-minimal op', frame: '8418' + '0a' + '01f680', code: 'E_FRAME_CBOR' },
    { name: 'map instead of an array', frame: 'a100f6', code: 'E_FRAME_CBOR' },
    { name: 'old typing has four elements', frame: hex(encode([49, 0, null, [USER, DEVICE, 1, 1758659010]])), code: 'E_FRAME_SHAPE' },
    { name: 'typing_set channel id has 17 bytes', frame: hex(encode([51, 0, null, [new Uint8Array(17).fill(0x0c), 1]])), code: 'E_FRAME_SHAPE' },
    { name: 'voice_self flags 4', frame: hex(encode([52, 0, null, [CALL, 4]])), code: 'E_FRAME_SHAPE' },
    { name: 'typing_set has group_id', frame: hex(encode([51, 0, GROUP, [CHANNEL, 1]])), code: 'E_FRAME_SHAPE' },
    // op 53 is server-to-client, so the instance refuses any client op 53 at the opcode check:
    // the corpus code is the instance's, E_FRAME_TYPE, as for 'server opcode from a client'. A
    // client decoder refuses both as E_FRAME_SHAPE (state above 2; four elements, not five).
    { name: 'call_ring state 3', frame: hex(encode([53, 0, null, [CALL, CHANNEL, USER, 3, 1758659030]])), code: 'E_FRAME_TYPE' },
    { name: 'call_ring with 4 elements', frame: hex(encode([53, 0, null, [CALL, CHANNEL, USER, 1]])), code: 'E_FRAME_TYPE' },
    // op 54 is server-to-client as op 53 is: the instance's code is E_FRAME_TYPE, and a client
    // decoder refuses two elements, not one, as E_FRAME_SHAPE.
    { name: 'community_changed with 2 elements', frame: hex(encode([54, 0, null, [COMMUNITY, 1]])), code: 'E_FRAME_TYPE' },
    // op 19 is server-to-client as op 53 is: the instance's code is E_FRAME_TYPE at the opcode
    // check, and a client decoder refuses eight elements, not six or seven, as E_FRAME_SHAPE.
    { name: 'message.ct with 8 elements', frame: hex(encode([19, 9, GROUP, [4130, 42, DEVICE, new Uint8Array([1, 2, 3]), TAG, 1758659700, 0, 0]])), code: 'E_FRAME_TYPE' },
    // op 55 is server-to-client as op 54 is: the instance's code is E_FRAME_TYPE, and a client
    // decoder refuses four elements, not three, as E_FRAME_SHAPE.
    { name: 'afk_away with 4 elements', frame: hex(encode([55, 0, null, [COMMUNITY, USER, 1758659040, 1]])), code: 'E_FRAME_TYPE' },
    // op 59's body is a bstr: a text body is refused before the request is dispatched.
    { name: 'request with a text body', frame: hex(encode([59, 0, null, [7, 'POST', '/v1/communities', 'body', null]])), code: 'E_FRAME_SHAPE' },
    // op 60 is server-to-client: the instance's code is E_FRAME_TYPE, and a client decoder refuses
    // a status outside 100..599 as E_FRAME_SHAPE.
    { name: 'response with status 600', frame: hex(encode([60, 0, null, [7, 600, new Uint8Array(0)]])), code: 'E_FRAME_TYPE' },
  ];

  return {
    version: 1,
    description:
      'gateway frames (02-delivery-service.md). frame = deterministic CBOR of [op, n, group_id, payload]; payload is the payload array alone. rejects = byte strings a conforming decoder must refuse with the named code.',
    cases,
    rejects,
  };
}
