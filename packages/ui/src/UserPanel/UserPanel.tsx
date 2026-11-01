import { ActionMenu, type ActionMenuItem } from '../ActionMenu/ActionMenu.tsx';
import { Avatar, type Presence } from '../Avatar/Avatar.tsx';
import { VOICE_ICONS, VOICE_SHORTCUTS } from '../CallControls/CallControls.tsx';
import { useIconToolbar } from '../internal/iconToolbar.ts';
import { IconButton } from '../IconButton/IconButton.tsx';
import { useId, useState } from 'react';
import './UserPanel.css';

export interface UserPanelProps {
  name: string;
  username: string;
  /**
   * The line under the name (Signal §5.4): the person's status, such as the presence label ("online", "do not disturb"). Without
   * it the line is `@username`.
   */
  line?: string;
  muted: boolean;
  deafened: boolean;
  /** The mute and deafen shortcuts as the tooltips show them, formatted per platform by the caller. */
  shortcuts?: { mute: string; deafen: string };
  /** `region` is `panel.region` (the toolbar's name), `mute` and `deafen` are `voice.mute` and `voice.deafen`, `settings` is the gear's name. */
  labels: { region: string; mute: string; deafen: string; settings?: string };
  /**
   * Push-to-talk mode (web3.9): a small `tag` ("PTT", aria-hidden) sits on the mic button, whose accessible description gains
   * `description` ("push to talk"); the avatar wears the speaking ring while the key is `held`.
   */
  ptt?: { tag: string; description: string; held: boolean };
  onMute(): void;
  onDeafen(): void;
  /** With it (and `labels.settings`) a settings gear ends the panel. Signal's panel has none: the top bar holds the gear. */
  onSettings?(): void;
  /**
   * The status menu behind the avatar and name block. `label` names the button and the menu with the current status
   * (`panel.status`); `current` is the chosen status and draws the avatar's dot ("invisible" as the ring that stands for
   * offline); `items` are the statuses in order.
   */
  status: { current: 'online' | 'idle' | 'dnd' | 'invisible'; label: string; items: readonly ActionMenuItem[]; onChoose(id: string): void };
}

/** A shortcut as the platform formats it ('Ctrl+Shift+M' or '⌘⇧M') to the chips the tooltip draws; none without one. */
function chord(text: string | undefined): string[] | undefined {
  if (text === undefined || text === '') return undefined;
  return text.includes('+') ? text.split('+') : Array.from(text);
}

const DOT: Record<UserPanelProps['status']['current'], Presence> = { online: 'online', idle: 'idle', dnd: 'dnd', invisible: 'offline' };

/**
 * The desktop user panel at the bottom left (Signal, README §5.4), --panel-h (64) high: the 30px avatar with its status dot,
 * the name with `@username`, mute and deafen as 30px pressed toggles that own the shortcuts (`aria-keyshortcuts`), and the
 * settings gear only when `onSettings` is given. A plain div with no role and no name: "your account" is named once, by
 * the toolbar of its toggles (one roving Tab stop). The avatar and name block is a menu button for the status
 * (Discord-style), with the current status as a dot on the avatar.
 */
export function UserPanel({ name, username, line, muted, deafened, shortcuts, labels, onMute, onDeafen, onSettings, status, ptt }: UserPanelProps) {
  const [menuOpen, setMenuOpen] = useState(false);
  const nameId = useId();
  const bar = useIconToolbar();
  const pttId = useId();
  return (
    <div className="d-user-panel">
      <ActionMenu label={status.label} className="d-user-panel__menu" triggerClassName="d-user-panel__who" open={menuOpen} onOpenChange={setMenuOpen}
        labelledBy={nameId}
        items={status.items.map(item => ({ ...item, checked: item.id === status.current }))} onSelect={status.onChoose}
        triggerContent={<>
          {/* WCAG 2.5.3: the button's name holds its visible text, then the status. */}
          <span id={nameId} className="d-sr-only">{line === undefined ? `${name} @${username}, ${status.label}` : `${name}, ${status.label}`}</span>
          <span className="d-user-panel__avatar" data-speaking={ptt?.held === true ? 'true' : undefined}>
            <Avatar name={name} size="panel" presence={DOT[status.current]} />
          </span>
          <span className="d-user-panel__text">
            <span className="d-user-panel__name" title={name}>{name}</span>
            {line === undefined
              ? <span className="d-user-panel__username" title={`@${username}`}>@{username}</span>
              : <span className="d-user-panel__username" title={line}>{line}</span>}
          </span>
        </>} />
      <div ref={bar.ref} role="toolbar" aria-label={labels.region} className="d-user-panel__controls" onKeyDown={bar.onKeyDown} onFocus={bar.onFocus}>
        {(() => {
          const mic = <IconButton icon={muted ? VOICE_ICONS.mute.iconOn : VOICE_ICONS.mute.icon} label={labels.mute} pressed={muted}
            keys={chord(shortcuts?.mute)} aria-keyshortcuts={VOICE_SHORTCUTS.mute} onClick={onMute}
            {...(ptt !== undefined ? { 'aria-describedby': pttId } : {})} />;
          return ptt === undefined ? mic : (
            <span className="d-user-panel__mic">
              {mic}
              <span id={pttId} className="d-sr-only">{ptt.description}</span>
              <span className="d-user-panel__ptt" aria-hidden="true">{ptt.tag}</span>
            </span>
          );
        })()}
        <IconButton icon={deafened ? VOICE_ICONS.deafen.iconOn : VOICE_ICONS.deafen.icon} label={labels.deafen} pressed={deafened}
          keys={chord(shortcuts?.deafen)} aria-keyshortcuts={VOICE_SHORTCUTS.deafen} onClick={onDeafen} />
      </div>
      {onSettings !== undefined && labels.settings !== undefined ? <IconButton icon="settings" label={labels.settings} onClick={onSettings} /> : null}
    </div>
  );
}
