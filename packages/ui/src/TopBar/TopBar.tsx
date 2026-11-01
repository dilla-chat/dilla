import { useId, type ReactNode } from 'react';
import { Icon } from '../Icon/Icon.tsx';
import { IconButton } from '../IconButton/IconButton.tsx';
import './TopBar.css';

export interface TopBarProps {
  /** The search trigger's name and its visible text ("Search or jump to…"). */
  searchLabel: string;
  /** The palette chord as this platform's keyboard writes it: "⌘K" on Apple platforms, "Ctrl K" elsewhere. */
  searchShortcut: string;
  onSearch(): void;
  /** Only while this device is in a call: the room and the last round trip (null until the first reading). */
  voice?: { room: string; pingMs: number | null; onOpen(): void };
  /** Reminders that are due; the bell draws a count badge above 0. */
  remindersDue: number;
  onReminders(): void;
  /** The reminders popover (README §6.7) while it is open; it hangs under the bell, which then reads as pressed. */
  reminders?: ReactNode;
  remindersOpen?: boolean;
  onSettings(): void;
  /** `voice` describes the chip (its name is its text); `remindersDue` words the badge for assistive tech. */
  labels: { reminders: string; settings: string; voice: string; remindersDue?(count: number): string };
}

/** The Dilla wordmark: the D tile, DILLA and the blinking caret. `bar` is the top bar's (18px tile), `page` the sign-in page's (20px). */
export function BrandMark({ size = 'page' }: { size?: 'bar' | 'page' }) {
  return (
    <span className="d-brand" data-size={size}>
      <span className="d-brand__tile" aria-hidden="true">D</span>
      <span className="d-brand__name">DILLA</span>
      <span className="d-brand__caret" aria-hidden="true" />
    </span>
  );
}

/**
 * The Signal top bar (README §5.2), 44px over the whole desktop shell: the wordmark, the search trigger in the centre, and on
 * the right the voice chip (in a call only), the reminders bell and the settings gear. A phone has none (§5.10).
 */
export function TopBar({ searchLabel, searchShortcut, onSearch, voice, remindersDue, onReminders, reminders, remindersOpen = false, onSettings, labels }: TopBarProps) {
  const id = useId();
  const voiceDesc = `${id}-voice`;
  const dueDesc = `${id}-due`;
  const due = remindersDue > 0;
  return (
    <header className="d-topbar">
      <div className="d-topbar__brand"><BrandMark size="bar" /></div>
      <div className="d-topbar__center">
        <button type="button" className="d-topbar__search" aria-label={searchLabel} aria-keyshortcuts="Control+K Meta+K" onClick={onSearch}>
          <Icon name="search" size={13} />
          <span className="d-topbar__search-text">{searchLabel}</span>
          <kbd className="d-topbar__kbd">{searchShortcut}</kbd>
        </button>
      </div>
      <div className="d-topbar__end">
        {voice === undefined ? null : (
          <>
            <button type="button" className="d-topbar__voice" aria-describedby={voiceDesc} onClick={voice.onOpen}>
              <span className="d-topbar__bars" aria-hidden="true"><i /><i /><i /></span>
              {voice.pingMs === null ? voice.room : `${voice.room} · ${String(voice.pingMs)} ms`}
            </button>
            <span id={voiceDesc} hidden>{labels.voice}</span>
          </>
        )}
        <span className="d-topbar__bell">
          <IconButton icon="bell" size={28} label={labels.reminders} aria-haspopup="dialog" aria-expanded={remindersOpen}
            data-open={remindersOpen ? 'true' : undefined} onClick={onReminders} aria-describedby={due ? dueDesc : undefined} />
          {due ? <span className="d-topbar__badge" aria-hidden="true">{remindersDue > 99 ? '99+' : remindersDue}</span> : null}
          {due ? <span id={dueDesc} hidden>{labels.remindersDue?.(remindersDue) ?? String(remindersDue)}</span> : null}
          {remindersOpen ? reminders : null}
        </span>
        <IconButton icon="settings" size={28} className="d-topbar__gear" label={labels.settings} onClick={onSettings} />
      </div>
    </header>
  );
}
