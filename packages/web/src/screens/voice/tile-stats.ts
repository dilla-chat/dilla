// Signal task 8c (README §5.6, the prototype's tile stats): the stage's per-tile stats, sampled about once a second while the
// stage of this device's call is on screen and the page is visible, and formatted as the prototype writes them.
import { useEffect, useState } from 'react';
import type { PageCall, TileStats } from '../../call/session.ts';
import { t } from '../../strings/index.ts';

/** About once a second (the brief's ~1 Hz). */
export const TILE_STATS_EVERY_MS = 1_000;

/**
 * The latest stats of `session`, read every `everyMs` while `on` and the page is visible; null otherwise. Nothing is read while
 * off or hidden, and no timer outlives the unmount or a change of `session` or `on` (one pending timeout at most, cleared).
 */
export function useTileStats(session: Pick<PageCall, 'tileStats'> | null, on: boolean, everyMs = TILE_STATS_EVERY_MS): TileStats | null {
  const [stats, setStats] = useState<TileStats | null>(null);
  useEffect(() => {
    if (session === null || !on) { setStats(null); return; }
    let timer: ReturnType<typeof setTimeout> | null = null;
    let live = true;
    let busy = false;
    const visible = () => typeof document === 'undefined' || document.visibilityState !== 'hidden';
    const tick = () => {
      timer = null;
      if (!live || busy || !visible()) return;
      busy = true;
      void session.tileStats().then(s => { if (live) setStats(s); }, () => undefined).finally(() => {
        busy = false;
        if (live && visible() && timer === null) timer = setTimeout(tick, everyMs);
      });
    };
    // Hidden: the pending read is dropped (nothing is read while hidden); visible again: a read at once.
    const onVisibility = () => {
      if (!visible()) { if (timer !== null) { clearTimeout(timer); timer = null; } return; }
      if (timer === null) tick();
    };
    tick();
    document.addEventListener('visibilitychange', onVisibility);
    return () => {
      live = false;
      if (timer !== null) clearTimeout(timer);
      timer = null;
      document.removeEventListener('visibilitychange', onVisibility);
    };
  }, [session, on, everyMs]);
  return stats;
}

/** The own microphone as the prototype writes it: "opus 48k" (the codec, its clock rate in kHz). */
export function codecStat(a: TileStats['ownAudio']): string | undefined {
  return a === null ? undefined : t('callStage.stat.codec', { codec: a.codec, khz: Math.round(a.clockRate / 1000) });
}

/** A screen as the prototype writes it: "1440p · 30 fps", and "· audio" when its sound goes with it. */
export function frameStat(f: { height: number; fps: number; audio?: boolean } | null | undefined): string | undefined {
  if (f == null) return undefined;
  return t(f.audio === true ? 'callStage.stat.frameAudio' : 'callStage.stat.frame', { height: f.height, fps: f.fps });
}
