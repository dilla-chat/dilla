import { Avatar } from '../Avatar/Avatar.tsx';
import './ReactionTooltip.css';

export interface ReactionTooltipProps { text: string; avatars: readonly { name: string }[] }

/** Content for Tooltip; the caller supplies the complete, localized reaction sentence. */
export function ReactionTooltip({ text, avatars }: ReactionTooltipProps) {
  return <span className="d-reaction-tooltip">
    <span className="d-reaction-tooltip__emoji" aria-hidden="true">{text.split(' ').at(-1)}</span>
    <span className="d-reaction-tooltip__body">
      <span className="d-reaction-tooltip__avatars" aria-hidden="true">{avatars.slice(0, 6).map((avatar, i) => <Avatar key={`${avatar.name}:${i}`} name={avatar.name} size="mini" />)}</span>
      <span>{text}</span>
    </span>
  </span>;
}
