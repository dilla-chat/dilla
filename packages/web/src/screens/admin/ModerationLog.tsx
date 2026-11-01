import { useEffect, useId, useRef, useState } from 'react';
import { Button } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { t } from '../../strings/index.ts';

interface ModerationCase {
  key: string; channelId: string; actorId: string; messageId: string; revision: string;
  reason: string; action: string; excerpt: string; receivedAt: string;
}
interface ModerationResponse {
  deliveryId: string; requestId: string; status: number; cases: ModerationCase[]; hasMore: boolean; nextCursor: string | null;
}
type ReadResult = { authorized: boolean; responses: ModerationResponse[]; through: string | null; blocked: boolean };
type Phase = 'idle' | 'sending' | 'waiting' | 'ready' | 'denied' | 'error';
type Source = 'automatic' | 'manual';
const POLL_MS = 2_000;
const MAX_DISPLAY_CASES = 100;

const requestId = (): string => [...crypto.getRandomValues(new Uint8Array(16))]
  .map(byte => byte.toString(16).padStart(2, '0')).join('');

export function ModerationLog({ communityId, showHeading = true }: { communityId: string; showHeading?: boolean }): React.JSX.Element {
  const client = useCore();
  const base = useId();
  const [forCommunity, setForCommunity] = useState(communityId);
  const [phase, setPhase] = useState<Phase>('idle');
  const [request, setRequest] = useState<string | null>(null);
  const [responses, setResponses] = useState<ModerationResponse[]>([]);
  const [ackThrough, setAckThrough] = useState<string | null>(null);
  const [blocked, setBlocked] = useState(false);
  const [cursor, setCursor] = useState<string | null>(null);
  const [source, setSource] = useState<Source>('manual');
  const [listedCases, setListedCases] = useState<ModerationCase[]>([]);
  const [automaticHasMore, setAutomaticHasMore] = useState(false);
  const generation = useRef(0);

  useEffect(() => {
    generation.current += 1;
    setForCommunity(communityId);
    setPhase('idle');
    setRequest(null);
    setResponses([]);
    setAckThrough(null);
    setBlocked(false);
    setCursor(null);
    setListedCases([]);
    setAutomaticHasMore(false);
    return () => { generation.current += 1; };
  }, [communityId]);

  const load = (kind: Source, after: string | null = null): void => {
    if (phase === 'sending' || forCommunity !== communityId) return;
    const request = requestId();
    const id = ++generation.current;
    setRequest(request);
    setSource(kind);
    if (after === null) setListedCases([]);
    setResponses([]);
    setAckThrough(null);
    setBlocked(false);
    setPhase('sending');
    const command = kind === 'manual'
      ? { m: 'listManualModerationEvidence' as const, communityId, requestId: request, cursor: after }
      : { m: 'listModerationCases' as const, communityId, requestId: request, offset: 0 as const };
    void Promise.resolve(client.call(command)).then(value => {
      if (id !== generation.current) return;
      const result = value as { status?: unknown };
      if (result.status === 'accepted' || result.status === 'pending') setPhase('waiting');
      else setPhase('error');
    }).catch(() => { if (id === generation.current) setPhase('error'); });
  };

  useEffect(() => {
    if ((phase !== 'waiting' && phase !== 'ready') || request === null) return;
    const id = generation.current;
    let canceled = false;
    let timer: number | undefined;
    const poll = (): void => {
      void Promise.resolve(client.call({ m: 'readHumanInteractionResponses', communityId, requestIds: [request] })).then(value => {
        if (canceled || id !== generation.current) return;
        const page = value as ReadResult;
        if (!page.authorized) {
          setResponses([]);
          setAckThrough(null);
          setBlocked(false);
          setPhase('denied');
          return;
        }
        setBlocked(page.blocked);
        if (page.responses.length > 0) {
          setResponses(page.responses);
          setListedCases(previous => {
            const seen = new Set(previous.map(row => row.key));
            const pageCases = page.responses.flatMap(response => response.cases);
            const additions = pageCases.filter(row => {
              if (seen.has(row.key)) return false;
              seen.add(row.key);
              return true;
            });
            return [...previous, ...additions].slice(0, MAX_DISPLAY_CASES);
          });
          if (page.through !== null) setAckThrough(page.through);
          const result = page.responses.find(response => response.requestId === request);
          if (result !== undefined) {
            setCursor(source === 'manual' ? result.nextCursor : null);
            setAutomaticHasMore(source === 'automatic' && result.hasMore);
            setPhase('ready');
          }
        } else if (page.through !== null) {
          // The core suppressed a no-longer-live source event; committing the empty page is safe.
          setAckThrough(page.through);
        }
        timer = window.setTimeout(poll, page.blocked ? 10_000 : phase === 'ready' ? 10_000 : POLL_MS);
      }).catch(() => {
        if (!canceled && id === generation.current) {
          setResponses([]);
          setAckThrough(null);
          setBlocked(false);
          setPhase('error');
        }
      });
    };
    poll();
    return () => { canceled = true; if (timer !== undefined) window.clearTimeout(timer); };
  }, [client, communityId, phase, request, source]);

  // This effect runs after React commits the response rows to the DOM. Until then the durable
  // journal remains readable across a reload or a permission-fetch failure.
  useEffect(() => {
    if (ackThrough === null) return;
    const through = ackThrough;
    void Promise.resolve(client.call({ m: 'ackHumanInteractionResponses', communityId, through })).then(() => {
      setAckThrough(value => value === through ? null : value);
    }).catch(() => undefined);
  }, [ackThrough, client, communityId]);

  const current = forCommunity === communityId;
  const currentResponses = current ? responses : [];
  const cases = current ? listedCases : [];
  const date = (seconds: string): string => {
    const timestamp = Number(seconds);
    if (!/^-?\d+$/u.test(seconds) || !Number.isSafeInteger(timestamp) || Math.abs(timestamp) > 8_640_000_000_000) {
      return t('admin.bot.moderationLog.unknownDate');
    }
    try {
      return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' })
        .format(new Date(timestamp * 1000));
    } catch {
      return t('admin.bot.moderationLog.unknownDate');
    }
  };

  return <section aria-label={showHeading ? undefined : t('admin.bot.moderationLog.title')} aria-labelledby={showHeading ? `${base}-title` : undefined}>
    {showHeading ? <h3 id={`${base}-title`}>{t('admin.bot.moderationLog.title')}</h3> : null}
    <Button aria-disabled={!current || phase === 'sending' || undefined} onClick={() => load('automatic', null)}>
      {phase === 'sending' && source === 'automatic' ? t('admin.bot.moderationLog.loading') : t('admin.bot.moderationLog.loadAutomatic')}
    </Button>
    <Button aria-disabled={!current || phase === 'sending' || undefined} onClick={() => load('manual', null)}>
      {phase === 'sending' ? t('admin.bot.moderationLog.loading') : t('admin.bot.moderationLog.load')}
    </Button>
    {phase === 'waiting' ? <p role="status">{t('admin.bot.moderationLog.waiting')}</p> : null}
    {phase === 'denied' ? <p role="note">{t('admin.bot.moderationLog.denied')}</p> : null}
    {current && blocked ? <p role="note">{t('admin.bot.moderationLog.blocked')}</p> : null}
    {phase === 'error' ? <p role="alert">{t('admin.bot.moderationLog.error')}</p> : null}
    {current && phase === 'ready' && currentResponses.some(response => response.requestId === request && response.status !== 0)
      ? <p role="status">{t('admin.bot.moderationLog.responseUnavailable')}</p> : null}
    {current && phase === 'ready' && cases.length === 0 && (source === 'manual' ? cursor === null : !automaticHasMore)
      && currentResponses.every(response => response.status === 0)
      ? <p role="note">{t('admin.bot.moderationLog.empty')}</p> : null}
    {cases.map(row => <article key={row.key}>
      <p>{t('admin.bot.moderationLog.caseMeta', { action: row.action, reason: row.reason, date: date(row.receivedAt) })}</p>
      <p>{row.excerpt}</p>
    </article>)}
    {current && phase === 'ready' && source === 'manual' && cursor !== null && listedCases.length < MAX_DISPLAY_CASES
      ? <Button onClick={() => load('manual', cursor)}>{t('admin.bot.moderationLog.more')}</Button> : null}
    {current && phase === 'ready' && source === 'automatic' && automaticHasMore ? <p role="note">{t('admin.bot.moderationLog.automaticMore')}</p> : null}
  </section>;
}
