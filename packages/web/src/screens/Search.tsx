import { useEffect, useRef, useState } from 'react';
import { SearchPalette } from '@dilla/ui';
import { usePhoneLayout } from '../layout.ts';
import { t } from '../strings/index.ts';
import type { PaletteGroup } from './palette.ts';

export interface SearchHitView { id: string; channelId: string; communityId: string | null; seq: string; snippet: string; location: string; author?: string; ts?: number }
export interface SearchProps {
  open: boolean; query: string;
  /** The result groups (palette.ts providers: Channels, People, Messages, Actions, …), in order. */
  groups: readonly PaletteGroup[];
  onQuery: (query: string) => void; onClose: () => void;
  /** The page still waits for the worker's answer to its newest message search. */
  loading?: boolean;
}

const TEXTBOX = 'input[type="text"]';

/**
 * The search overlay (W-06, J1, Signal §5.8). On a desktop it is the palette's own modal <dialog> (`SearchPalette modal`, R-S4): a
 * centred overlay over the shell, which stays mounted and goes inert, so Tab cannot reach the covered rail, list or composer. Escape
 * closes it from anywhere in the palette, never during IME composition; focus returns to where search was opened. On a phone
 * it is a <dialog> that follows the visual viewport (`.dw-search`, web3.7.2) around the inline palette, 12px in over the scrim.
 * Choosing a result runs it; one that is no longer there leaves the palette open and says so.
 */
export function Search(p: SearchProps): React.JSX.Element | null {
  const phone = usePhoneLayout();
  const [unavailable, setUnavailable] = useState(false);
  const root = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = root.current;
    if (!p.open || dialog === null) return;
    if (!dialog.open) dialog.showModal();
    dialog.querySelector<HTMLInputElement>(TEXTBOX)?.focus();
  }, [p.open, phone]);
  // The docs shoot names the open palette with message results (the desktop dialog belongs to the component, so it is marked here).
  const marked = p.open && p.groups.some(g => g.id === 'messages' && g.items.length > 0);
  useEffect(() => {
    if (!marked) return;
    const el = phone ? root.current : document.querySelector<HTMLElement>('dialog.d-search-palette-dialog');
    el?.setAttribute('data-doc-state', 'search-results');
    return () => el?.removeAttribute('data-doc-state');
  }, [marked, phone]);
  if (!p.open) return null;
  const pick = async (id: string) => {
    const item = p.groups.flatMap(g => g.items).find(row => row.id === id);
    if (!item) return;
    if (await item.run()) p.onClose();
    else { setUnavailable(true); document.querySelector<HTMLInputElement>(`.d-search-palette ${TEXTBOX}`)?.focus(); }
  };
  const query = p.query.trim();
  const palette = <SearchPalette open modal={!phone} query={p.query} groups={p.groups} onQuery={value => { setUnavailable(false); p.onQuery(value); }}
    onPick={id => { void pick(id); }} onClose={p.onClose}
    status={unavailable ? t('search.unavailable') : undefined}
    labels={{ search: t('search.placeholder'), placeholder: t('search.placeholder'),
      empty: query === '' ? '' : p.loading === true && query.length >= 2 ? t('search.loading') : t('search.empty', { query }),
      close: t('dialog.close'), hints: { move: t('layout.search.hint.move'), open: t('layout.search.hint.open') }, note: t('search.note') }} />;
  if (!phone) return palette;
  return <dialog ref={root} className="dw-search" aria-label={t('search.placeholder')}
    onCancel={event => { event.preventDefault(); p.onClose(); }}>{palette}</dialog>;
}
