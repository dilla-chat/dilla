import { Icon } from '../Icon/Icon.tsx';
import './AttachmentTray.css';

export interface AttachmentTrayProps {
  label: string;
  items: readonly {
    id: string; name: string; size: string; phase: string; failed: boolean;
    /** The machine phase, rendered as `data-phase`; `phase` is the text shown while the file is not ready. */
    step: 'reading' | 'preparing' | 'uploading' | 'ready' | 'failed';
    /** Said under a failed entry, only when given (read by a screen reader and shown as the card's tooltip). */
    hint?: string;
    /**
     * The 112px preview card (README §6.8): a still image's `blob:` URL (any other URL is not drawn), or a GIF, drawn as the
     * prototype draws one: its name centred on the striped tile with the GIF mark. Without it, the 200px file card.
     */
    preview?: { url?: string; gif?: boolean };
    removeLabel: string; onRemove(): void;
  }[];
  /** The remove button's tooltip (the prototype's "Remove"); its name stays each entry's `removeLabel`. */
  removeTip?: string;
}

/** The progress line's fill by phase (Jonas, task 7 fix round 1): it fills in steps, the whole line once the file is ready. */
const STEP_FILL: Record<AttachmentTrayProps['items'][number]['step'], string> = { reading: '25%', preparing: '50%', uploading: '75%', ready: '100%', failed: '100%' };

/** Only a page-made object URL becomes a CSS image: nothing from elsewhere reaches a `url()`. */
const safeBlobUrl = (url: string | undefined): string | null => (url !== undefined && /^blob:[\w.:/-]+$/.test(url) ? url : null);

/**
 * The files waiting to be sent, above the composer's box (README §6.8): a row of 84px cards, 112px previews or 200px file
 * cards, each with its name, its size (or its phase while it is not ready, a polite live region), a 3px progress line (the
 * accent while it works, ok when ready, danger when it failed) and remove ×. Its text is the caller's and never says how a
 * file is protected. Nothing renders while the tray is empty.
 */
/** A preview card for a GIF or a still with a blob: URL; anything else is a file card. */
function kindOf(preview: AttachmentTrayProps['items'][number]['preview']): 'gif' | 'image' | 'file' {
  if (preview?.gif === true) return 'gif';
  return safeBlobUrl(preview?.url) !== null ? 'image' : 'file';
}

export function AttachmentTray({ label, items, removeTip }: AttachmentTrayProps) {
  if (items.length === 0) return null;
  return (
    <ul className="d-attachment-tray" aria-label={label}>
      {items.map(item => (
        <li key={item.id} className="d-attachment-tray__item" data-phase={item.step} data-failed={item.failed ? 'true' : undefined}
          data-kind={kindOf(item.preview)} title={item.hint}>
          {kindOf(item.preview) === 'gif' ? (
            <span className="d-attachment-tray__preview" aria-hidden="true">
              <span className="d-attachment-tray__gif-name">{item.name}</span>
              <span className="d-attachment-tray__gif">GIF</span>
            </span>
          ) : kindOf(item.preview) === 'image' ? (
            <span className="d-attachment-tray__preview" style={{ backgroundImage: `url("${safeBlobUrl(item.preview?.url) ?? ''}")` }} />
          ) : null}
          <span className="d-attachment-tray__file">
            {kindOf(item.preview) === 'file' ? <span className="d-attachment-tray__glyph" aria-hidden="true"><Icon name="file" size={14} /></span> : null}
            <span className="d-attachment-tray__text">
              <span className="d-attachment-tray__name">{item.name}</span>
              <span className="d-attachment-tray__meta">
                {/* The size once the file is ready, the phase until then (the prototype's meta line); the phase stays heard. */}
                <span className="d-attachment-tray__size" hidden={item.step !== 'ready'}>{item.size}</span>
                <span className={item.step === 'ready' ? 'd-attachment-tray__phase d-sr-only' : 'd-attachment-tray__phase'} aria-live="polite">{item.phase}</span>
              </span>
            </span>
          </span>
          {item.hint !== undefined ? <span className="d-attachment-tray__hint d-sr-only">{item.hint}</span> : null}
          <span className="d-attachment-tray__bar" aria-hidden="true"><span style={{ width: STEP_FILL[item.step] }} /></span>
          <button type="button" className="d-attachment-tray__remove" aria-label={item.removeLabel} title={removeTip ?? item.removeLabel} onClick={() => item.onRemove()}>
            <Icon name="x" size={11} />
          </button>
        </li>
      ))}
    </ul>
  );
}
