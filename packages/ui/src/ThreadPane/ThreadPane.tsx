import type { KeyboardEvent, ReactNode } from 'react';
import { Icon } from '../Icon/Icon.tsx';
import './ThreadPane.css';

export interface ThreadPaneProps {
  /** The pane's heading (a level-two heading). */
  title: string;
  closeLabel: string;
  onClose(): void;
  /** The root row (a MessageRow the caller renders) or the missing note. */
  root: ReactNode;
  /** The replies (a MessageLog). */
  children: ReactNode;
  /** The thread's Composer. */
  composer: ReactNode;
  /** The rule between the root and the replies ("3 replies", drawn uppercase with a hairline; Ruling AL). Absent: no rule. */
  repliesLabel?: string;
  /** The place after the title in the head (`#general`, 10.5px --fg-3), as the prototype draws it. */
  sub?: string;
  /** True: a full-screen sheet (Task 11: the same head, × closes; the send button stays). False: a column in the shell's aside. */
  phone?: boolean;
}

/**
 * A public thread beside the channel: a heading with a close control, the root message, the replies, and the thread's
 * composer at the bottom. In the shell's aside it is a column that fits the aside's width. With `phone` it is a
 * full-screen sheet over the tab bar, with the column's head (× closes it; Task 11, Jonas Q4), the composer clearing the home indicator, and every
 * control 44 px. It is not a landmark of its own: the AppShell's <aside>, named by the caller's `asideLabel`, is the landmark
 * around it. Mount it in the aside WITHOUT `asideCloseLabel`, on a phone and in the narrow-desktop overlay alike: it carries
 * its own header and close control, and the shell would otherwise draw a second header above it.
 *
 * Escape anywhere inside closes the thread and nothing more: the key is stopped here, so the drawer around it (which
 * closes on Escape too) never sees it, and a key that something inside already handled (an open mention list, a row
 * returning focus to itself) is left alone.
 */
export function ThreadPane({ title, sub, repliesLabel, closeLabel, onClose, root, children, composer, phone }: ThreadPaneProps) {
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'Escape' || e.defaultPrevented || e.nativeEvent.isComposing || e.keyCode === 229) return;
    e.preventDefault();
    e.stopPropagation();
    onClose();
  };
  return (
    <div className="d-thread-pane" data-phone={phone === true ? 'true' : undefined} onKeyDown={onKeyDown}>
      {/* Task 11 (Jonas, Q4): a phone's sheet has the same head as the desktop column (the prototype's). */}
      <header className="d-thread-pane__head">
          {/* Task 9, the prototype's thread head: 44px, the 14px glyph, the 12px bold title, the place, a 32px close ×. */}
          <span className="d-thread-pane__glyph" aria-hidden="true"><Icon name="messages-square" size={14} /></span>
          <h2 className="d-thread-pane__title">{title}</h2>
          {sub === undefined ? null : <span className="d-thread-pane__sub">{sub}</span>}
          <button type="button" className="d-thread-pane__close" aria-label={closeLabel} onClick={onClose}><Icon name="x" size={15} /></button>
      </header>
      <div className="d-thread-pane__scroll">
        <div className="d-thread-pane__root">{root}</div>
        {repliesLabel === undefined || root === null ? null : <div className="d-thread-pane__rule">{repliesLabel}</div>}
        <div className="d-thread-pane__replies">{children}</div>
      </div>
      <div className="d-thread-pane__composer">{composer}</div>
    </div>
  );
}
