import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react';
import { BotUtilitiesStatusError, BotUtilitiesUncertainMutationError, PERM, createBotUtilities, decodePollVote, decodePollClose, type CoreClient, type PollSummary } from '@dilla/client-core';
import { Button } from '@dilla/ui';
import { utilityText as t } from './utility-copy.ts';
import './UtilityCards.css';

const MARKER = /^\[dilla-poll:v1:([0-9a-f]{32})\]$/u;
type PollAttempt = { id: string; kind: 'vote' | 'close'; choices: number[]; revision: string; scope: string };
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
function countLabel(value: bigint): string { return value.toString(); }
function definiteMutationError(cause: unknown, attempt: PollAttempt, operation: 73 | 74): boolean {
  return cause instanceof BotUtilitiesStatusError && cause.operation === operation && cause.requestId === attempt.id
    && (cause.status === 2 || cause.status === 3 || cause.status === 4);
}

export interface PollCardProps {
  client: CoreClient; communityId: string | null; channelId: string; threadId: string | null; body: string;
  authorId: string | null; bot: boolean; edited: boolean; deleted: boolean; selfId: string | null; bits: number | null;
  onOpenThread?(rootMessageId: string): void;
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
  const busyRef = useRef(busy); busyRef.current = busy;
  const [error, setError] = useState('');
  const [selected, setSelected] = useState<number[] | null>(null);
  const selectedRef = useRef<number[] | null>(selected); selectedRef.current = selected;
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
  const [voters, setVoters] = useState<{ option: number; ids: string[]; cursor: Uint8Array | null } | null>(null);
  const ref = useRef<HTMLElement>(null); const generation = useRef(0); const voterGeneration = useRef(0);
  const utilities = useMemo(() => createBotUtilities(props.client, props.communityId!), [props.client, props.communityId]);
  const invalidate = useCallback(() => { voterGeneration.current++; setRecord(null); setVerifiedScope(null); setVoters(null); }, []);
  const refresh = useCallback(async (): Promise<PollSummary | null> => {
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
      const retained = [...attemptMap(props.client)].find(([key]) => key.startsWith(`${identityScope}:`))?.[1];
      if (retained?.scope !== undefined && retained.scope !== boundScope) setError(t('poll.uncertain'));
      if (selectedRef.current === null) setSelected(retained?.choices ?? poll.myChoices ?? []);
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
    if (!active) { generation.current++; voterGeneration.current++; return; }
    const currentGeneration = generation;
    const currentVoterGeneration = voterGeneration;
    // A complete authenticated refresh can span several Bot gateway rounds. A
    // cadence tick must not supersede that read before its record can be shown.
    // Mutations also own their refresh until the action finishes.
    let pending = false;
    const update = async () => {
      if (pending) return;
      pending = true;
      try { await refresh(); } finally { pending = false; }
    };
    void update(); const timer = window.setInterval(() => { if (!busyRef.current) void update(); }, 30_000);
    return () => { window.clearInterval(timer); currentGeneration.current++; currentVoterGeneration.current++; };
  }, [active, refresh]);
  const poll = sourceAccessible && record?.client === props.client && record.scope === scopeKey && verifiedScope === scopeKey ? record.poll : null;
  const canClose = poll !== null && (poll.creatorId === props.selfId || (props.bits !== null && (props.bits & PERM.manageMessages) !== 0));
  if (poll === null) return <section ref={ref} className="dw-poll-card" aria-hidden="true" />;
  const visibleSelected = selected ?? poll.myChoices ?? [];
  const live = () => isSameOwner() && sourceAccessibleRef.current && accessScopeRef.current === accessScope && scopeRef.current === scopeKey
    && installationRef.current === installationId;
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
      setSelected(latest.myChoices ?? []);
      return 'accepted';
    } catch (cause) { return accepted ? 'accepted' : definiteMutationError(cause, prior, operation) ? 'conflict' : 'unknown'; }
  };
  const submit = async (choices: number[]) => {
    if (busy || (attempt !== null && (attempt.kind !== 'vote' || attempt.choices.join(',') !== choices.join(',')))) return;
    const pending = attempt ?? { id: requestId(), kind: 'vote' as const, choices: [...choices], revision: poll.revision.toString(), scope: scopeKey };
    if (pending.scope !== scopeKey || pending.kind !== 'vote') return;
    const immutable = { ...pending, choices: [...pending.choices] };
    rememberAttempt(immutable); setBusy(true);
    let started = false; let accepted = false;
    try {
      const caps = await utilities.capabilities();
      if (!live() || caps.installationId !== installationId || caps.botUserId !== props.authorId) { await refresh(); return; }
      started = true;
      const updated = await utilities.pollVote({ requestId: immutable.id, pollId: poll.id, channelId: poll.channelId, threadId: poll.threadId, revision: immutable.revision, choices: immutable.choices });
      accepted = true;
      const after = await utilities.capabilities();
      if (!live() || after.installationId !== installationId || after.botUserId !== props.authorId) return;
      setRecord({ client: props.client, scope: scopeKey, poll: updated }); setSelected(updated.myChoices ?? []); rememberAttempt(null); setError('');
    } catch (cause) {
      if (!live()) return;
      if (accepted || (attempt === null && !started)) { rememberAttempt(null); await refresh(); setError(t('poll.readError')); }
      else if (definiteMutationError(cause, immutable, 73)) { rememberAttempt(null); await refresh(); setError(t('poll.conflict')); }
      else if (cause instanceof BotUtilitiesUncertainMutationError) {
        const recovered = await reconcile(immutable, 73);
        if (recovered === 'accepted') { rememberAttempt(null); setError(''); }
        else if (recovered === 'conflict') { rememberAttempt(null); await refresh(); setError(t('poll.conflict')); }
        else if (recovered !== 'stale') { await refresh(); setError(t('poll.uncertain')); }
      } else { await refresh(); setError(t('poll.uncertain')); }
    } finally { setBusy(false); }
  };
  const close = async () => {
    if (!canClose || busy || (attempt !== null && attempt.kind !== 'close')) return;
    let pending = attempt ?? { id: requestId(), kind: 'close' as const, choices: [], revision: poll.revision.toString(), scope: scopeKey };
    if (pending.scope !== scopeKey || pending.kind !== 'close') return;
    rememberAttempt(pending); setBusy(true);
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
      setRecord({ client: props.client, scope: scopeKey, poll: updated }); rememberAttempt(null); setError(''); }
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
    finally { setBusy(false); }
  };
  const loadVoters = async (option: number, cursor: Uint8Array | null = null) => {
    // Record refreshes may complete while this authenticated page is loading.
    // Only a newer voter query or a changed/invalidated context supersedes it.
    const identity = owner; const installation = installationId; const source = scopeKey; const request = ++voterGeneration.current;
    try { const page = await utilities.pollVoters({ pollId: poll.id, channelId: poll.channelId, threadId: poll.threadId, option, cursor });
      const caps = await utilities.capabilities();
      if (!isSameOwner() || identity !== owner || installationRef.current !== installation || scopeRef.current !== source || voterGeneration.current !== request
        || accessScopeRef.current !== accessScope || !sourceAccessibleRef.current || caps.installationId !== installation || caps.botUserId !== props.authorId) return;
      setVoters(previous => ({ option, ids: cursor !== null && previous?.option === option ? [...previous.ids, ...page.voters] : page.voters, cursor: page.nextCursor })); setError(''); }
    catch { if (voterGeneration.current === request && scopeRef.current === source && isSameOwner()) setError(t('poll.readError')); }
  };
  const toggle = (index: number) => setSelected(current => {
    const selectedNow = current ?? poll.myChoices ?? [];
    return selectedNow.includes(index) ? selectedNow.filter(value => value !== index) : poll.multi ? [...selectedNow, index] : [index];
  });
  return <section ref={ref} className="dw-poll-card" aria-labelledby={`poll-${poll.id}-question`}>
    <h3 id={`poll-${poll.id}-question`}>{poll.question}</h3>
    <fieldset disabled={busy || attempt !== null || poll.closed}>
      <legend>{poll.multi ? t('poll.chooseMany') : t('poll.chooseOne')}</legend>
      {poll.answers.map((answer, index) => <label className="dw-poll-card__answer" key={`${poll.id}:${index}`}>
        <input type={poll.multi ? 'checkbox' : 'radio'} name={`poll-${poll.id}`} checked={visibleSelected.includes(index)} onChange={() => toggle(index)} />
        <span>{answer}</span><span className="dw-poll-card__count">{countLabel(poll.counts[index] ?? 0n)}</span>
      </label>)}
    </fieldset>
    <div className="dw-poll-card__actions">
      {!poll.closed ? <Button variant="accent" disabled={busy || (attempt !== null && (attempt.scope !== scopeKey || attempt.kind !== 'vote' || attempt.choices.join(',') !== visibleSelected.join(',')))} onClick={() => void submit(attempt?.kind === 'vote' ? attempt.choices : visibleSelected)}>{attempt?.kind === 'vote' ? t('poll.retryVote') : t('poll.submit')}</Button> : null}
      {!poll.closed && poll.myChoices !== null && poll.myChoices.length > 0 ? <Button variant="ghost" disabled={busy || (attempt !== null && (attempt.scope !== scopeKey || attempt.kind !== 'vote' || attempt.choices.length !== 0))} onClick={() => void submit(attempt?.kind === 'vote' ? attempt.choices : [])}>{attempt?.kind === 'vote' && attempt.choices.length === 0 ? t('poll.retryWithdraw') : t('poll.withdraw')}</Button> : null}
      {canClose && !poll.closed ? <Button variant="ghost" disabled={busy || (attempt !== null && (attempt.scope !== scopeKey || attempt.kind !== 'close'))} onClick={() => void close()}>{attempt?.kind === 'close' ? t('poll.retryClose') : t('poll.close')}</Button> : null}
    </div>
    <p className="dw-poll-card__due">{poll.closed ? t('poll.closed') : t('poll.closes', { time: new Date(Number(poll.dueAt) * 1000).toLocaleString() })}</p>
    {poll.answers.map((answer, index) => <div className="dw-poll-card__voters" key={`voters-${index}`}><Button variant="ghost" onClick={() => void loadVoters(index)}>{t('poll.voters', { answer, count: countLabel(poll.counts[index] ?? 0n) })}</Button></div>)}
    {voters !== null ? <div className="dw-poll-card__voter-list" aria-live="polite">{voters.ids.length ? <ul>{voters.ids.map(id => { const member = props.client.get(`members:${props.communityId}`)?.find(row => row.userId === id); return <li key={id}>{member?.display || member?.username || id.slice(0, 8)}</li>; })}</ul> : t('poll.noVoters')}{voters.cursor ? <Button variant="ghost" onClick={() => void loadVoters(voters.option, voters.cursor)}>{t('poll.moreVoters')}</Button> : null}</div> : null}
    {error ? <p role="alert">{error}</p> : null}
    {poll.threadId !== null && props.onOpenThread ? <Button variant="ghost" onClick={() => props.onOpenThread?.(poll.threadId!)}>{t('poll.openThread')}</Button> : null}
  </section>;
}
