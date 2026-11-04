import { useState, type ReactNode } from 'react';
import './ChannelHeader.css';
import { ActionMenu, type ActionMenuItem } from '../ActionMenu/ActionMenu.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { PhoneHeader } from '../PhoneHeader/PhoneHeader.tsx';

export interface ChannelHeaderProps {
  name: string;
  topic?: string;
  readable?: boolean;
  readableLabel?: string;
  kind?: 'channel' | 'dm' | 'announcement' | 'voice' | 'afk';
  /** A cluster of 32px `IconButton`s (R27). A child with `data-foldable` folds into the "⋯" menu below 36rem of header width. */
  actions?: ReactNode;
  /**
   * The "⋯" menu that holds the folded actions: `label` names it (`layout.header.more`), `items` mirror the foldable
   * actions in the same order, and `onSelect` receives the picked item's id. Shown only below 36rem of header width.
   */
  overflow?: { label: string; items: readonly ActionMenuItem[]; onSelect(id: string): void;
    /** Signal §5.5 (Ruling U): the menu shows at every width and holds what the prototype's header has no button for (pins,
     *  search, edit channel); nothing folds. */
    always?: boolean };
  /** On a phone: a PhoneHeader in place of the desktop header, with a back control and `menu` as its action. */
  phone?: { backLabel: string; onBack(): void; menu?: ReactNode; /** A call is up and the call bar has stepped aside while typing: the dot after the title, named by this. */ dot?: { label: string; onOpen: () => void } };
}

const GLYPH = { channel: 'hash', dm: 'at-sign', announcement: 'megaphone', voice: 'volume-2', afk: 'moon' } as const;

/**
 * The main pane's 44px header (Signal, README §5.5): a 14px glyph (hash for a channel, at-sign for a direct message, the
 * megaphone for an announcement channel, the speaker for voice; decorative, so the heading is named by the name alone) and
 * the name in 13.5px/700, the readable glyph when the server can read the channel, the topic in 12px, and the caller's
 * `actions` at the end. The glyph is its own flex item, a sibling of the name (C3). The title keeps 8rem and the name an
 * ellipsis; the topic gives way first. Below 36rem of the header's own width (a container query, so the aside being open
 * counts) the `data-foldable` actions hide and `overflow` shows (C1, R27). Without `overflow` nothing folds.
 */
function Glyph({ kind }: { kind: 'channel' | 'dm' | 'announcement' | 'voice' | 'afk' }) {
  return <span className="d-channel-header__glyph" aria-hidden="true"><Icon name={GLYPH[kind]} size={14} /></span>;
}

export function ChannelHeader({ name, topic, readable, readableLabel, kind = 'channel', actions, overflow, phone }: ChannelHeaderProps) {
  const [menuOpen, setMenuOpen] = useState(false);
  if (phone !== undefined) {
    return (
      <PhoneHeader backLabel={phone.backLabel} onBack={phone.onBack} actions={phone.menu} dot={phone.dot} topic={topic}
        marker={readable ? <span role="img" aria-label={readableLabel ?? 'Readable by this server'}><Icon name="eye" size={14} /></span> : undefined}
        title={<><Glyph kind={kind} /><span className="d-phone-header__text">{name}</span></>} />
    );
  }
  return (
    <header className="d-channel-header" data-overflow={overflow === undefined ? undefined : overflow.always === true ? 'always' : 'true'}>
      <div className="d-channel-header__title">
        <Glyph kind={kind} />
        <h2 className="d-channel-header__name">{name}</h2>
      </div>
      {readable ? <span className="d-channel-header__readable" role="img" aria-label={readableLabel ?? 'Readable by this server'}><Icon name="eye" size={14} /></span> : null}
      {topic ? <p className="d-channel-header__topic">{topic}</p> : null}
      {actions !== undefined || overflow !== undefined ? (
        <div className="d-channel-header__actions">
          {actions}
          {overflow !== undefined ? (
            <ActionMenu label={overflow.label} icon="ellipsis" className="d-channel-header__more" triggerClassName="d-channel-header__more-trigger"
              items={overflow.items} open={menuOpen} onOpenChange={setMenuOpen} onSelect={overflow.onSelect} />
          ) : null}
        </div>
      ) : null}
    </header>
  );
}
