import { describe, expect, it, vi } from 'vitest';
import { toHex } from '../hex';
import { COMMUNITY, world, toReady } from './world';

const ATTEMPT = new Uint8Array(16).fill(0x6a);
const STATE = 's'.repeat(43);

describe('bot Spotify worker commands', () => {
  it('bridges begin, callback and cancel without returning broker secrets', async () => {
    const w = world();
    const begin = vi.fn(() => Promise.resolve({ attemptId: ATTEMPT, authorizeUrl: 'https://accounts.spotify.com/authorize?...', expires: 1_800_000_000n }));
    const callback = vi.fn(() => Promise.resolve());
    const cancel = vi.fn(() => Promise.resolve());
    Object.assign(w.routes, { beginBotSpotifyAttempt: begin, callbackBotSpotifyAttempt: callback, cancelBotSpotifyAttempt: cancel });
    await toReady(w);
    w.call(10, { m: 'beginBotSpotifyAttempt', communityId: toHex(COMMUNITY), stepUp: 'step-up', clientId: 'client' });
    await vi.waitFor(() => expect(w.ret(10)).toEqual({ t: 'ret', id: 10, ok: true, value: {
      attemptId: toHex(ATTEMPT), authorizeUrl: 'https://accounts.spotify.com/authorize?...', expires: '1800000000',
    } }));
    expect(begin).toHaveBeenCalledWith(COMMUNITY, 'step-up', 'client');
    w.call(11, { m: 'callbackBotSpotifyAttempt', communityId: toHex(COMMUNITY), attemptId: toHex(ATTEMPT), state: STATE, code: 'oauth-code' });
    await vi.waitFor(() => expect(w.ret(11)).toMatchObject({ ok: true, value: null }));
    expect(callback).toHaveBeenCalledWith(COMMUNITY, ATTEMPT, STATE, 'oauth-code');
    w.call(12, { m: 'cancelBotSpotifyAttempt', communityId: toHex(COMMUNITY), attemptId: toHex(ATTEMPT) });
    await vi.waitFor(() => expect(w.ret(12)).toMatchObject({ ok: true, value: null }));
    expect(cancel).toHaveBeenCalledWith(COMMUNITY, ATTEMPT);
  });

  it('refuses out-of-phase and malformed commands before reaching a route', async () => {
    const early = world();
    const begin = vi.fn();
    Object.assign(early.routes, { beginBotSpotifyAttempt: begin });
    early.call(1, { m: 'beginBotSpotifyAttempt', communityId: toHex(COMMUNITY), stepUp: 'step-up', clientId: 'client' });
    await vi.waitFor(() => expect(early.ret(1)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } }));
    expect(begin).not.toHaveBeenCalled();

    const w = world();
    const start = vi.fn();
    const callback = vi.fn();
    Object.assign(w.routes, { beginBotSpotifyAttempt: start, callbackBotSpotifyAttempt: callback });
    await toReady(w);
    w.call(2, { m: 'beginBotSpotifyAttempt', communityId: toHex(COMMUNITY), stepUp: '', clientId: 'client' });
    w.call(3, { m: 'beginBotSpotifyAttempt', communityId: toHex(COMMUNITY), stepUp: 'x'.repeat(4097), clientId: 'client' });
    w.call(4, { m: 'callbackBotSpotifyAttempt', communityId: toHex(COMMUNITY), attemptId: toHex(ATTEMPT), state: '', code: 'oauth-code' });
    w.call(5, { m: 'callbackBotSpotifyAttempt', communityId: toHex(COMMUNITY), attemptId: toHex(ATTEMPT), state: STATE, code: 'x'.repeat(2049) });
    await vi.waitFor(() => expect(w.ret(5)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } }));
    for (const id of [2, 3, 4, 5]) expect(w.ret(id)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
    expect(start).not.toHaveBeenCalled();
    expect(callback).not.toHaveBeenCalled();
  });
});
