// The page's desktop notifications (Q30, L-TS-25). The worker only appends what may notify to the `notices` slice;
// the page decides, by the person's permission and settings, and builds the Notification itself. Badges are the
// in-app signal and never depend on any of this.
//
// Attacker statement (lesson e): the notifier refuses nothing. What it shows is the body of a message the core
// already decrypted and the timeline would render, on this device only, and only with the person's own permission.
import { effectiveNotifyMode, isMuted, mentionsDirectly, SETTINGS, suppressesEveryone } from '@dilla/client-core';
import type { CoreClient, Notice, Ring } from '@dilla/client-core';
import type { PermissionState } from './notify-permission.ts';
import { t } from './strings/index.ts';
import { mergeMembers, nameBook, plainBody } from './screens/shell-model.ts';
import { desktopBridge, desktopNotificationCtor } from './desktop.ts';
import { mobileBridge, mobileNotificationCtor } from './mobile.ts';

export interface NotificationHandle { onaction?: ((action: 'answer' | 'decline') => void) | null; onclick: ((ev: Event) => unknown) | null; close(): void; }
export interface NotificationCtor { readonly permission: NotificationPermission; new (title: string, options: NotificationOptions): NotificationHandle; }
export interface NotifyContext { permission: PermissionState; settings: Readonly<Record<string, string>>; visible: boolean; onScreen: string | null;
  presenceChoice?: string;
  /** The person's own user id and role ids, for a server that ignores @everyone and @here (WA5); either unset keeps every mention (fails open). */
  me?: string; roles?: readonly string[]; }

/** L-TS-25's decision for one notice; kind is the channel's for the mode: 0 a community channel, 3 | 4 a DM. */
export function shouldNotify(notice: Notice, kind: 0 | 3 | 4, ctx: NotifyContext): boolean {
  if (ctx.presenceChoice === undefined || ctx.presenceChoice === 'dnd') return false;
  if (ctx.permission !== 'granted') return false;
  // Settings → Notifications' Desktop notifications switch (Signal task 10): 'off' silences them on this device.
  if (ctx.settings[SETTINGS.notifyDesktop] === 'off') return false;
  const server = notice.communityId ?? undefined;
  if (isMuted(ctx.settings, notice.channelId, server)) return false;
  // The person is looking at it: the timeline shows it, so nothing pops up over it.
  if (ctx.visible && ctx.onScreen === notice.channelId) return false;
  const mode = effectiveNotifyMode(ctx.settings, notice.channelId, kind, server);
  if (mode === 'all') return true;
  // A server that ignores @everyone and @here turns a broadcast-only mention into a plain message.
  // It fails open: with the own user or roles unknown, a mention is never demoted.
  const broadcastOnly = notice.kind === 'mention' && server !== undefined && suppressesEveryone(ctx.settings, server)
    && ctx.me !== undefined && ctx.roles !== undefined && !mentionsDirectly(notice.body, ctx.me, ctx.roles);
  return mode === 'mentions' && !broadcastOnly && (notice.kind === 'mention' || notice.kind === 'dm');
}

/** notify.title.dm for a DM notice, else notify.title.channel; an unknown name reads as the first 8 hex of its id. */
export function noticeTitle(notice: Notice, names: { channel: string | null; server: string | null }): string {
  if (notice.communityId === null) {
    return t('notify.title.dm', { sender: notice.senderName !== '' ? notice.senderName : (notice.senderUser ?? notice.channelId).slice(0, 8) });
  }
  return t('notify.title.channel', {
    channel: names.channel ?? notice.channelId.slice(0, 8),
    server: names.server ?? notice.communityId.slice(0, 8),
  });
}

/** globalThis.Notification, or undefined where the browser has none. */
export function notificationCtor(): NotificationCtor | undefined {
  const n = (globalThis as { Notification?: unknown }).Notification;
  const mobile = mobileBridge();
  if (mobile !== undefined) return mobileNotificationCtor(mobile);
  const desktop = desktopBridge();
  if (desktop !== undefined) return desktopNotificationCtor(desktop, () => typeof n === 'function' ? (n as NotificationCtor).permission : 'denied');
  return typeof n === 'function' ? n as NotificationCtor : undefined;
}

export interface NotifierDeps {
  client: Pick<CoreClient, 'get' | 'subscribe'>;
  Notification: NotificationCtor | undefined;
  visible(): boolean;
  onScreen(): string | null;
  focus(): void;
  open(notice: Notice): void;
}

/** Follows the notices slice and shows what L-TS-25 allows; returns the unsubscribe. */
export function startNotifier(deps: NotifierDeps): () => void {
  const { client } = deps;
  // The first value seen only sets the mark: what the worker held before this page started is never replayed.
  let handledFrom: number | null = null;
  let stopped = false;

  const show = (item: Notice) => {
    const Ctor = deps.Notification;
    const kind: 0 | 3 | 4 = item.communityId === null
      ? client.get('dms')?.find(d => d.id === item.channelId)?.kind ?? 3
      : 0;
    const communityMembers = item.communityId === null ? undefined : client.get(`members:${item.communityId}`);
    const self = client.get('account')?.user ?? null;
    const selfId = self?.id;
    const ownMember = communityMembers?.find(m => m.userId === selfId);
    const ctx: NotifyContext = {
      me: selfId,
      roles: ownMember === undefined ? undefined : [...ownMember.roleIds],
      permission: Ctor?.permission ?? 'unsupported',
      settings: client.get('settings') ?? {},
      visible: deps.visible(),
      onScreen: deps.onScreen(),
      presenceChoice: client.get('account')?.presenceChoice,
    };
    if (Ctor === undefined || !shouldNotify(item, kind, ctx)) return;
    const communityId = item.communityId;
    const title = noticeTitle(item, {
      channel: communityId === null ? null : client.get(`channels:${communityId}`)?.find(c => c.id === item.channelId)?.name ?? null,
      server: communityId === null ? null : client.get('communities')?.find(c => c.id === communityId)?.name ?? null,
    });
    try {
      // the Global Constraints forbid a notification icon.
      const members = communityId !== null ? client.get(`members:${communityId}`) ?? []
        : mergeMembers((client.get('communities') ?? []).map(c => client.get(`members:${c.id}`)));
      const body = plainBody(item.body, nameBook(client.get('account')?.user ?? null, members));
      const note = new Ctor(title, { body, tag: `dilla:${item.channelId}`, silent: false });
      note.onclick = () => {
        deps.focus();
        deps.open(item);
        note.close();
      };
    } catch {
      // A browser that refuses to build one here (a page context without notifications) skips the item.
    }
  };

  const run = () => {
    if (stopped) return;
    const notices = client.get('notices');
    if (notices === undefined) return;
    if (handledFrom === null) {
      handledFrom = notices.nextId;
      return;
    }
    const from = handledFrom;
    handledFrom = notices.nextId;
    for (const item of notices.items) if (item.id >= from) show(item);
  };

  const unsubscribe = client.subscribe('notices', run);
  run();
  return () => {
    stopped = true;
    unsubscribe();
  };
}

export interface RingNotifyDeps {
  presenceChoice?: string;
  Notification: NotificationCtor | undefined;
  settings: Readonly<Record<string, string>>;
  /** The DM's kind for its notify mode: 3 a DM, 4 a group DM. */
  kind: 3 | 4;
  /** The server whose mute and mode apply to the ring's channel; null or unset for a DM (WA5). */
  communityId?: string | null;
  /** The page is visible: the in-app banner covers it, so no notification (flow 06, task-9 ruling). */
  visible: boolean;
  focus(): void;
  open(channelId: string): void;
  action?(action: 'answer' | 'decline'): void;
}

/**
 * The desktop notification of an incoming DM ring (flow 06, decision 6), or null when none may show: only while the page
 * is hidden (a visible page has the banner), only with the permission already granted (a ring never asks for it), never
 * for a busy ring (already in a call) or a quiet one (the
 * DM is muted, R1), and never when the DM's notify mode is nothing. One per call (`tag`); the caller closes it when
 * the ring ends. A click focuses the window, opens the DM and closes it.
 *
 * Attacker statement (lesson e): the notifier refuses nothing new. A ring reaches this page only after the worker
 * checked that the caller shares the DM with this person and that neither of the DM's groups is send-blocked (L-TS-56
 * step 8); what it shows is that caller's own display name.
 */
export function ringNotification(ring: Ring, deps: RingNotifyDeps): NotificationHandle | null {
  if (deps.presenceChoice === undefined || deps.presenceChoice === 'dnd') return null;
  const Ctor = deps.Notification;
  if (Ctor === undefined || Ctor.permission !== 'granted' || deps.visible) return null;
  const server = deps.communityId ?? undefined;
  if (ring.busy || ring.quiet || isMuted(deps.settings, ring.channelId, server)) return null;
  if (effectiveNotifyMode(deps.settings, ring.channelId, deps.kind, server) === 'nothing') return null;
  try {
    const note = new Ctor(t('call.ring.notifyTitle', { name: ring.callerName }),
      { body: t('call.ring.notifyBody'), tag: `dilla-ring-${ring.callId}-${ring.ringTs}`, silent: false });
    note.onaction = action => { note.close(); deps.action?.(action); };
    note.onclick = () => {
      deps.focus();
      deps.open(ring.channelId);
      note.close();
    };
    return note;
  } catch {
    return null;
  }
}
