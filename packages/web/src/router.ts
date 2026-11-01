import { useSyncExternalStore } from 'react';
import { errorOf, type UiError } from './core/errors.ts';
import type { PhoneTab } from './layout.ts';
import type { ServerSettingsSection, SettingsSection } from './settings/registry.ts';
import { SETTINGS_REGISTRY } from './settings/registry.ts';

export type { ServerSettingsSection, SettingsSection } from './settings/registry.ts';

export type SignInNotice = 'race' | 'evicted';
export type Route = { name: 'root' } | { name: 'welcome'; invite: string | null; signin: SignInNotice | null }
  | { name: 'channel'; communityId: string; channelId: string | null }
  | { name: 'dm'; channelId: string }
  /** Home with the `@` tile selected and no DM open (`/dm`). */
  | { name: 'dms' } | { name: 'activity' } | { name: 'you' }
  | { name: 'spotifyCallback'; search: string }
  | { name: 'settings'; section: SettingsSection | null; from: string | null;
    /** Privacy only: the contact whose comparison opens at once (the voice pause banner's action, flow 06). */
    compare?: string }
  | { name: 'serverSettings'; communityId: string; section: ServerSettingsSection | null; from: string | null };

/** The history-state key that carries a settings route's `from`: the path Settings was opened over. */
export const SETTINGS_FROM = 'settingsFrom';
/** The history-state key that carries a privacy route's `compare`: a 32-hex user id, nothing else. */
export const SETTINGS_COMPARE = 'compareUser';

/** The history-state key that marks an entry this app pushed on a phone: back is then `history.back()`, else a deep link's fallback. */
export const PHONE_BACK = 'phoneBack';
/** The path of the screen a pushed phone view was pushed from, kept beside `phoneBack` in the history state. */
const PHONE_FROM = 'phoneFrom';
const HISTORY_INDEX = 'dillaHistoryIndex';

const DM = /^\/dm\/([0-9a-f]{32})$/;
const SETTINGS_SECTION = /^\/settings\/([^/]+)$/;
const SERVER_SETTINGS_SECTION = /^\/c\/([0-9a-f]{32})\/settings(?:\/([^/]+))?$/;
const HEX32 = /^[0-9a-f]{32}$/;

let nextHistoryIndex = 1;
let currentHistoryIndex = 0;
function stateWithIndex(state: unknown, index: number): Record<string, unknown> {
  return { ...(typeof state === 'object' && state !== null ? state as Record<string, unknown> : {}), [HISTORY_INDEX]: index };
}
function entryIndex(state: unknown): number | null {
  if (typeof state !== 'object' || state === null) return null;
  const index = (state as Record<string, unknown>)[HISTORY_INDEX];
  return Number.isSafeInteger(index) && (index as number) >= 0 ? index as number : null;
}
function ensureInitialIndex(): void {
  const existing = entryIndex(history.state);
  if (existing !== null) { currentHistoryIndex = existing; nextHistoryIndex = Math.max(nextHistoryIndex, existing + 1); return; }
  currentHistoryIndex = 0;
  nextHistoryIndex = 1;
  history.replaceState(stateWithIndex(history.state, currentHistoryIndex), '', location.href);
}
if (typeof window !== 'undefined') ensureInitialIndex();

/**
 * The `from` a history state names: a same-origin path (one leading slash, never two) that is not itself a
 * settings path, so a crafted state can name only a page of this origin and never loops back into Settings.
 */
function fromState(state: unknown): string | null {
  if (typeof state !== 'object' || state === null) return null;
  const from = (state as Record<string, unknown>)[SETTINGS_FROM];
  if (typeof from !== 'string') return null;
  if (!from.startsWith('/') || from.startsWith('//') || from.startsWith('/settings') || /^\/c\/[0-9a-f]{32}\/settings(?:\/|$)/.test(from)) return null;
  return from;
}

/** The comparison a privacy route's history state asks for: a 32-hex user id, else nothing. */
function compareState(state: unknown): { compare?: string } {
  if (typeof state !== 'object' || state === null) return {};
  const user = (state as Record<string, unknown>)[SETTINGS_COMPARE];
  return typeof user === 'string' && HEX32.test(user) ? { compare: user } : {};
}

export function parseRoute(pathname: string, search: string, state: unknown = null): Route {
  if (pathname === '/settings/bot/music/spotify/callback') return { name: 'spotifyCallback', search };
  if (pathname === '/') return { name: 'root' };
  if (pathname === '/welcome') {
    const params = new URLSearchParams(search);
    const signin = params.get('signin');
    return { name: 'welcome', invite: params.get('invite') || null, signin: signin === 'race' || signin === 'evicted' ? signin : null };
  }
  const server = SERVER_SETTINGS_SECTION.exec(pathname);
  if (server) {
    const allowed = SETTINGS_REGISTRY.filter(entry => entry.scope === 'server').map(entry => entry.id);
    const raw = server[2];
    return { name: 'serverSettings', communityId: server[1], section: raw && allowed.some(id => id === raw) ? raw as ServerSettingsSection : null, from: fromState(state) };
  }
  const match = /^\/c\/([0-9a-f]{32})(?:\/([0-9a-f]{32}))?$/.exec(pathname);
  if (match) return { name: 'channel', communityId: match[1], channelId: match[2] ?? null };
  const dm = DM.exec(pathname);
  if (dm) return { name: 'dm', channelId: dm[1] };
  if (pathname === '/dm') return { name: 'dms' };
  if (pathname === '/activity') return { name: 'activity' };
  if (pathname === '/you') return { name: 'you' };
  if (pathname === '/settings') return { name: 'settings', section: null, from: fromState(state) };
  const section = SETTINGS_SECTION.exec(pathname);
  if (section) {
    const name = section[1] as SettingsSection;
    const allowed = SETTINGS_REGISTRY.filter(entry => entry.scope === 'user').map(entry => entry.id);
    if (!allowed.some(id => id === name)) return { name: 'settings', section: null, from: fromState(state) };
    return { name: 'settings', section: name, from: fromState(state), ...(name === 'privacy' ? compareState(state) : {}) };
  }
  return { name: 'root' };
}

export function routePath(route: Route): string {
  switch (route.name) {
    case 'root': return '/';
    case 'welcome': {
      const parts: string[] = [];
      if (route.invite) parts.push(`invite=${encodeURIComponent(route.invite)}`);
      if (route.signin !== null) parts.push(`signin=${route.signin}`);
      return parts.length > 0 ? `/welcome?${parts.join('&')}` : '/welcome';
    }
    case 'channel': return `/c/${route.communityId}${route.channelId ? `/${route.channelId}` : ''}`;
    case 'dm': return `/dm/${route.channelId}`;
    case 'dms': return '/dm';
    case 'activity': return '/activity';
    case 'you': return '/you';
    case 'spotifyCallback': return `/settings/bot/music/spotify/callback${route.search}`;
    case 'settings': return route.section === null ? '/settings' : `/settings/${route.section}`;
    case 'serverSettings': return `/c/${route.communityId}/settings${route.section === null ? '' : `/${route.section}`}`;
  }
}

/** parseRoute of a path that may carry a ?search, with no history state. */
export function routeOf(path: string): Route {
  const mark = path.indexOf('?');
  return parseRoute(mark < 0 ? path : path.slice(0, mark), mark < 0 ? '' : path.slice(mark), null);
}

/**
 * The route the shell shows (L-TS-27): under a settings route, the route Settings was opened over (`/` when it
 * names none); any other route as it is. `fromState` never yields a settings path, so the result is never one.
 */
export function baseRoute(route: Route): Route {
  if (route.name !== 'settings' && route.name !== 'serverSettings') return route;
  return route.from === null ? { name: 'root' } : routeOf(route.from);
}

const listeners = new Set<() => void>();
const committedTraversalListeners = new Set<() => void>();
function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}
let routeRevision = 0;
const snapshot = () => `${location.pathname}${location.search}#${routeRevision}`;
function notifyRoute(): void { routeRevision++; for (const listener of listeners) listener(); }
function notifyCommittedTraversal(): void { for (const listener of committedTraversalListeners) listener(); }

/** Subscribe to browser traversals after the guard accepts and the browser lands on the destination entry. */
export function subscribeCommittedTraversal(listener: () => void): () => void {
  committedTraversalListeners.add(listener);
  return () => { committedTraversalListeners.delete(listener); };
}

/** App history writes that do not change the URL (for example phone sheets) must keep the entry index. */
export function replaceHistoryState(state: unknown, title = '', url?: string | URL): void {
  if (url === undefined) history.replaceState(stateWithIndex(state, currentHistoryIndex), title);
  else history.replaceState(stateWithIndex(state, currentHistoryIndex), title, url);
  notifyRoute();
}
/** Push an indexed app history entry, including same-URL presentation state. */
export function pushHistoryState(state: unknown, title = '', url?: string | URL): void {
  currentHistoryIndex = nextHistoryIndex;
  nextHistoryIndex = currentHistoryIndex + 1;
  if (url === undefined) history.pushState(stateWithIndex(state, currentHistoryIndex), title);
  else history.pushState(stateWithIndex(state, currentHistoryIndex), title, url);
  notifyRoute();
}

type PendingTraversal = { delta: number; origin: number; phase: 'restore' | 'confirm' | 'replay' };
let navigationGuard: (() => Promise<boolean>) | null = null;
let pendingTraversal: PendingTraversal | null = null;
let guardRequest: Promise<boolean> | null = null;
let cancelGuardRequest: (() => void) | null = null;
let navigationRequestPending = false;
let approvedActionDepth = 0;
let internalPopExpected: number | null = null;

function cancelActiveGuardRequest(): void {
  cancelGuardRequest?.();
  navigationRequestPending = false;
  if (pendingTraversal?.phase === 'confirm') {
    pendingTraversal = null;
    internalPopExpected = null;
  }
}

function runGuard(): Promise<boolean> {
  if (!navigationGuard) return Promise.resolve(true);
  if (guardRequest) return guardRequest;
  let resolveTask!: (allowed: boolean) => void;
  const task = new Promise<boolean>(resolve => { resolveTask = resolve; });
  guardRequest = task;
  const settle = (allowed: boolean) => {
    if (guardRequest === task) {
      guardRequest = null;
      cancelGuardRequest = null;
    }
    resolveTask(allowed);
  };
  cancelGuardRequest = () => settle(false);
  void Promise.resolve().then(() => navigationGuard?.() ?? true).then(settle, () => settle(false));
  return task;
}

/** Install the app-wide navigation guard. The returned disposer clears only this registration. */
export function setNavigationGuard(guard: (() => Promise<boolean>) | null): () => void {
  if (navigationGuard !== guard) cancelActiveGuardRequest();
  navigationGuard = guard;
  return () => {
    if (navigationGuard !== guard) return;
    navigationGuard = null;
    cancelActiveGuardRequest();
  };
}

/** Ask permission before running a non-router departure action. Returns false if the user keeps editing. */
export function requestNavigation(action: () => void): Promise<boolean> {
  if (approvedActionDepth > 0) {
    action();
    return Promise.resolve(true);
  }
  if (pendingTraversal || navigationRequestPending) return Promise.resolve(false);
  if (!navigationGuard) {
    action();
    return Promise.resolve(true);
  }
  navigationRequestPending = true;
  return runGuard().then(allowed => {
    if (allowed) {
      approvedActionDepth++;
      try { action(); } finally { approvedActionDepth--; }
    }
    return allowed;
  }).finally(() => { navigationRequestPending = false; });
}

function onPopState(event: PopStateEvent): void {
  const target = entryIndex(event.state);
  if (internalPopExpected !== null && target === internalPopExpected) {
    internalPopExpected = null;
    currentHistoryIndex = target;
    const pending = pendingTraversal;
    if (!pending) { nextHistoryIndex = target + 1; notifyRoute(); return; }
    if (pending.phase === 'restore') {
      pending.phase = 'confirm';
      void runGuard().then(allowed => {
        if (pendingTraversal !== pending) return;
        if (allowed) {
          pending.phase = 'replay';
          internalPopExpected = pending.origin + pending.delta;
          history.go(pending.delta);
        } else {
          pendingTraversal = null;
          nextHistoryIndex = pending.origin + 1;
          notifyRoute();
        }
      });
      return;
    }
    if (pending.phase === 'replay') {
      pendingTraversal = null;
      nextHistoryIndex = target + 1;
      notifyRoute();
      notifyCommittedTraversal();
      return;
    }
  }
  if (navigationRequestPending && target !== null && target !== currentHistoryIndex) {
    // A browser traversal while an imperative navigation is asking for confirmation cannot replace that request.
    internalPopExpected = currentHistoryIndex;
    history.go(currentHistoryIndex - target);
    return;
  }
  if (pendingTraversal) {
    // A second browser traversal during confirmation is rolled back to the still-visible entry.
    const origin = pendingTraversal.origin;
    const current = target ?? currentHistoryIndex;
    if (current !== origin) { internalPopExpected = origin; history.go(origin - current); }
    return;
  }
  // Legacy/raw pop fixtures and non-app entries can be unindexed. We cannot safely compute a reversible step for them,
  // so update the observed route without arming a traversal that could wait forever for a nonexistent restore event.
  if (target === null) { notifyRoute(); return; }
  const delta = target - currentHistoryIndex;
  if (delta === 0) { notifyRoute(); return; }
  if (!navigationGuard) { currentHistoryIndex = target; nextHistoryIndex = target + 1; notifyRoute(); notifyCommittedTraversal(); return; }
  const pending: PendingTraversal = { delta, origin: currentHistoryIndex, phase: 'restore' };
  pendingTraversal = pending;
  // popstate cannot be cancelled: reverse to the current indexed entry before opening async confirmation.
  internalPopExpected = pending.origin;
  history.go(-delta);
}
if (typeof window !== 'undefined') window.addEventListener('popstate', onPopState);
/**
 * A settings route records where Settings was opened from in the history state: the current path when the
 * current route is not a settings route, else the current route's own `from` (a section switch keeps it). The
 * `from` of the route given is ignored. Every other route writes no state.
 */
function writeRoute(route: Route, replace = false, opts?: { phoneBack?: boolean; from?: string | null }): void {
  const path = routePath(route);
  if (!replace && path === location.pathname + location.search) return;
  let state: { [SETTINGS_FROM]?: string | null; [SETTINGS_COMPARE]?: string; [PHONE_BACK]?: true; [PHONE_FROM]?: string } | null = null;
  if (route.name === 'settings' || route.name === 'serverSettings') {
    const current = parseRoute(location.pathname, location.search, history.state);
    state = { [SETTINGS_FROM]: opts?.from !== undefined ? opts.from : current.name === 'settings' || current.name === 'serverSettings' ? current.from : location.pathname + location.search };
    if (route.name === 'settings' && route.section === 'privacy' && route.compare !== undefined && HEX32.test(route.compare)) state[SETTINGS_COMPARE] = route.compare;
  }
  if (opts?.phoneBack === true) state = { ...state, [PHONE_BACK]: true, [PHONE_FROM]: location.pathname + location.search };
  if (replace) replaceHistoryState(state, '', path);
  else pushHistoryState(state, '', path);
}

function navigate(route: Route, replace = false, opts?: { phoneBack?: boolean; from?: string | null }): Promise<boolean> {
  if (!replace && routePath(route) === location.pathname + location.search && route.name !== 'settings' && route.name !== 'serverSettings') return Promise.resolve(true);
  return requestNavigation(() => writeRoute(route, replace, opts));
}

/**
 * Back on a phone's pushed view: `history.back()` when this entry was pushed by the app (`phoneBack`), else (a deep
 * link, a reload into the view) the tab's list route, replacing the entry, so back never leaves the app.
 *
 * With `expect` (N5), the entry back lands on may not be the one the caller means: after the system forward button the
 * entry below is a forward one. A one-shot committed-traversal listener checks where back landed and, when that is not
 * `expect`, replaces with `expect`. It returns a function that removes that listener (a caller that unmounts before the
 * traversal commits calls it).
 */
export function goBack(fallback: Route, go: (route: Route, replace?: boolean) => void | Promise<boolean>, expect?: Route): () => void {
  const state: unknown = history.state;
  if (typeof state === 'object' && state !== null && (state as Record<string, unknown>)[PHONE_BACK] === true) {
    let off = () => undefined as void;
    if (expect !== undefined) {
      const onCommittedTraversal = () => {
        off();
        if (location.pathname + location.search !== routePath(expect)) void go(expect, true);
      };
      off = subscribeCommittedTraversal(onCommittedTraversal);
    }
    history.back();
    return off;
  }
  void go(fallback, true);
  return () => undefined;
}

/** Where the current history entry's phone view was pushed from, or null (a deep link, a reload, no push by the app). */
export function pushedFrom(): Route | null {
  const state: unknown = history.state;
  const from = typeof state === 'object' && state !== null ? (state as Record<string, unknown>)[PHONE_FROM] : undefined;
  return typeof from === 'string' ? routeOf(from) : null;
}

/** The phone tab a route belongs to: a channel, a DM and the DM list are home, Settings is you. */
export function tabOf(route: Route): PhoneTab {
  switch (route.name) {
    case 'dm': case 'dms': case 'channel': case 'root': case 'welcome': return 'home';
    case 'activity': return 'activity';
    case 'you': case 'settings': case 'serverSettings': return 'you';
    case 'spotifyCallback': return 'you';
  }
}

/**
 * The history state that carries a server join refused right after signup to /welcome?invite=…, so
 * the shell's join dialog opens with that error shown (pre-flight ruling (e), tasks 23 and 24). The
 * server's detail text is not carried: it is shown nowhere and parsed never.
 */
export function joinErrorState(error: UiError): { joinError: UiError } {
  return { joinError: { code: error.code, detail: '', status: error.status, retryAfterMs: error.retryAfterMs } };
}

/** The refused join a history state carries, or null. */
export function readJoinError(state: unknown): UiError | null {
  if (typeof state !== 'object' || state === null) return null;
  const { joinError } = state as Record<string, unknown>;
  if (typeof joinError !== 'object' || joinError === null || typeof (joinError as Record<string, unknown>).code !== 'string') return null;
  return errorOf(joinError);
}

export function useRoute(): [Route, (r: Route, replace?: boolean, opts?: { phoneBack?: boolean; from?: string | null }) => Promise<boolean>] {
  useSyncExternalStore(subscribe, snapshot);
  const route = parseRoute(location.pathname, location.search, window.history.state);
  return [route, navigate];
}
