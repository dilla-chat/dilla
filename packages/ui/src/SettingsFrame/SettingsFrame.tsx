import { useEffect, useId, useLayoutEffect, useRef, type ReactNode, type Ref } from 'react';
import { Icon } from '../Icon/Icon.tsx';
import { usePhoneLayout } from '../internal/phoneLayout.ts';
import './SettingsFrame.css';

export interface SettingsFrameProps {
  open: boolean;
  title: string;
  closeLabel: string;
  onClose(): void;
  nav: ReactNode;
  children: ReactNode;
  /** @deprecated Settings workspaces are always modal. */
  modal?: boolean;
  /** The one sticky save footer of the section. */
  footer?: ReactNode;
  identity?: ReactNode;
  search?: ReactNode;
  /**
   * Defaults to `section` for existing phone callers. A phone section shows the nav as a row of chips above the content
   * (README §5.10); the index shows the nav as a list and no content.
   */
  phonePage?: 'index' | 'section';
  /** @deprecated The phone section names itself through the nav's current chip (README §5.10); nothing is drawn. */
  sectionTitle?: string;
  /** @deprecated The phone section has no back button: the close button leaves (README §5.10). */
  backLabel?: string;
  /** @deprecated See `backLabel`. */
  backText?: string;
  /** @deprecated See `backLabel`. */
  onBack?: () => void;
  contentRef?: Ref<HTMLDivElement>;
  /** Gives list sections a wider content measure than normal forms. */
  wide?: boolean;
  /** Persistent call controls displayed inside the modal workspace. */
  activity?: ReactNode;
  /** Keeps idle connection announcements mounted without reserving visible header space. */
  activityVisible?: boolean;
  /** Nested dialogs remain available when the phone index hides section content. */
  overlays?: ReactNode;
}

function visibleFocusFallback(): HTMLElement | null {
  const candidates = document.querySelectorAll<HTMLElement>('[data-settings-focus-fallback], main, [role="main"]');
  for (const element of candidates) {
    let visible = element.isConnected;
    for (let node: HTMLElement | null = element; visible && node !== null; node = node.parentElement) {
      const style = getComputedStyle(node);
      visible = !node.hidden && style.display !== 'none' && style.visibility !== 'hidden';
    }
    if (visible) return element;
  }
  return null;
}

/**
 * The one settings workspace (Signal task 10 fix round 1): the user's and a server's settings are two frames, and moving
 * between them swaps one for the other in a single commit. The leaving frame hands its opener to the arriving one, which
 * then neither replays the entrance nor loses where focus returns on close.
 */
let handoff: { opener: HTMLElement | null; at: number } | null = null;
const HANDOFF_MS = 100;

/** A native modal workspace for settings; browser modal behavior keeps the mounted app inert. */
export function SettingsFrame({
  open, title, closeLabel, onClose, nav, children, footer, identity, search,
  phonePage = 'section', contentRef, wide, activity, activityVisible = true, overlays,
}: SettingsFrameProps) {
  const phone = usePhoneLayout();
  const ref = useRef<HTMLDialogElement>(null);
  const titleRef = useRef<HTMLHeadingElement>(null);
  const opener = useRef<HTMLElement | null>(null);
  const previousPhonePage = useRef(phonePage);
  const titleId = useId();
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;

  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (open && !el.open) {
      const active = document.activeElement;
      const taken = handoff !== null && performance.now() - handoff.at < HANDOFF_MS ? handoff : null;
      handoff = null;
      opener.current = taken !== null ? taken.opener
        : active instanceof HTMLElement && active !== document.body && !el.contains(active) ? active : null;
      if (taken !== null) el.dataset.handoff = 'true';
      else delete el.dataset.handoff;
      el.showModal();
      titleRef.current?.focus();
      // The nav scrolled its current item into view before the dialog was shown; a phone's chip row needs it now (README §5.10).
      el.querySelector<HTMLElement>('.d-settings-nav__item[aria-current="page"]')?.scrollIntoView?.({ block: 'nearest', inline: 'nearest' });
    } else if (!open && el.open) {
      el.close();
      const target = opener.current?.isConnected ? opener.current : visibleFocusFallback();
      opener.current = null;
      if (target) {
        if (!target.hasAttribute('tabindex') && !(target instanceof HTMLButtonElement || target instanceof HTMLInputElement || target instanceof HTMLAnchorElement)) target.tabIndex = -1;
        target.focus();
      }
    }
  }, [open]);

  useLayoutEffect(() => {
    const previous = previousPhonePage.current;
    previousPhonePage.current = phonePage;
    if (!open || !phone || previous === phonePage) return;
    const target = ref.current?.querySelector<HTMLButtonElement>('.d-settings-nav__item[aria-current="page"]');
    target?.focus();
  }, [open, phone, phonePage]);

  // Runs in the commit that unmounts this frame, before an arriving frame's open effect.
  useLayoutEffect(() => {
    const el = ref.current;
    return () => { if (el?.open) handoff = { opener: opener.current, at: performance.now() }; };
  }, []);

  useEffect(() => {
    const el = ref.current;
    return () => {
      if (el?.open) {
        el.close();
        // Another settings frame took over (the workspace's other scope): focus stays with it.
        if (document.querySelector('dialog.d-settings-frame[open]') !== null) return;
        const target = opener.current?.isConnected ? opener.current : visibleFocusFallback();
        if (target) {
          if (!target.hasAttribute('tabindex') && !(target instanceof HTMLButtonElement || target instanceof HTMLInputElement || target instanceof HTMLAnchorElement)) target.tabIndex = -1;
          target.focus();
        }
      }
    };
  }, []);

  const ownEvent = (target: EventTarget) => target instanceof Element && target.closest('dialog') === ref.current;
  const phoneIndex = phone && phonePage === 'index';
  return (
    <dialog ref={ref} className="d-settings-frame" aria-labelledby={titleId} aria-modal="true" data-layout={phone ? 'phone' : undefined} data-phone-page={phone ? phonePage : undefined}
      onCancel={e => { if (!ownEvent(e.target)) return; e.preventDefault(); onCloseRef.current(); }}
      onKeyDown={e => {
        if (e.key === 'Escape' && !e.defaultPrevented && !e.nativeEvent.isComposing && ownEvent(e.target)) {
          e.preventDefault(); onCloseRef.current();
        }
      }}>
      {open ? (
        <>
          <div className="d-settings-frame__panel">
            <div className="d-settings-frame__side">
              <h1 ref={titleRef} id={titleId} tabIndex={-1} className="d-settings-frame__title">{title}</h1>
            {/* README §5.9: a 34px circle at the viewport's top right that turns 90° on hover, ESC under it (not on a phone).
                Positioned against the dialog, it follows the title in the focus order. */}
            <button type="button" className="d-settings-frame__close" aria-label={closeLabel} aria-keyshortcuts="Escape" onClick={() => onCloseRef.current()}>
              <span className="d-settings-frame__close-ring"><Icon name="x" size={16} /></span>
              <span className="d-settings-frame__close-key" aria-hidden="true">ESC</span>
            </button>
              {identity ? <div className="d-settings-frame__identity">{identity}</div> : null}
              {search ? <div className="d-settings-frame__search">{search}</div> : null}
              <div className="d-settings-frame__nav">{nav}</div>
            </div>
            <div className="d-settings-frame__pane">
              {activity ? (
                <div className="d-settings-frame__bar" data-visible={activityVisible}>
                  <div className="d-settings-frame__activity" data-visible={activityVisible}>{activity}</div>
                </div>
              ) : null}
              <div ref={contentRef} className={`d-settings-frame__body${wide ? ' d-settings-frame__body--wide' : ''}`} hidden={phoneIndex}>
                {children}
              </div>
              {!phoneIndex && footer ? <div className="d-settings-frame__footer">{footer}</div> : null}
            </div>
            {overlays}
          </div>
        </>
      ) : null}
    </dialog>
  );
}
