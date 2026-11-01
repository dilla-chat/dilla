import { describe, expect, it } from 'vitest';
import { SETTINGS_REGISTRY, availableSettings, searchSettings } from './registry.ts';

describe('settings registry', () => {
  it('contains every user section and unified server section exactly once', () => {
    expect(availableSettings('user').map(s => s.id)).toEqual(['profile', 'signin', 'devices', 'notifications', 'voice', 'keyboard', 'appearance', 'privacy', 'diagnostics']); // Signal task 10: the prototype's order first
    expect(SETTINGS_REGISTRY.filter(s => s.scope === 'server').map(s => s.id)).toEqual(['overview', 'channels', 'gifs', 'members', 'roles', 'invites', 'selfRoles', 'bot', 'welcome', 'joinRoles', 'music', 'automoderation', 'moderationLog', 'bans', 'danger']);
  });
  it('shows nothing while server permissions load and only member pages without management rights', () => {
    expect(availableSettings('server')).toEqual([]);
    expect(availableSettings('server', { loaded: false, manageCommunity: true })).toEqual([]);
    expect(availableSettings('server', { loaded: true }).map(s => s.id)).toEqual(['members', 'selfRoles', 'danger']);
  });
  it('preserves join-role dual permission and separate audit-log authority', () => {
    const manager = { loaded: true, manageCommunity: true };
    expect(availableSettings('server', manager).map(s => s.id)).not.toContain('joinRoles');
    expect(availableSettings('server', manager).map(s => s.id)).not.toContain('moderationLog');
    expect(availableSettings('server', { ...manager, manageRoles: true, viewAuditLog: true }).map(s => s.id)).toEqual(expect.arrayContaining(['joinRoles', 'moderationLog']));
  });
  it('searches static synonyms with every term and never exposes forbidden settings', () => {
    expect(searchSettings('user', 'MICROPHONE input').some(r => r.section.id === 'voice')).toBe(true);
    expect(searchSettings('user', 'microphone bananas')).toEqual([]);
    expect(searchSettings('server', 'spotify', { loaded: true })).toEqual([]);
    expect(searchSettings('server', 'spotify', { loaded: true, manageCommunity: true }).map(r => r.section.id)).toEqual(expect.arrayContaining(['music']));
  });
  it('ranks exact labels before prefixes and uses static stable anchors', () => {
    expect(searchSettings('user', 'Profile')[0]?.label).toBe('Profile');
    const result = searchSettings('user', 'display name')[0];
    expect(result?.anchor).toBe('profile-display');
    expect(result?.section.id).toBe('profile');
    expect(searchSettings('user', '    ')).toEqual([]);
  });
});
