import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { SettingsFrame } from '@dilla/ui';
import { useSlice } from '../../core/use-slice.ts';
import { phoneNow } from '../../layout.ts';
import { useRoute, type SettingsSection } from '../../router.ts';
import { SettingsEditProvider, useSettingsWorkspace } from '../../settings/edit-state.tsx';
import { SettingsNavigation } from '../../settings/SettingsNavigation.tsx';
import { availableSettings, type ServerSettingsSection, type SettingsAccess } from '../../settings/registry.ts';
import { useSettingsFocus } from '../../settings/use-settings-focus.ts';
import { t } from '../../strings/index.ts';
import { BansSection } from './BansSection.tsx';
import { ChannelsSection } from './ChannelsSection.tsx';
import { DangerSection } from './DangerSection.tsx';
import { InvitesSection } from './InvitesSection.tsx';
import { RolesSection } from './RolesSection.tsx';
import { OverviewSection } from './OverviewSection.tsx';
import { BotSettingsProvider, BotPage, WelcomePage, JoinRolesPage, MusicPage, AutomoderationPage, ModerationLogPage } from './BotSettingsPages.tsx';
import { SelfRolesSection } from './SelfRolesSection.tsx';
import { GifsSettings } from './GifsSettings.tsx';
import { MembersSection } from './MembersSection.tsx';

/** Kept for the shell's existing admin actions while routes move to ServerSettingsSection. */
export type AdminTab = ServerSettingsSection;

export interface ServerSettingsProps {
  communityId: string;
  tab: ServerSettingsSection | null;
  onTab: (tab: ServerSettingsSection | null) => void;
  onClose: () => void;
  announce: (message: string) => void;
  activity?: ReactNode;
  activityVisible?: boolean;
  onMessage?: (userId: string) => void;
  onCompare?: (userId: string) => void;
}

export function ServerSettings(props: ServerSettingsProps): React.JSX.Element {
  return <SettingsEditProvider><BotSettingsProvider communityId={props.communityId}><ServerSettingsWorkspace {...props} /></BotSettingsProvider></SettingsEditProvider>;
}

function ServerSettingsWorkspace({ communityId, tab, onTab, onClose, announce, activity, activityVisible, onMessage = () => undefined, onCompare = () => undefined }: ServerSettingsProps): React.JSX.Element {
  // README §5.10 (Signal task 10 fix round 1): no phone index; a server's settings open on its first section everywhere.
  const [route, navigate] = useRoute();
  const from = route.name === 'serverSettings' ? route.from : null;
  const communities = useSlice('communities');
  const community = communities?.find(c => c.id === communityId);
  const rawAccess = useSlice(`permissions:${communityId}`);
  const access: SettingsAccess | undefined = rawAccess;
  const account = useSlice('account');
  const removals = useSlice('removals');
  const allowed = useMemo(() => availableSettings('server', access), [access]);
  const active = (allowed.find(entry => entry.id === tab)?.id as ServerSettingsSection | undefined)
      ?? (tab === null || access?.loaded === true ? (allowed[0]?.id as ServerSettingsSection | undefined) ?? null : null);
  const [closed, setClosed] = useState(false);
  const [anchor, setAnchor] = useState<string | null>(null);
  const [announcement, setAnnouncement] = useState('');
  const workspace = useSettingsWorkspace();
  const told = useRef(false);
  const workspaceRef = useRef(workspace);
  const closeRef = useRef(onClose);
  const shellAnnounceRef = useRef(announce);
  workspaceRef.current = workspace;
  closeRef.current = onClose;
  shellAnnounceRef.current = announce;
  const previousActive = useRef<ServerSettingsSection | null>(null);
  const announcedLossFor = useRef<ServerSettingsSection | null>(null);
  const seenRemovalIds = useRef<Set<number> | null>(null);
  const latestAnnouncement = useRef('');
  const known = useRef(community);
  if (community !== undefined) known.current = community;
  const shown = community ?? known.current;
  const gone = community === undefined && (known.current !== undefined
    || communities !== undefined && account?.phase === 'ready');
  const announceInWorkspace = useCallback((message: string) => {
    if (told.current) {
      shellAnnounceRef.current(message);
      return;
    }
    latestAnnouncement.current = message;
    setAnnouncement(message);
  }, []);
  const finish = useCallback(() => {
    if (told.current) return;
    told.current = true;
    if (latestAnnouncement.current !== '') shellAnnounceRef.current(latestAnnouncement.current);
    latestAnnouncement.current = '';
    workspaceRef.current?.clear();
    setClosed(true);
    closeRef.current();
  }, []);

  useEffect(() => { if (gone) finish(); }, [gone, finish]); // leave/delete drop the server before the request resolves
  useEffect(() => {
    if (removals === undefined) return;
    if (seenRemovalIds.current === null) {
      seenRemovalIds.current = new Set(removals.map(removal => removal.id));
      return;
    }
    const fresh = removals.filter(removal => !seenRemovalIds.current?.has(removal.id));
    for (const removal of fresh) seenRemovalIds.current.add(removal.id);
    if (fresh.some(removal => removal.communityId === communityId)) finish();
  }, [removals, communityId, finish]);
  useEffect(() => {
    if (access?.loaded !== true) {
      if (previousActive.current !== null) {
        workspace?.clear();
        latestAnnouncement.current = '';
        setAnnouncement(t('workspace.permissionChanged'));
      }
      previousActive.current = null;
      return;
    }
    const lost = previousActive.current !== null && !allowed.some(entry => entry.id === previousActive.current);
    const requestedAllowed = tab === null || allowed.some(entry => entry.id === tab);
    const newRequestedLoss = tab !== null && !requestedAllowed && announcedLossFor.current !== tab;
    if (lost || newRequestedLoss) {
      workspace?.clear();
    }
    if (newRequestedLoss && tab !== null) {
      setAnnouncement(t('workspace.permissionChanged'));
      announcedLossFor.current = tab;
      if (active !== null) onTab(active);
    } else if (tab === null || allowed.some(entry => entry.id === tab)) {
      announcedLossFor.current = null;
    }
    previousActive.current = active;
  }, [access?.loaded, allowed, active, onTab, tab, workspace]);

  const select = (section: ServerSettingsSection | null, nextAnchor: string | null = null) => {
    setAnchor(nextAnchor);
    setAnnouncement('');
    latestAnnouncement.current = '';
    if (section === tab) return;
    onTab(section);
  };
  const close = onClose;

  const contentRef = useSettingsFocus(active, anchor);
  const entry = allowed.find(item => item.id === active);
  const footer = workspace?.footer ?? null;
  const common = { communityId, serverName: shown?.name ?? '', announce: announceInWorkspace };
  const content = useMemo(() => {
    if (!access?.loaded || active === null || shown === undefined) return null;
    switch (active) {
      case 'overview': return <OverviewSection {...common} owner={access.owner === true} onGone={finish} />;
      case 'channels': return <ChannelsSection {...common} />;
      case 'gifs': return <section className="dw-settings-section"><h2 tabIndex={-1}>{t('workspace.gifs')}</h2><GifsSettings communityId={communityId} announce={announceInWorkspace} /></section>;
      case 'members': return <MembersSection communityId={communityId} selfId={account?.user?.id ?? ''} onMessage={onMessage} onCompare={onCompare} announce={announceInWorkspace} />;
      case 'roles': return <RolesSection {...common} />;
      case 'invites': return <InvitesSection {...common} />;
      case 'selfRoles': return <SelfRolesSection {...common} />;
      case 'bot': return <BotPage {...common} />;
      case 'welcome': return <WelcomePage {...common} />;
      case 'joinRoles': return <JoinRolesPage {...common} />;
      case 'music': return <MusicPage {...common} />;
      case 'automoderation': return <AutomoderationPage {...common} />;
      case 'moderationLog': return <ModerationLogPage {...common} />;
      case 'bans': return <BansSection {...common} />;
      case 'danger': return <DangerSection {...common} owner={access.owner === true} onGone={finish} />;
      default: return null;
    }
  // These components own their draft state. Recreate the selected section only when its identity changes.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active, access?.loaded, access?.owner, communityId, shown?.name, account?.user?.id, onMessage, onCompare, announceInWorkspace]);

  return <>
    <SettingsFrame open={!closed && !gone} title={t('admin.title')} closeLabel={t('settings.close')} onClose={close}
      nav={<SettingsNavigation active={{ scope: 'server', id: active }} communityId={communityId} label={t('admin.nav')}
        onServer={id => select(id as ServerSettingsSection)}
        onUser={id => void navigate({ name: 'settings', section: id as SettingsSection, from }, !phoneNow())} />}
      footer={footer} phonePage="section" contentRef={contentRef} wide={entry?.wide}
      overlays={workspace?.confirmation} activity={activity} activityVisible={activityVisible}>
      {!access?.loaded ? <p aria-busy="true">{t('admin.loading')}</p> : null}
      <p className="d-sr-only" role="status" aria-live="polite">{announcement}</p>
      {content}

    </SettingsFrame>
  </>;
}
