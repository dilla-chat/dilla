import { SettingsNav } from '@dilla/ui';
import { useSlice } from '../core/use-slice.ts';
import { t } from '../strings/index.ts';
import { availableSettings, SETTINGS_ICONS, type SettingsAccess, type SettingsEntry, type SettingsScope } from './registry.ts';
import './workspace.css';

const SERVER = 'server:';
/** No server selected: a permissions slice no server has, so the hook reads nothing. */
const NONE = '0'.repeat(32);

export interface SettingsNavigationProps {
  /** The section on show, in either scope. */
  active: { scope: SettingsScope; id: string | null };
  /** The server whose group follows "You": the selected one, or none. */
  communityId: string | null;
  onUser: (id: SettingsEntry['id']) => void;
  onServer: (id: SettingsEntry['id']) => void;
  /** The nav's accessible name: the server frame keeps its own ("server settings sections"). */
  label?: string;
}

/**
 * The one settings workspace's nav (Signal task 10 fix round 1, README §5.9): the group "You", then the selected server's
 * group under its name with only the sections the person may use. Searching settings is the palette's (⌘K).
 */
export function SettingsNavigation({ active, communityId, onUser, onServer, label }: SettingsNavigationProps) {
  const communities = useSlice('communities');
  const access: SettingsAccess | undefined = useSlice(`permissions:${communityId ?? NONE}`);
  const server = communityId === null ? undefined : communities?.find(c => c.id === communityId);
  const serverItems = server === undefined || access?.loaded !== true ? [] : availableSettings('server', access)
    .map(entry => ({ id: `${SERVER}${entry.id}`, label: t(entry.labelKey), group: server.name, icon: SETTINGS_ICONS[entry.id] }));
  const items = [
    ...availableSettings('user').map(entry => ({ id: entry.id, label: t(entry.labelKey), group: t(entry.group), icon: SETTINGS_ICONS[entry.id] })),
    ...serverItems,
  ];
  const activeId = active.id === null ? '' : active.scope === 'server' ? `${SERVER}${active.id}` : active.id;
  return <SettingsNav label={label ?? t('settings.nav.label')} items={items} activeId={activeId}
    onSelect={id => (id.startsWith(SERVER) ? onServer(id.slice(SERVER.length) as SettingsEntry['id']) : onUser(id as SettingsEntry['id']))} />;
}
