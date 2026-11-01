import type { IconName } from '@dilla/ui';
import { t, type StringKey } from '../strings/index.ts';

export type SettingsSection = 'profile' | 'signin' | 'devices' | 'appearance' | 'notifications' | 'voice' | 'keyboard' | 'privacy' | 'diagnostics';
export type ServerSettingsSection = 'overview' | 'channels' | 'gifs' | 'members' | 'roles' | 'invites' | 'selfRoles' | 'bot' | 'welcome' | 'joinRoles' | 'music' | 'automoderation' | 'moderationLog' | 'bans' | 'danger';
export type SettingsScope = 'user' | 'server';
/** Advisory UI gates; the worker and runtime independently authorize every operation. */
export interface SettingsAccess {
  loaded: boolean;
  manageCommunity?: boolean;
  manageChannels?: boolean;
  manageRoles?: boolean;
  createInvite?: boolean;
  banMembers?: boolean;
  viewAuditLog?: boolean;
  owner?: boolean;
}
export interface SettingAnchor {
  id: string;
  labelKey: StringKey;
  descriptionKey?: StringKey;
  keywords: readonly string[];
}
export interface SettingsEntry {
  id: SettingsSection | ServerSettingsSection;
  scope: SettingsScope;
  group: StringKey;
  labelKey: StringKey;
  keywords: readonly string[];
  visible(access?: SettingsAccess): boolean;
  anchors: readonly SettingAnchor[];
  wide?: boolean;
}
const member = (access?: SettingsAccess) => access?.loaded === true;
const can = (key: keyof SettingsAccess) => (access?: SettingsAccess) => member(access) && access?.[key] === true;
const anchor = (id: string, labelKey: StringKey, ...keywords: string[]): SettingAnchor => ({ id, labelKey, keywords });
const user = (id: SettingsSection, group: StringKey, keywords: string[], anchors: SettingAnchor[] = []): SettingsEntry => ({
  id, scope: 'user', group, labelKey: `workspace.${id}`, keywords, visible: () => true, anchors,
});
const server = (id: ServerSettingsSection, group: StringKey, visible: SettingsEntry['visible'], keywords: string[], anchors: SettingAnchor[] = [], wide = false): SettingsEntry => ({
  id, scope: 'server', group, labelKey: `workspace.${id}`, visible, keywords, anchors, wide,
});

/** Static metadata only. Search must never index configuration, account or member values. */
export const SETTINGS_REGISTRY: readonly SettingsEntry[] = [
  user('profile', 'workspace.group.you', ['account', 'identity'], [
    anchor('profile-display', 'profile.display.label', 'name', 'nickname'),
    anchor('profile-status', 'profile.status.label', 'presence', 'message'),
    anchor('profile-bio', 'profile.bio.label', 'biography', 'about'),
  ]),
  user('signin', 'workspace.group.you', ['password', 'authentication', 'totp', 'passkey', 'recovery', 'security', 'two factor'], [
    anchor('signin-password', 'factorSettings.password', 'change', 'password'),
    anchor('signin-totp', 'factorSettings.totp', 'authenticator', 'two factor'),
    anchor('signin-passkeys', 'factorSettings.passkeys', 'security key', 'webauthn'),
    anchor('signin-codes', 'factorSettings.codes', 'backup', 'recovery'),
  ]),
  user('devices', 'workspace.group.you', ['rename', 'revoke', 'recovery key', 'sign out', 'forget', 'sessions']),
  user('notifications', 'workspace.group.you', ['sound', 'mute', 'browser', 'permission', 'mentions', 'channel', 'server'], [
    anchor('notify-server', 'notify.server.hint', 'server', 'mentions', 'default'),
    anchor('notify-everyone', 'notify.everyone.label', 'everyone', 'here'),
    anchor('notify-desktop', 'notify.desktop.label', 'browser', 'permission', 'desktop'),
    anchor('notify-sounds', 'notify.sounds.label', 'join', 'leave', 'ring', 'sound'),
  ]),
  user('voice', 'workspace.group.you', ['microphone', 'input', 'output', 'speaker', 'permission', 'push to talk', 'ptt', 'noise', 'test'], [
    anchor('voice-test', 'signalSettings.voice.testStart', 'microphone', 'input', 'test', 'sensitivity', 'threshold', 'volume'),
  ]),
  user('keyboard', 'workspace.group.you', ['shortcut', 'binding', 'keys', 'push to talk']),
  user('appearance', 'workspace.group.you', ['theme', 'dark', 'light', 'contrast', 'density', 'motion'], [
    anchor('appearance-theme', 'appearance.theme.label', 'dark', 'light', 'system', 'contrast'),
    anchor('appearance-density', 'workspace.density', 'compact', 'regular'),
    anchor('appearance-motion', 'workspace.motion', 'animation', 'accessibility'),
  ]),
  user('privacy', 'workspace.group.you', ['contact', 'verification', 'safety number', 'changed key', 'compare']),
  user('diagnostics', 'workspace.group.you', ['connection', 'instance', 'device', 'error', 'copy', 'debug']),
  server('overview', 'workspace.group.server', can('manageCommunity'), ['name', 'afk', 'timeout', 'idle']),
  server('channels', 'workspace.group.server', can('manageChannels'), ['category', 'text', 'voice', 'order', 'permissions'], [], true),
  server('gifs', 'workspace.group.server', can('manageCommunity'), ['giphy', 'provider', 'key', 'rating']),
  server('members', 'workspace.group.people', member, ['people', 'role', 'kick', 'ban', 'timeout', 'warn'], [], true),
  server('roles', 'workspace.group.people', can('manageRoles'), ['create', 'edit', 'order', 'permissions']),
  server('invites', 'workspace.group.people', can('createInvite'), ['invitation', 'join', 'link']),
  server('selfRoles', 'workspace.group.people', member, ['self service', 'personal roles', 'role panel', 'publish']),
  server('bot', 'workspace.group.automation', can('manageCommunity'), ['installation', 'enable', 'disable', 'display name', 'capabilities']),
  server('welcome', 'workspace.group.automation', can('manageCommunity'), ['greeting', 'destination', 'preview', 'join message']),
  server('joinRoles', 'workspace.group.automation', a => can('manageCommunity')(a) && can('manageRoles')(a), ['automatic', 'new member', 'join role']),
  server('music', 'workspace.group.automation', can('manageCommunity'), ['dj roles', 'spotify', 'youtube', 'provider', 'link', 'client id', 'playback'], [
    anchor('music-providers', 'workspace.providers.title', 'spotify', 'youtube', 'linked', 'playback'),
  ]),
  server('automoderation', 'workspace.group.moderation', can('manageCommunity'), ['automatic', 'rules', 'actions', 'exemptions', 'filter']),
  server('moderationLog', 'workspace.group.moderation', can('viewAuditLog'), ['audit', 'cases', 'evidence', 'manual'], [], true),
  server('bans', 'workspace.group.moderation', can('banMembers'), ['unban', 'banned'], [], true),
  server('danger', 'workspace.group.actions', member, ['ownership', 'transfer', 'delete', 'leave']),
];

/**
 * Each section's 15px nav glyph (README §5.9; task 10). The prototype's own: Profile user, Sign-in lock, Devices
 * monitor-smartphone, Notifications bell, Voice mic, Keyboard keyboard, Overview server, Roles shield, Invites link,
 * Bot bot, Music music. The others are the closest glyph the vendored Lucide set holds.
 */
export const SETTINGS_ICONS: Readonly<Partial<Record<SettingsEntry['id'], IconName>>> = {
  profile: 'user', signin: 'lock', devices: 'monitor-smartphone', appearance: 'palette', notifications: 'bell', voice: 'mic',
  keyboard: 'keyboard', privacy: 'fingerprint', diagnostics: 'activity',
  overview: 'server', channels: 'hash', gifs: 'smile', members: 'users', roles: 'shield', invites: 'link', selfRoles: 'user',
  bot: 'bot', welcome: 'megaphone', joinRoles: 'user-plus', music: 'music', automoderation: 'shield', moderationLog: 'scroll-text', bans: 'ban',
  danger: 'triangle-alert',
};

export function availableSettings(scope: SettingsScope, access?: SettingsAccess): readonly SettingsEntry[] {
  return SETTINGS_REGISTRY.filter(entry => entry.scope === scope && entry.visible(access));
}
export interface SettingsSearchResult { section: SettingsEntry; label: string; breadcrumb: string; anchor: string | null }
export function searchSettings(scope: SettingsScope, query: string, access?: SettingsAccess): SettingsSearchResult[] {
  const normalized = query.trim().toLocaleLowerCase();
  if (!normalized) return [];
  const terms = normalized.split(/\s+/);
  const matches: (SettingsSearchResult & { rank: number })[] = [];
  for (const section of availableSettings(scope, access)) {
    const breadcrumb = `${t(section.group)} / ${t(section.labelKey)}`;
    const entries = [{ id: null, labelKey: section.labelKey, keywords: section.keywords }, ...section.anchors];
    for (const entry of entries) {
      const label = t(entry.labelKey);
      const lower = label.toLocaleLowerCase();
      const description = 'descriptionKey' in entry && entry.descriptionKey ? t(entry.descriptionKey) : '';
      const haystack = `${label} ${description} ${breadcrumb} ${entry.keywords.join(' ')}`.toLocaleLowerCase();
      if (!terms.every(term => haystack.includes(term))) continue;
      matches.push({ section, label, breadcrumb, anchor: entry.id, rank: lower === normalized ? 0 : lower.startsWith(normalized) ? 1 : 2 });
    }
  }
  return matches.sort((a, b) => a.rank - b.rank).map(({ section, label, breadcrumb, anchor }) => ({ section, label, breadcrumb, anchor }));
}
