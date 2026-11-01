import type { CSSProperties } from 'react';
import { Button } from '../Button/Button.tsx';
import { MediaPlayer, type MediaPlayerProps } from '../MediaPlayer/MediaPlayer.tsx';
import './AttachmentCard.css';
import { Icon } from '../Icon/Icon.tsx';

export interface AttachmentCardProps {
  /** AttachmentSummary.kind (L-TS-58). 'audio' and 'video' render a MediaPlayer when `player` is given. */
  kind: 'image' | 'audio' | 'video' | 'file';
  name: string;
  size: string;
  thumbUrl: string | null;
  w: number | null;
  h: number | null;
  openLabel: string;
  onOpen?(): void;
  saveLabel: string;
  /** The visible text of a file card's save button; its accessible name stays saveLabel. Defaults to saveLabel. */
  saveText?: string;
  /** Without it a file card has no save button (an unsent card). */
  onSave?(): void;
  state: 'idle' | 'loading' | 'failed' | 'too-large';
  stateText?: string;
  tabbable: boolean;
  /** A small visual mark on a still image card (a GIF that moves when opened); aria-hidden, the open label names the file. */
  badge?: string;
  /** The inline player of an audio or video card (L-UI-71); without it, or when too large, the card is a file card. */
  player?: Pick<MediaPlayerProps, 'src' | 'state' | 'labels' | 'onPlay' | 'onError'>;
}

/** The prototype's file meta (§6.8): "48 KB · PNG", the extension upper-cased; a name without one shows the size alone. */
function fileMeta(name: string, size: string): string {
  const dot = name.lastIndexOf('.');
  return dot > 0 && dot < name.length - 1 ? `${size} · ${name.slice(dot + 1).toUpperCase()}` : size;
}

/** The longest side of an image card, in CSS px: the prototype's 280px image (§6.8), 240px for a GIF. */
const CARD_MAX_PX = 280;
const GIF_MAX_PX = 240;

/**
 * The card's box: its width in rem (the image scaled so its longer side is at most 280 px, 240 for a GIF, never up) and its
 * aspect ratio, as custom properties the CSS reads. Unknown or non-positive sizes give a box of 4 / 3 at the most.
 */
function cardBox(w: number | null, h: number | null, max = CARD_MAX_PX): CSSProperties {
  if (w === null || h === null || !(w > 0) || !(h > 0)) {
    return { '--d-card-w': `${String(max / 16)}rem`, '--d-card-ratio': '4 / 3' } as CSSProperties;
  }
  const px = w * Math.min(1, max / Math.max(w, h));
  const rem = Math.round(px * 1000 / 16) / 1000;
  return { '--d-card-w': `${String(rem)}rem`, '--d-card-ratio': `${String(w)} / ${String(h)}` } as CSSProperties;
}

/**
 * One attachment of a message. An image card is one button (named `openLabel`) holding the thumbnail, whose alt
 * text is the file's shown name, or a neutral box of the image's proportions while there is none (never a broken
 * image). A file card shows a glyph, the name and the size, and a save button only when `onSave` is given. A card
 * too large for a browser has no button. `stateText` follows the card (opening, could not open, too large).
 * An audio or video card with a `player` shows the name and size, the inline MediaPlayer and the save button, which
 * shows `media.save` and is named `saveLabel` (flow 06, label in name); in the player's error state the player's
 * own save, with the same text and name, stands in for the card's.
 */
export function AttachmentCard({
  kind, name, size, thumbUrl, w, h, openLabel, onOpen, saveLabel, saveText, onSave, state, stateText, tabbable, badge, player,
}: AttachmentCardProps) {
  const tabIndex = tabbable ? 0 : -1;
  const tooLarge = state === 'too-large';
  const box = <span className="d-attachment-card__box" aria-hidden="true" />;
  let content;
  if (kind === 'image' && tooLarge) {
    // Too large to open here: the neutral box, the name and the size, and no button.
    content = (
      <>
        <div className="d-attachment-card__frame" style={cardBox(w, h)}>{box}</div>
        <p className="d-attachment-card__meta">
          <span className="d-attachment-card__name">{name}</span>
          <span className="d-attachment-card__size">{size}</span>
        </p>
      </>
    );
  } else if (kind === 'image') {
    content = (
      <button type="button" className="d-attachment-card__open" aria-label={openLabel} tabIndex={tabIndex}
        aria-busy={state === 'loading' || undefined} style={cardBox(w, h, badge !== undefined ? GIF_MAX_PX : CARD_MAX_PX)} onClick={() => onOpen?.()}>
        {thumbUrl !== null ? <img className="d-attachment-card__thumb" alt={name} src={thumbUrl} data-thumb="ready" /> : box}
        {badge !== undefined ? <span className="d-attachment-card__badge" aria-hidden="true">{badge}</span> : null}
      </button>
    );
  } else if ((kind === 'audio' || kind === 'video') && player !== undefined && !tooLarge) {
    content = (
      <div className="d-attachment-card__media">
        <p className="d-attachment-card__meta">
          <span className="d-attachment-card__name">{name}</span>
          <span className="d-attachment-card__size">{size}</span>
        </p>
        <MediaPlayer kind={kind} src={player.src} name={name} state={player.state} labels={player.labels} tabbable={tabbable}
          saveName={saveLabel} onPlay={player.onPlay} onError={player.onError} onSave={() => onSave?.()} />
        {player.state !== 'error' && onSave ? (
          <Button variant="default" size="md" className="d-attachment-card__save d-attachment-card__media-save" aria-label={saveLabel}
            tabIndex={tabIndex} onClick={() => onSave()}>{player.labels.save}</Button>
        ) : null}
      </div>
    );
  } else {
    content = (
      <div className="d-attachment-card__file">
        <span className="d-attachment-card__glyph" aria-hidden="true"><Icon name="file" size={15} /></span>
        <span className="d-attachment-card__text">
          <span className="d-attachment-card__name">{name}</span>
          <span className="d-attachment-card__size">{fileMeta(name, size)}</span>
        </span>
        {!tooLarge && onSave ? (
          <button type="button" className="d-attachment-card__download" aria-label={saveLabel} title={saveText ?? saveLabel} tabIndex={tabIndex}
            onClick={() => onSave()}><Icon name="download" size={14} /></button>
        ) : null}
      </div>
    );
  }
  return (
    <div className="d-attachment-card" data-kind={kind} data-state={state} data-gif={badge !== undefined ? 'true' : undefined}>
      {content}
      {stateText !== undefined ? <p className="d-attachment-card__state">{stateText}</p> : null}
    </div>
  );
}
