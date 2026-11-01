import { describe, expect, it } from 'vitest';
import { decode, encode, type CborInput } from '../cbor';
import { toHex } from '../hex';
import { type Bucket } from './client';
import { DillaHttpError } from './errors';
import {
  RecordingClient, bytesReply, cborReply, fakeTransport, noContent, refusal, textReply, type FakeOptions,
} from './fake-fetch';
import { HANDSHAKES_MAX, MEMBERS_PAGE, MESSAGES_MAX, MODERATION_REMOVALS_PAGE, Routes, WELCOMES_PAGE, profileTextProblem, profileTextValue } from './routes';

const fill = (n: number, b: number): Uint8Array => new Uint8Array(n).fill(b);
const INSTANCE = fill(16, 0x01);
const USER = fill(16, 0x02);
const DEVICE = fill(16, 0x03);
const COMMUNITY = fill(16, 0x04);
const CHANNEL = fill(16, 0x05);
const CATEGORY = fill(16, 0x06);
const GROUP = fill(16, 0x07);
const KEY_ID = fill(16, 0x08);
const BOT = fill(16, 0x13);
const ROLE = fill(16, 0x14);
const PUB = fill(32, 0x09);
const NONCE = fill(32, 0x0a);
const BLOB = fill(40, 0x0b);
const SIG = fill(64, 0x0c);
const ZERO32 = fill(32, 0x00);
const TAG = fill(32, 0x0d);
const COMMITMENT = fill(32, 0x0e);
const TREE = fill(24, 0x0f);
const HASH = fill(32, 0x10);
const REF = fill(32, 0x11);
const WIN = fill(48, 0x12);
const STORED = fill(5, 0x1a);
const ADMISSION_PAYLOAD = fill(128, 0x1b);
const ADMISSION_SIGNATURE = fill(64, 0x1c);

function octetReply(status: number, body: Uint8Array): Response {
  return new Response(body.slice(), { status, headers: { 'Content-Type': 'application/octet-stream', 'X-Dilla-Generation': '7' } });
}

const DEVICE_2 = fill(16, 0x19);
const PEER_USER = fill(16, 0x18);
const BLOB_ID = fill(32, 0x17);
const ASSERTION = 'asrt-1';
const OBJECT = encode([1, fill(12, 0x15), fill(86, 0x16)]);
const HISTORY = encode([[2, BLOB, SIG, HASH], [3, BLOB, SIG, HASH]]);

const BODY = encode(['request body built by the core', 1]);
const EMPTY_ARRAY = encode([]);
const INFO = encode([3, BLOB, HASH, 12]);
const TREE_BODY = encode([3, TREE, HASH]);
const HANDSHAKES = encode([[1, 0, 1, null, BLOB], [2, 1, 0, 0, BLOB]]);
const MESSAGES = encode([[3, 1, DEVICE, BLOB, COMMITMENT, TAG, 1_800_000_000, 0]]);
const PROPOSALS = encode([[REF, 1, 2, BLOB, 0]]);
const UPLOADED = encode([10, TAG, 1_800_000_001]);
const WELCOMES = encode([[5, GROUP, 1, 4, BLOB, TREE, HASH]]);
const INSTANCE_DOC = [[1], [1], [1], INSTANCE, 7, 'dilla.test', 0, [0, 3], 2, KEY_ID, PUB];

const hex = toHex;

function setup(opts: FakeOptions = {}) {
  const t = fakeTransport(opts);
  const http = new RecordingClient(t.deps);
  return { t, http, routes: new Routes(http) };
}

it('bounds a history page at one MiB and leaves HTTP error details worker-local', async () => {
  const { routes, t } = setup();
  t.replies.push(octetReply(200, new Uint8Array(1_048_577)));
  await expect(routes.getDeviceListHistory(USER, 0n)).rejects.toMatchObject({ code: 'E_BODY_TOO_LARGE' });
});

it('reads and writes the manager-approved self-role policy and member selection', async () => {
  const { routes, t, http } = setup();
  t.replies.push(cborReply(200, [[ROLE, 'Readers', 4, 0]]));
  await expect(routes.listSelfRoles(COMMUNITY)).resolves.toEqual([{ id: ROLE, name: 'Readers', position: 4, selfSelected: false }]);
  expect(http.requests[0]).toMatchObject({ method: 'GET', path: `/v1/communities/${hex(COMMUNITY)}/self-roles` });
  t.replies.push(cborReply(200, [USER, 1]));
  await routes.configureSelfRoles(COMMUNITY, USER, [ROLE]);
  expect(http.requests[1]).toMatchObject({ method: 'PUT', path: `/v1/communities/${hex(COMMUNITY)}/self-roles/policy`, body: encode([USER, [ROLE]]) });
  t.replies.push(noContent());
  await routes.selectSelfRole(COMMUNITY, ROLE, USER, true);
  expect(http.requests[2]).toMatchObject({ method: 'PUT', path: `/v1/communities/${hex(COMMUNITY)}/self-roles/${hex(ROLE)}`, body: encode([USER, 1]) });
});

it('registers only bounded unique mappings against an accepted Bot message identity', async () => {
  const { routes, t, http } = setup();
  t.replies.push(cborReply(200, [USER, 1]));
  await routes.registerRolePanel(COMMUNITY, USER, CHANNEL, 9n, [{ emoji: '✅', roleId: ROLE }]);
  expect(http.requests[0]).toMatchObject({ method: 'PUT', path: `/v1/communities/${hex(COMMUNITY)}/role-panels`,
    body: encode([USER, CHANNEL, 9n, [['✅', ROLE]]]), retry429: false });
  await expect(routes.registerRolePanel(COMMUNITY, USER, CHANNEL, 0n, [{ emoji: '✅', roleId: ROLE }])).rejects.toThrow(/invalid role panel identity/u);
  await expect(routes.registerRolePanel(COMMUNITY, USER, CHANNEL, 9n, [
    { emoji: '✅', roleId: ROLE }, { emoji: '✅', roleId: fill(16, 0x15) },
  ])).rejects.toThrow(/duplicate role panel mapping/u);
});

it('submits only a bounded explicit panel action after an accepted panel event', async () => {
  const { routes, t, http } = setup();
  t.replies.push(noContent());
  await routes.applyRolePanelReaction(COMMUNITY, CHANNEL, 9n, GROUP, 11n, '✅', true);
  expect(http.requests[0]).toMatchObject({ method: 'POST',
    path: `/v1/communities/${hex(COMMUNITY)}/role-panels/${hex(CHANNEL)}/9/reaction`,
    body: encode([GROUP, 11n, '✅', 1]), bucket: 'write', idempotent: true, retry429: false, ok: [204] });
  await expect(routes.applyRolePanelReaction(COMMUNITY, CHANNEL, 0n, GROUP, 11n, '✅', true)).rejects.toThrow(/invalid role panel reaction/u);
  await expect(routes.applyRolePanelReaction(COMMUNITY, CHANNEL, 9n, GROUP, 0n, '✅', false)).rejects.toThrow(/invalid role panel reaction/u);
});

it('returns the current interaction admission proof without interpreting its signed payload', async () => {
  const { routes, t, http } = setup();
  t.replies.push(cborReply(200, [ADMISSION_PAYLOAD, ADMISSION_SIGNATURE]));
  await expect(routes.getInteractionAdmissionContext(GROUP)).resolves.toEqual({
    payload: ADMISSION_PAYLOAD, signature: ADMISSION_SIGNATURE,
  });
  expect(http.requests[0]).toMatchObject({ method: 'GET', path: `/v1/groups/${hex(GROUP)}/interaction-admission-context`,
    bucket: 'read', idempotent: true, maxBodyBytes: 160 * 1024 });
});

it('rejects malformed current interaction admission proof envelopes', async () => {
  const { routes, t } = setup();
  t.replies.push(cborReply(200, [ADMISSION_PAYLOAD, fill(63, 0x1c)]));
  await expect(routes.getInteractionAdmissionContext(GROUP)).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
});

it('bounds the current interaction admission proof response to 160 KiB', async () => {
  const { routes, t } = setup();
  t.replies.push(bytesReply(200, fill(160 * 1024 + 1, 0)));
  await expect(routes.getInteractionAdmissionContext(GROUP)).rejects.toMatchObject({ code: 'E_BODY_TOO_LARGE', status: 200 });
});

it('reads the active bot join-role setting and writes it with a request ID', async () => {
  const { routes, t, http } = setup();
  t.replies.push(cborReply(200, [ROLE, BOT]));
  await expect(routes.getBotJoinRole(COMMUNITY)).resolves.toEqual({ roleId: ROLE, installationId: BOT });
  expect(http.requests[0]).toMatchObject({ method: 'GET', path: `/v1/communities/${hex(COMMUNITY)}/bot-join-role` });
  t.replies.push(cborReply(200, [USER, BOT]));
  await expect(routes.configureBotJoinRole(COMMUNITY, USER, ROLE, BOT)).resolves.toEqual(BOT);
  expect(http.requests[1]).toMatchObject({ method: 'PUT', path: `/v1/communities/${hex(COMMUNITY)}/bot-join-role`,
    body: encode([USER, ROLE, BOT]), idempotent: true });
});

// Task 10c: Glenn's Restart and the Music page's defaults; DJ is the control_music role bit, not a route any more.
it('asks for a Bot restart bound to the installation and reads the restart counters', async () => {
  const { routes, t, http } = setup();
  t.replies.push(cborReply(200, [2, 1]));
  await expect(routes.restartBot(COMMUNITY, BOT)).resolves.toEqual({ requested: 2n, handled: 1n });
  expect(http.requests[0]).toMatchObject({ method: 'POST', path: `/v1/communities/${hex(COMMUNITY)}/bot/restart`,
    body: encode([BOT]), idempotent: true });
  t.replies.push(cborReply(200, [1, 2]));
  await expect(routes.restartBot(COMMUNITY, BOT)).rejects.toMatchObject({ code: 'E_CBOR_RANGE' });
});

it('writes the music defaults and reads the widened installation back', async () => {
  const { routes, t, http } = setup();
  const widened = [BOT, COMMUNITY, 3, 4, USER, ROLE, 1 | 1 << 8 | 1 << 9, 99, 1, 1, '0.4.2', 50, 15];
  t.replies.push(cborReply(200, widened));
  await expect(routes.setBotMusicSettings(COMMUNITY, BOT, 50, 15)).resolves.toMatchObject({
    capabilities: 0x301, extra: { restartRequested: 1n, restartHandled: 1n, version: '0.4.2', musicVolume: 50, musicIdleMinutes: 15 } });
  expect(http.requests[0]).toMatchObject({ method: 'PUT', path: `/v1/communities/${hex(COMMUNITY)}/bot/music-settings`,
    body: encode([BOT, 50, 15]), idempotent: true });
  await expect(routes.setBotMusicSettings(COMMUNITY, BOT, 60, 15)).rejects.toThrow(/^E_ROUTE_INPUT: /);
  await expect(routes.setBotMusicSettings(COMMUNITY, BOT, 50, 10)).rejects.toThrow(/^E_ROUTE_INPUT: /);
  expect(http.requests).toHaveLength(1);
  for (const bad of [[...widened.slice(0, 11), 60, 15], [...widened.slice(0, 8), 1, 2, null, 50, 15], [...widened.slice(0, 10), '', 50, 15],
    [...widened.slice(0, 6), 1 << 10, 99]]) {
    t.replies.push(cborReply(200, bad));
    await expect(routes.getBotInstallation(COMMUNITY)).rejects.toThrow(/^E_CBOR_(RANGE|SHAPE)/);
  }
  t.replies.push(cborReply(200, widened.slice(0, 8)));
  await expect(routes.getBotInstallation(COMMUNITY)).resolves.not.toHaveProperty('extra');
});

function member(i: number): Uint8Array {
  const id = new Uint8Array(16);
  id[15] = i;
  return id;
}

interface Case {
  name: string;
  call(r: Routes): Promise<unknown>;
  method: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
  path: string;
  bucket: Bucket;
  idempotent: boolean;
  auth: boolean;
  accept?: string;
  sent: Uint8Array | null;
  reply(): Response;
  afterReply?: () => Response;
  expected: unknown;
}

const CASES: Case[] = [
  {
    name: 'AFK snapshot reads [id, timeout, [], pending]', call: r => r.getAFK(COMMUNITY),
    method: 'GET', path: `/v1/communities/${hex(COMMUNITY)}/afk`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [CHANNEL, 300, [], 1]),
    expected: { channelId: CHANNEL, timeoutSeconds: 300, pending: true, reason: 1, away: [] },
  },
  {
    // AW5 fix round 3: pending is a reason code; 2 is a cut because this user chose AFK on another device or tab.
    name: 'AFK snapshot reads pending reason 2 (AFK chosen elsewhere)', call: r => r.getAFK(COMMUNITY),
    method: 'GET', path: `/v1/communities/${hex(COMMUNITY)}/afk`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [CHANNEL, 300, [], 2, []]),
    expected: { channelId: CHANNEL, timeoutSeconds: 300, pending: true, reason: 2, away: [] },
  },
  {
    // AW4: the fifth element is the away roster [[user_id, since_ts]].
    name: 'AFK snapshot reads the away roster of a 5-element answer', call: r => r.getAFK(COMMUNITY),
    method: 'GET', path: `/v1/communities/${hex(COMMUNITY)}/afk`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [CHANNEL, 300, [], 0, [[member(1), 1758659040], [member(2), 1758659100]]]),
    expected: { channelId: CHANNEL, timeoutSeconds: 300, pending: false, reason: 0,
      away: [{ userId: member(1), since: 1758659040 }, { userId: member(2), since: 1758659100 }] },
  },
  {
    name: 'AFK snapshot of a 3-element answer is not pending', call: r => r.getAFK(COMMUNITY),
    method: 'GET', path: `/v1/communities/${hex(COMMUNITY)}/afk`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [null, 300, []]),
    expected: { channelId: null, timeoutSeconds: 300, pending: false, reason: 0, away: [] },
  },
  {
    name: 'AFK away (choosing AFK) sends [] and takes 204', call: r => r.putAFKAway(COMMUNITY),
    method: 'PUT', path: `/v1/communities/${hex(COMMUNITY)}/afk/away`, bucket: 'write', idempotent: true, auth: true,
    sent: encode([]), reply: () => noContent(), expected: undefined,
  },
  {
    name: 'AFK activity names the actual voice room', call: r => r.sendAFKActivity(COMMUNITY, CHANNEL),
    method: 'POST', path: `/v1/communities/${hex(COMMUNITY)}/afk/activity`, bucket: 'write', idempotent: true, auth: true,
    sent: encode([CHANNEL]), reply: () => noContent(), expected: undefined,
  },
  {
    name: 'AFK activity of an exempt report sends [id, 1]', call: r => r.sendAFKActivity(COMMUNITY, CHANNEL, true),
    method: 'POST', path: `/v1/communities/${hex(COMMUNITY)}/afk/activity`, bucket: 'write', idempotent: true, auth: true,
    sent: encode([CHANNEL, 1]), reply: () => noContent(), expected: undefined,
  },
  {
    name: 'AFK setting writes the timeout only', call: r => r.setAFKSettings(COMMUNITY, 300),
    method: 'PUT', path: `/v1/communities/${hex(COMMUNITY)}/afk/settings`, bucket: 'write', idempotent: true, auth: true,
    sent: encode([300]), reply: () => noContent(), expected: undefined,
  },
  {
    // S2 review (M5): the session's bearer goes along whenever there is one, so instance.discovery = "session" (E, F)
    // answers an owed client too; before sign-in token() is null and nothing is sent (below).
    name: 'getInstance reads elements 3 to 8 and 10, with the session bearer', call: (r) => r.getInstance(),
    method: 'GET', path: '/v1/instance', bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, INSTANCE_DOC),
    expected: { instanceId: INSTANCE, generation: 7n, name: 'dilla.test', registrationMode: 0, authMethods: [0, 3], policyVersion: 2n, externalSenderPub: PUB },
  },
  {
    name: 'getInvite asks for CBOR and escapes the code', call: (r) => r.getInvite('AB/CD EF'),
    method: 'GET', path: '/i/AB%2FCD%20EF', bucket: 'none', idempotent: true, auth: false, accept: 'application/cbor', sent: null,
    reply: () => cborReply(200, ['dilla.test', COMMUNITY, 1_900_000_000, 'loot']),
    expected: { communityId: COMMUNITY, communityName: 'loot', expires: 1_900_000_000n },
  },
  {
    name: 'getInvite of an instance invite', call: (r) => r.getInvite('ABCD'),
    method: 'GET', path: '/i/ABCD', bucket: 'none', idempotent: true, auth: false, accept: 'application/cbor', sent: null,
    reply: () => cborReply(200, ['dilla.test', null, 1_900_000_000, null]),
    expected: { communityId: null, communityName: null, expires: 1_900_000_000n },
  },
  {
    name: 'postAccount sends the core body unchanged', call: (r) => r.postAccount(BODY),
    method: 'POST', path: '/v1/accounts', bucket: 'none', idempotent: false, auth: false, sent: BODY,
    reply: () => cborReply(200, [USER, DEVICE, 'tok-new', 1_900_000_000]),
    expected: { userId: USER, deviceId: DEVICE, token: 'tok-new', expires: 1_900_000_000n },
  },
  {
    name: 'postChallenge sends an empty array', call: (r) => r.postChallenge(DEVICE),
    method: 'POST', path: `/v1/devices/${hex(DEVICE)}/sessions/challenge`, bucket: 'none', idempotent: true, auth: false, sent: EMPTY_ARRAY,
    reply: () => cborReply(201, [NONCE, 1_800_000_060]),
    expected: { nonce: NONCE, expires: 1_800_000_060n },
  },
  {
    name: 'postSession reads all seven elements', call: (r) => r.postSession(DEVICE, BODY),
    method: 'POST', path: `/v1/devices/${hex(DEVICE)}/sessions`, bucket: 'none', idempotent: false, auth: false, sent: BODY,
    reply: () => cborReply(201, ['tok-2', 0, USER, DEVICE, 1_800_604_800, 1_800_043_200, 7]),
    expected: { token: 'tok-2', scope: 0, userId: USER, deviceId: DEVICE, expires: 1_800_604_800n, idleExpires: 1_800_043_200n, generation: 7n },
  },
  {
    name: 'putDeviceList', call: (r) => r.putDeviceList(USER, BODY),
    method: 'PUT', path: `/v1/users/${hex(USER)}/device-list`, bucket: 'write', idempotent: true, auth: true, sent: BODY,
    reply: () => noContent(), expected: undefined,
  },
  {
    name: 'getDeviceList', call: (r) => r.getDeviceList(USER),
    method: 'GET', path: `/v1/users/${hex(USER)}/device-list`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [1, BLOB, SIG, ZERO32]), expected: { version: 1n, blob: BLOB, raw: encode([1, BLOB, SIG, ZERO32]) },
  },

  {
    name: 'getDeviceList answers null for a 404', call: (r) => r.getDeviceList(USER),
    method: 'GET', path: `/v1/users/${hex(USER)}/device-list`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => refusal(404, 'E_NOT_FOUND', 'not found'), expected: null,
  },
  {
    name: 'postKeyPackages', call: (r) => r.postKeyPackages(BODY),
    method: 'POST', path: '/v1/keypackages', bucket: 'write', idempotent: false, auth: true, sent: BODY,
    reply: () => cborReply(201, [32]), expected: 32,
  },
  {
    name: 'postTicket', call: (r) => r.postTicket(),
    method: 'POST', path: '/v1/gateway/ticket', bucket: 'write', idempotent: false, auth: true, sent: EMPTY_ARRAY,
    reply: () => cborReply(201, ['tkt-1', 1_800_000_030]), expected: { ticket: 'tkt-1', expires: 1_800_000_030n },
  },
  {
    name: 'listCommunities', call: (r) => r.listCommunities(),
    method: 'GET', path: '/v1/communities', bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [[COMMUNITY, 'loot', USER, 1]]),
    expected: [{ id: COMMUNITY, name: 'loot', owner: USER, policyVersion: 1n }],
  },
  {
    name: 'joinCommunity with an invite', call: (r) => r.joinCommunity(COMMUNITY, 'AB12'),
    method: 'POST', path: `/v1/communities/${hex(COMMUNITY)}/join`, bucket: 'none', idempotent: true, auth: true, sent: encode(['AB12']),
    reply: () => cborReply(200, [COMMUNITY]), expected: undefined,
  },
  {
    name: 'joinCommunity without an invite', call: (r) => r.joinCommunity(COMMUNITY, null),
    method: 'POST', path: `/v1/communities/${hex(COMMUNITY)}/join`, bucket: 'none', idempotent: true, auth: true, sent: encode([null]),
    reply: () => cborReply(200, [COMMUNITY]), expected: undefined,
  },
  // v0.1.0-web3.1: the create flow (internal/api communities.go createCommunityReq, channels.go createChannelReq,
  // invites.go createInviteReq), each a fixed-position array.
  {
    name: 'createCommunity sends [name, empty policy, 0, 0] and reads [id, role_everyone, policy_version]',
    call: (r) => r.createCommunity('Midgard Crew'),
    method: 'POST', path: '/v1/communities', bucket: 'write', idempotent: false, auth: true,
    sent: encode(['Midgard Crew', new Uint8Array(0), 0, 0]),
    reply: () => cborReply(201, [COMMUNITY, ROLE, 1]),
    expected: { id: COMMUNITY, roleEveryone: ROLE, policyVersion: 1n },
  },
  {
    name: 'createChannel of a text channel: kind 0, the e2ee mode, private, top level, no topic, no slowmode',
    call: (r) => r.createChannel(COMMUNITY, { kind: 'text', name: 'general', position: 0 }),
    method: 'POST', path: `/v1/communities/${hex(COMMUNITY)}/channels`, bucket: 'write', idempotent: false, auth: true,
    sent: encode([0, 0, 0, null, 'general', '', 0, 0]),
    reply: () => cborReply(201, [CHANNEL, 0, 0]),
    expected: { id: CHANNEL, mode: 0, visibility: 0 },
  },
  {
    name: 'createChannel inside a category sends the category as parent_id',
    call: (r) => r.createChannel(COMMUNITY, { kind: 'text', name: 'plans', position: 2, parentId: CHANNEL }),
    method: 'POST', path: `/v1/communities/${hex(COMMUNITY)}/channels`, bucket: 'write', idempotent: false, auth: true,
    sent: encode([0, 0, 0, CHANNEL, 'plans', '', 2, 0]),
    reply: () => cborReply(201, [CHANNEL, 0, 0]),
    expected: { id: CHANNEL, mode: 0, visibility: 0 },
  },
  {
    name: 'createChannel of a voice channel is kind 1 at its position',
    call: (r) => r.createChannel(COMMUNITY, { kind: 'voice', name: 'general', position: 1 }),
    method: 'POST', path: `/v1/communities/${hex(COMMUNITY)}/channels`, bucket: 'write', idempotent: false, auth: true,
    sent: encode([1, 0, 0, null, 'general', '', 1, 0]),
    reply: () => cborReply(201, [CHANNEL, 0, 0]),
    expected: { id: CHANNEL, mode: 0, visibility: 0 },
  },
  {
    name: 'createInvite sends [max_uses, ttl_seconds, 0] and reads [invite_id, code, url, expires]',
    call: (r) => r.createInvite(COMMUNITY, { maxUses: 10, ttlSeconds: 604_800 }),
    method: 'POST', path: `/v1/communities/${hex(COMMUNITY)}/invites`, bucket: 'write', idempotent: false, auth: true,
    sent: encode([10, 604_800, 0]),
    reply: () => cborReply(201, [KEY_ID, 'AB12CD34', 'https://dilla.test/i/AB12CD34', 1_800_604_800]),
    expected: { inviteId: KEY_ID, code: 'AB12CD34', url: 'https://dilla.test/i/AB12CD34', expires: 1_800_604_800n },
  },
  {
    name: 'listChannels reads text_group_id', call: (r) => r.listChannels(COMMUNITY),
    method: 'GET', path: `/v1/communities/${hex(COMMUNITY)}/channels`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [
      [CATEGORY, 2, 0, 0, null, 'text', '', 0, 0, 0, null],
      [CHANNEL, 0, 0, 0, CATEGORY, 'general', 'say hi', 1, 30, 5, GROUP],
    ]),
    expected: [
      { id: CATEGORY, kind: 2, mode: 0, visibility: 0, parentId: null, name: 'text', topic: '', position: 0, slowmode: 0, seq: 0n, textGroupId: null },
      { id: CHANNEL, kind: 0, mode: 0, visibility: 0, parentId: CATEGORY, name: 'general', topic: 'say hi', position: 1, slowmode: 30, seq: 5n, textGroupId: GROUP },
    ],
  },
  {
    name: 'listMembers reads names and kind', call: (r) => r.listMembers(COMMUNITY),
    method: 'GET', path: `/v1/communities/${hex(COMMUNITY)}/members`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [
      [USER, 1_700_000_000, '', [], 'jonas', 'Jonas', 0, 'on a call', 'hello\nthere'],
      [BOT, 1_700_000_100, 'helper', [ROLE], 'bot1', 'Bot One', 1],
    ]),
    expected: [
      { userId: USER, username: 'jonas', display: 'Jonas', kind: 0, nick: '', roleIds: [], statusMsg: 'on a call', bio: 'hello\nthere' },
      { userId: BOT, username: 'bot1', display: 'Bot One', kind: 1, nick: 'helper', roleIds: [ROLE], statusMsg: '', bio: '' },
    ],
  },
  {
    name: 'postGroup', call: (r) => r.postGroup(BODY),
    method: 'POST', path: '/v1/groups', bucket: 'write', idempotent: false, auth: true, sent: BODY,
    reply: () => cborReply(201, [GROUP, 1]), expected: { nextSeq: 1n },
  },
  {
    name: 'getGroupInfo returns the body unchanged', call: (r) => r.getGroupInfo(GROUP),
    method: 'GET', path: `/v1/groups/${hex(GROUP)}/info`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => bytesReply(200, INFO), expected: INFO,
  },
  {
    name: 'getGroupHealState decodes the member status', call: (r) => r.getGroupHealState(GROUP),
    method: 'GET', path: `/v1/groups/${hex(GROUP)}/heal-state`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [3, 14, 9, 4, 1]), expected: { epoch: 3n, nextSeq: 14n, generation: 9n, needFromSeq: 4n, needsHeal: true },
  },
  {
    name: 'postGroupHeal submits snapshot and reads status', call: async (r) => { const result = await r.postGroupHeal(GROUP, BODY); return result; },
    method: 'POST', path: `/v1/groups/${hex(GROUP)}/heal`, bucket: 'write', idempotent: false, auth: true, sent: BODY,
    reply: () => cborReply(200, [3, 14]), expected: { epoch: 3n, nextSeq: 14n },
  },
  {
    name: 'getGroupTree returns the body unchanged', call: (r) => r.getGroupTree(GROUP),
    method: 'GET', path: `/v1/groups/${hex(GROUP)}/tree`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => bytesReply(200, TREE_BODY), expected: TREE_BODY,
  },
  {
    name: 'getHandshakes counts rows and reads the last seq', call: (r) => r.getHandshakes(GROUP, 1n, 512),
    method: 'GET', path: `/v1/groups/${hex(GROUP)}/handshakes?from=1&limit=512`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => bytesReply(200, HANDSHAKES), expected: { raw: HANDSHAKES, count: 2, lastSeq: 2n },
  },
  {
    name: 'getHandshakes of an empty page', call: (r) => r.getHandshakes(GROUP, 9n, 512),
    method: 'GET', path: `/v1/groups/${hex(GROUP)}/handshakes?from=9&limit=512`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => bytesReply(200, EMPTY_ARRAY), expected: { raw: EMPTY_ARRAY, count: 0, lastSeq: null },
  },
  {
    name: 'getMessages counts rows and reads the last seq', call: (r) => r.getMessages(GROUP, 3n, 256),
    method: 'GET', path: `/v1/groups/${hex(GROUP)}/messages?from=3&limit=256`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => bytesReply(200, MESSAGES), expected: { raw: MESSAGES, count: 1, lastSeq: 3n },
  },
  {
    name: 'getProposals returns the body unchanged', call: (r) => r.getProposals(GROUP),
    method: 'GET', path: `/v1/groups/${hex(GROUP)}/proposals`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => bytesReply(200, PROPOSALS), expected: PROPOSALS,
  },
  {
    name: 'postCommit', call: (r) => r.postCommit(GROUP, BODY),
    method: 'POST', path: `/v1/groups/${hex(GROUP)}/commit`, bucket: 'commit', idempotent: false, auth: true, sent: BODY,
    reply: () => cborReply(200, [9, 2]), expected: { seq: 9n, epoch: 2n },
  },
  {
    name: 'postMessage returns the body for the core and its seq', call: (r) => r.postMessage(GROUP, BODY),
    method: 'POST', path: `/v1/groups/${hex(GROUP)}/message`, bucket: 'message', idempotent: false, auth: true, sent: BODY,
    reply: () => bytesReply(200, UPLOADED), expected: { raw: UPLOADED, seq: 10n },
  },
  {
    name: 'postCursor', call: (r) => r.postCursor(GROUP, BODY),
    method: 'POST', path: `/v1/groups/${hex(GROUP)}/cursor`, bucket: 'write', idempotent: true, auth: true, sent: BODY,
    reply: () => noContent(), expected: undefined,
  },
  {
    name: 'postResync', call: (r) => r.postResync(GROUP, BODY),
    method: 'POST', path: `/v1/groups/${hex(GROUP)}/resync`, bucket: 'commit', idempotent: false, auth: true, sent: BODY,
    reply: () => cborReply(200, [11, 3]), expected: { seq: 11n, epoch: 3n },
  },
  {
    name: 'getWelcomes counts rows and reads the last welcome id', call: (r) => r.getWelcomes(0n),
    method: 'GET', path: '/v1/welcomes?after=0&limit=64', bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => bytesReply(200, WELCOMES), expected: { raw: WELCOMES, count: 1, lastId: 5n },
  },
  {
    name: 'deleteWelcome', call: (r) => r.deleteWelcome(5n),
    method: 'DELETE', path: '/v1/welcomes/5', bucket: 'write', idempotent: true, auth: true, sent: null,
    reply: () => noContent(), expected: undefined,
  },
  {
    name: 'deleteWelcome counts a 404 as done', call: (r) => r.deleteWelcome(5n),
    method: 'DELETE', path: '/v1/welcomes/5', bucket: 'write', idempotent: true, auth: true, sent: null,
    reply: () => refusal(404, 'E_NOT_FOUND', 'welcome'), expected: undefined,
  },
  {
    name: 'getAccountMe ignores an appended element', call: (r) => r.getAccountMe(),
    method: 'GET', path: '/v1/accounts/me', bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [USER, 'jonas', 'Jonas', 0, 1, 1_700_000_000, 3, 'on a call', 'bio text', 'appended later']),
    expected: { userId: USER, username: 'jonas', display: 'Jonas', kind: 0, flags: 1n, presenceChoice: 3, statusMsg: 'on a call', bio: 'bio text' },
  },
  {
    name: 'getAccountMe reads a status message and bio as empty from an instance that sends neither', call: (r) => r.getAccountMe(),
    method: 'GET', path: '/v1/accounts/me', bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [USER, 'jonas', 'Jonas', 0, 1, 1_700_000_000, 3]),
    expected: { userId: USER, username: 'jonas', display: 'Jonas', kind: 0, flags: 1n, presenceChoice: 3, statusMsg: '', bio: '' },
  },
  {
    name: 'getLimits reads elements 0, 4, 5, 7 and 8', call: (r) => r.getLimits(),
    method: 'GET', path: '/v1/instance/limits', bucket: 'read', idempotent: true, auth: false, sent: null,
    reply: () => cborReply(200, [131072, 104857600, 4, 2, 32, 8, 10737418240, 30000, 131584, 30, 30]),
    expected: { maxCiphertextBytes: 131072, keypackagesPerDevice: 32, keypackageRefillThreshold: 8, heartbeatMs: 30000, maxFrameBytes: 131584 },
  },
  {
    name: 'passwordLogin sends both strings without a token', call: (r) => r.passwordLogin('ada', 'hunter2 hunter2'),
    method: 'POST', path: '/v1/auth/password/login', bucket: 'none', idempotent: false, auth: false, sent: encode(['ada', 'hunter2 hunter2']),
    reply: () => cborReply(200, [ASSERTION, 1]), expected: { assertion: ASSERTION, needs: 1 },
  },
  {
    name: 'passwordLogin without a second factor', call: (r) => r.passwordLogin('ada', 'pw'),
    method: 'POST', path: '/v1/auth/password/login', bucket: 'none', idempotent: false, auth: false, sent: encode(['ada', 'pw']),
    reply: () => cborReply(200, [ASSERTION, 0]), expected: { assertion: ASSERTION, needs: 0 },
  },
  {
    name: 'totpVerify', call: (r) => r.totpVerify(ASSERTION, '123456'),
    method: 'POST', path: '/v1/auth/totp/verify', bucket: 'none', idempotent: false, auth: false, sent: encode([ASSERTION, '123456']),
    reply: () => cborReply(200, ['asrt-2']), expected: { assertion: 'asrt-2' },
  },
  {
    name: 'postSessionPending reads the pending answer', call: (r) => r.postSessionPending(DEVICE, BODY),
    method: 'POST', path: `/v1/devices/${hex(DEVICE)}/sessions`, bucket: 'none', idempotent: false, auth: false, sent: BODY,
    reply: () => cborReply(201, ['tok-p', 1, USER, DEVICE, 1_800_604_800, 1_800_043_200, 7]),
    expected: { token: 'tok-p', scope: 1, userId: USER, deviceId: DEVICE, expires: 1_800_604_800n, idleExpires: 1_800_043_200n, generation: 7n },
  },
  {
    name: 'listBackups', call: (r) => r.listBackups(),
    method: 'GET', path: '/v1/backups', bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [[0, 0, 103, 1_800_000_000], [1, 0, 2048, 1_800_000_100]]),
    expected: [{ kind: 0, chunkSeq: 0, size: 103, created: 1_800_000_000n }, { kind: 1, chunkSeq: 0, size: 2048, created: 1_800_000_100n }],
  },
  {
    name: 'getBackup of the root object', call: (r) => r.getBackup(0),
    method: 'GET', path: '/v1/backups/0/0', bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [OBJECT, 1_800_000_000]), expected: { object: OBJECT, created: 1_800_000_000n },
  },
  {
    name: 'getBackup answers null for a 404', call: (r) => r.getBackup(1),
    method: 'GET', path: '/v1/backups/1/0', bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => refusal(404, 'E_NOT_FOUND', 'no such backup'), expected: null,
  },
  {
    name: 'putBackup of a new object', call: (r) => r.putBackup(0, OBJECT),
    method: 'PUT', path: '/v1/backups/0/0', bucket: 'write', idempotent: true, auth: true, sent: encode([OBJECT]),
    reply: () => cborReply(201, [BLOB_ID, 103]), expected: { blobId: BLOB_ID, size: 103, created: true },
  },
  {
    name: 'putBackup of a replaced object', call: (r) => r.putBackup(1, OBJECT),
    method: 'PUT', path: '/v1/backups/1/0', bucket: 'write', idempotent: true, auth: true, sent: encode([OBJECT]),
    reply: () => cborReply(200, [BLOB_ID, 103]), expected: { blobId: BLOB_ID, size: 103, created: false },
  },
  {
    name: 'listDevices', call: (r) => r.listDevices(),
    method: 'GET', path: '/v1/devices', bucket: 'read', idempotent: true, auth: true, sent: null,
    // The seventh element is the owner's label; a row of six (an instance before device names) has none.
    reply: () => cborReply(200, [[DEVICE, 1, 1, null, null, 1_800_000_000, 'work laptop'], [DEVICE_2, 0, 0, 1_799_000_000, 1_800_000_050, 1_799_999_999]]),
    expected: [
      { id: DEVICE, tier: 1, signerTier: 1, verifiedAt: null, revokedAt: null, lastSeen: 1_800_000_000n, label: 'work laptop' },
      { id: DEVICE_2, tier: 0, signerTier: 0, verifiedAt: 1_799_000_000n, revokedAt: 1_800_000_050n, lastSeen: 1_799_999_999n, label: null },
    ],
  },
  {
    name: 'listDevices with a null label', call: (r) => r.listDevices(),
    method: 'GET', path: '/v1/devices', bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [[DEVICE, 1, 1, null, null, 1_800_000_000, null]]),
    expected: [{ id: DEVICE, tier: 1, signerTier: 1, verifiedAt: null, revokedAt: null, lastSeen: 1_800_000_000n, label: null }],
  },
  {
    name: 'patchDevice names a device', call: (r) => r.patchDevice(DEVICE, 'work laptop'),
    method: 'PATCH', path: `/v1/devices/${hex(DEVICE)}`, bucket: 'write', idempotent: true, auth: true, sent: encode(['work laptop']),
    reply: () => noContent(), expected: undefined,
  },
  {
    name: 'patchDevice with null clears the name', call: (r) => r.patchDevice(DEVICE, null),
    method: 'PATCH', path: `/v1/devices/${hex(DEVICE)}`, bucket: 'write', idempotent: true, auth: true, sent: encode([null]),
    reply: () => noContent(), expected: undefined,
  },
  {
    name: 'deleteSessions', call: (r) => r.deleteSessions(DEVICE),
    method: 'DELETE', path: `/v1/devices/${hex(DEVICE)}/sessions`, bucket: 'write', idempotent: true, auth: true, sent: null,
    reply: () => noContent(), expected: undefined,
  },
  {
    name: 'getDeviceListHistory returns the body for the core and its last version', call: (r) => r.getDeviceListHistory(USER, 1n),
    method: 'GET', path: `/v1/users/${hex(USER)}/device-list?after=1`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => bytesReply(200, HISTORY), expected: { raw: HISTORY, count: 2, lastVersion: 3n },
  },
  {
    name: 'getDeviceListHistory with nothing newer', call: (r) => r.getDeviceListHistory(USER, 3n),
    method: 'GET', path: `/v1/users/${hex(USER)}/device-list?after=3`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => bytesReply(200, EMPTY_ARRAY), expected: { raw: EMPTY_ARRAY, count: 0, lastVersion: null },
  },
  {
    name: 'postDm opens a new DM', call: (r) => r.postDm([PEER_USER]),
    method: 'POST', path: '/v1/dms', bucket: 'write', idempotent: false, auth: true, sent: encode([[PEER_USER]]),
    reply: () => cborReply(201, [CHANNEL]), expected: { channelId: CHANNEL, created: true },
  },
  {
    name: 'postDm finds the existing DM', call: (r) => r.postDm([PEER_USER]),
    method: 'POST', path: '/v1/dms', bucket: 'write', idempotent: false, auth: true, sent: encode([[PEER_USER]]),
    reply: () => cborReply(200, [CHANNEL]), expected: { channelId: CHANNEL, created: false },
  },
  {
    name: 'listDms reads a null member list as empty', call: (r) => r.listDms(),
    method: 'GET', path: '/v1/dms', bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [[CHANNEL, 3, [USER, PEER_USER]], [CATEGORY, 4, null]]),
    expected: [{ channelId: CHANNEL, kind: 3, members: [USER, PEER_USER] }, { channelId: CATEGORY, kind: 4, members: [] }],
  },
  {
    name: 'getChannel of a DM reads the twelve-element row', call: (r) => r.getChannel(CHANNEL),
    method: 'GET', path: `/v1/channels/${hex(CHANNEL)}`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [CHANNEL, null, 3, 0, 0, null, '', '', 0, 0, 4, GROUP]),
    expected: { id: CHANNEL, kind: 3, mode: 0, visibility: 0, parentId: null, name: '', topic: '', position: 0, slowmode: 0, seq: 4n, textGroupId: GROUP },
  },
  {
    name: 'getChannel skips community_id and reads slow mode', call: (r) => r.getChannel(CHANNEL),
    method: 'GET', path: `/v1/channels/${hex(CHANNEL)}`, bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [CHANNEL, COMMUNITY, 0, 0, 0, CATEGORY, 'general', 'say hi', 1, 30, 5, null]),
    expected: { id: CHANNEL, kind: 0, mode: 0, visibility: 0, parentId: CATEGORY, name: 'general', topic: 'say hi', position: 1, slowmode: 30, seq: 5n, textGroupId: null },
  },
  {
    name: 'deleteDevice removes an unlisted device row (73)', call: (r) => r.deleteDevice(DEVICE_2),
    method: 'DELETE', path: `/v1/devices/${hex(DEVICE_2)}`, bucket: 'write', idempotent: false, auth: true, sent: null,
    reply: () => noContent(), expected: undefined,
  },
{
  name: 'deleteGroupMessage', call: (r) => r.deleteGroupMessage(GROUP, 12n),
  method: 'DELETE', path: `/v1/groups/${hex(GROUP)}/messages/12`, bucket: 'write', idempotent: true, auth: true, sent: null,
  reply: () => noContent(), expected: 'deleted',
},
{
  name: 'deleteGroupMessage answers gone for a 404', call: (r) => r.deleteGroupMessage(GROUP, 12n),
  method: 'DELETE', path: `/v1/groups/${hex(GROUP)}/messages/12`, bucket: 'write', idempotent: true, auth: true, sent: null,
  reply: () => refusal(404, 'E_NOT_FOUND', 'no such object'), afterReply: () => cborReply(200, []), expected: 'gone',
},
{
  name: 'putBlob of new bytes', call: (r) => r.putBlob(CHANNEL, BLOB_ID, STORED),
  method: 'PUT', path: `/v1/channels/${hex(CHANNEL)}/blobs/${hex(BLOB_ID)}`, bucket: 'upload', idempotent: true, auth: true,
  sent: STORED, reply: () => cborReply(201, [BLOB_ID, 5]), expected: { created: true, size: 5 },
},
{
  name: 'putBlob of bytes the instance already holds', call: (r) => r.putBlob(CHANNEL, BLOB_ID, STORED),
  method: 'PUT', path: `/v1/channels/${hex(CHANNEL)}/blobs/${hex(BLOB_ID)}`, bucket: 'upload', idempotent: true, auth: true,
  sent: STORED, reply: () => cborReply(200, [BLOB_ID, 5]), expected: { created: false, size: 5 },
},
{
  name: 'confirmBlob sends no body', call: (r) => r.confirmBlob(CHANNEL, BLOB_ID),
  method: 'POST', path: `/v1/channels/${hex(CHANNEL)}/blobs/${hex(BLOB_ID)}/confirm`, bucket: 'write', idempotent: true,
  auth: true, sent: null, reply: () => noContent(), expected: undefined,
},
{
  name: 'getBlob returns the stored bytes as served', call: (r) => r.getBlob(CHANNEL, BLOB_ID),
  method: 'GET', path: `/v1/channels/${hex(CHANNEL)}/blobs/${hex(BLOB_ID)}`, bucket: 'read', idempotent: true, auth: true,
  sent: null, reply: () => octetReply(200, STORED), expected: STORED,
},
{
  name: 'getBlob answers null for a 404', call: (r) => r.getBlob(CHANNEL, BLOB_ID),
  method: 'GET', path: `/v1/channels/${hex(CHANNEL)}/blobs/${hex(BLOB_ID)}`, bucket: 'read', idempotent: true, auth: true,
  sent: null, reply: () => refusal(404, 'E_NOT_FOUND', 'no such object'), expected: null,
},
{
  name: 'deleteBlob', call: (r) => r.deleteBlob(CHANNEL, BLOB_ID),
  method: 'DELETE', path: `/v1/channels/${hex(CHANNEL)}/blobs/${hex(BLOB_ID)}`, bucket: 'write', idempotent: true, auth: true,
  sent: null, reply: () => noContent(), expected: undefined,
},
{
  name: 'deleteBlob counts a 404 as done', call: (r) => r.deleteBlob(CHANNEL, BLOB_ID),
  method: 'DELETE', path: `/v1/channels/${hex(CHANNEL)}/blobs/${hex(BLOB_ID)}`, bucket: 'write', idempotent: true, auth: true,
  sent: null, reply: () => refusal(404, 'E_NOT_FOUND', 'no such object'), expected: undefined,
},
];

// ---- web3.6: second factors (flow 10) ----
const CRED = fill(32, 0x20);
const CEREMONY = fill(16, 0x21);
const CODES = ['AAAAAAAAAAAAAAAAAAAAAAAAAA', 'BBBBBBBBBBBBBBBBBBBBBBBBBB'];
const OPTIONS = '{"publicKey":{}}';
const RESPONSE = '{"id":"x"}';

const FACTOR_CASES: Case[] = [
  {
    name: 'recoveryVerify', call: (r) => r.recoveryVerify(ASSERTION, 'AAAAAAAAAAAAAAAAAAAAAAAAAA'),
    method: 'POST', path: '/v1/auth/recovery/verify', bucket: 'none', idempotent: false, auth: false,
    sent: encode([ASSERTION, 'AAAAAAAAAAAAAAAAAAAAAAAAAA']), reply: () => cborReply(200, ['asrt-3', 'appended']), expected: { assertion: 'asrt-3' },
  },
  {
    name: 'passwordLogin reads needs 2', call: (r) => r.passwordLogin('ada', 'pw'),
    method: 'POST', path: '/v1/auth/password/login', bucket: 'none', idempotent: false, auth: false, sent: encode(['ada', 'pw']),
    reply: () => cborReply(200, [ASSERTION, 2]), expected: { assertion: ASSERTION, needs: 2 },
  },
  {
    name: 'getFactors reads the six elements (Task 11: password_changed)', call: (r) => r.getFactors(),
    method: 'GET', path: '/v1/auth/factors', bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [1, 1_800_000_000, [[CRED, 'Laptop', 1_800_000_100, null], [fill(16, 0x22), 'Phone', 1_800_000_200, 1_800_000_300]], 4, 1, 1_800_000_400, 'appended']),
    expected: { owed: true, totp: 1_800_000_000, backupCodesLeft: 4, hasPassword: true, passwordChanged: 1_800_000_400,
      passkeys: [{ credId: CRED, name: 'Laptop', created: 1_800_000_100, lastUsed: null }, { credId: fill(16, 0x22), name: 'Phone', created: 1_800_000_200, lastUsed: 1_800_000_300 }] },
  },
  {
    name: 'getFactors of an account with no app and no password', call: (r) => r.getFactors(),
    method: 'GET', path: '/v1/auth/factors', bucket: 'read', idempotent: true, auth: true, sent: null,
    reply: () => cborReply(200, [0, null, [], 0, 0]), expected: { owed: false, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: false, passwordChanged: null },
  },
  {
    name: 'totpEnroll without a step-up sends null', call: (r) => r.totpEnroll(null),
    method: 'POST', path: '/v1/auth/totp/enroll', bucket: 'write', idempotent: false, auth: true, sent: encode([null]),
    reply: () => cborReply(200, ['JBSWY3DPEHPK3PXP', 'otpauth://totp/x?secret=JBSWY3DPEHPK3PXP']), expected: { secret: 'JBSWY3DPEHPK3PXP', uri: 'otpauth://totp/x?secret=JBSWY3DPEHPK3PXP' },
  },
  {
    name: 'totpEnroll with a step-up token', call: (r) => r.totpEnroll('su-1'),
    method: 'POST', path: '/v1/auth/totp/enroll', bucket: 'write', idempotent: false, auth: true, sent: encode(['su-1']),
    reply: () => cborReply(200, ['S', 'U']), expected: { secret: 'S', uri: 'U' },
  },
  {
    name: 'totpConfirm returns the minted codes', call: (r) => r.totpConfirm('123456'),
    method: 'POST', path: '/v1/auth/totp/confirm', bucket: 'write', idempotent: false, auth: true, sent: encode(['123456']),
    reply: () => cborReply(200, [CODES]), expected: CODES,
  },
  {
    name: 'totpConfirm returns no codes when the account already held some', call: (r) => r.totpConfirm('123456'),
    method: 'POST', path: '/v1/auth/totp/confirm', bucket: 'write', idempotent: false, auth: true, sent: encode(['123456']),
    reply: () => cborReply(200, [[]]), expected: [],
  },
  {
    name: 'passkeyRegisterBegin', call: (r) => r.passkeyRegisterBegin(null),
    method: 'POST', path: '/v1/auth/passkey/register/begin', bucket: 'write', idempotent: false, auth: true, sent: encode([null]),
    reply: () => cborReply(200, [CEREMONY, OPTIONS]), expected: { ceremonyId: CEREMONY, options: OPTIONS },
  },
  {
    name: 'passkeyRegisterFinish', call: (r) => r.passkeyRegisterFinish(CEREMONY, RESPONSE, 'Laptop'),
    method: 'POST', path: '/v1/auth/passkey/register/finish', bucket: 'write', idempotent: false, auth: true, sent: encode([CEREMONY, RESPONSE, 'Laptop']),
    reply: () => cborReply(200, [CRED, CODES]), expected: { credId: CRED, backupCodes: CODES },
  },
  {
    name: 'passkeyLoginBegin', call: (r) => r.passkeyLoginBegin(),
    method: 'POST', path: '/v1/auth/passkey/login/begin', bucket: 'none', idempotent: false, auth: false, sent: encode([]),
    reply: () => cborReply(200, [CEREMONY, OPTIONS]), expected: { ceremonyId: CEREMONY, options: OPTIONS },
  },
  {
    name: 'passkeyLoginFinish with user verification', call: (r) => r.passkeyLoginFinish(CEREMONY, RESPONSE),
    method: 'POST', path: '/v1/auth/passkey/login/finish', bucket: 'none', idempotent: false, auth: false, sent: encode([CEREMONY, RESPONSE]),
    reply: () => cborReply(200, [ASSERTION, 0]), expected: { assertion: ASSERTION, needs: 0 },
  },
  {
    name: 'passkeyLoginFinish without user verification', call: (r) => r.passkeyLoginFinish(CEREMONY, RESPONSE),
    method: 'POST', path: '/v1/auth/passkey/login/finish', bucket: 'none', idempotent: false, auth: false, sent: encode([CEREMONY, RESPONSE]),
    reply: () => cborReply(200, [ASSERTION, 1]), expected: { assertion: ASSERTION, needs: 1 },
  },
  {
    name: 'stepUp with the password', call: (r) => r.stepUp(0, 'hunter2 hunter2'),
    method: 'POST', path: '/v1/auth/stepup', bucket: 'write', idempotent: false, auth: true, sent: encode([0, 'hunter2 hunter2']),
    reply: () => cborReply(201, ['su-1', 1_800_000_300]), expected: { token: 'su-1', expires: 1_800_000_300n },
  },
  {
    name: 'stepUp with a code', call: (r) => r.stepUp(1, '123456'),
    method: 'POST', path: '/v1/auth/stepup', bucket: 'write', idempotent: false, auth: true, sent: encode([1, '123456']),
    reply: () => cborReply(201, ['su-2', 1_800_000_300]), expected: { token: 'su-2', expires: 1_800_000_300n },
  },
  {
    name: 'stepUp with a backup code', call: (r) => r.stepUp(2, 'AAAAAAAAAAAAAAAAAAAAAAAAAA'),
    method: 'POST', path: '/v1/auth/stepup', bucket: 'write', idempotent: false, auth: true, sent: encode([2, 'AAAAAAAAAAAAAAAAAAAAAAAAAA']),
    reply: () => cborReply(201, ['su-3', 1_800_000_300]), expected: { token: 'su-3', expires: 1_800_000_300n },
  },
  {
    name: 'stepUpPasskeyBegin', call: (r) => r.stepUpPasskeyBegin(),
    method: 'POST', path: '/v1/auth/stepup/passkey/begin', bucket: 'write', idempotent: false, auth: true, sent: encode([]),
    reply: () => cborReply(200, [CEREMONY, OPTIONS]), expected: { ceremonyId: CEREMONY, options: OPTIONS },
  },
  {
    name: 'stepUpPasskeyFinish', call: (r) => r.stepUpPasskeyFinish(CEREMONY, RESPONSE),
    method: 'POST', path: '/v1/auth/stepup/passkey/finish', bucket: 'write', idempotent: false, auth: true, sent: encode([CEREMONY, RESPONSE]),
    reply: () => cborReply(201, ['su-4', 1_800_000_300]), expected: { token: 'su-4', expires: 1_800_000_300n },
  },
  {
    name: 'removeFactor of the app', call: (r) => r.removeFactor('su-1', 0, null),
    method: 'POST', path: '/v1/auth/factors/remove', bucket: 'write', idempotent: false, auth: true, sent: encode(['su-1', 0, null]),
    reply: () => noContent(), expected: undefined,
  },
  {
    name: 'removeFactor of a passkey', call: (r) => r.removeFactor('su-1', 1, CRED),
    method: 'POST', path: '/v1/auth/factors/remove', bucket: 'write', idempotent: false, auth: true, sent: encode(['su-1', 1, CRED]),
    reply: () => noContent(), expected: undefined,
  },
  {
    name: 'changePassword with the old one', call: (r) => r.changePassword('old pw', 'new pw', null),
    method: 'POST', path: '/v1/auth/password', bucket: 'write', idempotent: false, auth: true, sent: encode(['old pw', 'new pw', null]),
    reply: () => noContent(), expected: undefined,
  },
  {
    name: 'changePassword, a first password with a step-up', call: (r) => r.changePassword(null, 'new pw', 'su-1'),
    method: 'POST', path: '/v1/auth/password', bucket: 'write', idempotent: false, auth: true, sent: encode([null, 'new pw', 'su-1']),
    reply: () => noContent(), expected: undefined,
  },
  {
    name: 'regenerateBackupCodes', call: (r) => r.regenerateBackupCodes('su-1'),
    method: 'POST', path: '/v1/auth/recovery/codes', bucket: 'write', idempotent: false, auth: true, sent: encode(['su-1']),
    reply: () => cborReply(200, [CODES]), expected: CODES,
  },
];

describe('Routes: one request per call, decoded as the server answers it', () => {
  for (const c of [...CASES, ...FACTOR_CASES]) {
    it(c.name, async () => {
      const { t, http, routes } = setup();
      t.replies.push(c.reply());
      if (c.afterReply) t.replies.push(c.afterReply());
      expect(await c.call(routes)).toEqual(c.expected);
      expect(t.seen).toHaveLength(c.afterReply ? 2 : 1);
      const s = t.seen[0];
      expect(s.method).toBe(c.method);
      expect(s.url).toBe(`https://dilla.test${c.path}`);
      expect(s.headers.get('authorization')).toBe(c.auth ? 'Bearer tok-1' : null);
      expect(s.headers.get('accept')).toBe(c.accept ?? null);
      expect(s.body).toEqual(c.sent);
      expect(http.requests[0].bucket).toBe(c.bucket);
      expect(http.requests[0].idempotent).toBe(c.idempotent);
      expect(http.requests[0].auth ?? true).toBe(c.auth);
    });
  }
});

describe('GET /v1/instance before sign-in', () => {
  it('sends no Authorization header while there is no session', async () => {
    const { t, routes } = setup({ token: null });
    t.replies.push(cborReply(200, INSTANCE_DOC));
    expect((await routes.getInstance()).name).toBe('dilla.test');
    expect(t.seen[0].headers.get('authorization')).toBeNull();
  });
});

describe('signed-list response bounds', () => {
  it('rejects an oversized head before CBOR decoding', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(200, [1, new Uint8Array(1024 * 1024 + 1)]));
    await expect(routes.getDeviceList(USER)).rejects.toMatchObject({ code: 'E_BODY_TOO_LARGE' });
  });
  it('rejects an oversized history page before CBOR decoding', async () => {
    const { t, routes } = setup();
    t.replies.push(bytesReply(200, encode([[1, new Uint8Array(4 * 1024 * 1024 + 1), SIG, HASH]])));
    await expect(routes.getDeviceListHistory(USER, 0n)).rejects.toMatchObject({ code: 'E_BODY_TOO_LARGE' });
  });
});

interface RefusalCase { name: string; call(r: Routes): Promise<unknown>; reply(): Response; status: number; code: string; }

const REFUSALS: RefusalCase[] = [
  { name: 'getInstance 501', call: (r) => r.getInstance(), reply: () => refusal(501, 'E_INTERNAL'), status: 501, code: 'E_INTERNAL' },
  { name: 'getInvite 410', call: (r) => r.getInvite('ABCD'), reply: () => refusal(410, 'E_INVITE_INVALID', 'the invite is spent, expired, revoked or unknown'), status: 410, code: 'E_INVITE_INVALID' },
  { name: 'postAccount 409 username taken', call: (r) => r.postAccount(BODY), reply: () => refusal(409, 'E_INVALID_REQUEST', 'username taken'), status: 409, code: 'E_INVALID_REQUEST' },
  { name: 'postChallenge 400', call: (r) => r.postChallenge(DEVICE), reply: () => refusal(400, 'E_INVALID_REQUEST'), status: 400, code: 'E_INVALID_REQUEST' },
  { name: 'postSession 401', call: (r) => r.postSession(DEVICE, BODY), reply: () => refusal(401, 'E_UNAUTHENTICATED'), status: 401, code: 'E_UNAUTHENTICATED' },
  { name: 'putDeviceList 409', call: (r) => r.putDeviceList(USER, BODY), reply: () => refusal(409, 'E_INVALID_REQUEST', 'device-list version 1 already published'), status: 409, code: 'E_INVALID_REQUEST' },
  { name: 'getDeviceList 403', call: (r) => r.getDeviceList(USER), reply: () => refusal(403, 'E_FORBIDDEN'), status: 403, code: 'E_FORBIDDEN' },
  { name: 'postKeyPackages 413', call: (r) => r.postKeyPackages(BODY), reply: () => refusal(413, 'E_TOO_LARGE'), status: 413, code: 'E_TOO_LARGE' },
  { name: 'postTicket 401', call: (r) => r.postTicket(), reply: () => refusal(401, 'E_UNAUTHENTICATED'), status: 401, code: 'E_UNAUTHENTICATED' },
  { name: 'listCommunities 403', call: (r) => r.listCommunities(), reply: () => refusal(403, 'E_FORBIDDEN'), status: 403, code: 'E_FORBIDDEN' },
  { name: 'joinCommunity 410', call: (r) => r.joinCommunity(COMMUNITY, 'AB12'), reply: () => refusal(410, 'E_INVITE_INVALID'), status: 410, code: 'E_INVITE_INVALID' },
  { name: 'createCommunity 400', call: (r) => r.createCommunity('x'), reply: () => refusal(400, 'E_INVALID_REQUEST', 'name must be 1..255 bytes'), status: 400, code: 'E_INVALID_REQUEST' },
  { name: 'createChannel 403', call: (r) => r.createChannel(COMMUNITY, { kind: 'text', name: 'x', position: 0 }), reply: () => refusal(403, 'E_FORBIDDEN', 'manage channels'), status: 403, code: 'E_FORBIDDEN' },
  { name: 'createInvite 403', call: (r) => r.createInvite(COMMUNITY, { maxUses: 10, ttlSeconds: 60 }), reply: () => refusal(403, 'E_FORBIDDEN'), status: 403, code: 'E_FORBIDDEN' },
  { name: 'listChannels 404', call: (r) => r.listChannels(COMMUNITY), reply: () => refusal(404, 'E_NOT_FOUND', 'no such object'), status: 404, code: 'E_NOT_FOUND' },
  { name: 'listMembers 404', call: (r) => r.listMembers(COMMUNITY), reply: () => refusal(404, 'E_NOT_FOUND'), status: 404, code: 'E_NOT_FOUND' },
  { name: 'postGroup 409', call: (r) => r.postGroup(BODY), reply: () => refusal(409, 'E_GROUP_EXISTS'), status: 409, code: 'E_GROUP_EXISTS' },
  { name: 'getGroupInfo 404', call: (r) => r.getGroupInfo(GROUP), reply: () => refusal(404, 'E_NOT_FOUND'), status: 404, code: 'E_NOT_FOUND' },
  { name: 'getGroupTree 404', call: (r) => r.getGroupTree(GROUP), reply: () => refusal(404, 'E_NOT_FOUND'), status: 404, code: 'E_NOT_FOUND' },
  { name: 'getHandshakes 410', call: (r) => r.getHandshakes(GROUP, 1n, 512), reply: () => refusal(410, 'E_PRUNED'), status: 410, code: 'E_PRUNED' },
  { name: 'getMessages 410', call: (r) => r.getMessages(GROUP, 1n, 256), reply: () => refusal(410, 'E_PRUNED'), status: 410, code: 'E_PRUNED' },
  { name: 'getProposals 404', call: (r) => r.getProposals(GROUP), reply: () => refusal(404, 'E_NOT_FOUND'), status: 404, code: 'E_NOT_FOUND' },
  { name: 'postCommit 409', call: (r) => r.postCommit(GROUP, BODY), reply: () => refusal(409, 'E_COMMIT_CONFLICT', 'another commit won this epoch', null, [WIN, [REF]]), status: 409, code: 'E_COMMIT_CONFLICT' },
  { name: 'postMessage 425', call: (r) => r.postMessage(GROUP, BODY), reply: () => refusal(425, 'E_COMMIT_REQUIRED', 'outstanding proposals must be committed first', 1500, [[REF]]), status: 425, code: 'E_COMMIT_REQUIRED' },
  { name: 'postCursor 400', call: (r) => r.postCursor(GROUP, BODY), reply: () => refusal(400, 'E_INVALID_REQUEST'), status: 400, code: 'E_INVALID_REQUEST' },
  { name: 'postResync 403', call: (r) => r.postResync(GROUP, BODY), reply: () => refusal(403, 'E_FORBIDDEN'), status: 403, code: 'E_FORBIDDEN' },
  { name: 'getWelcomes 401', call: (r) => r.getWelcomes(0n), reply: () => refusal(401, 'E_UNAUTHENTICATED'), status: 401, code: 'E_UNAUTHENTICATED' },
  { name: 'deleteWelcome 403', call: (r) => r.deleteWelcome(5n), reply: () => refusal(403, 'E_FORBIDDEN'), status: 403, code: 'E_FORBIDDEN' },
  { name: 'getAccountMe 401', call: (r) => r.getAccountMe(), reply: () => refusal(401, 'E_UNAUTHENTICATED'), status: 401, code: 'E_UNAUTHENTICATED' },
  { name: 'getLimits 501', call: (r) => r.getLimits(), reply: () => refusal(501, 'E_INTERNAL'), status: 501, code: 'E_INTERNAL' },
  { name: 'passwordLogin 401', call: (r) => r.passwordLogin('ada', 'x'), reply: () => refusal(401, 'E_UNAUTHENTICATED'), status: 401, code: 'E_UNAUTHENTICATED' },
  { name: 'totpVerify 401', call: (r) => r.totpVerify(ASSERTION, '000000'), reply: () => refusal(401, 'E_UNAUTHENTICATED'), status: 401, code: 'E_UNAUTHENTICATED' },
  { name: 'stepUp 403 E_STEP_UP', call: (r) => r.stepUp(0, 'x'), reply: () => refusal(403, 'E_STEP_UP'), status: 403, code: 'E_STEP_UP' },
  { name: 'totpEnroll 403 E_STEP_UP', call: (r) => r.totpEnroll(null), reply: () => refusal(403, 'E_STEP_UP'), status: 403, code: 'E_STEP_UP' },
  { name: 'removeFactor 403 E_STEP_UP', call: (r) => r.removeFactor('su', 0, null), reply: () => refusal(403, 'E_STEP_UP'), status: 403, code: 'E_STEP_UP' },
  { name: 'changePassword 403 wrong password', call: (r) => r.changePassword('x', 'y', null), reply: () => refusal(403, 'E_FORBIDDEN'), status: 403, code: 'E_FORBIDDEN' },
  { name: 'changePassword 429 throttled', call: (r) => r.changePassword('x', 'y', null), reply: () => refusal(429, 'E_RATE_LIMITED', '', 60_000), status: 429, code: 'E_RATE_LIMITED' },
  { name: 'removeFactor 409 last factor', call: (r) => r.removeFactor('su', 0, null), reply: () => refusal(409, 'E_INVALID_REQUEST'), status: 409, code: 'E_INVALID_REQUEST' },
  { name: 'getFactors 403 E_FACTOR_REQUIRED', call: (r) => r.getFactors(), reply: () => refusal(403, 'E_FACTOR_REQUIRED'), status: 403, code: 'E_FACTOR_REQUIRED' },
  { name: 'listDevices 403 E_FACTOR_REQUIRED', call: (r) => r.listDevices(), reply: () => refusal(403, 'E_FACTOR_REQUIRED'), status: 403, code: 'E_FACTOR_REQUIRED' },
  { name: 'grantRole 403 E_TARGET_NO_FACTOR', call: (r) => r.grantRole(COMMUNITY, USER, ROLE), reply: () => refusal(403, 'E_TARGET_NO_FACTOR'), status: 403, code: 'E_TARGET_NO_FACTOR' },
  { name: 'passkeyLoginFinish 401 E_PASSKEY_UNKNOWN', call: (r) => r.passkeyLoginFinish(CEREMONY, RESPONSE), reply: () => refusal(401, 'E_PASSKEY_UNKNOWN'), status: 401, code: 'E_PASSKEY_UNKNOWN' },
  { name: 'passkeyLoginFinish 401 E_UNAUTHENTICATED', call: (r) => r.passkeyLoginFinish(CEREMONY, RESPONSE), reply: () => refusal(401, 'E_UNAUTHENTICATED'), status: 401, code: 'E_UNAUTHENTICATED' },
  { name: 'recoveryVerify 401', call: (r) => r.recoveryVerify(ASSERTION, 'x'), reply: () => refusal(401, 'E_UNAUTHENTICATED'), status: 401, code: 'E_UNAUTHENTICATED' },
  { name: 'stepUp 429 beyond the retry window', call: (r) => r.stepUp(0, 'x'), reply: () => refusal(429, 'E_RATE_LIMITED', '', 1_200_000), status: 429, code: 'E_RATE_LIMITED' },
  { name: 'regenerateBackupCodes 403 E_STEP_UP', call: (r) => r.regenerateBackupCodes('su'), reply: () => refusal(403, 'E_STEP_UP'), status: 403, code: 'E_STEP_UP' },
  { name: 'postSessionPending 429 beyond the retry window', call: (r) => r.postSessionPending(DEVICE, BODY), reply: () => refusal(429, 'E_RATE_LIMITED', '', 1_200_000), status: 429, code: 'E_RATE_LIMITED' },
  { name: 'postSessionPending 403 device cap', call: (r) => r.postSessionPending(DEVICE, BODY), reply: () => refusal(403, 'E_FORBIDDEN', 'device cap reached'), status: 403, code: 'E_FORBIDDEN' },
  { name: 'listBackups 403', call: (r) => r.listBackups(), reply: () => refusal(403, 'E_FORBIDDEN'), status: 403, code: 'E_FORBIDDEN' },
  { name: 'getBackup 403', call: (r) => r.getBackup(0), reply: () => refusal(403, 'E_FORBIDDEN'), status: 403, code: 'E_FORBIDDEN' },
  { name: 'putBackup 409 root stored', call: (r) => r.putBackup(0, OBJECT), reply: () => refusal(409, 'E_INVALID_REQUEST', 'root object already stored'), status: 409, code: 'E_INVALID_REQUEST' },
  { name: 'listDevices 403', call: (r) => r.listDevices(), reply: () => refusal(403, 'E_FORBIDDEN'), status: 403, code: 'E_FORBIDDEN' },
  { name: 'deleteSessions 404', call: (r) => r.deleteSessions(DEVICE), reply: () => refusal(404, 'E_NOT_FOUND', 'not found'), status: 404, code: 'E_NOT_FOUND' },
  { name: 'getDeviceListHistory 400', call: (r) => r.getDeviceListHistory(USER, 0n), reply: () => refusal(400, 'E_INVALID_REQUEST'), status: 400, code: 'E_INVALID_REQUEST' },
  { name: 'postDm 404', call: (r) => r.postDm([PEER_USER]), reply: () => refusal(404, 'E_NOT_FOUND', 'no such user'), status: 404, code: 'E_NOT_FOUND' },
  { name: 'listDms 403', call: (r) => r.listDms(), reply: () => refusal(403, 'E_FORBIDDEN'), status: 403, code: 'E_FORBIDDEN' },
  { name: 'getChannel 404', call: (r) => r.getChannel(CHANNEL), reply: () => refusal(404, 'E_NOT_FOUND'), status: 404, code: 'E_NOT_FOUND' },
  { name: 'deleteDevice 404', call: (r) => r.deleteDevice(DEVICE_2), reply: () => refusal(404, 'E_NOT_FOUND', 'not found'), status: 404, code: 'E_NOT_FOUND' },
{ name: 'deleteGroupMessage 403 another user\'s upload', call: (r) => r.deleteGroupMessage(GROUP, 12n), reply: () => refusal(403, 'E_NOT_UPLOADER', 'only the uploading user may delete this message'), status: 403, code: 'E_NOT_UPLOADER' },
{ name: 'putBlob 413', call: (r) => r.putBlob(CHANNEL, BLOB_ID, STORED), reply: () => refusal(413, 'E_TOO_LARGE', 'at most 104857600 bytes'), status: 413, code: 'E_TOO_LARGE' },
{ name: 'putBlob 422 hash mismatch', call: (r) => r.putBlob(CHANNEL, BLOB_ID, STORED), reply: () => refusal(422, 'E_INVALID_REQUEST', 'the body does not hash to the requested blob_id'), status: 422, code: 'E_INVALID_REQUEST' },
{ name: 'putBlob 507 quota', call: (r) => r.putBlob(CHANNEL, BLOB_ID, STORED), reply: () => refusal(507, 'E_STORAGE_FULL', 'your attachment quota is exhausted'), status: 507, code: 'E_STORAGE_FULL' },
{ name: 'putBlob 410 purged', call: (r) => r.putBlob(CHANNEL, BLOB_ID, STORED), reply: () => refusal(410, 'E_PRUNED', 'these bytes were removed by the server operator'), status: 410, code: 'E_PRUNED' },
{ name: 'confirmBlob 403', call: (r) => r.confirmBlob(CHANNEL, BLOB_ID), reply: () => refusal(403, 'E_NOT_UPLOADER'), status: 403, code: 'E_NOT_UPLOADER' },
{ name: 'confirmBlob 404', call: (r) => r.confirmBlob(CHANNEL, BLOB_ID), reply: () => refusal(404, 'E_NOT_FOUND', 'no such object'), status: 404, code: 'E_NOT_FOUND' },
{ name: 'confirmBlob 410', call: (r) => r.confirmBlob(CHANNEL, BLOB_ID), reply: () => refusal(410, 'E_PRUNED'), status: 410, code: 'E_PRUNED' },
{ name: 'getBlob 410', call: (r) => r.getBlob(CHANNEL, BLOB_ID), reply: () => refusal(410, 'E_PRUNED'), status: 410, code: 'E_PRUNED' },
{ name: 'deleteBlob 403', call: (r) => r.deleteBlob(CHANNEL, BLOB_ID), reply: () => refusal(403, 'E_NOT_UPLOADER', 'only the uploading user may delete this object'), status: 403, code: 'E_NOT_UPLOADER' },
];

describe('Routes: a refusal propagates unchanged and is sent once', () => {
  for (const c of REFUSALS) {
    it(c.name, async () => {
      const { t, routes } = setup();
      t.replies.push(c.reply());
      const outcome = await c.call(routes).then(() => 'resolved', (e: unknown) => e);
      expect(outcome).toBeInstanceOf(DillaHttpError);
      expect(outcome).toMatchObject({ status: c.status, code: c.code });
      expect(t.seen).toHaveLength(1);
      expect(t.sleeps).toEqual([]);
    });
  }

  it('keeps the extras of a commit conflict', async () => {
    const { t, routes } = setup();
    t.replies.push(refusal(409, 'E_COMMIT_CONFLICT', 'another commit won this epoch', null, [WIN, [REF]]));
    const outcome = await routes.postCommit(GROUP, BODY).then(() => 'resolved', (e: unknown) => e);
    expect(outcome).toMatchObject({ code: 'E_COMMIT_CONFLICT', extra: [WIN, [REF]] });
  });

  it('postSession 401 never re-authenticates', async () => {
    const { t, routes } = setup({ reauth: () => ({ ok: true, token: 'tok-2' }) });
    t.replies.push(refusal(401, 'E_UNAUTHENTICATED'));
    await routes.postSession(DEVICE, BODY).then(() => 'resolved', (e: unknown) => e);
    expect(t.reauthCalls).toBe(0);
  });
});

describe('deleteGroupMessage membership check', () => {
  it('accepts E_PRUNED from the member-only read as proof of membership', async () => {
    const { t, routes } = setup();
    t.replies.push(refusal(404, 'E_NOT_FOUND'), refusal(410, 'E_PRUNED'));
    await expect(routes.deleteGroupMessage(GROUP, 12n)).resolves.toBe('gone');
    expect(t.seen).toHaveLength(2);
  });
  it('retains a 404 when the current member-only message read also returns 404', async () => {
    const { t, routes } = setup();
    t.replies.push(refusal(404, 'E_NOT_FOUND'), refusal(404, 'E_NOT_FOUND'));
    await expect(routes.deleteGroupMessage(GROUP, 12n)).rejects.toMatchObject({ status: 404, code: 'E_NOT_FOUND' });
    expect(t.seen.map((s) => [s.method, s.url])).toEqual([
      ['DELETE', `https://dilla.test/v1/groups/${hex(GROUP)}/messages/12`],
      ['GET', `https://dilla.test/v1/groups/${hex(GROUP)}/messages?from=0&limit=1`],
    ]);
  });
});

describe('Routes: retries and inputs', () => {
  it('sends one request for a short 429 on login and session routes', async () => {
    for (const call of [
      (routes: Routes) => routes.changePassword('old', 'new', null),
      (routes: Routes) => routes.passwordLogin('ada', 'pw'),
      (routes: Routes) => routes.postSession(DEVICE, BODY),
      (routes: Routes) => routes.postSessionPending(DEVICE, BODY),
    ]) {
      const { t, routes } = setup();
      t.replies.push(refusal(429, 'E_RATE_LIMITED', '', 1000));
      await expect(call(routes)).rejects.toMatchObject({ status: 429, retryAfterMs: 1000 });
      expect(t.seen).toHaveLength(1);
      expect(t.sleeps).toEqual([]);
    }
  });
  it('refuses bad web-2a inputs before sending', async () => {
    const { t, routes } = setup();
    const attempts: (() => Promise<unknown>)[] = [
      () => routes.getBackup(2 as 0),
      () => routes.putBackup(3 as 1, OBJECT),
      () => routes.deleteSessions(new Uint8Array(15)),
      () => routes.getDeviceListHistory(USER, -1n),
      () => routes.getDeviceListHistory(new Uint8Array(17), 0n),
      () => routes.postDm([]),
      () => routes.postDm([new Uint8Array(15)]),
      () => routes.postDm(Array.from({ length: 65 }, () => PEER_USER)),
      () => routes.getChannel(new Uint8Array(17)),
      () => routes.deleteDevice(new Uint8Array(15)),
    ];
    for (const [i, attempt] of attempts.entries()) {
      await expect(attempt(), `attempt ${i}`).rejects.toThrow(/^E_ROUTE_INPUT: /);
    }
    expect(t.seen).toHaveLength(0);
  });
  it('refuses a create the server would refuse, before sending (its own bounds and its control-character rule)', async () => {
    const { t, routes } = setup();
    const attempts: (() => Promise<unknown>)[] = [
      () => routes.createCommunity(''),
      () => routes.createCommunity('x'.repeat(256)),
      () => routes.createCommunity('å'.repeat(128)),            // 256 UTF-8 bytes in 128 characters
      () => routes.createCommunity('tab\there'),
      () => routes.createCommunity('del\u007f'),
      () => routes.createCommunity('c1\u0085'),
      () => routes.createChannel(COMMUNITY, { kind: 'text', name: '', position: 0 }),
      () => routes.createChannel(COMMUNITY, { kind: 'text', name: 'x'.repeat(101), position: 0 }),
      () => routes.createChannel(COMMUNITY, { kind: 'text', name: 'new\nline', position: 0 }),
      () => routes.createChannel(COMMUNITY, { kind: 'category' as 'text', name: 'x', position: 0 }),
      () => routes.createChannel(COMMUNITY, { kind: 'text', name: 'x', position: -1 }),
      () => routes.createChannel(COMMUNITY, { kind: 'text', name: 'x', position: 2 ** 31 }),
      () => routes.createChannel(new Uint8Array(15), { kind: 'text', name: 'x', position: 0 }),
      // 0 uses and 0 ttl are "∞" and "never" (Task 10c); negative ones are not.
      () => routes.createInvite(COMMUNITY, { maxUses: -1, ttlSeconds: 60 }),
      () => routes.createInvite(COMMUNITY, { maxUses: 1001, ttlSeconds: 60 }),
      () => routes.createInvite(COMMUNITY, { maxUses: 1.5, ttlSeconds: 60 }),
      () => routes.createInvite(COMMUNITY, { maxUses: 10, ttlSeconds: -1 }),
      () => routes.createInvite(COMMUNITY, { maxUses: 10, ttlSeconds: 30 * 24 * 3600 + 1 }),
      () => routes.createInvite(new Uint8Array(17), { maxUses: 10, ttlSeconds: 60 }),
    ];
    for (const [i, attempt] of attempts.entries()) {
      await expect(attempt(), `attempt ${i}`).rejects.toThrow(/^E_ROUTE_INPUT: /);
    }
    expect(t.seen).toHaveLength(0);
  });
  it('a server name of exactly 255 bytes of emoji is sent, and 256 bytes is refused before any request', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(201, [COMMUNITY, ROLE, 1]));
    const edge = '😀'.repeat(63) + 'abc';                         // 63×4 + 3 = 255 UTF-8 bytes
    expect(await routes.createCommunity(edge)).toEqual({ id: COMMUNITY, roleEveryone: ROLE, policyVersion: 1n });
    expect(t.seen[0].body).toEqual(encode([edge, new Uint8Array(0), 0, 0]));
    await expect(routes.createCommunity('😀'.repeat(64))).rejects.toThrow(/^E_ROUTE_INPUT: name must be 1\.\.255 bytes$/);
    expect(t.seen).toHaveLength(1);
  });
  it('a channel name of exactly 100 bytes is sent, 101 bytes or a bell character is refused before any request', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(201, [CHANNEL, 0, 0]));
    const edge = 'å'.repeat(50);                                  // 50×2 = 100 UTF-8 bytes
    await routes.createChannel(COMMUNITY, { kind: 'text', name: edge, position: 0 });
    expect(t.seen[0].body).toEqual(encode([0, 0, 0, null, edge, '', 0, 0]));
    await expect(routes.createChannel(COMMUNITY, { kind: 'text', name: edge + 'x', position: 0 }))
      .rejects.toThrow(/^E_ROUTE_INPUT: name must be 1\.\.100 bytes$/);
    await expect(routes.createChannel(COMMUNITY, { kind: 'text', name: 'a\u0007b', position: 0 }))
      .rejects.toThrow(/^E_ROUTE_INPUT: name contains a control character$/);
    expect(t.seen).toHaveLength(1);
  });
  it('postMessage is sent once after a network failure', async () => {
    const { t, routes } = setup();
    t.replies.push(new TypeError('fetch failed'), bytesReply(200, UPLOADED));
    const outcome = await routes.postMessage(GROUP, BODY).then(() => 'sent', (e: unknown) => e);
    expect(t.seen).toHaveLength(1);
    expect(outcome).toBeInstanceOf(DillaHttpError);
    expect(outcome).toMatchObject({ status: 0, code: 'E_NETWORK' });
  });

  it('a 429 on postMessage is not waited out: the send lane requeues it (CB2)', async () => {
    const { t, routes } = setup();
    t.replies.push(refusal(429, 'E_RATE_LIMITED', 'slowmode', 5000), bytesReply(200, UPLOADED));
    const outcome = await routes.postMessage(GROUP, BODY).then(() => 'sent', (e: unknown) => e);
    expect(outcome).toBeInstanceOf(DillaHttpError);
    expect(outcome).toMatchObject({ status: 429, code: 'E_RATE_LIMITED', retryAfterMs: 5000 });
    expect(t.seen).toHaveLength(1);
    expect(t.sleeps).toEqual([]);
  });

  it('postAccount is sent once after a 500', async () => {
    const { t, routes } = setup();
    t.replies.push(refusal(500, 'E_INTERNAL'), cborReply(200, [USER, DEVICE, 'tok-new', 1]));
    const outcome = await routes.postAccount(BODY).then(() => 'sent', (e: unknown) => e);
    expect(t.seen).toHaveLength(1);
    expect(outcome).toMatchObject({ status: 500, code: 'E_INTERNAL' });
  });

  it('a read is retried after a network failure', async () => {
    const { t, routes } = setup();
    t.replies.push(new TypeError('fetch failed'), bytesReply(200, MESSAGES));
    expect(await routes.getMessages(GROUP, 3n, 256)).toEqual({ raw: MESSAGES, count: 1, lastSeq: 3n });
    expect(t.seen).toHaveLength(2);
    expect(t.sleeps).toEqual([625]);
  });

  it('decodes a contiguous moderation removal page and refuses gaps or cursor mismatch', async () => {
    const { t, routes, http } = setup();
    const proof = encode([1, fill(16, 1), fill(16, 2), fill(16, 3), fill(16, 4), GROUP, 8, 20]);
    t.replies.push(cborReply(200, [[[5, proof, SIG, 19]], 5]));
    await expect(routes.getModerationRemovals(GROUP, 4n)).resolves.toEqual({
      rows: [{ revision: 5n, proof, signature: SIG, acceptedAt: 19n }], nextRevision: 5n,
    });
    expect(http.requests[0]).toMatchObject({ path: `/v1/groups/${hex(GROUP)}/moderation/removals?after=4&limit=${MODERATION_REMOVALS_PAGE}` });
    t.replies.push(cborReply(200, [[[7, proof, SIG, 19]], 7]));
    await expect(routes.getModerationRemovals(GROUP, 5n)).rejects.toThrow(/revision gap/);
    t.replies.push(cborReply(200, [[], 9]));
    await expect(routes.getModerationRemovals(GROUP, 5n)).rejects.toThrow(/cursor mismatch/);
  });

  it('refuses bad inputs before sending', async () => {
    const { t, routes } = setup();
    // Thunks, not promises: each call starts only when its expectation is attached, so no
    // rejection is ever unhandled between two awaits.
    const attempts: (() => Promise<unknown>)[] = [
      () => routes.getHandshakes(GROUP, 1n, HANDSHAKES_MAX + 1),
      () => routes.getHandshakes(GROUP, 1n, 0),
      () => routes.getHandshakes(GROUP, -1n, 1),
      () => routes.getMessages(GROUP, 1n, MESSAGES_MAX + 1),
      () => routes.getWelcomes(-1n),
      () => routes.deleteWelcome(-1n),
      () => routes.listChannels(new Uint8Array(15)),
      () => routes.postCommit(new Uint8Array(17), BODY),
      () => routes.postChallenge(new Uint8Array(0)),
    ];
    for (const [i, attempt] of attempts.entries()) {
      await expect(attempt(), `attempt ${i}`).rejects.toThrow(/^E_ROUTE_INPUT: /);
    }
    expect(t.seen).toHaveLength(0);
  });

  it('exports the server page sizes', () => {
    expect([MEMBERS_PAGE, HANDSHAKES_MAX, MESSAGES_MAX, WELCOMES_PAGE]).toEqual([200, 512, 256, 64]);
  });
});

describe('Routes: response shapes', () => {
  it('refuses an answer with an element missing', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(200, INSTANCE_DOC.slice(0, 10)));
    await expect(routes.getInstance()).rejects.toMatchObject({ name: 'CborError', code: 'E_CBOR_SHAPE' });
  });

  it('refuses a registration mode outside 0 to 2 and a member kind outside 0 to 1', async () => {
    const { t, routes } = setup();
    const doc = [...INSTANCE_DOC];
    doc[6] = 3;
    t.replies.push(cborReply(200, doc), cborReply(200, [[USER, 1, '', [], 'jonas', 'Jonas', 2]]));
    await expect(routes.getInstance()).rejects.toMatchObject({ code: 'E_CBOR_RANGE' });
    await expect(routes.listMembers(COMMUNITY)).rejects.toMatchObject({ code: 'E_CBOR_RANGE' });
  });

  it('refuses an id of the wrong length in an answer', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(200, [[fill(15, 1), 'loot', USER, 1]]));
    await expect(routes.listCommunities()).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
  });

  it('refuses a create answer that is short, and an invite link that is not http(s)', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(201, [COMMUNITY, ROLE]), cborReply(201, [CHANNEL, 0]),
      cborReply(201, [KEY_ID, 'AB12', 'https://dilla.test/i/AB12']),
      cborReply(201, [KEY_ID, 'AB12', 'javascript:alert(1)', 1_800_000_000]),
      cborReply(201, [KEY_ID, 'AB12', 'not a link', 1_800_000_000]));
    await expect(routes.createCommunity('x')).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
    await expect(routes.createChannel(COMMUNITY, { kind: 'text', name: 'x', position: 0 })).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
    await expect(routes.createInvite(COMMUNITY, { maxUses: 1, ttlSeconds: 1 })).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
    await expect(routes.createInvite(COMMUNITY, { maxUses: 1, ttlSeconds: 1 })).rejects.toMatchObject({ code: 'E_CBOR_RANGE' });
    await expect(routes.createInvite(COMMUNITY, { maxUses: 1, ttlSeconds: 1 })).rejects.toMatchObject({ code: 'E_CBOR_RANGE' });
  });

  // WB m6: the link is the instance's invite landing for the answered code, nothing else: path /i/<code>, no query, no
  // fragment. Attacker: an instance (or anything between) handing out a link that looks like an invite and leads elsewhere.
  it('refuses an invite link whose path is not /i/<the answered code>', async () => {
    const { t, routes } = setup();
    const bad = ['https://dilla.test/i/ZZ99', 'https://dilla.test/x/AB12', 'https://dilla.test/i/AB12/more',
      'https://dilla.test/', 'https://dilla.test/i/AB12?next=https://evil.test', 'https://dilla.test/i/AB12#x',
      'https://dilla.test/welcome?invite=AB12'];
    for (const url of bad) t.replies.push(cborReply(201, [KEY_ID, 'AB12', url, 1_800_000_000]));
    for (const url of bad) {
      await expect(routes.createInvite(COMMUNITY, { maxUses: 1, ttlSeconds: 1 }), url).rejects.toMatchObject({ code: 'E_CBOR_RANGE' });
    }
    t.replies.push(cborReply(201, [KEY_ID, 'AB12', 'http://dilla.test:8443/i/AB12', 1_800_000_000]));
    await expect(routes.createInvite(COMMUNITY, { maxUses: 1, ttlSeconds: 1 })).resolves.toMatchObject({ url: 'http://dilla.test:8443/i/AB12' });
  });

  it('keeps auth methods it does not know', async () => {
    const { t, routes } = setup();
    const doc = [...INSTANCE_DOC];
    doc[7] = [0, 9];
    t.replies.push(cborReply(200, doc));
    expect((await routes.getInstance()).authMethods).toEqual([0, 9]);
  });

  it('reads a text/plain 404 of the mux as E_HTTP', async () => {
    const { t, routes } = setup();
    t.replies.push(textReply(404, '404 page not found\n'));
    await expect(routes.listCommunities()).rejects.toMatchObject({ status: 404, code: 'E_HTTP' });
  });
});

describe('Routes: patchMe', () => {
  it('sends [display, status_msg, bio] with null for a field left alone', async () => {
    const { t, routes } = setup();
    t.replies.push(noContent());
    await routes.patchMe({ statusMsg: 'on a call' });
    expect(t.seen[0].init.method).toBe('PATCH');
    expect(t.seen[0].url).toBe('https://dilla.test/v1/accounts/me');
    expect(decode(t.seen[0].init.body as Uint8Array)).toEqual([null, 'on a call', null]);
  });
  it('sends an empty string to clear a field', async () => {
    const { t, routes } = setup();
    t.replies.push(noContent());
    await routes.patchMe({ display: 'Ada', bio: '' });
    expect(decode(t.seen[0].init.body as Uint8Array)).toEqual(['Ada', null, '']);
  });
  it('sends the text trimmed', async () => {
    const { t, routes } = setup();
    t.replies.push(noContent());
    await routes.patchMe({ statusMsg: '  hello  ', bio: '\nbio\n' });
    expect(decode(t.seen[0].init.body as Uint8Array)).toEqual([null, 'hello', 'bio']);
  });
  it('refuses a too long field before any request', async () => {
    const { t, routes } = setup();
    await expect(routes.patchMe({ display: 'x'.repeat(65) })).rejects.toThrow(/display/);
    await expect(routes.patchMe({ statusMsg: 'x'.repeat(191) })).rejects.toThrow(/statusMsg/);
    await expect(routes.patchMe({ statusMsg: 'a\nb' })).rejects.toThrow(/statusMsg/);
    await expect(routes.patchMe({ bio: 'x'.repeat(501) })).rejects.toThrow(/bio/);
    await expect(routes.patchMe({ bio: '   ' })).rejects.toThrow(/bio/);
    expect(t.seen).toHaveLength(0);
  });
});

// The mirror of internal/auth NormalizeUserText. Code points, never literals, so the test shows what it pins.
describe('profileTextProblem', () => {
  const glyph = (cp: number) => `a${String.fromCodePoint(cp)}b`;
  const refused: Record<string, number> = {
    tab: 0x09, nul: 0x00, esc: 0x1b, 'lone carriage return': 0x0d,
    lrm: 0x200e, rlm: 0x200f, lre: 0x202a, rlo: 0x202e, lri: 0x2066, pdi: 0x2069,
    'zero width space': 0x200b, zwnj: 0x200c, 'bare zwj': 0x200d, 'word joiner': 0x2060, bom: 0xfeff,
    'arabic letter mark': 0x061c, 'line separator': 0x2028, 'paragraph separator': 0x2029,
  };
  it('names what is wrong: long, chars, blank', () => {
    expect(profileTextProblem('x'.repeat(191), 190)).toBe('long');
    expect(profileTextProblem('x'.repeat(190), 190)).toBeNull();
    expect(profileTextProblem('   ', 190)).toBe('blank');
    expect(profileTextProblem('\u00a0\u3000', 190)).toBe('blank');
    expect(profileTextProblem('', 190)).toBeNull();
    for (const [name, cp] of Object.entries(refused)) expect(profileTextProblem(glyph(cp), 190, true), name).toBe('chars');
  });
  it('allows a line break only in a multiline field, and an emoji joiner sequence in both', () => {
    expect(profileTextProblem('a\nb', 190)).toBe('chars');
    expect(profileTextProblem('a\nb', 500, true)).toBeNull();
    expect(profileTextProblem('a\r\nb', 500, true)).toBeNull();
    expect(profileTextProblem('\n \n', 500, true)).toBe('blank');
    const zwj = String.fromCodePoint(0x200d);
    const family = [0x1f468, 0x1f469, 0x1f467].map((cp) => String.fromCodePoint(cp)).join(zwj);
    expect(profileTextProblem(family, 190)).toBeNull();
  });
  it('counts characters after NFC, not UTF-16 units', () => {
    expect(profileTextProblem('e\u0301'.repeat(190), 190)).toBeNull();
    expect(profileTextProblem(String.fromCodePoint(0x1f600).repeat(190), 190)).toBeNull();
  });
  it('profileTextValue is the NFC, trimmed text with CRLF as LF', () => {
    expect(profileTextValue('  e\u0301\r\nx  ', true)).toBe('\u00e9\nx');
  });
});

describe('Routes: member paging', () => {
  const row = (i: number) => [member(i), 1_700_000_000, '', [], `u${i}`, `U ${i}`, 0];

  it('follows ?after= until a short page', async () => {
    const { t, routes } = setup();
    const first = Array.from({ length: MEMBERS_PAGE }, (_, i) => row(i + 1));
    const second = [row(201), row(202), row(203)];
    t.replies.push(cborReply(200, first), cborReply(200, second));
    const members = await routes.listMembers(COMMUNITY);
    expect(t.seen.map((s) => s.url)).toEqual([
      `https://dilla.test/v1/communities/${hex(COMMUNITY)}/members`,
      `https://dilla.test/v1/communities/${hex(COMMUNITY)}/members?after=${hex(member(200))}`,
    ]);
    expect(members).toHaveLength(203);
    expect(members[0]).toEqual({ userId: member(1), username: 'u1', display: 'U 1', kind: 0, nick: '', roleIds: [], statusMsg: '', bio: '' });
    expect(members[202].userId).toEqual(member(203));
  });

  it('asks once more after a full last page and stops at an empty one', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(200, Array.from({ length: MEMBERS_PAGE }, (_, i) => row(i + 1))), cborReply(200, []));
    expect(await routes.listMembers(COMMUNITY)).toHaveLength(200);
    expect(t.seen).toHaveLength(2);
  });

  it('stops with a shape error when a full page does not advance', async () => {
    const { t, routes } = setup();
    const page = Array.from({ length: MEMBERS_PAGE }, (_, i) => row(i + 1));
    t.replies.push(cborReply(200, page), cborReply(200, page));
    await expect(routes.listMembers(COMMUNITY)).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
    expect(t.seen).toHaveLength(2);
  });
});

describe('Routes: web-2b deletes and blobs', () => {
  it('putBlob labels its body octet-stream and hands it to fetch uncopied', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(201, [BLOB_ID, 5]));
    await routes.putBlob(CHANNEL, BLOB_ID, STORED);
    expect(t.seen[0].headers.get('content-type')).toBe('application/octet-stream');
    expect(t.seen[0].init.body).toBe(STORED);
  });

  // WB I1: the 30 s per-attempt timeout is for small requests; a blob transfer may take longer on a slow link, so the
  // two blob calls opt out. A backup is capped (256 B kind 0, about 1 MiB kind 1) and sits on the sign-up, sign-in
  // and revoke paths, so it gets a longer 120 s bound rather than none. Everything else keeps the default.
  it('blob transfers opt out of the per-attempt timeout; backups get 120 s; a create keeps the default', async () => {
    const { t, http, routes } = setup();
    t.replies.push(cborReply(201, [BLOB_ID, 5]), bytesReply(200, STORED), cborReply(201, [new Uint8Array(32), 9]),
      cborReply(200, [new Uint8Array(4), 1]), cborReply(201, [CHANNEL, 0, 0]));
    await routes.putBlob(CHANNEL, BLOB_ID, STORED);
    await routes.getBlob(CHANNEL, BLOB_ID);
    await routes.putBackup(0, new Uint8Array(4));
    await routes.getBackup(0);
    await routes.createChannel(COMMUNITY, { kind: 'text', name: 'x', position: 0 });
    expect(http.requests.map((r) => r.timeoutMs)).toEqual([null, null, 120_000, 120_000, undefined]);
  });

  it('confirmBlob sends neither a body nor a content type', async () => {
    const { t, routes } = setup();
    t.replies.push(noContent());
    await routes.confirmBlob(CHANNEL, BLOB_ID);
    expect(t.seen[0].body).toBeNull();
    expect(t.seen[0].headers.get('content-type')).toBeNull();
  });

  it('waits out a 429 on an upload and sends the bytes once more', async () => {
    const { t, routes } = setup();
    t.replies.push(refusal(429, 'E_RATE_LIMITED', '', 1000), cborReply(201, [BLOB_ID, 5]));
    expect(await routes.putBlob(CHANNEL, BLOB_ID, STORED)).toEqual({ created: true, size: 5 });
    expect(t.sleeps).toEqual([1000]);
    expect(t.seen).toHaveLength(2);
  });

  it('getBlob bounds the body by maxBytes and is unbounded without it (FACTS-SECURITY-13)', async () => {
    const { t, routes } = setup();
    t.replies.push(octetReply(200, STORED));
    await expect(routes.getBlob(CHANNEL, BLOB_ID, STORED.length - 1)).rejects.toMatchObject({ code: 'E_BODY_TOO_LARGE', status: 200 });
    t.replies.push(octetReply(200, STORED));
    expect(await routes.getBlob(CHANNEL, BLOB_ID, STORED.length)).toEqual(STORED);
    t.replies.push(octetReply(200, STORED));
    expect(await routes.getBlob(CHANNEL, BLOB_ID)).toEqual(STORED);
    expect(t.seen).toHaveLength(3);
  });

  it('refuses bad ids before sending', async () => {
    const { t, routes } = setup();
    await expect(routes.deleteGroupMessage(fill(15, 1), 1n)).rejects.toThrow('E_ROUTE_INPUT: groupId must be 16 bytes');
    await expect(routes.deleteGroupMessage(GROUP, -1n)).rejects.toThrow('E_ROUTE_INPUT: seq must not be negative');
    await expect(routes.putBlob(CHANNEL, fill(31, 1), STORED)).rejects.toThrow('E_ROUTE_INPUT: blobId must be 32 bytes');
    await expect(routes.getBlob(fill(15, 1), BLOB_ID)).rejects.toThrow('E_ROUTE_INPUT: channelId must be 16 bytes');
    await expect(routes.confirmBlob(CHANNEL, fill(33, 1))).rejects.toThrow('E_ROUTE_INPUT: blobId must be 32 bytes');
    await expect(routes.deleteBlob(CHANNEL, fill(16, 1))).rejects.toThrow('E_ROUTE_INPUT: blobId must be 32 bytes');
    expect(t.seen).toHaveLength(0);
  });

  it('listMembers reads a null role list as empty', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(200, [[USER, 1_700_000_000, '', null, 'jonas', 'Jonas', 0]]));
    expect(await routes.listMembers(COMMUNITY)).toEqual([{ userId: USER, username: 'jonas', display: 'Jonas', kind: 0, nick: '', roleIds: [], statusMsg: '', bio: '' }]);
  });
});

describe('Routes: web-3 calls (L-TS-55)', () => {
  const CALL = fill(16, 0x21);
  const DEV_B = fill(16, 0x22);
  const party = (i: number): [Uint8Array, Uint8Array, number] => [member(i % 256), DEV_B, 1];
  const ICE: [string[], string, string][] = [[['turn:dilla.test:3478?transport=udp'], 'u', 'c']];

  it('getChannelCall reads the group, the live call and its participants', async () => {
    const { t, http, routes } = setup();
    t.replies.push(cborReply(200, [GROUP, CALL, [[USER, DEVICE, 1], [PEER_USER, DEV_B, 7]]]));
    expect(await routes.getChannelCall(CHANNEL)).toEqual({ groupId: GROUP, callId: CALL,
      participants: [{ userId: USER, deviceId: DEVICE, flags: 1 }, { userId: PEER_USER, deviceId: DEV_B, flags: 7 }] });
    expect(t.seen[0].method).toBe('GET');
    expect(t.seen[0].url).toBe(`https://dilla.test/v1/channels/${hex(CHANNEL)}/call`);
    expect(http.requests[0].bucket).toBe('read');
    t.replies.push(cborReply(200, [null, null, []]));
    expect(await routes.getChannelCall(CHANNEL)).toEqual({ groupId: null, callId: null, participants: [] });
  });

  // Attacker: an instance that answers a snapshot of unbounded size to exhaust the worker; the decoder stops at the
  // ledger's bound (1,024 participants, 4,096 calls) with E_CBOR_SHAPE.
  it('bounds the participants at 1,024 and the calls at 4,096', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(200, [GROUP, CALL, Array.from({ length: 1024 }, (_, i) => party(i))]));
    expect((await routes.getChannelCall(CHANNEL)).participants).toHaveLength(1024);
    t.replies.push(cborReply(200, [GROUP, CALL, Array.from({ length: 1025 }, (_, i) => party(i))]));
    await expect(routes.getChannelCall(CHANNEL)).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
    t.replies.push(cborReply(200, [[CHANNEL, CALL, Array.from({ length: 1025 }, (_, i) => party(i))]]));
    await expect(routes.getCommunityCalls(COMMUNITY)).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
    t.replies.push(cborReply(200, Array.from({ length: 4096 }, () => [CHANNEL, CALL, []])));
    expect(await routes.getCommunityCalls(COMMUNITY)).toHaveLength(4096);
    t.replies.push(cborReply(200, Array.from({ length: 4097 }, () => [CHANNEL, CALL, []])));
    await expect(routes.getCommunityCalls(COMMUNITY)).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
  });

  it('refuses a participant flag above 31 and an id of the wrong length', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(200, [GROUP, CALL, [[USER, DEVICE, 32]]]));
    await expect(routes.getChannelCall(CHANNEL)).rejects.toMatchObject({ code: 'E_CBOR_RANGE' });
    t.replies.push(cborReply(200, [fill(15, 1), CALL, []]));
    await expect(routes.getChannelCall(CHANNEL)).rejects.toBeInstanceOf(Error);
  });

  it('getCommunityCalls reads each live call of the community', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(200, [[CHANNEL, CALL, [[USER, DEVICE, 3]]]]));
    expect(await routes.getCommunityCalls(COMMUNITY)).toEqual([{ channelId: CHANNEL, callId: CALL,
      participants: [{ userId: USER, deviceId: DEVICE, flags: 3 }] }]);
    expect(t.seen[0].url).toBe(`https://dilla.test/v1/communities/${hex(COMMUNITY)}/calls`);
  });

  it('startCall sends the decoders, reads the six elements and tells a 201 from a 200', async () => {
    const { t, http, routes } = setup();
    t.replies.push(cborReply(201, [CALL, GROUP, 'wss://dilla.test/rtc', 'tok', ICE, [64_000, 2_500_000, 1]]));
    expect(await routes.startCall(CHANNEL, 'vp8,h264')).toEqual({ created: true, callId: CALL, groupId: GROUP,
      livekitUrl: 'wss://dilla.test/rtc', token: 'tok', iceServers: ICE, caps: [64_000, 2_500_000, 1], rung: null });
    expect(t.seen[0].url).toBe(`https://dilla.test/v1/channels/${hex(CHANNEL)}/calls`);
    expect(t.seen[0].body).toEqual(encode(['vp8,h264']));
    expect(http.requests[0].idempotent).toBe(false);
    t.replies.push(cborReply(200, [CALL, GROUP, 'wss://dilla.test/rtc', 'tok', [], [64_000, 2_500_000, 0]]));
    expect((await routes.startCall(CHANNEL, '')).created).toBe(false);
    expect(t.seen[1].body).toEqual(encode([]));
  });

  // WB product walk I6: a start that opens a DM call carries a seventh element, the users it rang.
  it('startCall reads the seventh element, the users a DM start rang', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(201, [CALL, GROUP, 'wss://dilla.test/rtc', 'tok', ICE, [64_000, 2_500_000, 1], [USER]]));
    expect((await routes.startCall(CHANNEL, '')).rung).toEqual([USER]);
    t.replies.push(cborReply(201, [CALL, GROUP, 'wss://dilla.test/rtc', 'tok', ICE, [64_000, 2_500_000, 1], []]));
    expect((await routes.startCall(CHANNEL, '')).rung).toEqual([]);
    t.replies.push(cborReply(201, [CALL, GROUP, 'wss://dilla.test/rtc', 'tok', ICE, [64_000, 2_500_000, 1], [new Uint8Array(3)]]));
    await expect(routes.startCall(CHANNEL, '')).rejects.toThrow();
  });

  it('share, unshare and stats map onto their routes', async () => {
    const { t, routes } = setup();
    t.replies.push(noContent(), noContent(), noContent());
    await routes.shareCall(CALL);
    await routes.unshareCall(CALL);
    await routes.postCallStats(CALL, [3, null, 40, 0, 0, 1_048_576]);
    expect(t.seen.map((s) => `${s.method} ${new URL(s.url).pathname}`)).toEqual([
      `POST /v1/calls/${hex(CALL)}/share`, `DELETE /v1/calls/${hex(CALL)}/share`, `POST /v1/calls/${hex(CALL)}/stats`]);
    expect(t.seen[2].body).toEqual(encode([3, null, 40, 0, 0, 1_048_576]));
  });

  it('endCallIfAlone is DELETE /v1/calls/{call_id}?if_alone=1, idempotent: 204 ended, 409 E_CALL_NOT_ALONE not', async () => {
    const { t, http, routes } = setup();
    t.replies.push(noContent());
    expect(await routes.endCallIfAlone(CALL)).toBe(true);
    const url = new URL(t.seen[0].url);
    expect(`${t.seen[0].method} ${url.pathname}${url.search}`).toBe(`DELETE /v1/calls/${hex(CALL)}?if_alone=1`);
    expect(http.requests[0].idempotent).toBe(true);
    t.replies.push(refusal(409, 'E_CALL_NOT_ALONE', ''));
    expect(await routes.endCallIfAlone(CALL)).toBe(false);
    t.replies.push(refusal(403, 'E_FORBIDDEN', ''));
    await expect(routes.endCallIfAlone(CALL)).rejects.toMatchObject({ status: 403, code: 'E_FORBIDDEN' });
  });

  it('task 8c: rtt is POST /v1/calls/{call_id}/rtt with [ms] or [] to clear, and a 429 is not waited out', async () => {
    const { t, routes } = setup();
    t.replies.push(noContent(), noContent());
    await routes.postCallRtt(CALL, 24);
    await routes.postCallRtt(CALL, null);
    expect(t.seen.map((s) => `${s.method} ${new URL(s.url).pathname}`)).toEqual([`POST /v1/calls/${hex(CALL)}/rtt`, `POST /v1/calls/${hex(CALL)}/rtt`]);
    expect(t.seen[0].body).toEqual(encode([24]));
    expect(t.seen[1].body).toEqual(encode([]));
    t.replies.push(refusal(429, 'E_RATE_LIMITED', '', 1500));
    await expect(routes.postCallRtt(CALL, 30)).rejects.toMatchObject({ status: 429 });
    expect(t.sleeps).toEqual([]);
  });

  it('a 429 on the stats route is not waited out', async () => {
    const { t, routes } = setup();
    t.replies.push(refusal(429, 'E_RATE_LIMITED', '', 4000));
    await expect(routes.postCallStats(CALL, [0, 2, 0, 1000, 7, 0])).rejects.toMatchObject({ status: 429 });
    expect(t.sleeps).toEqual([]);
  });

  it('postProposal sends the core body unchanged and reads the seq', async () => {
    const { t, routes } = setup();
    t.replies.push(cborReply(201, [42]));
    expect(await routes.postProposal(GROUP, BODY)).toEqual({ seq: 42n });
    expect(t.seen[0].url).toBe(`https://dilla.test/v1/groups/${hex(GROUP)}/proposal`);
    expect(t.seen[0].body).toEqual(BODY);
  });

  // web3.7 3a-client review I3: the boot sweep's requests can be cancelled; an abort stops the client's retries.
  it('getChannelCall, endCallIfAlone and postProposal hand their signal to the client', async () => {
    const { t, http, routes } = setup();
    const abort = new AbortController();
    t.replies.push(cborReply(200, [null, null, []]), noContent(), cborReply(201, [1]));
    await routes.getChannelCall(CHANNEL, abort.signal);
    await routes.endCallIfAlone(CALL, abort.signal);
    await routes.postProposal(GROUP, BODY, abort.signal);
    expect(http.requests.map((r) => r.signal)).toEqual([abort.signal, abort.signal, abort.signal]);
  });

  it('an end retrying on 503 stops once its signal aborts: no further attempt is sent', async () => {
    const { t, routes } = setup();
    const abort = new AbortController();
    t.replies.push(() => { abort.abort(); return refusal(503, 'E_UNAVAILABLE', '', 1000); });
    t.replies.push(noContent());
    await expect(routes.endCallIfAlone(CALL, abort.signal)).rejects.toMatchObject({ name: 'AbortError' });
    expect(t.seen).toHaveLength(1);
  });

  it('declineCall(callId, 1700000030) sends [1700000030] and gives 204; its 404 is surfaced as E_NOT_FOUND', async () => {
    const { t, routes } = setup();
    t.replies.push(noContent());
    expect(await routes.declineCall(CALL, 1700000030)).toBeUndefined();
    expect(t.seen[0].method).toBe('POST');
    expect(t.seen[0].url).toBe(`https://dilla.test/v1/calls/${hex(CALL)}/decline`);
    expect(t.seen[0].body).toEqual(encode([1700000030]));
    t.replies.push(refusal(404, 'E_NOT_FOUND', 'no such call'));
    await expect(routes.declineCall(CALL, 1700000030)).rejects.toMatchObject({ status: 404, code: 'E_NOT_FOUND' });
  });

  it('refuses bad call inputs before sending', async () => {
    const { t, routes } = setup();
    await expect(routes.getChannelCall(fill(15, 1))).rejects.toThrow('E_ROUTE_INPUT');
    await expect(routes.declineCall(CALL, -1)).rejects.toThrow('E_ROUTE_INPUT');
    await expect(routes.declineCall(CALL, 1.5)).rejects.toThrow('E_ROUTE_INPUT');
    await expect(routes.shareCall(fill(17, 1))).rejects.toThrow('E_ROUTE_INPUT');
    expect(t.seen).toHaveLength(0);
  });
});

// ---- wave 1: server admin and moderation (flow 08) ----
describe('Routes: server admin and moderation', () => {
  const OTHER = fill(16, 0x21);
  const INVITE = fill(16, 0x22);
  const TOP = 2 ** 31;
  const role = (hoist: number): (number | string | bigint | Uint8Array)[] => [ROLE, 'mods', 0xff8800, 5, 4096n, 0n, hoist, 1];
  const one = async (reply: Response, call: (r: Routes) => Promise<unknown>) => {
    const { t, http, routes } = setup();
    t.replies.push(reply);
    const result = await call(routes);
    return { result, seen: t.seen[0], http };
  };

  it('arms automatic moderation with the exact config digest and decodes the opaque one-use grant', async () => {
    const token='opaque-grant-token'; const digest=new Uint8Array(32).fill(0x61); const request=fill(16,0x62);
    const fixture=setup(); fixture.t.replies.push(cborReply(200,[token,900n]));
    const result=await fixture.routes.armAutoModerationGrant(GROUP,request,COMMUNITY,7n,800n,digest,'step-up-once');
    expect(result).toEqual({token,expires:900n});
    expect(fixture.http.requests[0]).toMatchObject({method:'POST',path:`/v1/groups/${hex(GROUP)}/automod-grants/arm`,bucket:'write',idempotent:true,
      body:encode([request,COMMUNITY,7n,800n,digest,'step-up-once'])});
    expect(fixture.http.requests[0].body).not.toContain('plaintext config');
  });

  it('posts the opaque ClientCore-signed removal body to the group route and surfaces server refusal', async () => {
    const body = encode([new Uint8Array([1, 2]), new Uint8Array(64).fill(0x7a)]);
    const ok = setup();
    ok.t.replies.push(cborReply(200, [INVITE, 1]));
    await ok.routes.postModerationRemoval(GROUP, body);
    expect(ok.http.requests[0]).toMatchObject({ method: 'POST', path: `/v1/groups/${hex(GROUP)}/moderation/removals`, body,
      bucket: 'write', idempotent: true, maxBodyBytes: 128 });
    const denied = setup();
    denied.t.replies.push(refusal(403, 'E_FORBIDDEN'));
    await expect(denied.routes.postModerationRemoval(GROUP, body)).rejects.toMatchObject({ status: 403, code: 'E_FORBIDDEN' });
  });

  it('getPermissions reads [bits, top]', async () => {
    const { result, seen, http } = await one(cborReply(200, [16_777_215, TOP]), (r) => r.getPermissions(COMMUNITY));
    expect(result).toEqual({ bits: 16_777_215n, top: TOP });
    expect(seen.method).toBe('GET');
    expect(seen.url).toBe(`https://dilla.test/v1/communities/${hex(COMMUNITY)}/permissions`);
    expect(http.requests[0].bucket).toBe('read');
    expect(http.requests[0].idempotent).toBe(true);
  });
  it('getPermissions refuses a short answer and a rank above the owner', async () => {
    const a = setup();
    a.t.replies.push(cborReply(200, [1]));
    await expect(a.routes.getPermissions(COMMUNITY)).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
    const b = setup();
    b.t.replies.push(cborReply(200, [1, TOP + 1]));
    await expect(b.routes.getPermissions(COMMUNITY)).rejects.toMatchObject({ code: 'E_CBOR_RANGE' });
  });
  it('listRoles reads the role rows', async () => {
    const { result, seen } = await one(cborReply(200, [role(1)]), (r) => r.listRoles(COMMUNITY));
    expect(result).toEqual([{ id: ROLE, name: 'mods', color: 0xff8800, position: 5, allow: 4096, deny: 0, hoist: true, mentionable: true }]);
    expect(seen.url).toBe(`https://dilla.test/v1/communities/${hex(COMMUNITY)}/roles`);
  });
  it('listRoles refuses 251 rows and a hoist of 2', async () => {
    const a = setup();
    a.t.replies.push(cborReply(200, Array.from({ length: 251 }, () => role(0))));
    await expect(a.routes.listRoles(COMMUNITY)).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
    const b = setup();
    b.t.replies.push(cborReply(200, [role(2)]));
    await expect(b.routes.listRoles(COMMUNITY)).rejects.toMatchObject({ code: 'E_CBOR_RANGE' });
    const c = setup();
    c.t.replies.push(cborReply(200, Array.from({ length: 250 }, () => role(0))));
    expect(await c.routes.listRoles(COMMUNITY)).toHaveLength(250);
  });
  it('patchCommunity sends [name, null, null, null] and reads the policy version', async () => {
    const { result, seen, http } = await one(cborReply(200, [4]), (r) => r.patchCommunity(COMMUNITY, { name: 'Asgard' }));
    expect(result).toBe(4n);
    expect(seen.method).toBe('PATCH');
    expect(seen.url).toBe(`https://dilla.test/v1/communities/${hex(COMMUNITY)}`);
    expect(seen.body).toEqual(encode(['Asgard', null, null, null]));
    expect(http.requests[0].bucket).toBe('write');
  });
  it('patchCommunity refuses an empty name, 256 bytes and a newline before any request', async () => {
    const { t, routes } = setup();
    for (const name of ['', 'x'.repeat(256), '\n']) {
      await expect(routes.patchCommunity(COMMUNITY, { name })).rejects.toThrow(/^E_ROUTE_INPUT: /);
    }
    expect(t.seen).toHaveLength(0);
  });
  it('deleteCommunity, leaveCommunity and transferOwnership send what the server reads', async () => {
    const del = await one(noContent(), (r) => r.deleteCommunity(COMMUNITY));
    expect(del.seen.method).toBe('DELETE');
    expect(del.seen.url).toBe(`https://dilla.test/v1/communities/${hex(COMMUNITY)}`);
    expect(del.http.requests[0].idempotent).toBe(true);
    const leave = await one(noContent(), (r) => r.leaveCommunity(COMMUNITY));
    expect(leave.seen.method).toBe('POST');
    expect(leave.seen.url).toBe(`https://dilla.test/v1/communities/${hex(COMMUNITY)}/leave`);
    expect(leave.seen.body).toEqual(encode([]));
    expect(leave.http.requests[0].idempotent).toBe(false);
    const give = await one(noContent(), (r) => r.transferOwnership(COMMUNITY, OTHER));
    expect(give.seen.method).toBe('POST');
    expect(give.seen.url).toBe(`https://dilla.test/v1/communities/${hex(COMMUNITY)}/owner`);
    expect(give.seen.body).toEqual(encode([OTHER]));
    expect(give.http.requests[0].idempotent).toBe(false);
  });
  it('createCategory sends [2, 0, 0, null, name, "", position, 0]', async () => {
    const { result, seen } = await one(cborReply(201, [CATEGORY, 0, 0]), (r) => r.createCategory(COMMUNITY, { name: 'rooms', position: 3 }));
    expect(result).toEqual({ id: CATEGORY, mode: 0, visibility: 0 });
    expect(seen.url).toBe(`https://dilla.test/v1/communities/${hex(COMMUNITY)}/channels`);
    expect(seen.body).toEqual(encode([2, 0, 0, null, 'rooms', '', 3, 0]));
  });
  it('patchChannel puts each change in its slot and null elsewhere', async () => {
    const top = await one(noContent(), (r) => r.patchChannel(CHANNEL, { parent: 'top' }));
    expect(top.seen.method).toBe('PATCH');
    expect(top.seen.url).toBe(`https://dilla.test/v1/channels/${hex(CHANNEL)}`);
    expect(top.seen.body).toEqual(encode([null, null, null, null, new Uint8Array(16), null, null]));
    const into = await one(noContent(), (r) => r.patchChannel(CHANNEL, { parent: CATEGORY }));
    expect(into.seen.body).toEqual(encode([null, null, null, null, CATEGORY, null, null]));
    const moved = await one(noContent(), (r) => r.patchChannel(CHANNEL, { position: 4 }));
    expect(moved.seen.body).toEqual(encode([null, null, null, null, null, 4, null]));
    const named = await one(noContent(), (r) => r.patchChannel(CHANNEL, { name: 'lobby', topic: 'a\nb' }));
    expect(named.seen.body).toEqual(encode(['lobby', 'a\nb', null, null, null, null, null]));
    const cleared = await one(noContent(), (r) => r.patchChannel(CHANNEL, { topic: '' }));
    expect(cleared.seen.body).toEqual(encode([null, '', null, null, null, null, null]));
  });
  it('patchChannel puts a slow-mode step in slot 6 and keeps it null otherwise', async () => {
    const slow = await one(noContent(), (r) => r.patchChannel(CHANNEL, { slowmode: 10 }));
    expect(slow.seen.body).toEqual(encode([null, null, null, null, null, null, 10]));
    const off = await one(noContent(), (r) => r.patchChannel(CHANNEL, { slowmode: 0 }));
    expect(off.seen.body).toEqual(encode([null, null, null, null, null, null, 0]));
    const none = await one(noContent(), (r) => r.patchChannel(CHANNEL, {}));
    expect(none.seen.body).toEqual(encode([null, null, null, null, null, null, null]));
  });
  it('patchChannel refuses a slow mode that is not a step, before any request', async () => {
    const { t, routes } = setup();
    const error = await routes.patchChannel(CHANNEL, { slowmode: 7 }).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(RangeError);
    expect((error as Error).message).toBe('E_ROUTE_INPUT: slowmode must be one of 0, 5, 10, 30, 60, 300, 900, 3600, 21600');
    expect(t.seen).toHaveLength(0);
  });
  it('patchChannel refuses a 1025-byte topic and a NUL before any request, and accepts 1024 bytes', async () => {
    const { t, routes } = setup();
    await expect(routes.patchChannel(CHANNEL, { topic: 'x'.repeat(1025) })).rejects.toThrow(/^E_ROUTE_INPUT: /);
    await expect(routes.patchChannel(CHANNEL, { topic: '\u0000' })).rejects.toThrow(/^E_ROUTE_INPUT: /);
    await expect(routes.patchChannel(CHANNEL, { name: 'a\nb' })).rejects.toThrow(/^E_ROUTE_INPUT: /);
    await expect(routes.patchChannel(CHANNEL, { position: -1 })).rejects.toThrow(/^E_ROUTE_INPUT: /);
    await expect(routes.patchChannel(new Uint8Array(15), { position: 1 })).rejects.toThrow(/^E_ROUTE_INPUT: /);
    expect(t.seen).toHaveLength(0);
    t.replies.push(noContent());
    await routes.patchChannel(CHANNEL, { topic: 'x'.repeat(1024) });
    expect(t.seen).toHaveLength(1);
  });
  it('deleteChannel, kickMember, deleteBan and revokeInvite use their paths', async () => {
    const ch = await one(noContent(), (r) => r.deleteChannel(CHANNEL));
    expect([ch.seen.method, ch.seen.url]).toEqual(['DELETE', `https://dilla.test/v1/channels/${hex(CHANNEL)}`]);
    const kick = await one(noContent(), (r) => r.kickMember(COMMUNITY, OTHER));
    expect([kick.seen.method, kick.seen.url]).toEqual(['DELETE', `https://dilla.test/v1/communities/${hex(COMMUNITY)}/members/${hex(OTHER)}`]);
    const unban = await one(noContent(), (r) => r.deleteBan(COMMUNITY, OTHER));
    expect([unban.seen.method, unban.seen.url]).toEqual(['DELETE', `https://dilla.test/v1/communities/${hex(COMMUNITY)}/bans/${hex(OTHER)}`]);
    const revoke = await one(noContent(), (r) => r.revokeInvite(INVITE));
    expect([revoke.seen.method, revoke.seen.url]).toEqual(['DELETE', `https://dilla.test/v1/invites/${hex(INVITE)}`]);
  });
  it('putBan sends [reason, expires|null]', async () => {
    const forever = await one(noContent(), (r) => r.putBan(COMMUNITY, OTHER, { reason: 'spam', expires: null }));
    expect(forever.seen.method).toBe('PUT');
    expect(forever.seen.url).toBe(`https://dilla.test/v1/communities/${hex(COMMUNITY)}/bans/${hex(OTHER)}`);
    expect(forever.seen.body).toEqual(encode(['spam', null]));
    const timed = await one(noContent(), (r) => r.putBan(COMMUNITY, OTHER, { reason: '', expires: 1_800_000_000n }));
    expect(timed.seen.body).toEqual(encode(['', 1_800_000_000]));
  });
  it('putBan refuses a 513-byte reason before any request', async () => {
    const { t, routes } = setup();
    await expect(routes.putBan(COMMUNITY, OTHER, { reason: 'x'.repeat(513), expires: null })).rejects.toThrow(/^E_ROUTE_INPUT: /);
    expect(t.seen).toHaveLength(0);
  });
  it('listBans reads seven elements, and a five-element row has username and display ""', async () => {
    const seven = [OTHER, 'spam', USER, 1_700_000_000, null, 'loki', 'Loki'];
    const five = [PEER_USER, '', USER, 1_700_000_001, 1_800_000_000];
    const { result, seen } = await one(cborReply(200, [seven, five]), (r) => r.listBans(COMMUNITY));
    expect(seen.url).toBe(`https://dilla.test/v1/communities/${hex(COMMUNITY)}/bans`);
    expect(result).toEqual([
      { userId: OTHER, reason: 'spam', byUser: USER, created: 1_700_000_000n, expires: null, username: 'loki', display: 'Loki' },
      { userId: PEER_USER, reason: '', byUser: USER, created: 1_700_000_001n, expires: 1_800_000_000n, username: '', display: '' },
    ]);
  });
  it('listInvites reads the invite rows past grants_admin', async () => {
    const row = [INVITE, COMMUNITY, USER, 0, 10, 3, 1_700_000_000, 1_700_600_000, null];
    const { result, seen } = await one(cborReply(200, [row]), (r) => r.listInvites(COMMUNITY));
    expect(seen.url).toBe(`https://dilla.test/v1/communities/${hex(COMMUNITY)}/invites`);
    expect(result).toEqual([{ id: INVITE, communityId: COMMUNITY, createdBy: USER, maxUses: 10, uses: 3,
      created: 1_700_000_000n, expires: 1_700_600_000n, revokedAt: null }]);
  });
  it('kickMember 403 is E_FORBIDDEN', async () => {
    const { t, routes } = setup();
    t.replies.push(refusal(403, 'E_FORBIDDEN', 'kick members'));
    await expect(routes.kickMember(COMMUNITY, OTHER)).rejects.toMatchObject({ status: 403, code: 'E_FORBIDDEN' });
  });
});

describe('Routes: wave 2a roles, overwrites and private channel create (internal/api roles.go, channels.go)', () => {
  const TARGET = fill(16, 0x31);
  const ROLE_2 = fill(16, 0x32);
  const one = async (reply: Response, call: (r: Routes) => Promise<unknown>) => {
    const { t, http, routes } = setup();
    t.replies.push(reply);
    const result = await call(routes);
    return { result, seen: t.seen[0], http };
  };
  const NEW_ROLE = { name: 'Crew', color: 0x30a46c, position: 1, allow: 8, deny: 0, hoist: true, mentionable: true };

  it('createRole sends seven slots and reads [role_id]', async () => {
    const { result, seen, http } = await one(cborReply(201, [ROLE_2]), (r) => r.createRole(COMMUNITY, NEW_ROLE));
    expect(result).toEqual({ roleId: ROLE_2 });
    expect([seen.method, seen.url]).toEqual(['POST', `https://dilla.test/v1/communities/${hex(COMMUNITY)}/roles`]);
    expect(seen.body).toEqual(encode(['Crew', 0x30a46c, 1, 8, 0, 1, 1]));
    expect([http.requests[0].bucket, http.requests[0].idempotent]).toEqual(['write', false]);
  });
  it('patchRole sends null for every absent field, in the server order', async () => {
    const { seen, http } = await one(noContent(), (r) => r.patchRole(ROLE_2, { allow: 8 }));
    expect([seen.method, seen.url]).toEqual(['PATCH', `https://dilla.test/v1/roles/${hex(ROLE_2)}`]);
    expect(seen.body).toEqual(encode([null, null, null, 8, null, null, null]));
    expect([http.requests[0].bucket, http.requests[0].idempotent]).toEqual(['write', true]);
    const all = await one(noContent(), (r) => r.patchRole(ROLE_2, { name: 'n', color: 1, position: 2, allow: 3, deny: 4, hoist: false, mentionable: true }));
    expect(all.seen.body).toEqual(encode(['n', 1, 2, 3, 4, 0, 1]));
  });
  it('deleteRole, grantRole, revokeRole, putOverwrite and deleteOverwrite use their paths and bodies', async () => {
    const del = await one(noContent(), (r) => r.deleteRole(ROLE_2));
    expect([del.seen.method, del.seen.url]).toEqual(['DELETE', `https://dilla.test/v1/roles/${hex(ROLE_2)}`]);
    const grantPath = `https://dilla.test/v1/communities/${hex(COMMUNITY)}/members/${hex(TARGET)}/roles/${hex(ROLE_2)}`;
    const grant = await one(noContent(), (r) => r.grantRole(COMMUNITY, TARGET, ROLE_2));
    expect([grant.seen.method, grant.seen.url, grant.seen.body]).toEqual(['PUT', grantPath, encode([])]);
    expect([grant.http.requests[0].bucket, grant.http.requests[0].idempotent]).toEqual(['write', true]);
    const revoke = await one(noContent(), (r) => r.revokeRole(COMMUNITY, TARGET, ROLE_2));
    expect([revoke.seen.method, revoke.seen.url]).toEqual(['DELETE', grantPath]);
    const put = await one(noContent(), (r) => r.putOverwrite(CHANNEL, 1, TARGET, { allow: 2, deny: 1 }));
    expect([put.seen.method, put.seen.url, put.seen.body]).toEqual(['PUT', `https://dilla.test/v1/channels/${hex(CHANNEL)}/overwrites/1/${hex(TARGET)}`, encode([2, 1])]);
    expect([put.http.requests[0].bucket, put.http.requests[0].idempotent]).toEqual(['write', true]);
    const rm = await one(noContent(), (r) => r.deleteOverwrite(CHANNEL, 0, TARGET));
    expect([rm.seen.method, rm.seen.url]).toEqual(['DELETE', `https://dilla.test/v1/channels/${hex(CHANNEL)}/overwrites/0/${hex(TARGET)}`]);
  });
  it('grant 403 is E_FORBIDDEN', async () => {
    const { routes, t } = setup();
    t.replies.push(refusal(403, 'E_FORBIDDEN', 'rank'));
    await expect(routes.grantRole(COMMUNITY, TARGET, ROLE_2)).rejects.toMatchObject({ status: 403, code: 'E_FORBIDDEN' });
  });
  it('rejects bad role input before any request', async () => {
    const { t, routes } = setup();
    const attempts: (() => Promise<unknown>)[] = [
      () => routes.createRole(COMMUNITY, { ...NEW_ROLE, name: '' }),
      () => routes.createRole(COMMUNITY, { ...NEW_ROLE, name: 'x'.repeat(101) }),
      () => routes.createRole(COMMUNITY, { ...NEW_ROLE, name: '\u0007' }),
      () => routes.createRole(COMMUNITY, { ...NEW_ROLE, color: 0x1000000 }),
      () => routes.createRole(COMMUNITY, { ...NEW_ROLE, position: 0 }),
      () => routes.createRole(COMMUNITY, { ...NEW_ROLE, position: 2 ** 31 }),
      () => routes.createRole(COMMUNITY, { ...NEW_ROLE, allow: 0x4000000 }),
      () => routes.createRole(COMMUNITY, { ...NEW_ROLE, deny: -1 }),
      () => routes.patchRole(ROLE_2, { color: -1 }),
      () => routes.patchRole(ROLE_2, { deny: 1.5 }),
      () => routes.putOverwrite(CHANNEL, 2 as 0, TARGET, { allow: 0, deny: 0 }),
      () => routes.putOverwrite(CHANNEL, 0, TARGET, { allow: 0x4000000, deny: 0 }),
      () => routes.deleteOverwrite(CHANNEL, 2 as 0, TARGET),
      () => routes.grantRole(new Uint8Array(15), TARGET, ROLE_2),
      () => routes.createChannel(COMMUNITY, { kind: 'text', name: 'a', position: 0, overwrites: [{ kind: 0, targetId: new Uint8Array(15), allow: 0, deny: 1 }] }),
    ];
    for (const [i, attempt] of attempts.entries()) await expect(attempt(), `attempt ${i}`).rejects.toThrow(/^E_ROUTE_INPUT: /);
    expect(t.seen).toHaveLength(0);
  });
  it('accepts the 100-byte role name edge', async () => {
    await one(cborReply(201, [ROLE_2]), (r) => r.createRole(COMMUNITY, { ...NEW_ROLE, name: 'x'.repeat(100) }));
  });
  it('accepts moderate_members on create and a role row masks only the next unknown bit', async () => {
    const created = await one(cborReply(201, [ROLE_2]), (r) => r.createRole(COMMUNITY, { ...NEW_ROLE, allow: 1 << 24 }));
    expect(created.seen.body).toEqual(encode(['Crew', 0x30a46c, 1, 1 << 24, 0, 1, 1]));
    const row = [ROLE, 'mods', 0xff8800, 5, 2 ** 40 + 2 ** 25 + 2 ** 24 + 8, 2 ** 40 + 2 ** 26, 0, 1];
    const { result } = await one(cborReply(200, [row]), (r) => r.listRoles(COMMUNITY));
    expect(result).toEqual([{ id: ROLE, name: 'mods', color: 0xff8800, position: 5, allow: 2 ** 25 + (1 << 24) + 8, deny: 0, hoist: false, mentionable: true }]);
  });
  it('listOverwrites reads [channel_id, kind, target_id, allow, deny] rows', async () => {
    const { result, seen, http } = await one(cborReply(200, [[CHANNEL, 0, ROLE, 0, 1], [CHANNEL, 1, TARGET, 2, 0]]), (r) => r.listOverwrites(COMMUNITY));
    expect(result).toEqual([
      { channelId: CHANNEL, kind: 0, targetId: ROLE, allow: 0, deny: 1 },
      { channelId: CHANNEL, kind: 1, targetId: TARGET, allow: 2, deny: 0 },
    ]);
    expect([seen.method, seen.url]).toEqual(['GET', `https://dilla.test/v1/communities/${hex(COMMUNITY)}/overwrites`]);
    expect([http.requests[0].bucket, http.requests[0].idempotent]).toEqual(['read', true]);
  });
  it('an overwrite row keeps moderate_members and masks the next unknown bit', async () => {
    const { result } = await one(cborReply(200, [[CHANNEL, 0, ROLE, 2 ** 40 + 2 ** 25 + 2 ** 24 + 8, 2 ** 26 + 1]]), (r) => r.listOverwrites(COMMUNITY));
    expect(result).toEqual([{ channelId: CHANNEL, kind: 0, targetId: ROLE, allow: 2 ** 25 + (1 << 24) + 8, deny: 1 }]);
  });
  it('listOverwrites refuses a kind of 2, a short row and 10 001 rows', async () => {
    const bad = async (reply: Response, code: string) => {
      const { t, routes } = setup();
      t.replies.push(reply);
      await expect(routes.listOverwrites(COMMUNITY)).rejects.toMatchObject({ code });
    };
    await bad(cborReply(200, [[CHANNEL, 2, ROLE, 0, 0]]), 'E_CBOR_RANGE');
    await bad(cborReply(200, [[CHANNEL, 0, ROLE, 0]]), 'E_CBOR_SHAPE');
    await bad(cborReply(200, Array.from({ length: 10_001 }, () => [CHANNEL, 0, ROLE, 0, 0])), 'E_CBOR_SHAPE');
    const { t, routes } = setup();
    t.replies.push(cborReply(200, Array.from({ length: 10_000 }, () => [CHANNEL, 0, ROLE, 0, 0])));
    expect(await routes.listOverwrites(COMMUNITY)).toHaveLength(10_000);
  });
  it('createChannel with one overwrite sends a ninth element', async () => {
    const owner = fill(16, 0x33);
    const { seen, result } = await one(cborReply(201, [CHANNEL, 0, 0]), (r) =>
      r.createChannel(COMMUNITY, { kind: 'text', name: 'staff', position: 0, overwrites: [{ kind: 0, targetId: owner, allow: 0, deny: 1 }] }));
    expect(result).toEqual({ id: CHANNEL, mode: 0, visibility: 0 });
    expect(seen.body).toEqual(encode([0, 0, 0, null, 'staff', '', 0, 0, [[0, owner, 0, 1]]]));
  });
  it('createChannel with an empty or absent overwrites list sends eight elements, and 101 entries are refused', async () => {
    const empty = await one(cborReply(201, [CHANNEL, 0, 0]), (r) => r.createChannel(COMMUNITY, { kind: 'text', name: 'a', position: 0, overwrites: [] }));
    expect(empty.seen.body).toEqual(encode([0, 0, 0, null, 'a', '', 0, 0]));
    const { t, routes } = setup();
    const entry = { kind: 0 as const, targetId: TARGET, allow: 0, deny: 1 };
    await expect(routes.createChannel(COMMUNITY, { kind: 'text', name: 'a', position: 0, overwrites: Array.from({ length: 101 }, () => entry) }))
      .rejects.toThrow(/^E_ROUTE_INPUT: /);
    await expect(routes.createChannel(COMMUNITY, { kind: 'text', name: 'a', position: 0, overwrites: [{ ...entry, kind: 2 as 0 }] }))
      .rejects.toThrow(/^E_ROUTE_INPUT: /);
    await expect(routes.createChannel(COMMUNITY, { kind: 'text', name: 'a', position: 0, overwrites: [{ ...entry, deny: 0x4000000 }] }))
      .rejects.toThrow(/^E_ROUTE_INPUT: /);
    expect(t.seen).toHaveLength(0);
    t.replies.push(cborReply(201, [CHANNEL, 0, 0]));
    await routes.createChannel(COMMUNITY, { kind: 'text', name: 'a', position: 0, overwrites: Array.from({ length: 100 }, () => entry) });
    expect(t.seen).toHaveLength(1);
  });
});

describe('Routes: second-factor shapes and bounds', () => {
  const bad = async (reply: Response, call: (r: Routes) => Promise<unknown>, code: string): Promise<void> => {
    const { t, routes } = setup();
    t.replies.push(reply);
    await expect(call(routes)).rejects.toMatchObject({ code });
  };
  it('refuses a needs of 3 on a password login', async () => {
    await bad(cborReply(200, [ASSERTION, 3]), (r) => r.passwordLogin('ada', 'pw'), 'E_CBOR_RANGE');
  });
  it('refuses a needs of 2 on a passkey login finish', async () => {
    await bad(cborReply(200, [ASSERTION, 2]), (r) => r.passkeyLoginFinish(CEREMONY, RESPONSE), 'E_CBOR_RANGE');
  });
  it('refuses short answers', async () => {
    await bad(cborReply(200, [ASSERTION]), (r) => r.passwordLogin('ada', 'pw'), 'E_CBOR_SHAPE');
    await bad(cborReply(200, [1, null, [], 0]), (r) => r.getFactors(), 'E_CBOR_SHAPE');
    await bad(cborReply(200, [CRED]), (r) => r.passkeyRegisterFinish(CEREMONY, RESPONSE, 'x'), 'E_CBOR_SHAPE');
    await bad(cborReply(201, ['su-1']), (r) => r.stepUp(0, 'x'), 'E_CBOR_SHAPE');
  });
  it('refuses an owed flag, a ceremony id or a password flag outside the wire shape', async () => {
    await bad(cborReply(200, [2, null, [], 0, 1]), (r) => r.getFactors(), 'E_CBOR_RANGE');
    await bad(cborReply(200, [0, null, [], 0, 2]), (r) => r.getFactors(), 'E_CBOR_RANGE');
    await bad(cborReply(200, [fill(15, 1), OPTIONS]), (r) => r.passkeyLoginBegin(), 'E_CBOR_SHAPE');
  });
  it('bounds the backup codes at 16 and the passkeys at 1024', async () => {
    await bad(cborReply(200, [Array.from({ length: 17 }, () => 'A')]), (r) => r.totpConfirm('123456'), 'E_CBOR_SHAPE');
    await bad(cborReply(200, [Array.from({ length: 17 }, () => 'A')]), (r) => r.regenerateBackupCodes('su'), 'E_CBOR_SHAPE');
    await bad(cborReply(200, [CRED, Array.from({ length: 17 }, () => 'A')]), (r) => r.passkeyRegisterFinish(CEREMONY, RESPONSE, 'x'), 'E_CBOR_SHAPE');
    const row = [CRED, 'p', 1, null];
    await bad(cborReply(200, [0, null, Array.from({ length: 1025 }, () => row), 0, 1]), (r) => r.getFactors(), 'E_CBOR_SHAPE');
    const { t, routes } = setup();
    t.replies.push(cborReply(200, [0, null, Array.from({ length: 1024 }, () => row), 0, 1]));
    expect((await routes.getFactors()).passkeys).toHaveLength(1024);
  });
  it('refuses inputs the server would refuse, before sending', async () => {
    const { t, routes } = setup();
    const attempts: (() => Promise<unknown>)[] = [
      () => routes.passkeyRegisterFinish(CEREMONY, RESPONSE, ''),
      () => routes.passkeyRegisterFinish(CEREMONY, RESPONSE, 'x'.repeat(65)),
      () => routes.passkeyRegisterFinish(CEREMONY, RESPONSE, 'tab\there'),
      () => routes.passkeyRegisterFinish(new Uint8Array(15), RESPONSE, 'x'),
      () => routes.stepUp(3 as 0, 'x'),
      () => routes.removeFactor('su', 2 as 0, null),
      () => routes.removeFactor('su', 1, null),
      () => routes.removeFactor('su', 0, CRED),
    ];
    for (const [i, attempt] of attempts.entries()) await expect(attempt(), `attempt ${i}`).rejects.toThrow(/^E_ROUTE_INPUT: /);
    expect(t.seen).toHaveLength(0);
  });
  it('sends one request for a short 429 on the step-up and the passkey and backup-code logins', async () => {
    for (const call of [
      (routes: Routes) => routes.stepUp(0, 'x'),
      (routes: Routes) => routes.recoveryVerify(ASSERTION, 'x'),
      // The lockout's 429 spends the assertion (dillad 9bbde5c8): a retry could only meet a 401 and hide the wait.
      (routes: Routes) => routes.totpVerify(ASSERTION, '123456'),
      (routes: Routes) => routes.passkeyLoginFinish(CEREMONY, RESPONSE),
    ]) {
      const { t, routes } = setup();
      t.replies.push(refusal(429, 'E_RATE_LIMITED', '', 1000));
      await expect(call(routes)).rejects.toMatchObject({ status: 429, retryAfterMs: 1000 });
      expect(t.seen).toHaveLength(1);
      expect(t.sleeps).toEqual([]);
    }
  });
});

describe('the built-in AFK channel routes (web3.9)', () => {
  it('a 2-element snapshot is a decode error', async () => {
    const { routes, t } = setup();
    t.replies.push(cborReply(200, [CHANNEL, 300]));
    await expect(routes.getAFK(COMMUNITY)).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
  });
  // AW5 fix round 3b (protocol/07): any non-zero pending is pending; an unknown reason (3 or more) reads as 2, the calm one.
  it('an unknown pending reason of 3 or more is pending and reads as reason 2, never an error', async () => {
    for (const n of [3, 7, 255]) {
      const one = setup();
      one.t.replies.push(cborReply(200, [CHANNEL, 300, [], n]));
      await expect(one.routes.getAFK(COMMUNITY)).resolves.toMatchObject({ pending: true, reason: 2 });
    }
  });
  it('a raw CBOR true and a raw CBOR false pending are refused (the subset has no booleans)', async () => {
    for (const tail of [0xf5, 0xf4]) {
      const head = encode([CHANNEL, 300, []]);
      const raw = new Uint8Array([...head, tail]);
      raw[0] = 0x84;
      const raw1 = setup();
      raw1.t.replies.push(bytesReply(200, raw));
      await expect(raw1.routes.getAFK(COMMUNITY)).rejects.toMatchObject({ code: 'E_CBOR_TYPE' });
    }
  });
  it('an exempt of false sends [id] like no flag', async () => {
    const { routes, t } = setup();
    t.replies.push(noContent());
    await routes.sendAFKActivity(COMMUNITY, CHANNEL, false);
    expect(Array.from(t.seen[0].body ?? [])).toEqual(Array.from(encode([CHANNEL])));
  });
  // AW4: a web3.9 instance answers four elements; its roster is empty.
  it('a 4-element web3.9 snapshot has an empty away roster', async () => {
    const { routes, t } = setup();
    t.replies.push(cborReply(200, [CHANNEL, 300, [], 1]));
    expect((await routes.getAFK(COMMUNITY)).away).toEqual([]);
  });
  // Fix round 1, N3: a row with since 0 (cleared) is skipped, and a repeated user keeps its first row.
  it('an away roster skips since 0 and keeps the first row of a repeated user', async () => {
    const { routes, t } = setup();
    t.replies.push(cborReply(200, [CHANNEL, 300, [], 0, [[member(1), 0], [member(2), 50], [member(2), 70], [member(3), 90]]]));
    expect((await routes.getAFK(COMMUNITY)).away).toEqual([{ userId: member(2), since: 50 }, { userId: member(3), since: 90 }]);
  });
  it('an away roster over 1024 rows, a short user id, a missing since or a since past the safe integers is refused', async () => {
    const uid = (i: number) => { const u = member(i % 256); u[14] = i >> 8; return u; };
    const rows = (n: number) => Array.from({ length: n }, (_, i) => [uid(i), 1]);
    const bad: [CborInput[], string][] = [
      [rows(1025), 'E_CBOR_SHAPE'],
      [[[member(1).slice(1), 1]], 'E_CBOR_SHAPE'],
      [[[member(1)]], 'E_CBOR_SHAPE'],
      [[[member(1), 2n ** 60n]], 'E_CBOR_RANGE'],
    ];
    for (const [away, code] of bad) {
      const { routes, t } = setup();
      t.replies.push(cborReply(200, [CHANNEL, 300, [], 0, away]));
      await expect(routes.getAFK(COMMUNITY)).rejects.toMatchObject({ code });
    }
    const ok = setup();
    ok.t.replies.push(cborReply(200, [CHANNEL, 300, [], 0, rows(1024)]));
    expect((await ok.routes.getAFK(COMMUNITY)).away).toHaveLength(1024);
  });
  it('moveToAFK and leaveAFK no longer exist', () => {
    const { routes } = setup();
    // @ts-expect-error retired in web3.9
    expect(routes.moveToAFK).toBeUndefined();
    // @ts-expect-error retired in web3.9
    expect(routes.leaveAFK).toBeUndefined();
  });
});

describe('member timeout routes', () => {
  const REQUEST = fill(16, 0x41);

  it('reads an active timeout as a bigint timestamp', async () => {
    const { routes, t } = setup();
    t.replies.push(cborReply(200, [1_800_000_000]));
    await expect(routes.getMemberTimeout(COMMUNITY, PEER_USER)).resolves.toBe(1_800_000_000n);
    expect(t.seen[0]).toMatchObject({ method: 'GET', url: `https://dilla.test/v1/communities/${hex(COMMUNITY)}/members/${hex(PEER_USER)}/timeout` });
  });

  it('reads an absent timeout as null', async () => {
    const { routes, t } = setup();
    t.replies.push(cborReply(200, [null]));
    await expect(routes.getMemberTimeout(COMMUNITY, PEER_USER)).resolves.toBeNull();
  });

  it('writes the request id and absolute expiry, then decodes the expiry', async () => {
    const { routes, t } = setup();
    t.replies.push(cborReply(200, [REQUEST, 1_800_000_000]));
    await expect(routes.putMemberTimeout(COMMUNITY, PEER_USER, REQUEST, 1_800_000_000n)).resolves.toBe(1_800_000_000n);
    expect(t.seen[0]).toMatchObject({ method: 'PUT', url: `https://dilla.test/v1/communities/${hex(COMMUNITY)}/members/${hex(PEER_USER)}/timeout`, body: encode([REQUEST, 1_800_000_000]) });
  });

  it('clears a timeout with its request id and expects no content', async () => {
    const { routes, t } = setup();
    t.replies.push(noContent());
    await expect(routes.deleteMemberTimeout(COMMUNITY, PEER_USER, REQUEST)).resolves.toBeUndefined();
    expect(t.seen[0]).toMatchObject({ method: 'DELETE', url: `https://dilla.test/v1/communities/${hex(COMMUNITY)}/members/${hex(PEER_USER)}/timeout`, body: encode([REQUEST]) });
  });
});

describe('manual warning route', () => {
  const REQUEST = fill(16, 0x41);
  it('sends only a stable request id and returns immutable accepted action metadata', async () => {
    const { routes, t } = setup();
    const action = fill(16, 0x52);
    t.replies.push(cborReply(200, [REQUEST, action, 7, 1_800_000_000]));
    await expect(routes.warnMember(COMMUNITY, PEER_USER, REQUEST)).resolves.toEqual({
      requestId: REQUEST, actionId: action, revision: 7, acceptedAt: 1_800_000_000,
    });
    expect(t.seen[0]).toMatchObject({ method: 'POST',
      url: `https://dilla.test/v1/communities/${hex(COMMUNITY)}/members/${hex(PEER_USER)}/warning`, body: encode([REQUEST]) });
  });
  it('rejects a response that changes the request id', async () => {
    const { routes, t } = setup();
    t.replies.push(cborReply(200, [fill(16, 0x45), fill(16, 0x52), 7, 1_800_000_000]));
    await expect(routes.warnMember(COMMUNITY, PEER_USER, REQUEST)).rejects.toThrow('warning response request id mismatch');
  });
  it('reads the exact accepted warning receipt for Bot evidence binding', async () => {
    const { routes, t } = setup();
    const action = fill(16, 0x52), actorDevice = fill(16, 0x53);
    t.replies.push(cborReply(200, [REQUEST, action, COMMUNITY, USER, actorDevice, PEER_USER, 7, 1_800_000_000]));
    await expect(routes.acceptedManualWarning(BOT, REQUEST)).resolves.toEqual({ requestId: REQUEST, actionId: action,
      communityId: COMMUNITY, actorUserId: USER, actorDeviceId: actorDevice, targetUserId: PEER_USER, revision: 7, acceptedAt: 1_800_000_000 });
    expect(t.seen[0]).toMatchObject({ method: 'GET', url: `https://dilla.test/v1/bot-runtime/installations/${hex(BOT)}/moderation/warnings/${hex(REQUEST)}` });
  });
});

describe('managed community bot installation routes', () => {
  const installation = [BOT, COMMUNITY, 1, 3, USER, DEVICE, 1, 1_800_000_000];

  it('reads a bounded installation, treats CBOR null as absent, and preserves refusals', async () => {
    const { routes, t } = setup();
    t.replies.push(cborReply(200, installation));
    await expect(routes.getBotInstallation(COMMUNITY)).resolves.toMatchObject({
      id: BOT, communityId: COMMUNITY, state: 1, revision: 3n, userId: USER, deviceId: DEVICE, capabilities: 1,
    });
    expect(t.seen[0]).toMatchObject({ method: 'GET', url: `https://dilla.test/v1/communities/${hex(COMMUNITY)}/bot` });
    t.replies.push(cborReply(200, null));
    await expect(routes.getBotInstallation(COMMUNITY)).resolves.toBeNull();
    t.replies.push(refusal(404, 'E_NOT_FOUND', 'not found'));
    await expect(routes.getBotInstallation(COMMUNITY)).rejects.toMatchObject({ status: 404, detail: 'not found' });
    t.replies.push(refusal(403, 'E_FORBIDDEN', 'denied'));
    await expect(routes.getBotInstallation(COMMUNITY)).rejects.toMatchObject({ status: 403 });
  });

  it('reads member-safe command status without exposing installation configuration', async () => {
    const { routes, t } = setup();
    t.replies.push(cborReply(200, [1, BOT, USER, 3, 9]));
    await expect(routes.getBotCommandStatus(COMMUNITY)).resolves.toEqual({
      installationId: BOT, botUserId: USER, state: 3, revision: 9n,
    });
    expect(t.seen[0]).toMatchObject({ method: 'GET', url: `https://dilla.test/v1/communities/${hex(COMMUNITY)}/bot/command-status` });
    t.replies.push(cborReply(200, null));
    await expect(routes.getBotCommandStatus(COMMUNITY)).resolves.toBeNull();
    t.replies.push(cborReply(200, [1, BOT, null, 4, 10]));
    await expect(routes.getBotCommandStatus(COMMUNITY)).resolves.toMatchObject({ state: 4, botUserId: null });
    t.replies.push(cborReply(200, [2, BOT, USER, 3, 9]));
    await expect(routes.getBotCommandStatus(COMMUNITY)).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
  });

  it('sends exact request ID, revision, capabilities and step-up for configure', async () => {
    const { routes, t } = setup();
    t.replies.push(cborReply(201, installation));
    await routes.configureBotInstallation(COMMUNITY, USER, null, 1, 'stepup');
    expect(t.seen[0]).toMatchObject({ method: 'PUT', url: `https://dilla.test/v1/communities/${hex(COMMUNITY)}/bot?v=2` });
    expect(Array.from(t.seen[0].body ?? [])).toEqual(Array.from(encode([USER, null, 1, 'stepup'])));
  });

  it('updates a managed bot display name with the normalized value', async () => {
    const { routes, t } = setup();
    t.replies.push(cborReply(200, ['Music Bot']));
    await expect(routes.patchBotProfile(COMMUNITY, '  Music Bot  ')).resolves.toBe('Music Bot');
    expect(t.seen[0]).toMatchObject({ method: 'PATCH', url: `https://dilla.test/v1/communities/${hex(COMMUNITY)}/bot/profile`, body: encode(['Music Bot']) });
  });

  it('refuses malformed or foreign installation rows before exposing them to settings', async () => {
    const rows = [installation.slice(0, 7), [BOT, USER, 1, 3, USER, DEVICE, 1, 1_800_000_000],
      [BOT, COMMUNITY, 1, 0, USER, DEVICE, 1, 1_800_000_000],
      [BOT, COMMUNITY, 6, 3, USER, DEVICE, 1, 1_800_000_000],
      [BOT, COMMUNITY, 1, 3, USER, DEVICE, 1024, 1_800_000_000]];
    for (const row of rows) {
      const { routes, t } = setup();
      t.replies.push(cborReply(200, row));
      await expect(routes.getBotInstallation(COMMUNITY)).rejects.toMatchObject({ code: /E_CBOR_/ });
    }
  });

  it('sends revision-bound disable with its stable request ID', async () => {
    const { routes, t } = setup();
    t.replies.push(cborReply(200, installation));
    await routes.disableBotInstallation(COMMUNITY, USER, 3n);
    expect(t.seen[0]).toMatchObject({ method: 'POST', url: `https://dilla.test/v1/communities/${hex(COMMUNITY)}/bot/disable` });
    expect(Array.from(t.seen[0].body ?? [])).toEqual(Array.from(encode([USER, 3n])));
  });
});

describe('bot Spotify authorization routes', () => {
  const ATTEMPT = fill(16, 0x29);
  const STATE = 'a'.repeat(43);
  const CODE = 'spotify-code';
  const authorizeUrl = 'https://accounts.spotify.com/authorize?response_type=code&client_id=client&redirect_uri=https%3A%2F%2Fdilla.test%2Fspotify&scope=streaming%20user-read-email%20user-read-private&state=' + STATE + '&code_challenge_method=S256&code_challenge=' + 'b'.repeat(43);

  it('starts an attempt, forwards state/code once, and cancels with no content', async () => {
    const { routes, t } = setup();
    t.replies.push(cborReply(200, [ATTEMPT, authorizeUrl, 1_800_000_000]));
    await expect(routes.beginBotSpotifyAttempt(COMMUNITY, 'step-up', 'client')).resolves.toEqual({
      attemptId: ATTEMPT, authorizeUrl, expires: 1_800_000_000n,
    });
    expect(t.seen[0]).toMatchObject({ method: 'POST', url: `https://dilla.test/v1/communities/${hex(COMMUNITY)}/bot/spotify/attempts`, body: encode(['step-up', 'client']) });
    t.replies.push(cborReply(202, [1]));
    await expect(routes.callbackBotSpotifyAttempt(COMMUNITY, ATTEMPT, STATE, CODE)).resolves.toBeUndefined();
    expect(t.seen[1]).toMatchObject({ method: 'POST', url: `https://dilla.test/v1/communities/${hex(COMMUNITY)}/bot/spotify/attempts/${hex(ATTEMPT)}/callback`, body: encode([STATE, CODE]) });
    t.replies.push(noContent());
    await expect(routes.cancelBotSpotifyAttempt(COMMUNITY, ATTEMPT)).resolves.toBeUndefined();
  });

  it('surfaces authorization refusals and rejects malformed broker replies', async () => {
    const refused = setup();
    refused.t.replies.push(refusal(403, 'E_FORBIDDEN', 'step-up required'));
    await expect(refused.routes.beginBotSpotifyAttempt(COMMUNITY, 'stale', 'client')).rejects.toMatchObject({ status: 403, code: 'E_FORBIDDEN' });
    const withQuery = (mutate: (url: URL) => void): string => { const url = new URL(authorizeUrl); mutate(url); return url.toString(); };
    const invalidUrls = ['https://evil.test/authorize', withQuery(u => { u.port = '444'; }),
      withQuery(u => { u.searchParams.append('state', STATE); }), withQuery(u => { u.searchParams.set('client_id', ''); }),
      withQuery(u => { u.searchParams.set('code_challenge', ''); }), withQuery(u => { u.searchParams.append('unexpected', 'x'); })];
    for (const reply of [...invalidUrls.map(url => [ATTEMPT, url, 1_800_000_000]), [ATTEMPT, authorizeUrl], [ATTEMPT, authorizeUrl, 0]]) {
      const { routes, t } = setup();
      t.replies.push(cborReply(200, reply));
      await expect(routes.beginBotSpotifyAttempt(COMMUNITY, 'step-up', 'client')).rejects.toMatchObject({ code: /E_CBOR_/ });
    }
    const malformedAck = setup();
    malformedAck.t.replies.push(cborReply(202, [2]));
    await expect(malformedAck.routes.callbackBotSpotifyAttempt(COMMUNITY, ATTEMPT, STATE, CODE)).rejects.toMatchObject({ code: 'E_CBOR_SHAPE' });
  });
});

// Task 10c: an open-ended invite is max_uses 0 and ttl_seconds 0 on the wire; the answer's expires is 0 for "never".
it('sends "never" and "∞" as zeros and reads a never-expiring answer', async () => {
  const { routes, t, http } = setup();
  t.replies.push(cborReply(201, [BOT, 'code1234', 'https://dilla.test/i/code1234', 0]));
  await expect(routes.createInvite(COMMUNITY, { maxUses: 0, ttlSeconds: 0 })).resolves.toMatchObject({ expires: 0n });
  expect(http.requests[0].body).toEqual(encode([0, 0, 0]));
});
