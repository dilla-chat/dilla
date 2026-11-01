// The GIF picker over the worker (E8, flow 16). The page never holds the session and never contacts Giphy: it asks the
// worker to search and to fetch for this server, shows the previews from page-minted blob URLs, and a pick becomes an
// ordinary file in the tray (attachFiles), so it is encrypted like any file.
import { useCallback, useEffect, useRef, useState } from 'react';
import { GifPicker, type GifPickerItem, type GifPickerStatus } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { errorOf } from '../../core/errors.ts';
import { t } from '../../strings/index.ts';
import { attachFiles } from './Attachments.tsx';
import type { ConversationAction } from './Conversation.tsx';

/** A typed word is searched this long after the last key. */
export const GIF_SEARCH_DEBOUNCE_MS = 250;
/** Previews fetched at once: the instance lets one member hold three fetches, and a search or a pick needs one. */
export const GIF_PREVIEWS_AT_ONCE = 2;
/** A preview the instance was too busy to give is asked for again after this, doubled each time, up to the tries below. */
export const GIF_RETRY_BASE_MS = 400;
export const GIF_PREVIEW_TRIES = 3;

interface Found { id: string; title: string; width: number; height: number }
interface Page { items: Found[]; next: number | null }

function isPage(v: unknown): v is Page {
  if (typeof v !== 'object' || v === null) return false;
  const o = v as Record<string, unknown>;
  return Array.isArray(o.items) && (o.next === null || typeof o.next === 'number');
}
function isBlobResult(v: unknown): v is { blob: Blob } {
  return typeof v === 'object' && v !== null && (v as Record<string, unknown>).blob instanceof Blob;
}
/** A refusal worth asking again after: the instance was busy or limited. */
const busy = (e: unknown): boolean => { const s = errorOf(e).status; return s === 503 || s === 429; };

export interface GifPickerPanelProps {
  id: string;
  communityId: string;
  initialQuery?: string;
  channelId: string;
  dispatch(a: ConversationAction): void;
  /** Speaks a line once, politely (the shell's announcer). */
  announce?: (text: string) => void;
  onClose(reason: 'escape' | 'outside' | 'picked'): void;
}

export function GifPickerPanel(p: GifPickerPanelProps): React.JSX.Element {
  const client = useCore();
  const { communityId } = p;
  const [query, setQuery] = useState(p.initialQuery ?? '');
  const [items, setItems] = useState<Found[]>([]);
  const [next, setNext] = useState<number | null>(null);
  const [status, setStatus] = useState<GifPickerStatus | null>({ kind: 'loading', text: t('gif.loading') });
  const [srcs, setSrcs] = useState<Readonly<Record<string, string>>>({});
  const [picking, setPicking] = useState<string | null>(null);
  const alive = useRef(true);
  const seq = useRef(0);
  const urls = useRef(new Map<string, string>());
  const wanted = useRef(new Set<string>());
  const queue = useRef<string[]>([]);
  const tries = useRef(new Map<string, number>());
  const timers = useRef(new Set<number>());
  const inFlight = useRef(0);
  const first = useRef(true);

  useEffect(() => {
    alive.current = true;
    const minted = urls.current;
    const pending = timers.current;
    return () => {
      alive.current = false;
      for (const url of minted.values()) URL.revokeObjectURL(url);
      minted.clear();
      for (const handle of pending) window.clearTimeout(handle);
      pending.clear();
    };
  }, []);

  const pump = useCallback(() => {
    while (alive.current && inFlight.current < GIF_PREVIEWS_AT_ONCE) {
      const gifId = queue.current.shift();
      if (gifId === undefined) return;
      inFlight.current += 1;
      const attempt = (tries.current.get(gifId) ?? 0) + 1;
      tries.current.set(gifId, attempt);
      void client.call({ m: 'gifFetch', communityId, gifId, variant: 'preview' }).then(result => {
        if (!alive.current || !isBlobResult(result)) return;
        const url = URL.createObjectURL(result.blob);
        urls.current.set(gifId, url);
        setSrcs(cur => ({ ...cur, [gifId]: url }));
      }, (e: unknown) => {
        // Busy or limited: ask again after a pause that doubles; any other refusal leaves the tile blank.
        if (!alive.current || !busy(e) || attempt >= GIF_PREVIEW_TRIES) return;
        const handle = window.setTimeout(() => {
          timers.current.delete(handle);
          queue.current.push(gifId);
          pump();
        }, GIF_RETRY_BASE_MS * 2 ** (attempt - 1));
        timers.current.add(handle);
      }).finally(() => {
        inFlight.current -= 1;
        pump();
      });
    }
  }, [client, communityId]);

  const want = useCallback((found: readonly Found[]) => {
    for (const f of found) {
      if (wanted.current.has(f.id)) continue;
      wanted.current.add(f.id);
      queue.current.push(f.id);
    }
    pump();
  }, [pump]);

  const search = useCallback((q: string, offset: number) => {
    const mine = ++seq.current;
    setStatus({ kind: 'loading', text: t('gif.loading') });
    void client.call({ m: 'gifSearch', communityId, q, offset }).then(result => {
      if (!alive.current || mine !== seq.current) return;
      if (!isPage(result)) throw new Error('E_GIF_SHAPE');
      setItems(cur => offset === 0 ? result.items : [...cur, ...result.items.filter(f => !cur.some(c => c.id === f.id))]);
      setNext(result.next);
      setStatus(offset === 0 && result.items.length === 0 ? { kind: 'empty', text: t('gif.empty') } : null);
      want(result.items);
    }).catch(() => {
      if (!alive.current || mine !== seq.current) return;
      setStatus({ kind: 'error', text: t('gif.error') });
    });
  }, [client, communityId, want]);

  // The first search (trending) is at once; a changed query waits for the typing to stop.
  useEffect(() => {
    if (first.current) {
      first.current = false;
      search(query, 0);
      return;
    }
    const handle = window.setTimeout(() => { search(query, 0); }, GIF_SEARCH_DEBOUNCE_MS);
    return () => { window.clearTimeout(handle); };
  }, [query, search]);

  const pick = (gifId: string) => {
    if (picking !== null) return;
    setPicking(gifId);
    setStatus({ kind: 'busy', text: t('gif.picking') });
    const run = async () => {
      try {
        const result = await client.call({ m: 'gifFetch', communityId, gifId, variant: 'full' });
        if (!isBlobResult(result)) throw new Error('E_GIF_SHAPE');
        const file = new File([result.blob], `giphy-${gifId}.gif`, { type: 'image/gif' });
        if (!alive.current) return;
        if (!(await attachFiles(client, p.channelId, [file], a => { p.dispatch(a); }))) throw new Error('E_GIF_ATTACH');
        p.announce?.(t('gif.added'));
        p.onClose('picked');
      } catch {
        if (!alive.current) return;
        setPicking(null);
        setStatus({ kind: 'error', text: t('gif.pickFailed') });
      }
    };
    void run();
  };

  const shown: GifPickerItem[] = items.map(f => ({ ...f, src: srcs[f.id] ?? null }));
  return <GifPicker id={p.id} label={t('gif.picker')} searchLabel={t('gif.search')} searchPlaceholder={t('gif.search.placeholder')}
    query={query} onQuery={setQuery} items={shown} status={status} poweredBy={t('gif.powered')} untitled={t('gif.untitled')}
    moreLabel={t('gif.more')} hasMore={next !== null} onMore={() => { if (next !== null) search(query, next); }}
    pickingId={picking} onPick={pick} onClose={reason => { p.onClose(reason); }} />;
}
