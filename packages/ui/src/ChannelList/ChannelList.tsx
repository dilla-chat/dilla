import { useEffect, useLayoutEffect, useRef, useState, type FocusEvent, type KeyboardEvent, type MouseEvent, type PointerEvent, type ReactNode } from 'react';
import { ActionMenu, type ActionMenuItem } from '../ActionMenu/ActionMenu.tsx';
import { ChannelRow } from '../ChannelRow/ChannelRow.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { IconButton } from '../IconButton/IconButton.tsx';
import { TrailingSlot } from '../TrailingSlot/TrailingSlot.tsx';
import { VoiceChannelRow, type VoiceChannelRowProps } from '../VoiceChannelRow/VoiceChannelRow.tsx';
import type { TileMenu, VoiceParticipantView } from '../CallTile/CallTile.tsx';
import { useRovingFocus } from '../internal/roving.ts';
import './ChannelList.css';

export interface ChannelListChannel {
  id: string; name: string; kind: 'text' | 'voice' | 'dm' | 'announcement'; readable: boolean; unread?: number; mentions?: number; muted?: boolean; badge?: string;
  /** The category this channel sits in; absent, null or an unknown id is top level. */
  parentId?: string | null;
  /** This device is in this voice channel (a collapsed category keeps the row, R23). */
  joined?: boolean;
  /** The built-in AFK channel. */
  afk?: boolean;
  /** Task 11 (Q2): a DM drawn as a person (ChannelRow's `person`). */
  person?: { sub?: string; presence?: 'online' | 'idle' | 'dnd' | 'offline'; group?: boolean; initials?: string };
}

export interface ChannelListProps {
  label: string;
  title: string;
  /** 'server' (default) sets the title in the display serif, which is only for the server name. 'plain' (a list's own heading, such as direct messages) is the mono heading style. */
  titleKind?: 'server' | 'plain';
  channels: readonly ChannelListChannel[];
  /** Task 11 (Q2): a quiet section label over the rows (the prototype's "DIRECT MESSAGES" with its rule). */
  sectionLabel?: string;
  /** The categories in display order. The uncategorised rows come first, then each category's header and its children. */
  categories?: readonly { id: string; name: string; collapsed: boolean }[];
  /** A header is a button; Enter or a click on it asks to fold or unfold the category. */
  onToggleCategory?(id: string): void;
  /** With it, each category header has a trailing "+" named `addInCategoryLabel(name)` (`layout.category.add`). */
  onAddInCategory?(id: string): void;
  addInCategoryLabel?(name: string): string;
  activeId: string | null;
  onSelect(id: string): void;
  emptyLabel: string;
  rowLabel?(c: { name: string; unread: number; mentions: number; muted: boolean }): string;
  tabs?: ReactNode;
  footer?: ReactNode;
  /** The server's own actions (a new channel, an invite link), a group named `actionsLabel` beside the heading, before the
   *  tabs and outside the rows' roving group (flow 07). */
  actions?: ReactNode;
  actionsLabel?: string;
  /** A control at the right of the server's name (the phone's server menu), in one row with the heading. */
  headerAction?: ReactNode;
  /**
   * The stat line under the heading (Signal §5.4), e.g. "3 online · 2 in voice" after a dot: 10.5px, the parts after a "·" (a comma for a
   * screen reader). A part with `live` follows the 6px --live dot (the online count). Absent or empty, no line.
   */
  stats?: readonly { text: string; live?: boolean }[];
  /** The unread tag's visible text for a count (`layout.row.new`, '{n} new'), passed to every ChannelRow. */
  newLabel?(count: string): string;
  /** Shown in place of the rows (and the empty label), inside the tab panel when there are tabs: a server that did
   *  not load, or no server at all, while the tabs stay (WORKER-WEB-02, A11Y-DESIGN-02). */
  content?: ReactNode;
  /**
   * With it, every voice channel is a VoiceChannelRow in its place among the others: its people (by channel id; absent
   * is nobody) under the row. `labels.join` (required) is `voice.join`; with `onJoin` a row that is neither `joined` nor `held`
   * has a join icon in its trailing slot, which follows the row's tab index.
   */
  voice?: {
    labels: VoiceChannelRowProps['labels'];
    /** `count` (AW6 fix round 1): this room's own `labels.count`, as the AFK row's `voice.afk.count` ('{n} in AFK'). */
    rooms: Readonly<Record<string, { joined: boolean; held?: boolean; participants: readonly VoiceParticipantView[]; count?: string }>>;
    onJoin?(id: string): void;
    /** Fix round 1 (Jonas): an occupant's person menu (Signal §5.4); `roomId` is the voice channel. */
    personMenu?(roomId: string, person: VoiceParticipantView): TileMenu | undefined;
  };
  /**
   * A text row's context menu (Ruling X: the channel's pins and edit channel live here, since the header has no "⋯"). It opens on a
   * right-click, a long press on a touch screen, and Shift+F10 or the ContextMenu key on a focused row; it has no visible trigger.
   * `items(id)` empty means that row has no menu.
   */
  rowMenu?: { label(name: string): string; items(id: string): readonly ActionMenuItem[]; onSelect(channelId: string, itemId: string): void };
}

type Entry =
  | { kind: 'category'; id: string; name: string; collapsed: boolean }
  | { kind: 'channel'; channel: ChannelListChannel };

/**
 * The rows in display order: the uncategorised channels, then each category's header and its children, then the AFK channel. A collapsed
 * category still shows the children that are the active one, that have unread or mentions, or that this device has joined (R23).
 */
function entriesOf(channels: readonly ChannelListChannel[], categories: NonNullable<ChannelListProps['categories']>,
  activeId: string | null, joined: (c: ChannelListChannel) => boolean): Entry[] {
  const known = new Set(categories.map(c => c.id));
  // The AFK channel is the very last row of the list, in no category, whatever its parent; folding never hides it.
  const out: Entry[] = channels.filter(c => c.afk !== true && (c.parentId == null || !known.has(c.parentId))).map(channel => ({ kind: 'channel', channel }));
  for (const cat of categories) {
    out.push({ kind: 'category', id: cat.id, name: cat.name, collapsed: cat.collapsed });
    for (const channel of channels) {
      if (channel.parentId !== cat.id || channel.afk === true) continue;
      if (cat.collapsed && channel.id !== activeId && (channel.unread ?? 0) === 0 && (channel.mentions ?? 0) === 0 && !joined(channel)) continue;
      out.push({ kind: 'channel', channel });
    }
  }
  for (const channel of channels) if (channel.afk === true) out.push({ kind: 'channel', channel });
  return out;
}

/**
 * The sidebar: the server name as the shell's <h1> (pre-flight ruling (e)), in
 * serif italic and never case-transformed, then the channels as the existing
 * ChannelRow buttons. One tab stop with roving focus, the category headers included. A row with something to
 * report (an unread count, mentions, or muted) is named by `rowLabel` when one
 * is given; a quiet row keeps its bare name. With `tabs`, the tabs sit between
 * the heading and a tab panel named like the list; an optional footer ends the
 * list, outside the roving group. `content`, when given, stands in place of the
 * rows inside the panel. With `voice`, a voice channel is a VoiceChannelRow with its people below it, still one
 * stop of the same roving group.
 */
export function ChannelList({ sectionLabel, label, title, channels, categories = [], onToggleCategory, onAddInCategory, addInCategoryLabel, activeId, onSelect, emptyLabel, rowLabel, tabs, footer, content, voice, actions, actionsLabel, headerAction, titleKind = 'server', stats, newLabel, rowMenu }: ChannelListProps) {
  const isJoined = (c: ChannelListChannel) => c.joined === true || (voice !== undefined && Object.hasOwn(voice.rooms, c.id) && voice.rooms[c.id].joined);
  const entries = entriesOf(channels, categories, activeId, isJoined);
  const roving = useRovingFocus(entries.findIndex(e => e.kind === 'channel' && e.channel.id === activeId));
  // A join or add icon holds its row's tab index: the stop row's icon is a tab stop, no other is.
  const syncExtras = () => {
    for (const li of Array.from(roving.ref.current?.children ?? [])) {
      const row = li.firstElementChild;
      const extra = li.querySelector<HTMLButtonElement>(':scope > .d-voice-row__join button, :scope > .d-channel-list__cat-add button');
      if (row instanceof HTMLButtonElement && extra !== null) extra.tabIndex = row.tabIndex;
    }
  };
  // Declared after the roving hook, so its placement has run when this does.
  useLayoutEffect(syncExtras);
  const onKeyDown = (e: KeyboardEvent<HTMLUListElement>) => { roving.onKeyDown(e); syncExtras(); };
  const onFocus = (e: FocusEvent<HTMLUListElement>) => { roving.onFocus(e); syncExtras(); };
  // The row context menu (Ruling X). `row` is the row's button, where focus goes back when the menu closes.
  const [menu, setMenu] = useState<{ id: string; name: string; at: { x: number; y: number }; row: HTMLElement } | null>(null);
  // Where the platform sends both a keydown and a contextmenu for one Shift+F10, the menu opens once.
  const keyOpenedAt = useRef(-Infinity);
  // A long press: a touch held 500ms opens the menu; the click that ends it then does not open the channel.
  const press = useRef<{ timer: ReturnType<typeof setTimeout>; x: number; y: number } | null>(null);
  const swallowClick = useRef(false);
  useEffect(() => () => { if (press.current !== null) clearTimeout(press.current.timer); }, []);
  // Fix round 1 (Jonas): a voice row has the row menu too (its call code), from the same handlers on its list item.
  const menuFor = (c: ChannelListChannel) => (rowMenu === undefined ? [] : rowMenu.items(c.id));
  const openMenu = (c: ChannelListChannel, li: HTMLElement, at: { x: number; y: number }) => {
    const row = li.querySelector<HTMLElement>('.d-chrow, .d-voice-row');
    if (row !== null) setMenu({ id: c.id, name: c.name, at, row });
  };
  // Fix round 2: only the row's own DOM asks for the row menu: not the people under a voice row (their person menu) and not an
  // event bubbling through React from a portalled menu.
  const fromRow = (e: { target: EventTarget; currentTarget: HTMLLIElement }) => e.target instanceof Element && e.currentTarget.contains(e.target)
    && e.target.closest('.d-voice-row__people') === null;
  const rowHandlers = (c: ChannelListChannel) => menuFor(c).length === 0 ? {} : {
    onContextMenu: (e: MouseEvent<HTMLLIElement>) => {
      if (!fromRow(e)) return;
      e.preventDefault();
      if (e.timeStamp - keyOpenedAt.current < 500) return;
      openMenu(c, e.currentTarget, { x: e.clientX, y: e.clientY });
    },
    onKeyDown: (e: KeyboardEvent<HTMLLIElement>) => {
      if (!fromRow(e) || (e.key !== 'ContextMenu' && !(e.key === 'F10' && e.shiftKey))) return;
      e.preventDefault();
      keyOpenedAt.current = e.timeStamp;
      const box = e.currentTarget.getBoundingClientRect();
      openMenu(c, e.currentTarget, { x: box.left + 8, y: box.bottom });
    },
    onPointerDown: (e: PointerEvent<HTMLLIElement>) => {
      // A new press forgets a long press whose closing click never came (a platform that sends none after a context menu).
      swallowClick.current = false;
      if (e.pointerType !== 'touch' || !fromRow(e)) return;
      const li = e.currentTarget;
      const at = { x: e.clientX, y: e.clientY };
      if (press.current !== null) clearTimeout(press.current.timer);
      press.current = { x: at.x, y: at.y, timer: setTimeout(() => {
        press.current = null;
        swallowClick.current = true;
        // The click that ends a long press comes at once or not at all: after a second, nothing is left to swallow.
        setTimeout(() => { swallowClick.current = false; }, 1000);
        openMenu(c, li, at);
      }, 500) };
    },
    onPointerMove: (e: PointerEvent<HTMLLIElement>) => {
      const p = press.current;
      if (p !== null && Math.hypot(e.clientX - p.x, e.clientY - p.y) > 10) { clearTimeout(p.timer); press.current = null; }
    },
    onPointerUp: () => { if (press.current !== null) { clearTimeout(press.current.timer); press.current = null; } },
    onPointerCancel: () => { if (press.current !== null) { clearTimeout(press.current.timer); press.current = null; } },
    onClickCapture: (e: MouseEvent<HTMLLIElement>) => {
      if (!swallowClick.current) return;
      swallowClick.current = false;
      e.preventDefault();
      e.stopPropagation();
    },
  };
  const renderChannel = (c: ChannelListChannel) => {
    const unread = c.unread ?? 0;
    const mentions = c.mentions ?? 0;
    const muted = c.muted ?? false;
    const ariaLabel = rowLabel && (unread > 0 || mentions > 0 || muted) ? rowLabel({ name: c.badge === undefined ? c.name : `${c.name}, ${c.badge}`, unread, mentions, muted }) : undefined;
    if (c.kind === 'voice' && voice !== undefined) {
      const room = Object.hasOwn(voice.rooms, c.id) ? voice.rooms[c.id] : undefined;
      const joined = room?.joined ?? false;
      // Final review M4: the AFK row hosts no call, so it offers no join icon (its page has go AFK; a desktop click goes AFK).
      const canJoin = voice.onJoin !== undefined && !joined && room?.held !== true && c.afk !== true;
      return (
        <li key={c.id} className="d-voice-item" data-joined={joined ? 'true' : undefined} {...rowHandlers(c)}>
          <VoiceChannelRow name={c.name} current={c.id === activeId} joined={joined} participants={room?.participants ?? []} afk={c.afk === true}
            readable={c.readable} labels={room?.count === undefined ? voice.labels : { ...voice.labels, count: room.count }} onOpen={() => onSelect(c.id)} onJoin={canJoin ? () => voice.onJoin?.(c.id) : undefined}
            {...(voice.personMenu !== undefined ? { personMenu: (p: VoiceParticipantView) => voice.personMenu?.(c.id, p) } : {})} />
        </li>
      );
    }
    return (
      <li key={c.id} {...rowHandlers(c)}>
        <ChannelRow name={c.name} kind={c.afk === true ? 'afk' : c.kind} readable={c.readable} active={c.id === activeId}
          unread={unread} mentions={mentions} muted={muted} badge={c.badge} ariaLabel={ariaLabel} newLabel={newLabel} onSelect={() => onSelect(c.id)}
          {...(c.person !== undefined ? { person: c.person } : {})} />
      </li>
    );
  };
  const body = content !== undefined ? content : channels.length === 0 && categories.length === 0 ? (
    <p className="d-channel-list__empty">{emptyLabel}</p>
  ) : (
    <>
    {sectionLabel !== undefined ? (
      <p className="d-channel-list__section"><span>{sectionLabel}</span><span className="d-channel-list__cat-rule" aria-hidden="true" /></p>
    ) : null}
    <ul role="list" className="d-channel-list__list" ref={roving.ref} onKeyDown={onKeyDown} onFocus={onFocus}>
      {entries.map(e => e.kind === 'channel' ? renderChannel(e.channel) : (
        <li key={`cat:${e.id}`} className="d-channel-list__cat">
          <button type="button" className="d-channel-list__cat-head" data-row aria-expanded={!e.collapsed} data-collapsed={e.collapsed ? 'true' : undefined}
            onClick={() => onToggleCategory?.(e.id)}>
            <span className="d-channel-list__cat-chevron" aria-hidden="true"><Icon name="chevron-down" size={11} /></span>
            <span className="d-channel-list__cat-label">{e.name}</span>
            <span className="d-channel-list__cat-rule" aria-hidden="true" />
            {onAddInCategory !== undefined ? <TrailingSlot /> : null}
          </button>
          {onAddInCategory !== undefined ? (
            <span className="d-channel-list__cat-add">
              <TrailingSlot>
                <IconButton icon="plus" size={28} label={addInCategoryLabel?.(e.name) ?? e.name} onClick={() => onAddInCategory(e.id)} />
              </TrailingSlot>
            </span>
          ) : null}
        </li>
      ))}
    </ul>
    </>
  );
  const statLine = stats !== undefined && stats.length > 0 ? (
    <p className="d-channel-list__stats">
      {stats.map((part, i) => (
        <span key={i} className="d-channel-list__stat" data-live={part.live === true ? 'true' : undefined}>
          {i > 0 ? <><span className="d-channel-list__sep" aria-hidden="true">·</span><span className="d-sr-only">, </span></> : null}
          {part.live === true ? <span className="d-channel-list__live" aria-hidden="true" /> : null}
          {part.text}
        </span>
      ))}
    </p>
  ) : null;
  const foot = footer !== undefined ? <div className="d-channel-list__footer">{footer}</div> : null;
  const contextMenu = menu !== null && rowMenu !== undefined ? (
    <ActionMenu label={rowMenu.label(menu.name)} items={rowMenu.items(menu.id)} open trigger="none" at={menu.at}
      returnFocus={() => (menu.row.isConnected ? menu.row : null)}
      onOpenChange={open => { if (!open) setMenu(null); }} onSelect={item => rowMenu.onSelect(menu.id, item)} />
  ) : null;
  return (
    <nav className="d-channel-list" aria-label={label}>
      {headerAction !== undefined ? (
        <div className="d-channel-list__head"><h1 className="d-channel-list__title" data-kind={titleKind === 'plain' ? 'plain' : undefined}>{title}</h1>{headerAction}</div>
      ) : <h1 className="d-channel-list__title" data-kind={titleKind === 'plain' ? 'plain' : undefined}>{title}</h1>}
      {statLine}
      {actions !== undefined ? <div className="d-channel-list__actions" role="group" aria-label={actionsLabel}>{actions}</div> : null}
      {tabs !== undefined ? (
        <>
          {tabs}
          <div className="d-channel-list__panel" role="tabpanel" aria-label={label}>{body}{foot}</div>
        </>
      ) : (
        <>{body}{foot}</>
      )}
      {contextMenu}
    </nav>
  );
}
