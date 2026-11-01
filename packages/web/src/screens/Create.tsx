// Flow 07 (v0.1.0-web3.1): start a server, add a channel, invite people. Each flow is a @dilla/ui dialog driven by one
// worker command at a time; every refusal is said by its code, never by the instance's detail text.
import { useEffect, useRef, useState } from 'react';
import { Button, CheckList, CreateChannel, CreateServer, InviteLink, Toggle, type NewChannelKind } from '@dilla/ui';
import { useCore } from '../core/context.tsx';
import { errorOf, type UiError } from '../core/errors.ts';
import { canCreateAnnouncement } from '@dilla/client-core';
import { useSlice } from '../core/use-slice.ts';
import { t } from '../strings/index.ts';

/** The instance's own bounds (internal/api maxCommunityNameBytes, maxChannelNameBytes); the worker checks them again. */
export const SERVER_NAME_MAX_BYTES = 255;
export const CHANNEL_NAME_MAX_BYTES = 100;
/** An invite link works this many times and for this long (flow 07, Copy). */
export const INVITE_MAX_USES = 10;
export const INVITE_TTL_DAYS = 7;
export const INVITE_TTL_S = INVITE_TTL_DAYS * 24 * 3600;
/** What a new server starts with, in order: #general, then the voice channel general. */
/** The most roles a private channel can be opened to (the worker refuses more). */
const PRIVATE_ROLES_MAX = 98;
/** The kinds a server's starter channels take. */
type StarterKind = Exclude<NewChannelKind, 'announcement'>;
const STARTERS: readonly { kind: StarterKind; name: string }[] = [{ kind: 'text', name: 'general' }, { kind: 'voice', name: 'general' }];

const UTF8 = new TextEncoder();
const CONTROL = /\p{Cc}/u;
const HEX32 = /^[0-9a-f]{32}$/;

/** Why the instance would refuse `raw` (trimmed) as a name of at most `maxBytes` UTF-8 bytes; null when it would not. */
export function nameProblem(raw: string, maxBytes: number): 'empty' | 'long' | 'control' | null {
  const name = raw.trim();
  if (name === '') return 'empty';
  if (UTF8.encode(name).length > maxBytes) return 'long';
  if (CONTROL.test(name)) return 'control';
  return null;
}

/** How a typed name is stored: a text or announcement channel as a lower-case slug (`ship-talk`), a voice channel as typed, trimmed. */
export function channelName(kind: NewChannelKind, raw: string): string {
  const slug = kind !== 'voice';
  const spaced = raw.trim().replace(/\s+/g, slug ? '-' : ' ');
  return slug ? spaced.toLowerCase() : spaced;
}

/** The hex id a create answered with under `key`, or null when the answer is not { [key]: <32 hex> }. */
function idIn(value: unknown, key: 'communityId' | 'channelId'): string | null {
  if (typeof value !== 'object' || value === null) return null;
  const id = (value as Record<string, unknown>)[key];
  return typeof id === 'string' && HEX32.test(id) ? id : null;
}

/** The most roles an announcement channel is opened to (the worker's cap). */
const ANNOUNCE_ROLES_MAX = 99;

const BAD_RESULT: UiError = { code: 'E_BAD_RESULT', detail: '', status: 0, retryAfterMs: null };

/** What the notice says is missing, by which starters are still missing (indices into STARTERS). */
function partialKey(missing: readonly number[]): 'create.server.partial' | 'create.server.partialText' | 'create.server.partialVoice' {
  if (missing.length === 1) return missing[0] === 0 ? 'create.server.partialText' : 'create.server.partialVoice';
  return 'create.server.partial';
}

interface MadeServer { communityId: string; name: string; channels: Map<number, string> }

/** The channels this browser created in this page session, per worker client: nothing in them predates its join (walk M1). */
const madeChannels = new WeakMap<object, Set<string>>();
function rememberMade(client: object, channelId: string): void {
  let made = madeChannels.get(client);
  if (made === undefined) { made = new Set(); madeChannels.set(client, made); }
  made.add(channelId);
}
/** Whether this browser created `channelId` in this page session. */
export function madeHere(client: object, channelId: string): boolean {
  return madeChannels.get(client)?.has(channelId) ?? false;
}

/** Servers whose starter channels failed after their dialog closed, by communityId, per worker client (WB m4). */
const leftServers = new WeakMap<object, Map<string, { server: MadeServer; failure: UiError }>>();
function leftovers(client: object): Map<string, { server: MadeServer; failure: UiError }> {
  let held = leftServers.get(client);
  if (held === undefined) { held = new Map(); leftServers.set(client, held); }
  return held;
}

/**
 * "Start a server": the name, then createCommunity, then the two starter channels. `onServer` runs as soon as the server
 * exists, so the shell selects it; `onDone` once both starters exist. A starter channel that fails leaves the server as it
 * is and names what is missing, with a retry that adds only the missing channels (never a silent rollback, never a second
 * #general). One server per press: a ref guards the in flight create, since two submits can land before `busy` renders.
 * A create still running when the dialog closes finishes its work but no longer speaks to this dialog or calls `onDone`;
 * `busy` follows that create, so a dialog reopened meanwhile says it is still creating (review I2, M5).
 *
 * The retry first reads the server's channel list again: dillad keeps no uniqueness on channel names, and a create whose
 * answer was lost may still have made its channel, so a listed starter counts as made (review I1). A retry whose read
 * fails sends no create.
 *
 * A starter that fails after the dialog closed is kept by communityId for the page session and named, once, by the next
 * open (or at once, when the dialog was opened again meanwhile), with the same retry (WB m4).
 *
 * A create whose answer was lost (E_NETWORK) may still have made the server, so the page has the worker read the server
 * list again (refreshCommunities). A server this account owns, with exactly that name, in the list now but not before the
 * press, is that server: it is adopted and gets its starters. A later press with the same name reads the list again
 * before it creates anything, and a failed read sends no create, so a second press never makes a second server (WB m5).
 */
export function CreateServerFlow(props: {
  open: boolean; onClose(): void; onServer(communityId: string): void; onDone(result: { communityId: string; channelId: string }): void;
}): React.JSX.Element {
  const client = useCore();
  const [name, setName] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [partial, setPartial] = useState<string | null>(null);
  // Each refusal or notice shown counts one: the same text again is a new alert (review M3).
  const [attempt, setAttempt] = useState(0);
  const running = useRef(false);
  // Each open of the dialog is one session; work from an earlier one is ignored once it closed.
  const session = useRef(0);
  const open = useRef(props.open);
  // The server this dialog created and the starter channels it holds so far, for the retry.
  const made = useRef<MadeServer | null>(null);
  // A create whose answer was lost: its name and the servers listed before its press (WB m5).
  const lost = useRef<{ name: string; before: ReadonlySet<string> } | null>(null);

  const fail = (message: string) => { setError(message); setAttempt(a => a + 1); };
  const notice = (server: MadeServer, failure: UiError) => {
    const missing = STARTERS.flatMap((_, i) => (server.channels.has(i) ? [] : [i]));
    setPartial(t(partialKey(missing), { name: server.name, code: failure.code }));
    setAttempt(a => a + 1);
  };
  /** Names the latest server whose starters failed after a close, in this session, with the retry; told once. */
  const takeLeftover = () => {
    const held = leftovers(client);
    const last = [...held.values()].at(-1);
    if (last === undefined) return;
    held.delete(last.server.communityId);
    made.current = last.server;
    setError(null);
    notice(last.server, last.failure);
  };

  useEffect(() => {
    open.current = props.open;
    if (props.open) {
      takeLeftover();
      return;
    }
    session.current += 1;
    setName(''); setError(null); setPartial(null);
    made.current = null;
    // eslint-disable-next-line react-hooks/exhaustive-deps -- once per open and close; takeLeftover reads refs and setters only
  }, [props.open]);

  const starters = async (server: MadeServer, mine: () => boolean) => {
    let failure: UiError | null = null;
    for (const [i, starter] of STARTERS.entries()) {
      if (server.channels.has(i)) continue;
      try {
        const id = idIn(await client.call({ m: 'createChannel', communityId: server.communityId, kind: starter.kind, name: starter.name }), 'channelId');
        if (id === null) failure ??= BAD_RESULT;
        else { server.channels.set(i, id); rememberMade(client, id); }
      } catch (e) {
        failure ??= errorOf(e);
      }
    }
    const general = server.channels.get(0);
    const missing = STARTERS.flatMap((_, i) => (server.channels.has(i) ? [] : [i]));
    const complete = missing.length === 0 && general !== undefined;
    if (!mine()) {
      // The dialog closed meanwhile: what is missing waits for the next open, or is said now in a dialog opened again.
      if (complete) leftovers(client).delete(server.communityId);
      else leftovers(client).set(server.communityId, { server, failure: failure ?? BAD_RESULT });
      if (!complete && open.current) takeLeftover();
      return;
    }
    if (complete && general !== undefined) {
      props.onDone({ communityId: server.communityId, channelId: general });
      return;
    }
    notice(server, failure ?? BAD_RESULT);
  };

  const run = (work: (mine: () => boolean) => Promise<void>) => {
    if (running.current) return;
    running.current = true;
    setBusy(true);
    const at = session.current;
    void work(() => session.current === at).finally(() => { running.current = false; setBusy(false); });
  };

  /** The ids of the servers this account owns, named exactly `serverName`, listed now and not in `before`. Only a single
   *  one is adopted: two or more cannot be told apart (another tab pressed too, say), so none is. */
  const adoptable = (serverName: string, before: ReadonlySet<string>): string[] => {
    const me = client.get('account')?.user?.id;
    if (me === undefined) return [];
    return (client.get('communities') ?? []).filter(c => c.name === serverName && c.owner === me && !before.has(c.id)).map(c => c.id);
  };

  const submit = () => {
    const problem = nameProblem(name, SERVER_NAME_MAX_BYTES);
    if (problem !== null) {
      fail(t(problem === 'empty' ? 'create.server.error.empty' : problem === 'long' ? 'create.server.error.long' : 'create.name.control'));
      return;
    }
    const serverName = name.trim();
    // A press after a lost answer with the same name compares with the list from before that first press.
    const earlier = lost.current?.name === serverName ? lost.current.before : null;
    const before = earlier ?? new Set((client.get('communities') ?? []).map(c => c.id));
    // The worker reads the instance's server list again (also with the socket up), then the list is searched.
    const look = async (listedBefore: ReadonlySet<string>): Promise<string[]> => {
      await client.call({ m: 'refreshCommunities' });
      return adoptable(serverName, listedBefore);
    };
    run(async mine => {
      let communityId: string | null = null;
      if (earlier !== null) {
        // After a lost answer, nothing is created until the list has been read: a failed read sends no create, and
        // neither does an ambiguous one.
        let found: string[];
        try {
          found = await look(earlier);
        } catch (e) {
          if (mine()) fail(t('create.server.error.other', { code: errorOf(e).code }));
          return;
        }
        if (found.length > 1) { if (mine()) fail(t('create.server.error.ambiguous', { name: serverName })); return; }
        communityId = found[0] ?? null;
      }
      if (communityId === null) {
        try {
          communityId = idIn(await client.call({ m: 'createCommunity', name: serverName }), 'communityId');
        } catch (e) {
          const code = errorOf(e).code;
          const found = code === 'E_NETWORK' ? await look(before).catch(() => []) : [];
          if (found.length > 1) {
            lost.current = { name: serverName, before };
            if (mine()) fail(t('create.server.error.ambiguous', { name: serverName }));
            return;
          }
          communityId = found[0] ?? null;
          if (communityId === null) {
            if (code === 'E_NETWORK') lost.current = { name: serverName, before };
            // The worker's E_BAD_INPUT here is its own name check (Task 1), so it reads as a refused name too.
            if (mine()) fail(code === 'E_INVALID_REQUEST' || code === 'E_BAD_INPUT' ? t('create.server.error.refused') : t('create.server.error.other', { code }));
            return;
          }
        }
      }
      if (communityId === null) { if (mine()) fail(t('create.server.error.other', { code: BAD_RESULT.code })); return; }
      lost.current = null;
      const server: MadeServer = { communityId, name: serverName, channels: new Map<number, string>() };
      if (mine()) {
        made.current = server;
        props.onServer(communityId);
      }
      await starters(server, mine);
    });
  };

  // The notice stays while the retry runs; it changes only with the retry's result.
  const retry = () => {
    const server = made.current;
    if (server === null) return;
    run(async mine => {
      try {
        await client.call({ m: 'selectCommunity', communityId: server.communityId });
      } catch (e) {
        if (mine()) notice(server, errorOf(e));
        return;
      }
      for (const [i, starter] of STARTERS.entries()) {
        if (server.channels.has(i)) continue;
        const kind = starter.kind === 'voice' ? 1 : 0;
        const listed = (client.get(`channels:${server.communityId}`) ?? []).find(c => c.kind === kind && c.name === starter.name);
        if (listed !== undefined) server.channels.set(i, listed.id);
      }
      await starters(server, mine);
    });
  };

  return (
    <CreateServer open={props.open} name={name} onName={v => { setName(v); setError(null); }} error={error} busy={busy} partial={partial}
      labels={{ title: t('create.server.title'), body: t('create.server.body'), name: t('create.server.name'), hint: t('create.server.hint'),
        submit: t('create.server.submit'), busy: t('create.server.busy'), close: t('dialog.close'), retry: t('create.server.retry') }}
      attempt={attempt} onSubmit={submit} onRetry={retry} onClose={() => props.onClose()} />
  );
}

/**
 * "New channel": a kind and a name, then createChannel; `onDone` hands over the new channel to select. As in
 * CreateServerFlow, a create still running when the dialog closes no longer speaks to it or calls `onDone`, and `busy`
 * follows it into a dialog reopened meanwhile (review I2).
 */
export function CreateChannelFlow(props: {
  open: boolean; community: { id: string; name: string } | null;
  /** The category the channel is made in (its header's "+"); null or absent is the top level. */
  parentId?: string | null; onClose(): void; onDone(channelId: string, kind: NewChannelKind): void;
}): React.JSX.Element {
  const client = useCore();
  const [picked, setKind] = useState<NewChannelKind>('text');
  const [name, setName] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const [priv, setPriv] = useState(false);
  const [ticked, setTicked] = useState<readonly string[]>([]);
  const [announceTicked, setAnnounceTicked] = useState<readonly string[]>([]);
  const running = useRef(false);
  const session = useRef(0);
  const communityId = props.community?.id ?? '';
  const access = useSlice(`permissions:${communityId}`);
  const roles = useSlice(`roles:${communityId}`);

  useEffect(() => {
    if (props.open) return;
    session.current += 1;
    setKind('text'); setName(''); setError(null); setPriv(false); setTicked([]); setAnnounceTicked([]);
  }, [props.open]);

  // Private create needs the manage-roles right; the roles on offer are those below my top (all of them for the owner).
  const canPrivate = access?.manageRoles === true;
  const offered = (roles ?? []).filter(r => r.id !== access?.everyoneId && (access?.owner === true || r.position < (access?.top ?? 0)))
    .sort((a, b) => b.position - a.position);

  // The announcement kind is offered to a person who may create one; the worker checks it again.
  const canAnnounce = access !== undefined && canCreateAnnouncement(access);
  // Only roles below the creator's top (all of them for the owner), as for a private channel: the server answers 403 for the rest.
  const announceRoles = (roles ?? []).filter(r => r.id !== access?.everyoneId && (access?.owner === true || r.position < (access?.top ?? 0)))
    .sort((a, b) => b.position - a.position);
  // The kind is missing for a creator who may add channels but not manage roles: say why, in one line.
  const needsRoles = access !== undefined && !canAnnounce && access.manageChannels && !access.manageRoles;

  // A chosen announcement kind falls back to text when the right to create one is lost before submit.
  const kind: NewChannelKind = picked === 'announcement' && !canAnnounce ? 'text' : picked;

  const fail = (message: string) => { setError(message); setAttempt(a => a + 1); };

  const shown = channelName(kind, name);
  const hint = shown === '' ? t('create.channel.hint')
    : t(kind !== 'voice' ? 'create.channel.hintText' : 'create.channel.hintVoice', { name: shown });

  const submit = () => {
    const community = props.community;
    if (community === null || running.current) return;
    const problem = nameProblem(shown, CHANNEL_NAME_MAX_BYTES);
    if (problem !== null) {
      fail(t(problem === 'empty' ? 'create.channel.error.empty' : problem === 'long' ? 'create.channel.error.long' : 'create.name.control'));
      return;
    }
    // dillad keeps no uniqueness on channel names: a name this server already lists for the kind is refused here (walk M2).
    const listedKind = kind === 'voice' ? 1 : 0;
    if ((client.get(`channels:${community.id}`) ?? []).some(c => c.kind === listedKind && c.name === shown)) {
      fail(t(kind !== 'voice' ? 'create.channel.error.takenText' : 'create.channel.error.takenVoice', { name: shown }));
      return;
    }
    running.current = true;
    setBusy(true);
    const chosen = kind;
    const sendPrivate = canPrivate && priv && chosen !== 'announcement';
    const roleIds = ticked.filter(id => offered.some(r => r.id === id));
    const announceIds = announceTicked.filter(id => announceRoles.some(r => r.id === id));
    const at = session.current;
    const mine = () => session.current === at;
    void (async () => {
      try {
        const id = idIn(await client.call({ m: 'createChannel', communityId: community.id, kind: chosen, name: shown,
          ...(props.parentId != null ? { parentId: props.parentId } : {}),
          ...(sendPrivate ? { private: { roleIds } } : {}), ...(chosen === 'announcement' ? { announce: { roleIds: announceIds } } : {}) }), 'channelId');
        if (id !== null) rememberMade(client, id);
        if (!mine()) return;
        if (id === null) fail(t('create.channel.error.other', { code: BAD_RESULT.code }));
        else props.onDone(id, chosen);
      } catch (e) {
        if (!mine()) return;
        // Only the instance's E_INVALID_REQUEST is about the name; the worker's E_BAD_INPUT here is not (review M7). Nor is it
        // the 98-role cap, which the ticks already prevent: the code cannot tell the cap from "roles not loaded" or "unknown
        // server", and the detail is never read (protocol/02), so it is worded by its code (W2a review 8).
        const { code } = errorOf(e);
        fail(code === 'E_FORBIDDEN' ? t(chosen === 'announcement' ? 'announce.roleTooHigh' : 'create.channel.error.forbidden')
          : code === 'E_INVALID_REQUEST' ? t('create.channel.error.refused')
            : t('create.channel.error.other', { code }));
      } finally {
        running.current = false;
        setBusy(false);
      }
    })();
  };

  return (
    <CreateChannel open={props.open} kind={kind} kinds={canAnnounce ? ['text', 'voice', 'announcement'] : ['text', 'voice']}
      onKind={k => { setKind(k); setError(null); }} name={name}
      onName={v => { setName(v); setError(null); }} hint={hint} error={error} busy={busy} attempt={attempt}
      labels={{ title: t('create.channel.title', { server: props.community?.name ?? '' }), kind: t('create.channel.kind'),
        text: t('create.channel.text'), voice: t('create.channel.voice'), announcement: t('announce.kind'), name: t('create.channel.name'), submit: t('create.channel.submit'),
        busy: t('create.channel.busy'), close: t('dialog.close') }}
      onSubmit={submit} onClose={() => props.onClose()}>
      {kind === 'announcement' ? <>
        <CheckList label={t('announce.roles')} emptyLabel={t('announce.noRoles')} busyId={null} describedBy="dw-announce-hint"
          items={announceRoles.map(r => {
            const checked = announceTicked.includes(r.id);
            return { id: r.id, label: r.name, color: r.color, checked, disabled: !checked && announceTicked.length >= ANNOUNCE_ROLES_MAX };
          })}
          onToggle={(id, on) => setAnnounceTicked(list => (on ? [...list.filter(x => x !== id), id] : list.filter(x => x !== id)))} />
        <p id="dw-announce-hint" className="d-select__hint">{t('announce.hint')}</p>
      </> : needsRoles ? <p className="d-select__hint">{t('announce.needsRoles')}</p> : canPrivate ? <>
        <Toggle id="dw-create-private" label={t('chperm.create.private')} showLabel hint={t('chperm.create.privateHint')} checked={priv}
          onChange={on => { setPriv(on); setError(null); }} />
        {priv ? <CheckList label={t('chperm.create.roles')} emptyLabel={t('chperm.create.noRoles')} busyId={null}
          items={offered.map(r => {
            const checked = ticked.includes(r.id);
            // At most PRIVATE_ROLES_MAX roles: the rows past it are disabled, so the instance's refusal is unreachable.
            const full = !checked && ticked.length >= PRIVATE_ROLES_MAX;
            return { id: r.id, label: r.name, color: r.color, checked, disabled: full, ...(full ? { detail: t('chperm.create.tooMany') } : {}) };
          })}
          onToggle={(id, on) => setTicked(list => (on ? [...list.filter(x => x !== id), id] : list.filter(x => x !== id)))} /> : null}
      </> : null}
    </CreateChannel>
  );
}

/** Copies `url`; true when the browser let it. A blocked or absent clipboard is not an error: the dialog shows the link. */
async function copied(url: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(url);
    return true;
  } catch {
    return false;
  }
}

const DAY_MS = 24 * 3600 * 1000;
/** A link whose answer took longer than this is not copied by itself: the browser no longer counts the press (WB m3). */
export const AUTO_COPY_MS = 1000;

/** The last invite made per server, for this page session (one map per worker client): the link and when it was made. */
const madeInvites = new WeakMap<object, Map<string, { url: string; at: number }>>();
function inviteMemory(client: object): Map<string, { url: string; at: number }> {
  let memory = madeInvites.get(client);
  if (memory === undefined) { memory = new Map(); madeInvites.set(client, memory); }
  return memory;
}

/**
 * The invite action: on open, createInvite for INVITE_MAX_USES uses over INVITE_TTL_DAYS days, then the link is copied and
 * "Invite link copied." is announced. The link stays in the dialog for a clipboard the browser blocked.
 *
 * The last link per server is reused for the page session while more than a day of it is left, so neither an open nor a
 * switch between servers mints another (WB m3); `invite.limits` then counts the days left. The link is copied by itself
 * only when it is at hand within AUTO_COPY_MS of the open; a slower answer shows the link and copy link, which copies inside
 * the person's own press.
 *
 * `onMoreOptions`, when given, adds the `invite.options` button beside close once the link is shown: it closes this dialog
 * and calls back, and the shell opens Server settings on its invites tab (flow 08).
 */
export function InviteFlow(props: {
  open: boolean; community: { id: string; name: string } | null; onClose(): void; onCreated?: (() => void) | undefined; onMoreOptions?: (() => void) | undefined;
}): React.JSX.Element {
  const client = useCore();
  const [state, setState] = useState<'creating' | 'ready' | 'failed'>('creating');
  const [url, setUrl] = useState('');
  const [days, setDays] = useState(INVITE_TTL_DAYS);
  const [status, setStatus] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [attempt, setAttempt] = useState(0);
  const communityId = props.community?.id ?? null;
  const onCreated = useRef(props.onCreated);
  onCreated.current = props.onCreated;

  useEffect(() => {
    if (!props.open || communityId === null) {
      setState('creating'); setUrl(''); setStatus(''); setError(null);
      return;
    }
    let live = true;
    setState('creating');
    setStatus('');
    const memory = inviteMemory(client);
    const opened = Date.now();
    void (async () => {
      try {
        let held = memory.get(communityId);
        if (held === undefined || opened - held.at >= (INVITE_TTL_DAYS - 1) * DAY_MS) {
          const value = await client.call({ m: 'createInvite', communityId, maxUses: INVITE_MAX_USES, ttlSeconds: INVITE_TTL_S });
          const link = typeof value === 'object' && value !== null ? (value as Record<string, unknown>).url : undefined;
          if (typeof link !== 'string' || link === '') throw Object.assign(new Error(BAD_RESULT.code), BAD_RESULT);
          held = { url: link, at: Date.now() };
          memory.set(communityId, held);
        }
        if (!live) return;
        const now = Date.now();
        setUrl(held.url);
        setDays(Math.min(INVITE_TTL_DAYS, Math.ceil((held.at + INVITE_TTL_DAYS * DAY_MS - now) / DAY_MS)));
        setState('ready');
        onCreated.current?.();
        if (now - opened > AUTO_COPY_MS) return;
        const ok = await copied(held.url);
        if (live) setStatus(t(ok ? 'invite.copied' : 'invite.blocked'));
      } catch (e) {
        if (!live) return;
        const code = errorOf(e).code;
        setError(code === 'E_FORBIDDEN' ? t('invite.error.forbidden') : t('invite.error.other', { code }));
        setState('failed');
      }
    })();
    return () => { live = false; };
  }, [props.open, communityId, attempt, client]);

  const copyAgain = () => {
    setStatus('');
    void copied(url).then(ok => setStatus(t(ok ? 'invite.copied' : 'invite.blocked')));
  };

  const { onMoreOptions } = props;
  return (
    <InviteLink open={props.open} state={state} url={url} status={status} error={error} attempt={attempt}
      extra={state === 'ready' && onMoreOptions !== undefined
        ? <Button variant="ghost" onClick={() => { props.onClose(); onMoreOptions(); }}>{t('invite.options')}</Button> : undefined}
      limits={t('invite.limits', { uses: INVITE_MAX_USES, days, server: props.community?.name ?? '' })}
      labels={{ title: t('invite.title', { server: props.community?.name ?? '' }), creating: t('invite.creating'), link: t('invite.link'),
        copy: t('invite.copy'), close: t('dialog.close'), retry: t('invite.retry') }}
      onCopy={copyAgain} onRetry={() => setAttempt(a => a + 1)} onClose={() => props.onClose()} />
  );
}
