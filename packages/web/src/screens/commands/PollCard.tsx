import { useCallback, useEffect, useMemo, useLayoutEffect, useRef, useState, useSyncExternalStore } from 'react';
import { BotUtilitiesStatusError, BotUtilitiesUncertainMutationError, PERM, createBotUtilities, decodePollVote, decodePollClose, type CoreClient, type PollSummary } from '@dilla/client-core';
import { PollCard as SignalPollCard, showToast } from '@dilla/ui';
import { utilityText as t } from './utility-copy.ts';
import { walkPollVoters } from './poll-voters.ts';
import './UtilityCards.css';

const MARKER = /^\[dilla-poll:v1:([0-9a-f]{32})\]$/u;
type PollAttempt = { id: string; kind: 'vote' | 'close'; choices: number[]; revision: string; scope: string; answer?: number };
const attemptsByClient = new WeakMap<CoreClient, Map<string, PollAttempt>>();
function attemptMap(client: CoreClient): Map<string, PollAttempt> {
  const prior = attemptsByClient.get(client); if (prior) return prior; const map = new Map<string, PollAttempt>(); attemptsByClient.set(client, map); return map;
}
function sourceScope(client: CoreClient, communityId: string, channelId: string, owner: string): string {
  const community = client.get('communities')?.find(row => row.id === communityId);
  const channel = client.get(`channels:${communityId}`)?.find(row => row.id === channelId);
  const access = client.get(`permissions:${communityId}`);
  return [owner, community?.id ?? '', channel?.id ?? '', channel?.kind ?? '', channel?.group ?? '', access?.loaded ? 1 : 0,
    access?.overwritesLoaded ? 1 : 0, access?.everyoneId ?? ''].join(':');
}
function useSourceScope(client: CoreClient, communityId: string, channelId: string, owner: string): string {
  const subscribe = useCallback((listener: () => void) => {
    const off = [client.subscribe('account', listener), client.subscribe('communities', listener), client.subscribe(`channels:${communityId}`, listener),
      client.subscribe(`permissions:${communityId}`, listener)];
    return () => { for (const unsubscribe of off) unsubscribe(); };
  }, [client, communityId]);
  const read = useCallback(() => sourceScope(client, communityId, channelId, owner), [client, communityId, channelId, owner]);
  return useSyncExternalStore(subscribe, read, read);
}
export function finalPollMarker(body: string): string | null {
  const finalLine = body.split(/\r?\n/u).at(-1) ?? '';
  return MARKER.exec(finalLine)?.[1] ?? null;
}
function requestId(): string {
  const bytes = new Uint8Array(16); crypto.getRandomValues(bytes);
  if (bytes.every(value => value === 0)) bytes[15] = 1;
  return [...bytes].map(value => value.toString(16).padStart(2, '0')).join('');
}
function definiteMutationError(cause: unknown, attempt: PollAttempt, operation: 73 | 74): boolean {
  return cause instanceof BotUtilitiesStatusError && cause.operation === operation && cause.requestId === attempt.id
    && (cause.status === 2 || cause.status === 3 || cause.status === 4);
}

export interface PollCardProps {
  client: CoreClient; communityId: string | null; channelId: string; threadId: string | null; body: string;
  authorId: string | null; bot: boolean; edited: boolean; deleted: boolean; selfId: string | null; bits: number | null;
  /** Update the message heading before paint, without replacing its focusable row. */
  onVerified?: (poll: PollSummary | null) => void;
}

/** A poll is interactive only after the authenticated bot author, final marker and durable record agree. */
export function PollCard(props: PollCardProps): React.JSX.Element | null {
  const pollId = finalPollMarker(props.body);
  if (pollId === null || props.communityId === null || !props.bot || props.edited || props.deleted || props.authorId === null || props.selfId === null) return null;
  return <VerifiedPollCard key={`${props.communityId}:${props.channelId}:${props.threadId ?? ''}:${pollId}:${props.selfId}`} {...props} pollId={pollId} />;
}

function VerifiedPollCard(props: PollCardProps & { pollId: string }): React.JSX.Element | null {
  const owner = `${props.client.get('account')?.instance?.id ?? ''}:${props.selfId}:${props.client.get('account')?.deviceId ?? ''}`;
  const isSameOwner = useCallback(() => { const account = props.client.get('account'); return `${account?.instance?.id ?? ''}:${account?.user?.id ?? ''}:${account?.deviceId ?? ''}` === owner; }, [props.client, owner]);
  const accessScope = useSourceScope(props.client, props.communityId!, props.channelId, owner);
  const source = props.client.get(`channels:${props.communityId}` as never)?.find((row: { id: string; kind: number; group: string }) => row.id === props.channelId);
  const communityAccessible = props.client.get('communities')?.some(row => row.id === props.communityId) === true;
  const sourceAccessible = communityAccessible && source?.kind === 0 && source.group === 'active';
  // Attempt identity is the immutable target/binding. Permission snapshots and render
  // generations gate work below, but must not fork an unresolved request into a new ID.
  const identityScope = `${owner}:${props.communityId}:${props.channelId}:${props.threadId ?? ''}:${props.pollId}`;
  const [installationId, setInstallationId] = useState<string | null>(null);
  const installationRef = useRef(installationId); installationRef.current = installationId;
  const scopeKey = `${identityScope}:${installationId ?? ''}`;
  const scopeRef = useRef(scopeKey); scopeRef.current = scopeKey;
  const [record, setRecord] = useState<{ client: CoreClient; scope: string; poll: PollSummary } | null>(null);
  const [verifiedScope, setVerifiedScope] = useState<string | null>(null);
  const [active, setActive] = useState(false);
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState('');
  const [selected, setSelected] = useState<number[] | null>(null);
  const accessScopeRef = useRef(accessScope); accessScopeRef.current = accessScope;
  const sourceAccessibleRef = useRef(sourceAccessible); sourceAccessibleRef.current = sourceAccessible;
  const attemptKey = scopeKey;
  const [storedAttempt, setStoredAttempt] = useState<{ key: string; value: PollAttempt | null } | null>(null);
  const matchingAttempt = [...attemptMap(props.client)].find(([key]) => key.startsWith(`${identityScope}:`));
  const attempt = storedAttempt?.key === attemptKey ? storedAttempt.value : attemptMap(props.client).get(attemptKey) ?? matchingAttempt?.[1] ?? null;
  const actualAttemptKey = attempt?.scope ?? attemptKey;
  const rememberAttempt = (next: PollAttempt | null) => {
    if (next === null) attemptMap(props.client).delete(actualAttemptKey); else attemptMap(props.client).set(next.scope, next);
    setStoredAttempt({ key: attemptKey, value: next });
  };
  const [voterData, setVoterData] = useState<{ scope: string; revision: bigint; previews: string[][]; distinct: number; capped: boolean } | null>(null);
  const ref = useRef<HTMLDivElement>(null); const generation = useRef(0); const voterGeneration = useRef(0);
  const voterTarget = useRef<string | null>(null);
  const voterPending = useRef(false);
  const lastVoterWalk = useRef<number | null>(null);
  const utilities = useMemo(() => createBotUtilities(props.client, props.communityId!), [props.client, props.communityId]);
  const invalidate = useCallback(() => { voterGeneration.current++; voterTarget.current = null; setRecord(null); setVerifiedScope(null); setVoterData(null); }, []);
  const refresh = useCallback(async (walkVoters = false): Promise<PollSummary | null> => {
    const current = ++generation.current;
    try {
      const caps = await utilities.capabilities();
      if (generation.current !== current || !isSameOwner() || !sourceAccessible || accessScope !== accessScopeRef.current || caps.botUserId !== props.authorId
        || utilities.binding?.installationId !== caps.installationId || utilities.binding?.botUserId !== caps.botUserId) {
        if (generation.current === current) { setInstallationId(caps.installationId); invalidate(); }
        return null;
      }
      setInstallationId(caps.installationId);
      const poll = await utilities.pollGet(props.pollId, { channelId: props.channelId, threadId: props.threadId });
      const latestCaps = await utilities.capabilities();
      if (generation.current !== current || !isSameOwner() || !sourceAccessible || accessScope !== accessScopeRef.current || latestCaps.installationId !== caps.installationId || latestCaps.botUserId !== props.authorId
        || poll.channelId !== props.channelId || poll.threadId !== props.threadId) {
        if (generation.current === current) invalidate();
        return null;
      }
      const boundScope = `${identityScope}:${caps.installationId}`;
      setRecord({ client: props.client, scope: boundScope, poll }); setVerifiedScope(boundScope);
      // Only the initial/cadence refresh may start a full walk. Mutation refreshes
      // preserve the last completed count; a pending read survives record revisions.
      const voterKey = `${boundScope}:${poll.revision}:${accessScope}`;
      if (walkVoters && !voterPending.current && voterTarget.current !== voterKey
        && (lastVoterWalk.current === null || performance.now() - lastVoterWalk.current >= 30_000)) {
        voterTarget.current = voterKey; voterPending.current = true; lastVoterWalk.current = performance.now();
        const voterRequest = ++voterGeneration.current;
        const votersCurrent = () => voterGeneration.current === voterRequest && isSameOwner()
          && sourceAccessibleRef.current && accessScope === accessScopeRef.current;
        void (async () => {
          try {
            const data = await walkPollVoters(poll.counts, poll.multi, (option, cursor) => utilities.pollVoters({ pollId: poll.id,
              channelId: poll.channelId, threadId: poll.threadId, option, cursor }), votersCurrent);
            if (!votersCurrent()) return;
            const latest = await utilities.capabilities();
            if (!votersCurrent() || latest.installationId !== caps.installationId || latest.botUserId !== props.authorId) return;
            setVoterData({ scope: boundScope, revision: poll.revision, ...data });
          } catch {
            if (votersCurrent()) { voterTarget.current = null; setError(t('poll.readError')); }
          } finally { voterPending.current = false; }
        })();
      }

      const retained = [...attemptMap(props.client)].find(([key]) => key.startsWith(`${identityScope}:`))?.[1];
      if (!busyRef.current || retained === undefined || poll.closed) setSelected(retained?.kind === 'vote' ? retained.choices : poll.myChoices ?? []);
      // Refreshes update the record without dismissing a mutation or voter-read notice.
      if (!busyRef.current && retained !== undefined) setError(t(retained.kind === 'vote' ? 'poll.voteUncertain' : 'poll.uncertain'));
      return poll;
    } catch { if (generation.current === current) invalidate(); return null; }
  }, [utilities, isSameOwner, invalidate, identityScope, sourceAccessible, accessScope, props.client, props.authorId, props.pollId, props.channelId, props.threadId]);
  useEffect(() => {
    const node = ref.current; if (node === null) return;
    if (typeof IntersectionObserver === 'undefined') { setActive(true); return; }
    const observer = new IntersectionObserver(entries => setActive(entries.some(entry => entry.isIntersecting)), { rootMargin: '160px' });
    observer.observe(node); return () => observer.disconnect();
  }, []);
  useEffect(() => {
    if (!active) { generation.current++; voterGeneration.current++; voterTarget.current = null; return; }
    const currentGeneration = generation;
    const currentVoterGeneration = voterGeneration;
    // A complete authenticated refresh can span several Bot gateway rounds. A
    // cadence tick must not supersede that read before its record can be shown.
    // Mutations also own their refresh until the action finishes.
    let pending = false;
    const update = async () => {
      if (pending || busyRef.current) return;
      pending = true;
      try { await refresh(true); } finally { pending = false; }
    };
    void update(); const timer = window.setInterval(() => { void update(); }, 30_000);
    return () => { window.clearInterval(timer); currentGeneration.current++; currentVoterGeneration.current++; voterTarget.current = null; };
  }, [active, refresh]);
  const poll = sourceAccessible && record?.client === props.client && record.scope === scopeKey && verifiedScope === scopeKey ? record.poll : null;
  const { onVerified } = props;
  useLayoutEffect(() => {
    onVerified?.(poll);
    return () => onVerified?.(null);
  }, [onVerified, poll]);
  const canClose = poll !== null && (poll.creatorId === props.selfId || (props.bits !== null && (props.bits & PERM.manageMessages) !== 0));
  if (poll === null) return <div ref={ref} className="dw-poll-card" aria-hidden="true" />;
  const visibleSelected = selected ?? poll.myChoices ?? [];
  const live = () => isSameOwner() && sourceAccessibleRef.current && accessScopeRef.current === accessScope && scopeRef.current === scopeKey
    && installationRef.current === installationId;
  const dropWithdrawnSelf = (choices: readonly number[] | null) => {
    if (choices === null) return;
    setVoterData(current => current?.scope === scopeKey ? { ...current,
      previews: current.previews.map((ids, index) => choices.includes(index) ? ids : ids.filter(id => id !== props.selfId)) } : current);
  };
  const reconcile = async (prior: PollAttempt, operation: 73 | 74): Promise<'accepted' | 'conflict' | 'unknown' | 'stale'> => {
    let accepted = false;
    try {
      // read() binds its response to the requested operation; also require this attempt's ID.
      const response = await utilities.read(prior.id, operation);
      if (!live()) return 'stale';
      if (response?.requestId !== prior.id) return 'unknown';
      if (response.status === 2 || response.status === 3 || response.status === 4) return 'conflict';
      if (response.status !== 0) return 'unknown';
      // A raw status-0 receipt is proof only after the adapter's payload checks.
      const expected = { pollId: props.pollId, channelId: props.channelId, threadId: props.threadId, responseRevision: response.revision };
      if (operation === 73) decodePollVote(response.data, { ...expected, choices: prior.choices });
      else decodePollClose(response.data, expected);
      accepted = true;
      const caps = await utilities.capabilities();
      if (!live() || caps.installationId !== installationId || caps.botUserId !== props.authorId) return 'stale';
      const latest = await utilities.pollGet(poll.id, { channelId: props.channelId, threadId: props.threadId });
      if (!live() || latest.channelId !== props.channelId || latest.threadId !== props.threadId) return 'stale';
      setRecord({ client: props.client, scope: scopeKey, poll: latest }); setVerifiedScope(scopeKey);
      setSelected(latest.myChoices ?? []); dropWithdrawnSelf(latest.myChoices);
      return 'accepted';
    } catch (cause) { return accepted ? 'accepted' : definiteMutationError(cause, prior, operation) ? 'conflict' : 'unknown'; }
  };
  const submit = async (choices: number[], answer: number) => {
    if (busy || (attempt !== null && (attempt.kind !== 'vote' || attempt.choices.join(',') !== choices.join(',')))) return;
    const pending = attempt ?? { id: requestId(), kind: 'vote' as const, choices: [...choices], revision: poll.revision.toString(), scope: scopeKey, answer };
    if (pending.scope !== scopeKey || pending.kind !== 'vote') return;
    const immutable = { ...pending, choices: [...pending.choices] };
    rememberAttempt(immutable); setSelected(choices); busyRef.current = true; setBusy(true); setError('');
    let started = false; let accepted = false;
    try {
      const caps = await utilities.capabilities();
      if (!live() || caps.installationId !== installationId || caps.botUserId !== props.authorId) { await refresh(); return; }
      started = true;
      const updated = await utilities.pollVote({ requestId: immutable.id, pollId: poll.id, channelId: poll.channelId, threadId: poll.threadId, revision: immutable.revision, choices: immutable.choices });
      accepted = true;
      const after = await utilities.capabilities();
      if (!live() || after.installationId !== installationId || after.botUserId !== props.authorId) return;
      setRecord({ client: props.client, scope: scopeKey, poll: updated }); setSelected(updated.myChoices ?? []); dropWithdrawnSelf(updated.myChoices); rememberAttempt(null); await refresh();
    } catch (cause) {
      if (!live()) return;
      if (accepted || (attempt === null && !started)) { rememberAttempt(null); await refresh(); setError(t('poll.readError')); }
      else if (definiteMutationError(cause, immutable, 73)) { rememberAttempt(null); await refresh(); setError(t('poll.conflict')); }
      else if (cause instanceof BotUtilitiesUncertainMutationError) {
        const recovered = await reconcile(immutable, 73);
        if (recovered === 'accepted') { rememberAttempt(null); setError(''); }
        else if (recovered === 'conflict') { rememberAttempt(null); await refresh(); setError(t('poll.conflict')); }
        else if (recovered !== 'stale') { await refresh(); setError(t('poll.voteUncertain')); }
      } else { await refresh(); setError(t('poll.voteUncertain')); }
    } finally { busyRef.current = false; setBusy(false); }
  };
  const close = async () => {
    if (!canClose || busy || (attempt !== null && attempt.kind !== 'close')) return;
    let pending = attempt ?? { id: requestId(), kind: 'close' as const, choices: [], revision: poll.revision.toString(), scope: scopeKey };
    if (pending.scope !== scopeKey || pending.kind !== 'close') return;
    rememberAttempt(pending); busyRef.current = true; setBusy(true); setError('');
    let started = false; let accepted = false; let refreshed = false;
    try { const caps = await utilities.capabilities(); if (!live() || caps.installationId !== installationId || caps.botUserId !== props.authorId) { await refresh(); return; }
      started = true;
      let updated: PollSummary;
      try {
        updated = await utilities.pollClose({ requestId: pending.id, pollId: poll.id, channelId: poll.channelId, threadId: poll.threadId, revision: pending.revision });
      } catch (cause) {
        if (!live() || !definiteMutationError(cause, pending, 74) || !(cause instanceof BotUtilitiesStatusError) || cause.status !== 4) throw cause;
        // A confirmed revision refusal has no effect. Read current results and
        // make one fresh close attempt; uncertain outcomes retain their exact ID.
        rememberAttempt(null);
        const latest = await refresh(); refreshed = true;
        if (!live()) return;
        if (latest === null) { setError(t('poll.readError')); return; }
        if (latest.closed) { setError(''); return; }
        pending = { ...pending, id: requestId(), revision: latest.revision.toString() };
        rememberAttempt(pending);
        updated = await utilities.pollClose({ requestId: pending.id, pollId: latest.id, channelId: latest.channelId, threadId: latest.threadId, revision: pending.revision });
      }
      accepted = true;
      const after = await utilities.capabilities(); if (!live() || after.installationId !== installationId || after.botUserId !== props.authorId) return;
      setRecord({ client: props.client, scope: scopeKey, poll: updated }); rememberAttempt(null); setError(''); showToast(t('poll.closed')); }
    catch (cause) {
      if (!live()) return;
      if (accepted || (attempt === null && !started)) { rememberAttempt(null); await refresh(); setError(t('poll.readError')); }
      else if (definiteMutationError(cause, pending, 74)) { rememberAttempt(null); if (!refreshed) await refresh(); setError(t('poll.closeConflict')); }
      else if (cause instanceof BotUtilitiesUncertainMutationError) {
        const recovered = await reconcile(pending, 74);
        if (recovered === 'accepted') { rememberAttempt(null); setError(''); }
        else if (recovered === 'conflict') { rememberAttempt(null); if (!refreshed) await refresh(); setError(t('poll.closeConflict')); }
        else if (recovered !== 'stale') { await refresh(); setError(t('poll.uncertain')); }
      } else { await refresh(); setError(t('poll.uncertain')); }
    }
    finally { busyRef.current = false; setBusy(false); }
  };
  const vote = (index: number) => {
    if (poll.closed || busy) return;
    if (attempt?.kind === 'vote') { if (attempt.answer === index) void submit(attempt.choices, index); return; }
    const choices = visibleSelected.includes(index) ? visibleSelected.filter(value => value !== index) : poll.multi ? [...visibleSelected, index] : [index];
    void submit(choices, index);
  };
  const votes = poll.counts.reduce((sum, count) => sum + count, 0n);
  const max = poll.counts.reduce((largest, count) => count > largest ? count : largest, 0n);
  const currentVoters = voterData?.scope === scopeKey ? voterData : null;
  const durationSeconds = Number(poll.dueAt - poll.createdAt);
  const duration = durationSeconds >= 86400 * 3 ? t('poll.days', { count: durationSeconds / 86400 }) : durationSeconds === 3600 ? t('poll.hour') : t('poll.hours', { count: durationSeconds / 3600 });
  const count = poll.multi ? currentVoters?.distinct : Number(votes);
  const meta = [poll.closed ? t('poll.final') : t('poll.openFor', { duration }), t(poll.multi ? 'poll.pickAny' : 'poll.pickOne'),
    ...(count === undefined ? [] : [t(poll.multi && currentVoters?.capped ? 'poll.voterLowerBound' : count === 1 ? 'poll.voter' : 'poll.voterCount', { count })])].join(' · ');
  return <div ref={ref} className="dw-poll-card">
    <SignalPollCard question={poll.question} closed={poll.closed} multi={poll.multi} meta={meta}
      options={poll.answers.map((text, index) => ({ text, pct: votes > 0n ? Math.round(Number(poll.counts[index] ?? 0n) / Number(votes) * 100) : 0,
        mine: visibleSelected.includes(index), winner: poll.closed && max > 0n && poll.counts[index] === max,
        voters: (currentVoters?.previews[index] ?? []).map(id => { const member = props.client.get(`members:${props.communityId}`)?.find(row => row.userId === id); return { name: member?.display || member?.username || id.slice(0, 8) }; }) }))}
      canVote={attempt === null || (attempt.scope === scopeKey && attempt.kind === 'vote')} canClose={canClose} busy={busy} pendingAnswer={attempt?.kind === 'vote' ? attempt.answer : undefined}
      labels={{ closed: t('poll.closedTag'), close: t('poll.close') }} onVote={vote} onClose={() => void close()} />
    {error ? <p role="alert">{error}</p> : null}
  </div>;
}
