import { describe, expect, it, vi } from 'vitest';
import { decode, encode, type CborInput, type CborValue } from '../cbor';
import { toHex } from '../hex';
import { BotCatalogStore, decodeBotCatalogResponse, decodeBotChannels, decodeBotRevisions, type BotCatalogDeps } from './catalog';

const fid = (byte: number): Uint8Array => new Uint8Array(16).fill(byte);
/** A command as a decoded frame carries it (bigints). */
const cv = (value: CborInput): CborValue => decode(encode(value));
const COMMUNITY = toHex(fid(0x0e));
const BOT_USER = fid(0x0b);
/** A command-key publication's leading [1, community_id, key_id, …]: enough for the community check (the core verifies the rest). */
const PUBLICATION = encode([1, fid(0x0e), fid(0x0a)]);
const KEY = fid(0x0a);

const remind: CborInput = ['remind', 'Set a reminder', 80, [['when', 4, 1], ['text', 0, 1]], 0, 0, 0];
const play: CborInput = ['play', 'Play a song', 12, [['query', 0, 1]], 0, 0, 0];
const poll: CborInput = ['poll create', 'Ask the channel', 70, [['question', 0, 1], ['channel', 3, 0]], 0, 1, 0];

/** GET …/bot/catalog: [revision, commands, publication | null, [state, since | null]]. */
const catalogBody = (revision: number, commands: CborInput[], publication: Uint8Array | null = PUBLICATION, status: CborInput = [0, null]) =>
  encode([revision, commands, publication, status]);

/** The core's per-community Bot-user pin (CorePort.botUserPin / botUserPinned), as the store keeps it: a Map that outlives
 *  a BotCatalogStore stands for the core store across a reload. */
function corePins(pins = new Map<string, Uint8Array>()) {
  return {
    pins,
    pinBotUser: vi.fn((communityId: string, botUserId: Uint8Array): { state: 0 | 1 | 2; pinned: Uint8Array } => {
      const held = pins.get(communityId);
      if (held === undefined) { pins.set(communityId, botUserId); return { state: 0, pinned: botUserId }; }
      return { state: toHex(held) === toHex(botUserId) ? 1 : 2, pinned: held };
    }),
    pinnedBotUser: (communityId: string): Uint8Array | null => pins.get(communityId) ?? null,
  };
}

function fakeDeps(over: Partial<BotCatalogDeps> = {}, pins = new Map<string, Uint8Array>()) {
  let catalog = catalogBody(3, [remind, play]);
  let revisions: CborInput[] = [[3, fid(0x0e), 3]];
  const fetchCatalog = vi.fn(() => Promise.resolve(catalog));
  const fetchRevisions = vi.fn(() => Promise.resolve(encode(revisions)));
  const verify = vi.fn<BotCatalogDeps['verify']>(() => ({ keyId: KEY, fingerprint: '0123456789AB' }));
  const deps: BotCatalogDeps = {
    fetchCatalog,
    fetchBotChannels: () => Promise.resolve(encode([[fid(0x21), 1758659000, fid(0x01)]])),
    fetchRevisions,
    botUserId: () => Promise.resolve(BOT_USER),
    verify,
    ...corePins(pins),
    pinBotDeviceList: vi.fn(() => Promise.resolve<0 | 1 | 2>(0)),
    ingestBotAttestation: vi.fn(() => Promise.resolve()),
    ...over,
  };
  return { deps, fetchCatalog, fetchRevisions, verify,
    set: (body: Uint8Array) => { catalog = body; }, setRevisions: (rows: CborInput[]) => { revisions = rows; } };
}

describe('catalog decoding', () => {
  it('reads the catalog route, the revisions and the Bot channels', () => {
    const decoded = decodeBotCatalogResponse(catalogBody(7, [remind, poll], null, [2, 1758659040]));
    expect(decoded.revision).toBe(7n);
    expect(decoded.publication).toBeNull();
    expect(decoded.status).toEqual({ state: 'offline', since: 1758659040 });
    expect(decoded.commands).toEqual([
      { name: 'remind', description: 'Set a reminder', operation: 80, args: [{ name: 'when', type: 4, required: true }, { name: 'text', type: 0, required: true }],
        permissionBit: 0, needsBotChannel: false, handlerKind: 0 },
      { name: 'poll create', description: 'Ask the channel', operation: 70, args: [{ name: 'question', type: 0, required: true }, { name: 'channel', type: 3, required: false }],
        permissionBit: 0, needsBotChannel: true, handlerKind: 0 },
    ]);
    expect(decodeBotRevisions(encode([[3, fid(0x0e), 9], [0, fid(0x0e), 2]]))).toEqual([{ kind: 3, scopeId: COMMUNITY, revision: 9n }, { kind: 0, scopeId: COMMUNITY, revision: 2n }]);
    expect(decodeBotChannels(encode([[fid(0x21), 1758659000, fid(0x01)]]))).toEqual([toHex(fid(0x21))]);
  });

  it('refuses a catalog it cannot read whole: a bad type code, a duplicate name, a bad state', () => {
    expect(() => decodeBotCatalogResponse(catalogBody(1, [['x', '', 1, [['a', 6, 1]], 0, 0, 0]]))).toThrow();
    expect(() => decodeBotCatalogResponse(catalogBody(1, [remind, remind]))).toThrow();
    expect(() => decodeBotCatalogResponse(catalogBody(1, [['', '', 1, [], 0, 0, 0]]))).toThrow();
    expect(() => decodeBotCatalogResponse(catalogBody(1, [remind], null, [5, null]))).toThrow();
    expect(() => decodeBotCatalogResponse(catalogBody(1, [['x', '', 1, [], 0, 2, 0]]))).toThrow();
  });
});

describe('BotCatalogStore', () => {
  it('connect sync fetches only when the cached revision is behind', async () => {
    const f = fakeDeps();
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    expect(f.fetchCatalog).toHaveBeenCalledTimes(1);
    expect(store.get(COMMUNITY)?.revision).toBe(3n);
    // A reconnect whose revisions say 3 again: nothing is fetched.
    store.newConnection();
    await store.syncOnConnect(COMMUNITY);
    expect(f.fetchCatalog).toHaveBeenCalledTimes(1);
    expect(f.fetchRevisions).toHaveBeenCalledTimes(2);
    // A reconnect after the catalog moved on while this device was away (op 61 is not replayed): one fetch.
    f.setRevisions([[3, fid(0x0e), 5]]);
    f.set(catalogBody(5, [remind]));
    store.newConnection();
    await store.syncOnConnect(COMMUNITY);
    expect(f.fetchCatalog).toHaveBeenCalledTimes(2);
    expect(store.get(COMMUNITY)?.commands.map(c => c.name)).toEqual(['remind']);
  });

  it('reads the revisions once per connection, however many communities sync', async () => {
    const f = fakeDeps();
    const store = new BotCatalogStore(f.deps);
    store.newConnection();
    await Promise.all([store.syncOnConnect(COMMUNITY), store.syncOnConnect(toHex(fid(0x0f)))]);
    expect(f.fetchRevisions).toHaveBeenCalledTimes(1);
  });

  it('a pushed catalog replaces the list without a fetch', async () => {
    const f = fakeDeps();
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    const seen = vi.fn();
    store.subscribe(COMMUNITY, seen);
    store.apply({ communityId: fid(0x0e), revision: 4n, commands: [cv(poll)], publication: PUBLICATION });
    await vi.waitFor(() => expect(store.get(COMMUNITY)?.revision).toBe(4n));
    expect(store.get(COMMUNITY)?.commands.map(c => c.name)).toEqual(['poll create']);
    // The push keeps the status the last read held: op 61 carries none.
    expect(store.get(COMMUNITY)?.status).toEqual({ state: 'ready', since: null });
    expect(f.fetchCatalog).toHaveBeenCalledTimes(1);
    expect(seen).toHaveBeenCalled();
    // An older push (a reordered or late one) changes nothing.
    store.apply({ communityId: fid(0x0e), revision: 2n, commands: [cv(remind)], publication: PUBLICATION });
    await Promise.resolve();
    expect(store.get(COMMUNITY)?.commands.map(c => c.name)).toEqual(['poll create']);
  });

  it('a push from a Bot read as absent or offline shows it ready; a degraded one stays degraded', async () => {
    const f = fakeDeps();
    f.set(catalogBody(1, [], null, [4, null]));
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    expect(store.get(COMMUNITY)?.status.state).toBe('absent');
    store.apply({ communityId: fid(0x0e), revision: 2n, commands: [cv(remind)], publication: PUBLICATION });
    await vi.waitFor(() => expect(store.get(COMMUNITY)?.status).toEqual({ state: 'ready', since: null }));
    f.set(catalogBody(3, [remind], PUBLICATION, [1, 1758659040]));
    await store.refresh(COMMUNITY);
    store.apply({ communityId: fid(0x0e), revision: 4n, commands: [cv(remind)], publication: PUBLICATION });
    await vi.waitFor(() => expect(store.get(COMMUNITY)?.revision).toBe(4n));
    expect(store.get(COMMUNITY)?.status).toEqual({ state: 'degraded', since: 1758659040 });
  });

  it('a pushed new key is verified before it is used', async () => {
    const f = fakeDeps();
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    const next = encode([1, fid(0x0e), fid(0x0b)]);
    store.apply({ communityId: fid(0x0e), revision: 4n, commands: [cv(remind)], publication: next });
    await vi.waitFor(() => expect(store.get(COMMUNITY)?.revision).toBe(4n));
    expect(f.verify).toHaveBeenLastCalledWith(next, BOT_USER);
    expect(store.publication(COMMUNITY)).toEqual(next);
  });

  it('a catalog whose key signature fails is shown greyed, not used', async () => {
    const f = fakeDeps({ verify: vi.fn(() => { throw Object.assign(new Error('E_BOT_SIGNATURE'), { code: 'E_BOT_SIGNATURE' }); }) });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    const catalog = store.get(COMMUNITY)!;
    // The commands stay (greyed, not hidden), but there is no key to seal to and the state is its own reason (M10).
    expect(catalog.commands.map(c => c.name)).toEqual(['remind', 'play']);
    expect(catalog.keyId).toBeNull();
    expect(catalog.fingerprint).toBeNull();
    expect(catalog.status.state).toBe('unverified');
    expect(store.publication(COMMUNITY)).toBeNull();
  });

  it('a verified catalog carries the key id and the fingerprint (for admins), and the Bot channels', async () => {
    const store = new BotCatalogStore(fakeDeps().deps);
    await store.syncOnConnect(COMMUNITY);
    expect(store.get(COMMUNITY)).toMatchObject({ keyId: KEY, fingerprint: '0123456789AB', botChannels: [toHex(fid(0x21))], botUserId: toHex(BOT_USER) });
  });

  it('an offline, degraded, disabled or absent Bot keeps its state and its since', async () => {
    const f = fakeDeps();
    const store = new BotCatalogStore(f.deps);
    for (const [code, state] of [[1, 'degraded'], [2, 'offline'], [3, 'disabled'], [4, 'absent']] as const) {
      f.set(catalogBody(10 + code, [remind], PUBLICATION, [code, 1758659040]));
      await store.refresh(COMMUNITY);
      expect(store.get(COMMUNITY)?.status).toEqual({ state, since: 1758659040 });
    }
  });

  it('a failed fetch keeps the last good catalog', async () => {
    const f = fakeDeps();
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    f.set(new Uint8Array([0xff]));
    await store.refresh(COMMUNITY);
    expect(store.get(COMMUNITY)?.commands.map(c => c.name)).toEqual(['remind', 'play']);
  });

  it('a Bot key that could not be checked yet is checked again locally, with no request', async () => {
    let ok = false;
    const f = fakeDeps({ verify: vi.fn(() => { if (!ok) throw Object.assign(new Error('stale'), { code: 'E_DEVICE_LIST_STALE' }); return { keyId: KEY, fingerprint: 'F' }; }) });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    expect(store.publication(COMMUNITY)).toBeNull();
    ok = true;
    expect(store.reverify(COMMUNITY)).toBe(true);
    expect(store.publication(COMMUNITY)).toEqual(PUBLICATION);
    expect(store.get(COMMUNITY)?.status.state).toBe('ready');
    expect(f.fetchCatalog).toHaveBeenCalledTimes(1);
  });
});

describe('fix round 1', () => {
  it('drops a command whose name or argument names are outside the picker grammar (I1)', () => {
    const bad = ['x"],body{display:none}[id="', 'Play', ' play', 'play  now', 'a'.repeat(33), 'ünïcode'];
    const decoded = decodeBotCatalogResponse(catalogBody(1, [remind, ...bad.map(name => [name, '', 1, [], 0, 0, 0]),
      ['ok name', '', 2, [['bad arg!', 0, 1]], 0, 0, 0], ['queue-move', '', 3, [['to_pos', 1, 1]], 0, 0, 0]]));
    expect(decoded.commands.map(c => c.name)).toEqual(['remind', 'queue-move']);
  });

  it('refuses another community’s publication before the core sees it (M1)', async () => {
    const f = fakeDeps();
    f.set(catalogBody(3, [remind], encode([1, fid(0x0f), fid(0x0a)])));
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    expect(f.verify).not.toHaveBeenCalled();
    expect(store.publication(COMMUNITY)).toBeNull();
    expect(store.get(COMMUNITY)?.status.state).toBe('unverified');
  });

  it('invalidate drops this device’s verdict, so the next check is real (M2)', async () => {
    const f = fakeDeps();
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    expect(store.publication(COMMUNITY)).toEqual(PUBLICATION);
    store.invalidate(COMMUNITY);
    expect(store.publication(COMMUNITY)).toBeNull();
    f.verify.mockImplementationOnce(() => { throw Object.assign(new Error('E_BOT_STALE'), { code: 'E_BOT_STALE' }); });
    expect(store.reverify(COMMUNITY)).toBe(false);
    expect(store.reverify(COMMUNITY)).toBe(true);
    expect(f.verify).toHaveBeenCalledTimes(3);
  });

  // The picker learns the Bot's status without a reload: a refresh asked for while a read is in flight (op 54 after the
  // Bot came up, say) reads again once that read ends, so a status that changed after the first read started is seen.
  it('a refresh asked for during a read reads again after it, and its status wins', async () => {
    const f = fakeDeps();
    const releases: ((body: Uint8Array) => void)[] = [];
    const fetchCatalog = vi.fn(() => new Promise<Uint8Array>(resolve => { releases.push(resolve); }));
    const store = new BotCatalogStore({ ...f.deps, fetchCatalog });
    const first = store.refresh(COMMUNITY);
    await Promise.resolve();
    const second = store.refresh(COMMUNITY); // the Bot came up meanwhile
    releases[0]?.(catalogBody(3, [remind], PUBLICATION, [2, 1758659040]));
    await first;
    await vi.waitFor(() => { expect(fetchCatalog).toHaveBeenCalledTimes(2); });
    releases[1]?.(catalogBody(3, [remind], PUBLICATION, [0, null]));
    await second;
    expect(store.get(COMMUNITY)?.status.state).toBe('ready');
    // Two more asks during one read coalesce into one more read.
    const third = store.refresh(COMMUNITY);
    void store.refresh(COMMUNITY);
    const fourth = store.refresh(COMMUNITY);
    await vi.waitFor(() => { expect(fetchCatalog).toHaveBeenCalledTimes(3); });
    releases[2]?.(catalogBody(3, [remind]));
    await third;
    await vi.waitFor(() => { expect(fetchCatalog).toHaveBeenCalledTimes(4); });
    releases[3]?.(catalogBody(3, [remind]));
    await fourth;
    expect(fetchCatalog).toHaveBeenCalledTimes(4);
  });

  // Reads are numbered when they start: an older read that answers after a newer one landed is dropped, so its status (the
  // Bot offline, read before it came up) never overwrites the newer one; a read from before clear() lands nowhere.
  it('an older catalog read answering after a newer one is ignored', async () => {
    const f = fakeDeps();
    const releases: ((body: Uint8Array) => void)[] = [];
    const fetchCatalog = vi.fn(() => new Promise<Uint8Array>(resolve => { releases.push(resolve); }));
    const store = new BotCatalogStore({ ...f.deps, fetchCatalog });
    const older = store.refresh(COMMUNITY);
    await vi.waitFor(() => { expect(fetchCatalog).toHaveBeenCalledTimes(1); });
    store.clear(); // sign-out and back in: the next read does not wait for the one in flight
    const newer = store.refresh(COMMUNITY);
    await vi.waitFor(() => { expect(fetchCatalog).toHaveBeenCalledTimes(2); });
    releases[1]?.(catalogBody(3, [remind], PUBLICATION, [0, null]));
    await newer;
    expect(store.get(COMMUNITY)?.status.state).toBe('ready');
    releases[0]?.(catalogBody(3, [remind], PUBLICATION, [2, 1758659040])); // the older read answers last: offline
    await older;
    expect(store.get(COMMUNITY)?.status.state).toBe('ready');
    // A read in flight across clear() with nothing newer does not bring the cleared catalog back either.
    const stale = store.refresh(COMMUNITY);
    await vi.waitFor(() => { expect(fetchCatalog).toHaveBeenCalledTimes(3); });
    store.clear();
    releases[2]?.(catalogBody(3, [remind]));
    await stale;
    expect(store.get(COMMUNITY)).toBeNull();
  });

  // The same numbering covers the Bot-user lookup a read starts: one still in flight at clear() (sign-out) pins nothing,
  // neither in this store's copy nor in the core store, so the next account's first sight of its Bot is its own.
  it('a Bot-user lookup in flight at clear() does not pin its answer', async () => {
    const before = corePins();
    const after = corePins();
    let core = before;
    const lookups: ((user: Uint8Array) => void)[] = [];
    let answer: Uint8Array | null = null;
    const ingestBotAttestation = vi.fn(() => Promise.resolve());
    const f = fakeDeps({
      ingestBotAttestation,
      botUserId: () => answer === null ? new Promise<Uint8Array>(resolve => { lookups.push(resolve); }) : Promise.resolve(answer),
      pinBotUser: (communityId, user) => core.pinBotUser(communityId, user),
      pinnedBotUser: (communityId) => core.pinnedBotUser(communityId),
    });
    const store = new BotCatalogStore(f.deps);
    const stale = store.refresh(COMMUNITY);
    await vi.waitFor(() => { expect(lookups).toHaveLength(1); });
    store.clear();
    core = after; // the next account's core store
    lookups[0]?.(BOT_USER);
    await stale;
    expect(before.pinBotUser).not.toHaveBeenCalled();
    expect(after.pinBotUser).not.toHaveBeenCalled();
    expect(ingestBotAttestation).not.toHaveBeenCalled();
    // The next account's community has another Bot: it is pinned on first sight, not greyed as a changed Bot.
    const other = fid(0x0c);
    answer = other;
    await store.refresh(COMMUNITY);
    expect(after.pins.get(COMMUNITY)).toEqual(other);
    expect(store.get(COMMUNITY)).toMatchObject({ botUserId: toHex(other), status: { state: 'ready' } });
  });

  it('a push that lands during a catalog read wins over the older read (M7)', async () => {
    let release!: (body: Uint8Array) => void;
    const f = fakeDeps();
    const store = new BotCatalogStore({ ...f.deps, fetchCatalog: () => new Promise(resolve => { release = resolve; }) });
    const reading = store.refresh(COMMUNITY);
    await Promise.resolve();
    store.apply({ communityId: fid(0x0e), revision: 5n, commands: [cv(poll)], publication: PUBLICATION });
    release(catalogBody(4, [remind]));
    await reading;
    expect(store.get(COMMUNITY)?.revision).toBe(5n);
    expect(store.get(COMMUNITY)?.commands.map(c => c.name)).toEqual(['poll create']);
  });

  // The CI shape of a Bot that just came up: a read served before the Bot published (no key, ready) answers after the push
  // that carries the key, while the push's first Bot-user lookup is still in flight. The read keeps the newer push's
  // catalog and key, checked before any pin existed; the lookup's pin then checks the entry that is held, not only the
  // push's own, so the commands do not stay greyed "can't check the Bot yet" for the whole connection.
  it('a read that lands during a push\'s first Bot-user lookup is checked once the pin is written', async () => {
    let release!: (body: Uint8Array) => void;
    let found!: (user: Uint8Array) => void;
    const f = fakeDeps({
      fetchCatalog: () => new Promise(resolve => { release = resolve; }),
      botUserId: () => new Promise(resolve => { found = resolve; }),
    });
    const store = new BotCatalogStore(f.deps);
    const reading = store.refresh(COMMUNITY);
    await Promise.resolve();
    store.apply({ communityId: fid(0x0e), revision: 4n, commands: [cv(remind)], publication: PUBLICATION });
    release(catalogBody(3, [remind], null, [0, null]));
    await reading;
    expect(store.get(COMMUNITY)?.status.state).toBe('unverified'); // no pin yet: nothing to check the key against
    found(BOT_USER);
    await vi.waitFor(() => { expect(store.get(COMMUNITY)?.status.state).toBe('ready'); });
    expect(store.get(COMMUNITY)).toMatchObject({ revision: 4n, keyId: KEY, botUserId: toHex(BOT_USER) });
    expect(store.publication(COMMUNITY)).toEqual(PUBLICATION);
  });

  it('pins the Bot user on first sight; another one later is greyed, never switched to (A, M8)', async () => {
    let bot: Uint8Array | null | 'ambiguous' = BOT_USER;
    const f = fakeDeps({ botUserId: () => Promise.resolve(bot) });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    expect(store.get(COMMUNITY)?.botUserId).toBe(toHex(BOT_USER));
    bot = fid(0x0c);
    await store.reresolve(COMMUNITY);
    expect(store.get(COMMUNITY)).toMatchObject({ botUserId: toHex(BOT_USER), status: { state: 'changed' } });
    expect(store.publication(COMMUNITY)).toBeNull();
    // A read does not adopt it either.
    await store.refresh(COMMUNITY);
    expect(store.get(COMMUNITY)?.status.state).toBe('changed');
    expect(f.verify.mock.calls.every(call => toHex(call[1]) === toHex(BOT_USER))).toBe(true);
  });

  it('refuses a community with more than one Bot account (A)', async () => {
    const f = fakeDeps({ botUserId: () => Promise.resolve('ambiguous') });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    expect(f.verify).not.toHaveBeenCalled();
    expect(store.get(COMMUNITY)?.status.state).toBe('unverified');
    expect(store.publication(COMMUNITY)).toBeNull();
  });

  it('a signer refusal looks the Bot user up again (M8)', async () => {
    const lookups = vi.fn(() => Promise.resolve<Uint8Array | null>(BOT_USER));
    const f = fakeDeps({ botUserId: lookups, verify: vi.fn(() => { throw Object.assign(new Error('E_DEVICE_UNLISTED'), { code: 'E_DEVICE_UNLISTED' }); }) });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    await vi.waitFor(() => expect(lookups).toHaveBeenCalledTimes(2));
    expect(store.get(COMMUNITY)?.status.state).toBe('unverified');
  });
});

describe('task 20b: the Bot device list fetched and pinned, the Bot user pinned in the store', () => {
  const stale = () => Object.assign(new Error('E_DEVICE_LIST_STALE'), { code: 'E_DEVICE_LIST_STALE' });
  const settle = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };

  it('a member who shares no group with the Bot: greyed, then working after one fetch and pin', async () => {
    let pinned = false;
    const verify = vi.fn<BotCatalogDeps['verify']>(() => { if (!pinned) throw stale(); return { keyId: KEY, fingerprint: '0123456789AB' }; });
    const pinBotDeviceList = vi.fn((bot: Uint8Array) => { expect(toHex(bot)).toBe(toHex(BOT_USER)); pinned = true; return Promise.resolve<0 | 1 | 2>(0); });
    const f = fakeDeps({ verify, pinBotDeviceList });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    await vi.waitFor(() => expect(store.get(COMMUNITY)?.status.state).toBe('ready'));
    expect(pinBotDeviceList).toHaveBeenCalledTimes(1);
    expect(store.publication(COMMUNITY)).toEqual(PUBLICATION);
    expect(store.get(COMMUNITY)?.keyId).toEqual(KEY);
  });

  it('fetches at most once per connect per Bot user; a failed fetch keeps it greyed unverified', async () => {
    const verify = vi.fn<BotCatalogDeps['verify']>(() => { throw stale(); });
    const pinBotDeviceList = vi.fn(() => Promise.reject(Object.assign(new Error('E_DEVICE_LIST_STALE'), { code: 'E_DEVICE_LIST_STALE' })));
    const f = fakeDeps({ verify, pinBotDeviceList });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    await settle();
    expect(store.get(COMMUNITY)?.status.state).toBe('unverified');
    // Every later check this connection is local: no second fetch.
    store.reverify(COMMUNITY);
    await store.reresolve(COMMUNITY);
    await store.refresh(COMMUNITY);
    await settle();
    expect(pinBotDeviceList).toHaveBeenCalledTimes(1);
    expect(store.get(COMMUNITY)?.status.state).toBe('unverified');
    // The next connection may fetch once more.
    store.newConnection();
    await store.reresolve(COMMUNITY);
    await settle();
    expect(pinBotDeviceList).toHaveBeenCalledTimes(2);
  });

  it('a pin that is behind (state 2) keeps the commands greyed unverified', async () => {
    const f = fakeDeps({ verify: vi.fn<BotCatalogDeps['verify']>(() => { throw stale(); }), pinBotDeviceList: vi.fn(() => Promise.resolve<0 | 1 | 2>(2)) });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    await settle();
    expect(store.get(COMMUNITY)?.status.state).toBe('unverified');
    expect(store.publication(COMMUNITY)).toBeNull();
  });

  it('a held key change (state 1) greys the commands as changed', async () => {
    let held = false;
    const verify = vi.fn<BotCatalogDeps['verify']>(() => {
      throw held ? Object.assign(new Error('E_UMK_CHANGED'), { code: 'E_UMK_CHANGED' }) : stale();
    });
    const f = fakeDeps({ verify, pinBotDeviceList: vi.fn(() => { held = true; return Promise.resolve<0 | 1 | 2>(1); }) });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    await vi.waitFor(() => expect(store.get(COMMUNITY)?.status.state).toBe('changed'));
    expect(store.publication(COMMUNITY)).toBeNull();
  });

  it('a key change the core already holds (after a reload) greys the commands as changed with no fetch', async () => {
    const pinBotDeviceList = vi.fn(() => Promise.resolve<0 | 1 | 2>(0));
    const f = fakeDeps({ pinBotDeviceList,
      verify: vi.fn<BotCatalogDeps['verify']>(() => { throw Object.assign(new Error('E_UMK_CHANGED'), { code: 'E_UMK_CHANGED' }); }) });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    await settle();
    expect(store.get(COMMUNITY)?.status.state).toBe('changed');
    expect(pinBotDeviceList).not.toHaveBeenCalled();
  });

  it('the Bot user pin is the core store’s: a different Bot user after a reload is greyed changed, never adopted', async () => {
    const pins = new Map<string, Uint8Array>();
    const first = fakeDeps({}, pins);
    const before = new BotCatalogStore(first.deps);
    await before.syncOnConnect(COMMUNITY);
    expect(before.get(COMMUNITY)?.status.state).toBe('ready');
    expect(toHex(pins.get(COMMUNITY)!)).toBe(toHex(BOT_USER));
    // Reload: a new store (the worker's memory is gone), the same core store, and the server now names another account.
    const other = fid(0x0c);
    const after = fakeDeps({ botUserId: () => Promise.resolve(other) }, pins);
    const reloaded = new BotCatalogStore(after.deps);
    await reloaded.syncOnConnect(COMMUNITY);
    expect(reloaded.get(COMMUNITY)).toMatchObject({ botUserId: toHex(BOT_USER), status: { state: 'changed' } });
    expect(reloaded.publication(COMMUNITY)).toBeNull();
    expect(after.verify.mock.calls.every(call => toHex(call[1]) === toHex(BOT_USER))).toBe(true);
    expect(toHex(pins.get(COMMUNITY)!)).toBe(toHex(BOT_USER));
  });

  it('after a reload the stored pin is used before the server is asked again', async () => {
    const pins = new Map<string, Uint8Array>([[COMMUNITY, BOT_USER]]);
    const f = fakeDeps({ botUserId: () => Promise.resolve(null) }, pins);
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    expect(store.get(COMMUNITY)).toMatchObject({ botUserId: toHex(BOT_USER), status: { state: 'ready' } });
  });
});

// Task 20b review I1 (Task 21b): the community pin is written only for an account the core holds a Bot attestation for. The
// client reads the candidate's device-list head and ingests its attestation first; the core refuses a pin with none
// (E_DEVICE_LIST_STALE) and a human (E_CREDENTIAL). A refused pin leaves the commands greyed unverified, and is tried again
// at most once per connection.
describe('task 21b: the Bot user is pinned only after its Bot attestation', () => {
  const HUMAN = fid(0x0c);
  const settle = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
  /** The core as the I1 fix makes it: attestations by user, and a pin that refuses an account without a Bot one. */
  function attestingCore(server: Map<string, 'bot' | 'human'>) {
    const held = new Map<string, 'bot' | 'human'>();
    const pins = new Map<string, Uint8Array>();
    const order: string[] = [];
    const ingestBotAttestation = vi.fn((user: Uint8Array) => {
      order.push(`ingest ${toHex(user)}`);
      const kind = server.get(toHex(user));
      if (kind !== undefined) held.set(toHex(user), kind);
      return Promise.resolve();
    });
    const pinBotUser = vi.fn((communityId: string, user: Uint8Array): { state: 0 | 1 | 2; pinned: Uint8Array } => {
      order.push(`pin ${toHex(user)}`);
      // fef388bd: refused before anything is read or written, also with a pin held.
      const kind = held.get(toHex(user));
      if (kind === undefined) throw Object.assign(new Error('E_DEVICE_LIST_STALE'), { code: 'E_DEVICE_LIST_STALE' });
      if (kind === 'human') throw Object.assign(new Error('E_CREDENTIAL'), { code: 'E_CREDENTIAL' });
      const prior = pins.get(communityId);
      if (prior !== undefined) return { state: toHex(prior) === toHex(user) ? 1 : 2, pinned: prior };
      pins.set(communityId, user);
      return { state: 0, pinned: user };
    });
    return { pins, held, order, ingestBotAttestation, pinBotUser, pinnedBotUser: (communityId: string) => pins.get(communityId) ?? null };
  }

  it('a human the server names as the Bot is never pinned, and the real Bot is pinned on a later connection', async () => {
    const server = new Map<string, 'bot' | 'human'>([[toHex(HUMAN), 'human'], [toHex(BOT_USER), 'bot']]);
    const core = attestingCore(server);
    let named = HUMAN;
    const f = fakeDeps({ ...core, botUserId: () => Promise.resolve(named) });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    await settle();
    expect(core.order).toEqual([`ingest ${toHex(HUMAN)}`, `pin ${toHex(HUMAN)}`]);
    expect(core.pins.size).toBe(0);
    expect(store.get(COMMUNITY)?.status.state).toBe('unverified');
    expect(store.publication(COMMUNITY)).toBeNull();
    // Task 23 fix round 1: within the backoff a reresolve asks nothing; a catalog read tries the pin again, and the core
    // refuses the human again, so nothing is pinned.
    await store.reresolve(COMMUNITY);
    expect(core.pinBotUser).toHaveBeenCalledTimes(1);
    await store.refresh(COMMUNITY);
    await settle();
    expect(core.pinBotUser).toHaveBeenCalledTimes(2);
    expect(core.pins.size).toBe(0);
    expect(store.publication(COMMUNITY)).toBeNull();
    // The server names the real Bot: on the next connection it is pinned and its key is used.
    named = BOT_USER;
    store.newConnection();
    await store.reresolve(COMMUNITY);
    await settle();
    expect(core.pins.get(COMMUNITY)).toEqual(BOT_USER);
    expect(store.get(COMMUNITY)?.status.state).toBe('ready');
    expect(store.publication(COMMUNITY)).toEqual(PUBLICATION);
  });

  it('the real Bot with no attestation yet stays unverified, and is pinned once its attestation arrives', async () => {
    const server = new Map<string, 'bot' | 'human'>();
    const core = attestingCore(server);
    const f = fakeDeps({ ...core });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    await settle();
    expect(core.pins.size).toBe(0);
    expect(store.get(COMMUNITY)?.status.state).toBe('unverified');
    expect(f.verify).not.toHaveBeenCalled();
    server.set(toHex(BOT_USER), 'bot');
    store.newConnection();
    await store.reresolve(COMMUNITY);
    await settle();
    expect(core.order.slice(-2)).toEqual([`ingest ${toHex(BOT_USER)}`, `pin ${toHex(BOT_USER)}`]);
    expect(core.pins.get(COMMUNITY)).toEqual(BOT_USER);
    expect(store.get(COMMUNITY)?.status.state).toBe('ready');
  });

  it('a Bot already pinned and attested is compared with no device-list read', async () => {
    const core = attestingCore(new Map([[toHex(BOT_USER), 'bot']]));
    core.pins.set(COMMUNITY, BOT_USER);
    core.held.set(toHex(BOT_USER), 'bot');
    const f = fakeDeps({ ...core });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    expect(core.ingestBotAttestation).not.toHaveBeenCalled();
    expect(store.get(COMMUNITY)?.status.state).toBe('ready');
  });

  it('a held pin whose attestation the core lacks reads it once, then compares', async () => {
    const core = attestingCore(new Map([[toHex(BOT_USER), 'bot']]));
    core.pins.set(COMMUNITY, BOT_USER);
    const f = fakeDeps({ ...core });
    const store = new BotCatalogStore(f.deps);
    await store.syncOnConnect(COMMUNITY);
    await settle();
    expect(core.order).toEqual([`pin ${toHex(BOT_USER)}`, `ingest ${toHex(BOT_USER)}`, `pin ${toHex(BOT_USER)}`]);
    expect(store.get(COMMUNITY)?.status.state).toBe('ready');
  });
});

// Task 21b fix round 1, review M6: a transient failure to read the candidate's head is not a refusal. It leaves the pin to
// be tried again within the connection, and the trust refresh that ingests the Bot's attestation retries it at once.
describe('task 21b fix round 1: a transient head-read failure does not grey the Bot for the connection', () => {
  const settle = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
  function core(server: Map<string, 'bot' | 'human'>, failReads: { n: number }) {
    const held = new Map<string, 'bot' | 'human'>();
    const pins = new Map<string, Uint8Array>();
    return {
      pins, held,
      ingestBotAttestation: vi.fn((user: Uint8Array) => {
        if (failReads.n > 0) { failReads.n--; return Promise.reject(Object.assign(new Error('E_UNAVAILABLE'), { code: 'E_UNAVAILABLE' })); }
        const kind = server.get(toHex(user));
        if (kind !== undefined) held.set(toHex(user), kind);
        return Promise.resolve();
      }),
      pinBotUser: vi.fn((communityId: string, user: Uint8Array): { state: 0 | 1 | 2; pinned: Uint8Array } => {
        const kind = held.get(toHex(user));
        if (kind === undefined) throw Object.assign(new Error('E_DEVICE_LIST_STALE'), { code: 'E_DEVICE_LIST_STALE' });
        if (kind === 'human') throw Object.assign(new Error('E_CREDENTIAL'), { code: 'E_CREDENTIAL' });
        const prior = pins.get(communityId);
        if (prior !== undefined) return { state: toHex(prior) === toHex(user) ? 1 : 2, pinned: prior };
        pins.set(communityId, user);
        return { state: 0, pinned: user };
      }),
      pinnedBotUser: (communityId: string) => pins.get(communityId) ?? null,
    };
  }

  it('a head read that fails once is tried again on the next sync within the connection', async () => {
    const c = core(new Map([[toHex(BOT_USER), 'bot']]), { n: 1 });
    const store = new BotCatalogStore(fakeDeps({ ...c }).deps);
    await store.syncOnConnect(COMMUNITY);
    await settle();
    expect(c.pins.size).toBe(0);
    expect(store.get(COMMUNITY)?.status.state).toBe('unverified');
    await store.refresh(COMMUNITY);
    await settle();
    expect(c.pins.get(COMMUNITY)).toEqual(BOT_USER);
    expect(store.get(COMMUNITY)?.status.state).toBe('ready');
  });

  it('a refusal after the head was read is retried, and the trust refresh that brings the Bot attestation pins it', async () => {
    const server = new Map<string, 'bot' | 'human'>();
    const c = core(server, { n: 0 });
    const store = new BotCatalogStore(fakeDeps({ ...c }).deps);
    await store.syncOnConnect(COMMUNITY);
    await settle();
    expect(c.pins.size).toBe(0);
    await store.refresh(COMMUNITY);
    await settle();
    // Task 23 fix round 1: the catalog read tried the pin again (refused again: no attestation yet).
    expect(c.ingestBotAttestation).toHaveBeenCalledTimes(2);
    expect(c.pins.size).toBe(0);
    // The trust refresh ingested the Bot's attestation (from a leaf): the pin is tried again at once.
    c.held.set(toHex(BOT_USER), 'bot');
    await store.attestationsChanged();
    await settle();
    expect(c.pins.get(COMMUNITY)).toEqual(BOT_USER);
    expect(store.get(COMMUNITY)?.status.state).toBe('ready');
  });
});

// Bot v2 Task 23 fix round 1: a refused first pin (the Bot's attestation not on the instance yet: E_DEVICE_LIST_STALE)
// no longer sticks for the connection. A newer op-61 revision or a catalog read tries it again, and so does a timer after
// a few seconds' backoff, so the picker's source tag leaves the fallback name once the attestation arrives.
describe('task 23 fix round 1: a refused first pin is tried again', () => {
  const settle = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
  function attestationLater() {
    const held = new Set<string>();
    const pins = new Map<string, Uint8Array>();
    let attested = false;
    return {
      pins, attest: () => { attested = true; },
      ingestBotAttestation: vi.fn((user: Uint8Array) => { if (attested) held.add(toHex(user)); return Promise.resolve(); }),
      pinBotUser: vi.fn((communityId: string, user: Uint8Array): { state: 0 | 1 | 2; pinned: Uint8Array } => {
        if (!held.has(toHex(user))) throw Object.assign(new Error('E_DEVICE_LIST_STALE'), { code: 'E_DEVICE_LIST_STALE' });
        pins.set(communityId, user);
        return { state: 0, pinned: user };
      }),
      pinnedBotUser: (communityId: string) => pins.get(communityId) ?? null,
    };
  }

  it('a catalog read after the attestation arrived pins the Bot', async () => {
    const c = attestationLater();
    const store = new BotCatalogStore(fakeDeps({ ...c }).deps);
    await store.syncOnConnect(COMMUNITY);
    await settle();
    expect(store.get(COMMUNITY)?.botUserId).toBeNull();
    c.attest();
    await store.refresh(COMMUNITY);
    await settle();
    expect(store.get(COMMUNITY)?.botUserId).toBe(toHex(BOT_USER));
  });

  it('a newer op-61 revision tries the refused pin again', async () => {
    const c = attestationLater();
    const store = new BotCatalogStore(fakeDeps({ ...c }).deps);
    await store.syncOnConnect(COMMUNITY);
    await settle();
    c.attest();
    store.apply({ communityId: fid(0x0e), revision: 9n, commands: [cv(remind)], publication: PUBLICATION });
    await settle();
    expect(store.get(COMMUNITY)?.botUserId).toBe(toHex(BOT_USER));
    expect(store.get(COMMUNITY)?.status.state).toBe('ready');
  });

  it('with nothing else happening, the pin is tried again after a backoff', async () => {
    vi.useFakeTimers();
    try {
      const c = attestationLater();
      const store = new BotCatalogStore(fakeDeps({ ...c }).deps);
      await store.syncOnConnect(COMMUNITY);
      await settle();
      c.attest();
      expect(store.get(COMMUNITY)?.botUserId).toBeNull();
      await vi.advanceTimersByTimeAsync(10_000);
      await settle();
      expect(store.get(COMMUNITY)?.botUserId).toBe(toHex(BOT_USER));
    } finally { vi.useRealTimers(); }
  });
});
