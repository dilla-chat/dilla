// Task 9 fix round 1 (Jonas): the search palette's result groups, as the Signal prototype draws them (Channels, People, Messages,
// Actions). Each group comes from a provider; the palette lists the providers' groups in order, so another feature adds a group
// by adding a provider (task 10's settings results plug in here).
import type { ChannelSummary, CommunitySummary, DmSummary } from '@dilla/client-core';
import type { IconName } from '@dilla/ui';
import { t } from '../strings/index.ts';

/** A result and what choosing it does: true when it went somewhere, false when the target is gone (the palette says so). */
export interface PaletteItem { id: string; label: string; sub?: string; icon?: IconName; avatar?: { name: string; bot?: boolean }; run: () => boolean | Promise<boolean> }
export interface PaletteGroup { id: string; label: string; items: PaletteItem[] }
/** One group for a query (trimmed, lower case), or null when it has nothing to say. */
export type PaletteProvider = (query: string) => PaletteGroup | null;

/** The groups of `providers` for `query`, in order, empty ones left out. */
export function paletteGroups(providers: readonly PaletteProvider[], query: string): PaletteGroup[] {
  const q = query.trim().toLowerCase();
  return providers.map(p => p(q)).filter((g): g is PaletteGroup => g !== null && g.items.length > 0);
}

/**
 * Channels: every text and voice channel of every server (not categories, not the AFK channel) whose name, or whose server's
 * name, holds the query; "{server}" or "{server} · voice" under it. All of them while nothing is typed, as the prototype.
 */
export function channelsProvider(communities: readonly CommunitySummary[], channels: readonly ChannelSummary[], afk: ReadonlySet<string>,
  open: (c: ChannelSummary) => boolean): PaletteProvider {
  return q => {
    const items: PaletteItem[] = [];
    for (const c of channels) {
      if (c.kind === 2 || afk.has(c.id) || items.length >= 10) continue;
      const server = communities.find(s => s.id === c.communityId)?.name ?? '';
      if (q !== '' && !c.name.toLowerCase().includes(q) && !server.toLowerCase().includes(q)) continue;
      items.push({ id: `ch:${c.id}`, icon: c.kind === 1 ? 'volume-2' : 'hash', label: c.name,
        sub: c.kind === 1 ? t('search.palette.voiceSub', { server }) : server, run: () => open(c) });
    }
    return { id: 'channels', label: t('search.group.channels'), items };
  };
}

/** People: the direct messages, by name: "@username · status" for a 1:1, "group DM · n people" for a group. */
export function peopleProvider(dms: readonly DmSummary[], line: (d: DmSummary) => string, open: (d: DmSummary) => boolean): PaletteProvider {
  return q => ({
    id: 'people', label: t('search.group.people'),
    items: dms.filter(d => q === '' || d.name.toLowerCase().includes(q)).slice(0, 6)
      .map(d => ({ id: `dm:${d.id}`, label: d.name, avatar: { name: d.name }, sub: line(d), run: () => open(d) })),
  });
}

export interface PaletteSetting { id: string; label: string; breadcrumb: string; run: () => boolean }
/**
 * Settings (Signal task 10 fix round 1: settings search moved from the workspace to ⌘K): up to 8 sections or rows whose words
 * hold the query, "{group} / {section}" under each; nothing while the query is empty. Picking one opens its section.
 */
export function settingsProvider(search: (query: string) => readonly PaletteSetting[]): PaletteProvider {
  return q => {
    if (q === '') return null;
    return { id: 'settings', label: t('search.group.settings'), items: search(q).slice(0, 8)
      .map(s => ({ id: `set:${s.id}`, label: s.label, sub: s.breadcrumb, icon: 'settings' as const, run: s.run })) };
  };
}

export interface PaletteMessage { id: string; snippet: string; location: string; author?: string; time?: string; current: boolean; run: () => Promise<boolean> }
/**
 * Messages: the worker's hits for two or more characters, the open conversation's first (the scope is implicit: here, then
 * everywhere). Each reads "#place · author · time" under the snippet.
 */
export function messagesProvider(hits: readonly PaletteMessage[]): PaletteProvider {
  return q => {
    if (q.length < 2) return null;
    const ordered = [...hits.filter(h => h.current), ...hits.filter(h => !h.current)];
    return { id: 'messages', label: t('search.group'), items: ordered.slice(0, 50).map(h => ({
      id: `msg:${h.id}`, icon: 'message-square' as const, label: h.snippet.replace(/\s+/g, ' '),
      sub: [h.location, h.author, h.time].filter((v): v is string => v !== undefined && v !== '').join(' · '), run: h.run,
    })) };
  };
}

export interface PaletteAction { id: string; label: string; sub?: string; icon: IconName; /** Words besides the label that find it. */ keywords?: string; run: () => boolean }
/** Actions: things the app really does from here (open settings, mute in a call, message someone), found by their words. */
export function actionsProvider(actions: readonly PaletteAction[]): PaletteProvider {
  return q => ({
    id: 'actions', label: t('search.group.actions'),
    items: actions.filter(a => q === '' || `${a.label} ${a.keywords ?? ''}`.toLowerCase().includes(q))
      .map(a => ({ id: `act:${a.id}`, label: a.label, icon: a.icon, ...(a.sub === undefined ? {} : { sub: a.sub }), run: a.run })),
  });
}
