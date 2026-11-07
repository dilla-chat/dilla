import { describe, expect, it, vi } from 'vitest';
import { decode, encode, type CborInput, type CborValue } from '../cbor';
import { toHex } from '../hex';
import { BotCatalogStore, decodeBotCatalogResponse, decodeBotChannels, decodeBotRevisions, type BotCatalogDeps } from './catalog';

const fid = (byte: number): Uint8Array => new Uint8Array(16).fill(byte);
/** A command as a decoded frame carries it (bigints). */
const cv = (value: CborInput): CborValue => decode(encode(value));
const COMMUNITY = toHex(fid(0x0e));
const BOT_USER = fid(0x0b);
const PUBLICATION = new Uint8Array([1, 2, 3]);
const KEY = fid(0x0a);

const remind: CborInput = ['remind', 'Set a reminder', 80, [['when', 4, 1], ['text', 0, 1]], 0, 0, 0];
const play: CborInput = ['play', 'Play a song', 12, [['query', 0, 1]], 0, 0, 0];
const poll: CborInput = ['poll create', 'Ask the channel', 70, [['question', 0, 1], ['channel', 3, 0]], 0, 1, 0];

/** GET …/bot/catalog: [revision, commands, publication | null, [state, since | null]]. */
const catalogBody = (revision: number, commands: CborInput[], publication: Uint8Array | null = PUBLICATION, status: CborInput = [0, null]) =>
  encode([revision, commands, publication, status]);

function fakeDeps(over: Partial<BotCatalogDeps> = {}) {
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
    const next = new Uint8Array([9, 9]);
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
    // The commands stay (greyed, not hidden), but there is no key to seal to and the state is the offline reason.
    expect(catalog.commands.map(c => c.name)).toEqual(['remind', 'play']);
    expect(catalog.keyId).toBeNull();
    expect(catalog.fingerprint).toBeNull();
    expect(catalog.status.state).toBe('offline');
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
