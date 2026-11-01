import { useEffect, useId, useLayoutEffect, useRef, useState, type CSSProperties, type KeyboardEvent } from 'react';
import type { ActionMenuItem } from '../ActionMenu/ActionMenu.tsx';
import { Avatar, avatarStyle, type Presence } from '../Avatar/Avatar.tsx';
import { focusReturnTarget } from '../Dialog/Dialog.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { hex } from '../internal/hex.ts';
import { usePhoneLayout } from '../internal/phoneLayout.ts';
import { PRESENCE_WORD } from '../internal/presenceWord.ts';
import './ProfileCard.css';

export interface ProfileCardRole { name: string; /** 0xRRGGBB; null, 0 or left out: the --fg-2 dot (the prototype's @everyone). */ color?: number | null }
/** An action: `primary` is the accent button with its label (Message); the others are 34px glyph buttons named by `label` (task 9). */
export type ProfileCardAction = ActionMenuItem & { primary?: boolean };

export interface ProfileCardProps {
  /** The display name, which also names the dialog. */
  name: string;
  username: string;
  presence?: Presence;
  /** A custom status, drawn after the presence word. */
  status?: string;
  /** The member's bio, drawn under the head with its line breaks; nothing when empty. */
  bio?: string;
  isOwner?: boolean;
  isYou?: boolean;
  /** A bot: the rounded-square avatar. */
  isBot?: boolean;
  /** The client tag after the name (the prototype's WEB), when the caller knows it. */
  client?: string;
  roles: readonly ProfileCardRole[];
  /** Message (primary), compare safety numbers and edit roles, as the prototype's card row: the caller decides which. */
  actions: readonly ProfileCardAction[];
  onAction(actionId: string): void;
  onClose(): void;
  /** The row the card was opened from: the popover sits beside it and focus returns to it on close. */
  anchor?: HTMLElement | null;
  /** `popover` beside the row (desktop) or `sheet` from the bottom (phone). Default: a sheet inside a phone shell, else a popover. */
  layout?: 'popover' | 'sheet';
  /** A popover's side: `side` (default) beside a member row, `below` under a mention or an author name (above when there is no room below). */
  placement?: 'side' | 'below';
  /** The bio's name for assistive technology. */
  bioLabel?: string;
  ownerLabel?: string;
  youLabel?: string;
  rolesLabel?: string;
  closeLabel?: string;
  /** The words for the presence states; each defaults to the English word. */
  presenceLabels?: Partial<Record<Presence, string>>;
  /** Where focus goes on close when the anchor is gone (and no row carries its focus key). */
  fallbackFocus?: () => HTMLElement | null;
  /** Restricts trigger lookup to a workspace when another covered surface has matching focus keys. */
  focusScope?: ParentNode | null;
}

const GAP = 8;
/** A bot's band takes the hue of its --brand square (#005799), as the prototype's Glenn (205). */
const BOT_HUE = 206;
const FOCUSABLE = 'button:not(:disabled), [href], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])';

/**
 * A member's profile: avatar, display name, @username, presence and status, an owner and "you" line, the roles as
 * chips and the actions as buttons. It is a `dialog` the caller mounts while it is open. Focus moves in (the first
 * action, else the close button), Tab stays inside, Escape and the close button call `onClose`, and on unmount focus
 * goes back to the row it was opened from. On a desktop it is a popover fixed beside the row (left of it when there is
 * room, else right); on a phone it is a modal sheet from the bottom over a scrim.
 */
export function ProfileCard({
  name, username, presence, status, bio, isOwner = false, isBot = false, client, roles, actions, onAction, onClose, anchor, layout, placement = 'side',
  bioLabel = 'about', ownerLabel = 'owner', rolesLabel = 'roles', closeLabel = 'Close', presenceLabels, fallbackFocus, focusScope,
}: ProfileCardProps) {
  const phone = usePhoneLayout();
  const mode = layout ?? (phone ? 'sheet' : 'popover');
  const ref = useRef<HTMLDivElement>(null);
  const titleId = useId();
  const opener = useRef<HTMLElement | null>(null);
  const aboutRef = useRef<HTMLDivElement>(null);
  const [scrolls, setScrolls] = useState(false);
  const [at, setAt] = useState<{ x: number; y: number } | null>(null);
  const onCloseRef = useRef(onClose);
  const fallbackRef = useRef(fallbackFocus);
  const focusScopeRef = useRef(focusScope);
  fallbackRef.current = fallbackFocus;
  focusScopeRef.current = focusScope;
  onCloseRef.current = onClose;

  // The bio box is a tab stop only while it overflows (WCAG 2.1.1: a keyboard reaches what scrolls, and nothing else gets a stop).
  useLayoutEffect(() => {
    const box = aboutRef.current;
    setScrolls(box !== null && box.scrollHeight > box.clientHeight);
  }, [bio, mode]);

  // The row to return to is read before focus moves in: the anchor, else whatever had focus.
  useLayoutEffect(() => {
    const active = document.activeElement;
    opener.current = anchor ?? (active instanceof HTMLElement && active !== document.body ? active : null);
  }, [anchor]);

  // Focus moves in on open and back to the row on close (resolved then: the row may have been re-mounted meanwhile).
  useEffect(() => {
    const el = ref.current;
    // The first action, else the card itself (the keyboard's close shows only when Tab reaches it, as the prototype draws none).
    (el?.querySelector<HTMLElement>('.d-profile-card__action, .d-profile-card__icon-action') ?? el)?.focus();
    return () => {
      const active = document.activeElement;
      const inside = active === null || active === document.body || el?.contains(active) === true;
      if (!inside) return;
      focusReturnTarget(opener.current, () => fallbackRef.current?.(), focusScopeRef.current)?.focus();
    };
  }, []);

  // A popover sits beside its row, inside the viewport (or inside the app shell, a size container and so the containing block of a fixed element).
  // It follows the row: re-placed on a window resize and on a scroll of any container.
  useLayoutEffect(() => {
    if (mode !== 'popover' || !anchor || !ref.current) { setAt(null); return; }
    const place = () => {
      if (!ref.current) return;
      // The row went away (a re-mount, a kick): close; the unmount returns focus to that member's row if it is back, else the list.
      if (!anchor.isConnected) { onCloseRef.current(); return; }
      const w = ref.current.offsetWidth;
      const h = ref.current.offsetHeight;
      const shell = anchor.closest('.d-app-shell')?.getBoundingClientRect();
      const box = shell ?? { left: 0, top: 0, width: window.innerWidth, height: window.innerHeight };
      const a = anchor.getBoundingClientRect();
      let x: number;
      let y: number;
      if (placement === 'below') {
        // Under the anchor at its left edge (a mention, an author name), above it when there is no room below.
        x = a.left - box.left;
        y = a.bottom - box.top + GAP;
        if (y + h > box.height) y = Math.max(0, a.top - box.top - GAP - h);
      } else {
        const leftOfRow = a.left - box.left - GAP - w;
        x = leftOfRow >= GAP ? leftOfRow : a.right - box.left + GAP;
        y = Math.max(0, Math.min(a.top - box.top, box.height - h));
      }
      const next = { x: Math.max(0, Math.min(x, box.width - w)), y };
      setAt(cur => (cur !== null && cur.x === next.x && cur.y === next.y ? cur : next));
    };
    place();
    window.addEventListener('resize', place);
    document.addEventListener('scroll', place, true);
    return () => { window.removeEventListener('resize', place); document.removeEventListener('scroll', place, true); };
  }, [mode, anchor, placement]);

  // A pointerdown outside the popover (and its own row, which decides for itself) closes it.
  useEffect(() => {
    if (mode !== 'popover') return;
    const down = (e: PointerEvent) => {
      if (!(e.target instanceof Node)) return;
      if (ref.current?.contains(e.target) || anchor?.contains(e.target)) return;
      // An author's avatar is the same control as the author name that opened the card: its click toggles.
      const el = e.target instanceof Element ? e.target : e.target.parentElement;
      if (anchor?.classList.contains('d-message-row__author-open') === true && el?.closest('[data-profile="true"]') != null
        && anchor.closest('.d-message-row')?.contains(el) === true) return;
      onCloseRef.current();
    };
    document.addEventListener('pointerdown', down, true);
    return () => document.removeEventListener('pointerdown', down, true);
  }, [mode, anchor]);

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); onClose(); return; }
    if (e.key !== 'Tab') return;
    const list = Array.from(ref.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? []);
    const first = list[0]; const last = list[list.length - 1];
    if (!first || !last) return;
    if (e.shiftKey && (document.activeElement === first || document.activeElement === ref.current)) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  };

  const style = { ...(mode === 'popover' ? { position: 'fixed', left: `${at?.x ?? 0}px`, top: `${at?.y ?? 0}px` } : {}), ...(isBot ? { '--avatar-hue': String(BOT_HUE) } : avatarStyle(name)) } as CSSProperties;
  const word = presence === undefined ? null : presenceLabels?.[presence] ?? PRESENCE_WORD[presence];
  // The prototype's line: "@handle · status", the status being the custom one, else the presence word.
  const line = [username === '' ? null : `@${username}`, status ? null : word].filter(v => v !== null).join(' · ');
  const primary = actions.find(a => a.primary === true);
  const rest = actions.filter(a => a !== primary);
  const card = (
    <div ref={ref} className="d-profile-card" role="dialog" aria-modal={mode === 'sheet' ? true : undefined} aria-labelledby={titleId} data-layout={mode} style={style}
      tabIndex={-1} onKeyDown={onKeyDown} onClick={e => e.stopPropagation()}>
      {/* Task 9, the prototype's card: a 56px band in the person's hue (135°), the 56px avatar over it in a 4px ring. */}
      <div className="d-profile-card__band" aria-hidden="true" />
      <div className="d-profile-card__body">
        <span className="d-profile-card__avatar" aria-hidden="true"><Avatar name={name} size="card" shape={isBot ? 'bot' : 'circle'} /></span>
        <div className="d-profile-card__title">
          <h2 id={titleId} className="d-profile-card__name">{name}</h2>
          {isOwner ? <span className="d-profile-card__crown" role="img" aria-label={ownerLabel}><Icon name="crown" size={13} /></span> : null}
          {client === undefined ? null : <span className="d-profile-card__client">{client}</span>}
        </div>
        {line !== '' || status ? (
          <p className="d-profile-card__status" data-presence={presence}>{line}{line !== '' && status ? ' · ' : null}{status ? <bdi dir="auto">{status}</bdi> : null}{status && word !== null ? <span className="d-sr-only">{`, ${word}`}</span> : null}</p>
        ) : null}
        {bio ? <div ref={aboutRef} className="d-profile-card__about" role="group" aria-label={bioLabel} tabIndex={scrolls ? 0 : undefined}><p className="d-profile-card__bio" dir="auto">{bio}</p></div> : null}
        {roles.length > 0 ? (
          <ul className="d-profile-card__roles" role="list" aria-label={rolesLabel}>
            {roles.map(r => <li key={r.name} className="d-profile-card__role"><span className="d-profile-card__role-dot" aria-hidden="true" style={r.color ? { background: hex(r.color) } : undefined} />{r.name}</li>)}
          </ul>
        ) : null}
        {actions.length > 0 ? (
          <div className="d-profile-card__actions" role="group" aria-label={name} data-primary={primary === undefined ? undefined : 'true'}>
            {primary === undefined ? null : (
              <button type="button" className="d-profile-card__action" onClick={() => onAction(primary.id)}>
                {primary.icon === undefined ? null : <Icon name={primary.icon} size={14} />}{primary.label}
              </button>
            )}
            {rest.map(a => (
              <button key={a.id} type="button" className="d-profile-card__icon-action" aria-label={a.label} title={a.label} data-danger={a.danger ? 'true' : undefined} onClick={() => onAction(a.id)}>
                {a.icon === undefined ? a.label : <Icon name={a.icon} size={15} />}
              </button>
            ))}
          </div>
        ) : null}
      </div>
      {/* The prototype draws no close control: Escape, the scrim and a click outside close it. This one is for a keyboard and a screen reader, shown only while focused. */}
      <button type="button" className="d-profile-card__close" aria-label={closeLabel} onClick={onClose}><Icon name="x" size={15} /></button>
    </div>
  );
  return mode === 'sheet' ? <div className="d-profile-card__scrim" onClick={onClose}>{card}</div> : card;
}
