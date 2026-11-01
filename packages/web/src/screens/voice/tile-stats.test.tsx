import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, render, renderHook } from '@testing-library/react';
import type { CallTileProps } from '@dilla/ui';
import type { TileStats } from '../../call/session.ts';
import { codecStat, frameStat, TILE_STATS_EVERY_MS, useTileStats } from './tile-stats.ts';
import { withTileStats } from './VoiceView.tsx';

const STATS: TileStats = {
  ownAudio: { codec: 'opus', clockRate: 48000 },
  ownScreen: { height: 1080, fps: 30, audio: true },
  screens: { bobDev: { height: 1440, fps: 30 } },
};

describe('tile stats: the prototype’s formats', () => {
  it('the own microphone is "opus 48k"; a screen "1440p · 30 fps", with "· audio" when its sound goes too', () => {
    expect(codecStat(STATS.ownAudio)).toBe('opus 48k');
    expect(frameStat({ height: 1440, fps: 30 })).toBe('1440p · 30 fps');
    expect(frameStat({ height: 1080, fps: 30, audio: true })).toBe('1080p · 30 fps · audio');
    expect(codecStat(null)).toBeUndefined();
    expect(frameStat(undefined)).toBeUndefined();
  });

  it('each tile gets its own stream’s stat; another person’s tile gets none (no ping is known), and no placeholder', () => {
    const tile = (id: string, own: boolean, source: CallTileProps['source'], deviceId: string) =>
      ({ id, own, source, view: { deviceId } }) as unknown as CallTileProps;
    const tiles = [tile('me:person', true, 'avatar', 'me'), tile('me:screen', true, 'screen', 'me'),
      tile('bobDev:person', false, 'camera', 'bobDev'), tile('bobDev:screen', false, 'screen', 'bobDev'), tile('carl:screen', false, 'screen', 'carl')];
    expect(withTileStats(tiles, STATS).map(t => t.stat)).toEqual(['opus 48k', '1080p · 30 fps · audio', undefined, '1440p · 30 fps', undefined]);
    expect(withTileStats(tiles, null).map(t => t.stat)).toEqual([undefined, undefined, undefined, undefined, undefined]);
  });
});

describe('useTileStats: the sampling lifecycle', () => {
  beforeEach(() => { vi.useFakeTimers(); });
  afterEach(() => { vi.useRealTimers(); });
  const flush = async () => { await act(async () => { await Promise.resolve(); await Promise.resolve(); }); };

  it('reads at once and then about once a second while on; nothing is left pending after unmount', async () => {
    const session = { tileStats: vi.fn(() => Promise.resolve(STATS)) };
    const { result, unmount } = renderHook(() => useTileStats(session, true));
    await flush();
    expect(session.tileStats).toHaveBeenCalledTimes(1);
    expect(result.current).toEqual(STATS);
    await act(async () => { await vi.advanceTimersByTimeAsync(TILE_STATS_EVERY_MS); });
    expect(session.tileStats).toHaveBeenCalledTimes(2);
    await act(async () => { await vi.advanceTimersByTimeAsync(TILE_STATS_EVERY_MS * 3); });
    expect(session.tileStats).toHaveBeenCalledTimes(5);
    unmount();
    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(TILE_STATS_EVERY_MS * 5);
    expect(session.tileStats).toHaveBeenCalledTimes(5);
  });

  it('off (the stage hidden, a phone, a pause) reads nothing and clears the last read', async () => {
    const session = { tileStats: vi.fn(() => Promise.resolve(STATS)) };
    const { result, rerender } = renderHook(({ on }) => useTileStats(session, on), { initialProps: { on: true } });
    await flush();
    expect(result.current).toEqual(STATS);
    rerender({ on: false });
    expect(result.current).toBeNull();
    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(TILE_STATS_EVERY_MS * 3);
    expect(session.tileStats).toHaveBeenCalledTimes(1);
  });

  it('a hidden page reads nothing until it shows again', async () => {
    let state: DocumentVisibilityState = 'visible';
    const spy = vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => state);
    const session = { tileStats: vi.fn(() => Promise.resolve(STATS)) };
    renderHook(() => useTileStats(session, true));
    await flush();
    state = 'hidden';
    act(() => { document.dispatchEvent(new Event('visibilitychange')); });
    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(TILE_STATS_EVERY_MS * 3);
    expect(session.tileStats).toHaveBeenCalledTimes(1);
    state = 'visible';
    act(() => { document.dispatchEvent(new Event('visibilitychange')); });
    await flush();
    expect(session.tileStats).toHaveBeenCalledTimes(2);
    spy.mockRestore();
  });

  it('a component that unmounts while a read is in flight sets nothing and schedules nothing', async () => {
    let resolve: (s: TileStats) => void = () => undefined;
    const session = { tileStats: vi.fn(() => new Promise<TileStats>(r => { resolve = r; })) };
    function Probe() { useTileStats(session, true); return null; }
    const { unmount } = render(<Probe />);
    unmount();
    resolve(STATS);
    await flush();
    expect(vi.getTimerCount()).toBe(0);
  });
});
