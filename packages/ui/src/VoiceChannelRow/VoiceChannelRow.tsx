import { useId, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { voiceStateWords, VoiceStates, type TileMenu, type VoiceParticipantView, type VoiceStateLabels, type VoiceStateWord } from '../CallTile/CallTile.tsx';
import { PersonContextMenu } from '../PersonContextMenu/PersonContextMenu.tsx';
import { useContextMenuTrigger } from '../internal/contextMenuTrigger.ts';
import { Avatar } from '../Avatar/Avatar.tsx';
import { IconButton } from '../IconButton/IconButton.tsx';
import { Tag } from '../Tag/Tag.tsx';
import { TrailingSlot } from '../TrailingSlot/TrailingSlot.tsx';
import './VoiceChannelRow.css';
import { Icon } from '../Icon/Icon.tsx';

export interface VoiceChannelRowProps {
  name: string;
  /** The voice the main pane shows (aria-current). */
  current: boolean;
  /** This device is in this voice. */
  joined: boolean;
  participants: readonly VoiceParticipantView[];
  /** The channel is readable by this server (joinable by public invite): the readable glyph stays on the row. */
  readable?: boolean;
  /** The built-in AFK channel: the moon in place of the speaker (Signal §5.4). */
  afk?: boolean;
  /**
   * `open` is `voice.open` ('voice {name}'), `count` is `voice.count` ('{n} in voice'), both filled here;
   * `playbackOnly` is `voice.bot.playbackOnly` ('playback only'), shown with the bot tag; `join` is `voice.join`, the slot's icon button; `joined` is `voice.row.joined` ('connected'), added to the description (after the count) of the row this device is in; the name stays 'voice {name}';
   * `away` is `afk.away` ('away'), the word beside a person of the AFK channel's away roster (AW6).
   */
  labels: { open: string; count: string; playbackOnly: string; state: VoiceStateLabels; join: string; joined: string; away?: string };
  /** Opens the voice in the main pane (the row's own click). */
  onOpen(): void;
  /** The trailing join icon; without it, or when `joined`, the row has none. Its own click never opens the row (Shell decides, R24). */
  onJoin?(): void;
  /** Fix round 1: the person menu of an occupant (right-click, long press, Shift+F10); none for a person without one. */
  personMenu?(person: VoiceParticipantView): TileMenu | undefined;
}

/** The mute and deafen glyphs sit in a participant's trailing slot; every other state stays beside the name. */
const SLOT_STATES = new Set(['muted', 'deafened']);

/**
 * One entry per person (A7, AUDIT-2 FA3): a person with several devices in the call is listed once, as on Discord. The
 * state is the union across their devices: speaking if any device speaks, camera or screen if any shares one, muted
 * and deafened only if every device is. State is the worst across devices (unverified, connecting, not-heard, ok); web and bot if any. The first device stands for the person (name, id); order is kept.
 */
/**
 * Signal §5.4: a person's state glyphs are 12px (the call stage keeps its 14px ones), a shared screen is the LIVE tag in
 * place of the monitor glyph, and a deafened person shows headphone-off without mic-off. Every other state keeps its glyph and word.
 */
function rowWords(words: readonly VoiceStateWord[]): VoiceStateWord[] {
  const small: Partial<Record<string, ReactNode>> = {
    muted: <Icon name="mic-off" size={12} />, deafened: <Icon name="headphone-off" size={12} />, video: <Icon name="video" size={12} />, screen: <span aria-hidden="true"><Tag kind="live" /></span>,
  };
  // Deafened already silences the mic: the prototype draws headphone-off alone (the deafened word says both to a screen reader's ear).
  const deaf = words.some(w => w.key === 'deafened');
  return words.filter(w => !(deaf && w.key === 'muted')).map(w => (small[w.key] !== undefined ? { ...w, glyph: small[w.key] } : w));
}

/** Worst first: an unverified stream is a security warning and must never read as ok. */
const WORST: readonly VoiceParticipantView['state'][] = ['unverified', 'connecting', 'not-heard', 'ok'];

export function groupPeople(devices: readonly VoiceParticipantView[]): VoiceParticipantView[] {
  const byUser = new Map<string, VoiceParticipantView[]>();
  for (const d of devices) byUser.set(d.userId, [...(byUser.get(d.userId) ?? []), d]);
  return [...byUser.values()].map(group => ({
    ...group[0],
    speaking: group.some(d => d.speaking), video: group.some(d => d.video), screen: group.some(d => d.screen),
    state: WORST.find(w => group.some(d => d.state === w)) ?? 'ok', web: group.some(d => d.web), bot: group.some(d => d.bot),
    muted: group.every(d => d.muted), deafened: group.every(d => d.deafened),
    ...(group.some(d => d.away === true) ? { away: true } : {}),
  }));
}

/**
 * One person under a voice row. Fix round 1 (Jonas): with `menu`, a right-click, the ContextMenu key or Shift+F10, or a long
 * press opens the person menu (Signal §5.4) at the pointer; focus comes back to the row.
 */
function PersonRow({ menu, speaking, away, label, children }: { menu: TileMenu | undefined; speaking: boolean; away: boolean; label: string;
  /** The row's content; `statesId` names the two state wrappers the row's description points at. */
  children: (statesId: string) => ReactNode }) {
  const ref = useRef<HTMLLIElement>(null);
  const statesId = useId();
  const [at, setAt] = useState<{ x: number; y: number } | null>(null);
  const trigger = useContextMenuTrigger<HTMLLIElement>(menu !== undefined ? (_el, point) => { setAt(point); } : undefined);
  // Fix round 2: with a menu, the person is a keyboard stop outside the tab order (no extra Tab stops in the sidebar): ArrowRight
  // on the voice row enters its people, ArrowUp/Down move between them, ArrowLeft goes back to the row, and Shift+F10 or the
  // ContextMenu key opens the menu.
  const onKeyDown = (e: React.KeyboardEvent<HTMLLIElement>) => {
    trigger.onKeyDown?.(e);
    if (e.defaultPrevented || e.target !== e.currentTarget || e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
    const people = Array.from(e.currentTarget.parentElement?.querySelectorAll<HTMLElement>(':scope > .d-voice-row__person[tabindex]') ?? []);
    const i = people.indexOf(e.currentTarget);
    const go = (el: HTMLElement | null | undefined) => { if (el) { e.preventDefault(); el.focus(); } };
    if (e.key === 'ArrowDown') go(people[i + 1]);
    else if (e.key === 'ArrowUp') go(people[i - 1]);
    else if (e.key === 'ArrowLeft') go(e.currentTarget.closest('ul')?.parentElement?.querySelector<HTMLElement>(':scope > .d-voice-row'));
  };
  return (
    <li ref={ref} className="d-voice-row__person" data-row data-speaking={speaking ? 'true' : undefined} data-away={away ? 'true' : undefined}
      data-menu={at !== null ? 'true' : undefined} {...trigger}
      // Task 11 (8 minor): the label names the person; the state words (speaking, muted, camera on…) stay readable as its description.
      {...(menu !== undefined ? { tabIndex: -1, 'aria-label': label, 'aria-describedby': `${statesId}-a ${statesId}-b`, onKeyDown } : {})}>
      {children(statesId)}
      {menu !== undefined && at !== null ? createPortal(
        <PersonContextMenu {...menu} open at={at} onOpenChange={open => { if (!open) setAt(null); }} returnFocus={() => ref.current} />, document.body) : null}
    </li>
  );
}

/**
 * A voice channel in the sidebar (Signal, README §5.4), on ChannelRow's grammar: the 14px speaker (the moon for AFK), the name,
 * and the occupant count at the right (10.5px). The row you are in has a --fg 600 name and the --live glyph; the row the main
 * pane shows has the selected fill, the accent glyph and the 2px accent bar. The join icon is a sibling drawn over the
 * trailing slot (a button cannot sit inside the row's button), shown while the row is hovered or focused and always on a
 * device without hover; never on the channel you are in. Under the row, one 26px sub-row per person (R25, A7), 30px in:
 * an 18px avatar (the bot's rounded square, with "playback only" and no visible BOT tag), the name in 12px, its tags, then camera, LIVE, and the mute and deafen glyphs
 * in the slot. Speaking is only a ring on the avatar (no meter in the list; the word stays for assistive technology). The
 * row and its parts are siblings, so the button stays the first element of a list item (the roving lists find it there);
 * a caller outside ChannelList wraps them in `.d-voice-item`.
 */
export function VoiceChannelRow({ name, current, joined, participants: devices, readable, afk = false, labels, onOpen, onJoin, personMenu }: VoiceChannelRowProps) {
  const countId = useId();
  const participants = groupPeople(devices);
  const n = participants.length;
  return (
    <>
      <button type="button" className="d-voice-row" data-row aria-label={labels.open.replaceAll('{name}', name)}
        onKeyDown={e => {
          // Fix round 2: ArrowRight enters the row's people (each opens its person menu from the keyboard).
          if (e.key !== 'ArrowRight' || e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
          const first = e.currentTarget.parentElement?.querySelector<HTMLElement>(':scope > .d-voice-row__people > .d-voice-row__person[tabindex]');
          if (first) { e.preventDefault(); first.focus(); }
        }}
        aria-describedby={n > 0 || joined ? countId : undefined} aria-current={current ? 'page' : undefined}
        data-joined={joined ? 'true' : undefined} data-live={n > 0 ? 'true' : undefined} data-afk={afk ? 'true' : undefined} onClick={() => onOpen()}>
        <span className="d-voice-row__bar" aria-hidden="true" />
        <span className="d-voice-row__glyph" aria-hidden="true"><Icon name={afk ? 'moon' : 'volume-2'} size={14} /></span>
        <span className="d-voice-row__title">
          <span className="d-voice-row__name">{name}</span>
        </span>
        {n > 0 || joined ? <span id={countId} className="d-sr-only">{[...(n > 0 ? [labels.count.replaceAll('{n}', String(n))] : []), ...(joined ? [labels.joined] : [])].join(', ')}</span> : null}
        {readable === true ? <Tag kind="readable" /> : null}
        {n > 0 ? <span className="d-voice-row__count" aria-hidden="true">{String(n)}</span> : null}
        {onJoin !== undefined && !joined ? <TrailingSlot /> : null}
      </button>
      {onJoin !== undefined && !joined ? (
        <span className="d-voice-row__join">
          <TrailingSlot>
            <IconButton icon="phone" size={28} label={labels.join} onClick={() => onJoin()} />
          </TrailingSlot>
        </span>
      ) : null}
      {n > 0 ? (
        <ul role="list" className="d-voice-row__people">
          {participants.map(p => {
            const words = rowWords(voiceStateWords(p, labels.state));
            return (
              <PersonRow key={p.userId} menu={personMenu?.(p)} speaking={p.speaking} away={p.away === true} label={p.name}>{statesId => <>
                <span className="d-voice-row__avatar" aria-hidden="true"><Avatar name={p.name} size="voice" shape={p.bot ? 'bot' : 'circle'} /></span>
                <span className="d-voice-row__person-name">{p.name}</span>
                <span className="d-voice-row__states" id={`${statesId}-a`}>
                  {p.bot ? (
                    <>
                      {/* Signal §5.4 (fix round 1): the rounded-square avatar marks the bot; the word stays for a screen reader. */}
                      {' '}<span className="d-sr-only">{labels.state.bot}</span>
                      {' '}<span className="d-tag" data-kind="playbackOnly">{labels.playbackOnly}</span>
                    </>
                  ) : null}
                  {p.away === true && labels.away !== undefined ? <>{' '}<span className="d-voice-row__state" data-state="away">{labels.away}</span></> : null}
                  <span className="d-voice-row__fill" aria-hidden="true" />
                  <VoiceStates words={words.filter(w => !SLOT_STATES.has(w.key) && w.key !== 'speaking')} prefix="d-voice-row" />
                  {p.speaking ? <>{' '}<span className="d-sr-only">{labels.state.speaking}</span></> : null}
                </span>
                <TrailingSlot>
                  {words.some(w => SLOT_STATES.has(w.key)) ? (
                    <span className="d-voice-row__slot-states" id={`${statesId}-b`}><VoiceStates words={words.filter(w => SLOT_STATES.has(w.key))} prefix="d-voice-row" /></span>
                  ) : null}
                </TrailingSlot>
              </>}</PersonRow>
            );
          })}
        </ul>
      ) : null}
    </>
  );
}
