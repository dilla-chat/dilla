import { useId } from 'react';
import { t } from '../../strings/index.ts';
import { MemberPane } from './MemberPane.tsx';

export interface MembersSectionProps {
  communityId: string;
  selfId: string;
  onMessage(userId: string): void;
  onCompare(userId: string): void;
  announce(message: string): void;
}

/** Member list and the existing profile/moderation actions, scoped to this settings pane. */
export function MembersSection(props: MembersSectionProps): React.JSX.Element {
  const titleId = useId();
  return <section className="dw-settings-section" aria-labelledby={titleId}>
    <h2 id={titleId} tabIndex={-1}>{t('workspace.members')}</h2>
    <MemberPane {...props} focusScope="settings" />
  </section>;
}
