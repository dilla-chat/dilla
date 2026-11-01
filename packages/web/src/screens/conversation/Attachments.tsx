// Page-side attachment cards, downloads, lightbox and upload tray.
import { phoneNow } from '../../layout.ts';
import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { BROWSER_ATTACHMENT_CAP, safeName, type AttachmentSummary, type CoreClient, type TimelineItem, type TrayItem } from '@dilla/client-core';
import { AttachmentCard, AttachmentTray, Lightbox } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { errorOf } from '../../core/errors.ts';
import { en, t } from '../../strings/index.ts';
import { formatSize } from '../shell-model.ts';
import type { ConversationAction } from './Conversation.tsx';
import { useBlobUrl } from './useBlobUrl.tsx';

export const REVOKE_SAVED_AFTER_MS = 10_000;
export interface Opened { blob: Blob; name: string; mime: string }

export function isOpened(v: unknown): v is Opened {
  if (typeof v !== 'object' || v === null) return false;
  const o = v as Record<string, unknown>;
  return o.blob instanceof Blob && typeof o.name === 'string' && typeof o.mime === 'string';
}

export function saveBlob(blob: Blob, name: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  a.rel = 'noopener';
  document.body.append(a);
  a.click();
  a.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), REVOKE_SAVED_AFTER_MS);
}

/**
 * Task 7 (Signal §6.8): the tray's previews. The core names a tray entry only once it is processed, but appends a batch's entries
 * to the slice at once and in order, so the page keys a preview by the order of its files: each attach queues one entry per file
 * (a blob: URL for a still image, a GIF mark for a GIF, nothing otherwise) and the tray hands them to its new ids in that order.
 * A URL is revoked when its entry leaves the tray (removed or sent), when its batch is refused, and when the tray unmounts.
 */
interface PendingPreview { channelId: string; preview: TrayPreview | null }
export interface TrayPreview { url?: string; gif?: boolean }
const pendingPreviews: PendingPreview[] = [];
const trayPreviews = new Map<string, TrayPreview | null>();
const trayMounts = new Map<string, number>();
const previewKey = (channelId: string, trayId: string) => `${channelId}:${trayId}`;
const revoke = (preview: TrayPreview | null | undefined) => { if (preview?.url !== undefined) URL.revokeObjectURL(preview.url); };
function previewOf(file: File): TrayPreview | null {
  if (file.type === 'image/gif') return { gif: true };
  if (file.type.startsWith('image/') && file.type !== 'image/svg+xml' && typeof URL.createObjectURL === 'function') return { url: URL.createObjectURL(file) };
  return null;
}

export async function attachFiles(client: Pick<CoreClient, 'call'>, channelId: string, files: readonly File[], dispatch: (a: ConversationAction) => void): Promise<boolean> {
  if (files.length === 0) return true;
  const queued = files.map((file): PendingPreview => ({ channelId, preview: previewOf(file) }));
  pendingPreviews.push(...queued);
  try {
    await client.call({ m: 'attachFiles', channelId, files });
    dispatch({ type: 'trayError', text: null });
    return true;
  } catch (err) {
    // A refused batch adds nothing to the tray: its previews go.
    for (const q of queued) {
      const at = pendingPreviews.indexOf(q);
      if (at >= 0) { pendingPreviews.splice(at, 1); revoke(q.preview); }
    }
    const code = errorOf(err).code;
    const text = code === 'E_ATTACHMENT_TOO_LARGE'
      ? t('shell.tray.tooLarge', { name: safeName(files.find(f => f.size > BROWSER_ATTACHMENT_CAP)?.name ?? files[0]?.name ?? '', t('shell.attachment.unnamed')) })
      : code === 'E_ATTACHMENT_COUNT' ? t('shell.tray.tooMany') : t('shell.banner.commandError', { code });
    dispatch({ type: 'trayError', text });
    return false;
  }
}

/** A GIF plays inline only up to this size (bytes); a larger one stays a still until it is opened (flow 04). */
export const GIF_AUTOPLAY_MAX = 8 * 1024 * 1024;
/** How long a GIF animates in its card, counted from the first time at least half of the card was visible. */
export const GIF_PLAY_MS = 5 * 60_000;
// First sighting per `${channelId}:${seq}:${index}`, for the page's lifetime: a re-mounted or virtualised card keeps its
// deadline, and a card that scrolls out and back in never restarts the clock.
const gifSeen = new Map<string, number>();

function Card(p: { channelId: string; seq: string | null; summary: AttachmentSummary; tabbable: boolean;
  onOpenImage(index: number, opener: HTMLElement): void; onError(e: unknown): void }) {
  const client = useCore();
  const { summary: a } = p;
  const [thumb, setThumb] = useState<Blob | null>(null);
  const [state, setState] = useState<'idle' | 'loading' | 'failed'>('idle');
  const [code, setCode] = useState('');
  const requested = useRef(new Set<string>());
  const mounted = useRef(false);
  const activeThumb = useRef<string | null>(null);
  const buttonRef = useRef<HTMLButtonElement | null>(null);
  const thumbUrl = useBlobUrl(thumb);
  // The GIF autoplay (flow 04, L-UI-71 stays for audio and video): the full blob replaces the still in the same img slot
  // for GIF_PLAY_MS after the first sighting, then is dropped (useBlobUrl revokes it). A failed open keeps the still.
  const gifOpenable = a.kind === 'image' && a.mime === 'image/gif' && !a.tooLarge && p.seq !== null;
  const gifAuto = gifOpenable && a.size <= GIF_AUTOPLAY_MAX;
  const [full, setFull] = useState<Blob | null>(null);
  const fullUrl = useBlobUrl(full);
  const cardRef = useRef<HTMLDivElement | null>(null);
  // The inline player (flow 06, L-UI-71, R9): nothing is fetched before the person's play; the blob is opened then and
  // shown from a page-minted URL that useBlobUrl revokes on unmount (or when an error drops it). Never autoplayed.
  const playable = (a.kind === 'audio' || a.kind === 'video') && !a.tooLarge && p.seq !== null;
  const [media, setMedia] = useState<Blob | null>(null);
  const [playState, setPlayState] = useState<'idle' | 'loading' | 'error'>('idle');
  const mediaUrl = useBlobUrl(media);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  const play = async () => {
    if (p.seq === null || playState === 'loading') return;
    setPlayState('loading');
    try {
      const result = await client.call({ m: 'openAttachment', channelId: p.channelId, seq: p.seq, index: a.index, thumb: false });
      if (!isOpened(result)) throw new Error('E_BLOB_OPEN');
      if (alive.current) setMedia(result.blob);
    } catch {
      if (alive.current) setPlayState('error');
    }
  };
  const playerState = playState === 'error' ? 'error' : mediaUrl !== null ? 'ready' : playState;
  useEffect(() => {
    if (a.kind !== 'image' || !a.thumb || a.tooLarge || p.seq === null) return;
    const key = `${p.seq}:${a.index}`;
    mounted.current = true;
    activeThumb.current = key;
    if (requested.current.has(key)) return () => { mounted.current = false; };
    requested.current.add(key);
    client.call({ m: 'openAttachment', channelId: p.channelId, seq: p.seq, index: a.index, thumb: true })
      .then(v => { if (mounted.current && activeThumb.current === key && isOpened(v)) setThumb(v.blob); })
      .catch(() => undefined);
    return () => { mounted.current = false; };
  }, [client, p.channelId, p.seq, a.index, a.kind, a.thumb, a.tooLarge]);
  useEffect(() => {
    const seq = p.seq;
    const box = cardRef.current;
    if (!gifAuto || seq === null || box === null || typeof IntersectionObserver === 'undefined') return;
    if (typeof window.matchMedia === 'function' && window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
    const key = `${p.channelId}:${seq}:${a.index}`;
    let cancelled = false;
    let timer: number | undefined;
    let shown = false;
    let turn = 0;
    // Leaving the view drops the blob (useBlobUrl revokes it); coming back before the first-sighting deadline opens it again.
    const leave = () => { shown = false; turn += 1; window.clearTimeout(timer); setFull(null); };
    const watcher = new IntersectionObserver(entries => {
      const last = entries[entries.length - 1] as IntersectionObserverEntry | undefined;
      if (last === undefined) return;
      if (!(last.isIntersecting && last.intersectionRatio >= 0.5)) { if (shown) leave(); return; }
      if (shown) return;
      const now = Date.now();
      const first = gifSeen.get(key) ?? now;
      gifSeen.set(key, first);
      const deadline = first + GIF_PLAY_MS;
      if (deadline - now <= 0) return;
      shown = true;
      const mine = ++turn;
      client.call({ m: 'openAttachment', channelId: p.channelId, seq, index: a.index, thumb: false })
        .then(v => {
          // A late answer (after the deadline, after leaving the view, or after unmount) never flashes the GIF.
          if (cancelled || mine !== turn || !isOpened(v) || Date.now() >= deadline) return;
          setFull(v.blob);
          timer = window.setTimeout(() => setFull(null), deadline - Date.now());
        })
        .catch(() => undefined);
    }, { threshold: 0.5 });
    watcher.observe(box);
    return () => { cancelled = true; watcher.disconnect(); window.clearTimeout(timer); setFull(null); };
  }, [client, p.channelId, p.seq, a.index, gifAuto]);
  const save = async () => {
    if (p.seq === null || a.tooLarge) return;
    setState('loading');
    try {
      const result = await client.call({ m: 'openAttachment', channelId: p.channelId, seq: p.seq, index: a.index, thumb: false });
      if (!isOpened(result)) throw new Error('E_BLOB_OPEN');
      saveBlob(result.blob, result.name || t('shell.attachment.unnamed'));
      setState('idle');
    } catch (e) {
      setCode(errorOf(e).code);
      setState('failed');
    }
  };
  const name = safeName(a.name, t('shell.attachment.unnamed'));
  return <div ref={el => { cardRef.current = el; buttonRef.current = el?.querySelector('button') ?? null; }} data-doc-state={playable ? 'media-player' : undefined}>
    <AttachmentCard kind={a.kind} name={name} size={formatSize(a.size)} thumbUrl={fullUrl ?? thumbUrl} w={a.w} h={a.h}
      openLabel={t('shell.attachment.open', { name })} saveLabel={t('shell.attachment.save', { name })} saveText={t('media.save')}
      {...(a.kind === 'image' && p.seq !== null && !a.tooLarge ? { onOpen: () => { if (buttonRef.current) p.onOpenImage(a.index, buttonRef.current); } } : {})}
      {...(p.seq !== null && !a.tooLarge ? { onSave: () => { void save(); } } : {})}
      state={a.tooLarge ? 'too-large' : state}
      stateText={a.tooLarge ? t('shell.attachment.tooLarge') : state === 'loading' ? t('shell.attachment.opening') : state === 'failed' ? t('shell.attachment.failed', { code }) : undefined}
      tabbable={p.tabbable}
      {...(gifOpenable && fullUrl === null ? { badge: t('shell.attachment.gif') } : {})}
      {...(playable ? { player: {
        src: playerState === 'ready' ? mediaUrl : null, state: playerState,
        labels: { play: en['media.play'], loading: t('media.loading'), error: t('media.error'), save: t('media.save') },
        onPlay: () => { void play(); },
        onError: () => { setMedia(null); setPlayState('error'); },
      } } : {})} />
  </div>;
}

export function AttachmentList(p: { channelId: string; item: TimelineItem; tabbable: boolean;
  onOpenImage(index: number, opener: HTMLElement): void; onError(e: unknown): void }): React.JSX.Element | null {
  if (p.item.attachments.length === 0) return null;
  return <div className="dw-attachments">{p.item.attachments.map(a =>
    <Card key={a.index} channelId={p.channelId} seq={p.item.seq} summary={a} tabbable={p.tabbable}
      onOpenImage={(index, opener) => p.onOpenImage(index, opener)} onError={e => p.onError(e)} />)}</div>;
}

export interface LightboxState { seq: string; images: readonly AttachmentSummary[]; index: number; opener: HTMLElement | null }
export function AttachmentLightbox(p: { channelId: string; state: LightboxState; onIndex(index: number): void; onClose(): void;
  onError(e: unknown): void }): React.JSX.Element {
  const client = useCore();
  const [blob, setBlob] = useState<Blob | null>(null);
  const request = useRef(0);
  const callbacks = useRef(p);
  callbacks.current = p;
  const item = p.state.images[p.state.index];
  const url = useBlobUrl(blob);
  useEffect(() => {
    const id = ++request.current;
    setBlob(null);
    client.call({ m: 'openAttachment', channelId: p.channelId, seq: p.state.seq, index: item.index, thumb: false })
      .then(v => { if (id === request.current && isOpened(v)) setBlob(v.blob); })
      .catch(e => { if (id === request.current) { callbacks.current.onError(e); callbacks.current.onClose(); } });
    return () => { if (request.current === id) request.current = id + 1; };
  }, [client, p.channelId, p.state.seq, item.index]);
  const name = safeName(item.name, t('shell.attachment.unnamed'));
  return <Lightbox open label={t('shell.lightbox.label', { name, size: formatSize(item.size) })} src={url} alt={name}
    closeLabel={t('shell.lightbox.close')} onClose={() => p.onClose()} saveLabel={t('shell.lightbox.save')}
    onSave={() => { if (blob) saveBlob(blob, name); }}
    previous={p.state.index > 0 ? { label: t('shell.lightbox.previous'), onPrevious: () => p.onIndex(p.state.index - 1) } : undefined}
    next={p.state.index + 1 < p.state.images.length ? { label: t('shell.lightbox.next'), onNext: () => p.onIndex(p.state.index + 1) } : undefined} />;
}

/** Each tray entry's preview: a new id takes the next queued preview of its channel (see attachFiles). */
function previewsFor(channelId: string, items: readonly TrayItem[]): (TrayPreview | null)[] {
  return items.map(item => {
    const key = previewKey(channelId, item.id);
    if (!trayPreviews.has(key)) {
      const at = pendingPreviews.findIndex(q => q.channelId === channelId);
      trayPreviews.set(key, at >= 0 ? pendingPreviews.splice(at, 1)[0].preview : null);
    }
    return trayPreviews.get(key) ?? null;
  });
}

export function TrayList(p: { channelId: string; items: readonly TrayItem[]; onError(e: unknown): void }): React.JSX.Element | null {
  const client = useCore();
  const pendingFocus = useRef<number | null>(null);
  const previews = previewsFor(p.channelId, p.items);
  // An entry that left the tray (removed, or sent) gives its URL back.
  useEffect(() => {
    const live = new Set(p.items.map(i => previewKey(p.channelId, i.id)));
    for (const [key, preview] of trayPreviews) {
      if (key.startsWith(`${p.channelId}:`) && !live.has(key)) { revoke(preview); trayPreviews.delete(key); }
    }
  }, [p.channelId, p.items]);
  // Unmounting the tray gives back every URL of its channel (after a tick, so a remount of the same tray keeps them).
  useEffect(() => {
    trayMounts.set(p.channelId, (trayMounts.get(p.channelId) ?? 0) + 1);
    return () => {
      trayMounts.set(p.channelId, (trayMounts.get(p.channelId) ?? 1) - 1);
      window.setTimeout(() => {
        if ((trayMounts.get(p.channelId) ?? 0) > 0) return;
        for (const [key, preview] of trayPreviews) {
          if (key.startsWith(`${p.channelId}:`)) { revoke(preview); trayPreviews.delete(key); }
        }
        for (let i = pendingPreviews.length - 1; i >= 0; i--) {
          if (pendingPreviews[i].channelId === p.channelId) { revoke(pendingPreviews[i].preview); pendingPreviews.splice(i, 1); }
        }
      }, 0);
    };
  }, [p.channelId]);
  useLayoutEffect(() => {
    if (pendingFocus.current === null) return;
    const buttons = document.querySelectorAll<HTMLButtonElement>('.d-attachment-tray__remove');
    (buttons[pendingFocus.current] ?? (phoneNow() ? null : document.querySelector<HTMLTextAreaElement>('.d-composer textarea')))?.focus();
    pendingFocus.current = null;
  }, [p.items]);
  if (p.items.length === 0) return null;
  return <AttachmentTray label={t('shell.tray.label')} removeTip={t('shell.tray.removeTip')} items={p.items.map((a, i) => ({
    ...(previews[i] ? { preview: previews[i] } : {}),
    id: a.id, name: a.name, size: formatSize(a.size),
    phase: a.phase === 'failed' ? t('shell.tray.failed', { code: a.reason }) : t(`shell.tray.${a.phase}`),
    failed: a.phase === 'failed', step: a.phase,
    ...(a.phase === 'failed' ? { hint: t('shell.tray.failedHint') } : {}),
    removeLabel: t('shell.tray.remove', { name: a.name }),
    onRemove: () => { pendingFocus.current = i; client.call({ m: 'discardAttachment', channelId: p.channelId, trayId: a.id }).catch(e => p.onError(e)); },
  }))} />;
}
