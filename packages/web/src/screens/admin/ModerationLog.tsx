import { useEffect, useId, useRef, useState } from 'react';
import { Button } from '@dilla/ui';
import { MODERATION_CASES, MODERATION_EVIDENCE, readModerationPage, type ModerationCase } from '@dilla/client-core';
import { useCore } from '../../core/context.tsx';
import { t } from '../../strings/index.ts';

type Phase = 'idle' | 'sending' | 'ready' | 'denied' | 'unavailable' | 'error';
type Source = 'automatic' | 'manual';
const MAX_DISPLAY_CASES = 100;
/** The v1 status the Bot answers when the reader may not view the audit log. */
const FORBIDDEN = 2;

/** Bot v2 Task 23: each page is a sealed Bot read (automatic cases, manual evidence) the Bot answers directly. */
export function ModerationLog({ communityId, showHeading = true }: { communityId: string; showHeading?: boolean }): React.JSX.Element {
  const client = useCore();
  const base = useId();
  const [forCommunity, setForCommunity] = useState(communityId);
  const [phase, setPhase] = useState<Phase>('idle');
  const [cursor, setCursor] = useState<string | null>(null);
  const [source, setSource] = useState<Source>('manual');
  const [listedCases, setListedCases] = useState<ModerationCase[]>([]);
  const [automaticHasMore, setAutomaticHasMore] = useState(false);
  const generation = useRef(0);

  useEffect(() => {
    generation.current += 1;
    setForCommunity(communityId);
    setPhase('idle');
    setCursor(null);
    setListedCases([]);
    setAutomaticHasMore(false);
    return () => { generation.current += 1; };
  }, [communityId]);

  const load = (kind: Source, after: string | null = null): void => {
    if (phase === 'sending' || forCommunity !== communityId) return;
    const id = ++generation.current;
    setSource(kind);
    if (after === null) setListedCases([]);
    setPhase('sending');
    const operation = kind === 'manual' ? MODERATION_EVIDENCE : MODERATION_CASES;
    void readModerationPage(client, communityId, operation, kind === 'manual' ? after : null).then(page => {
      if (id !== generation.current) return;
      if (page.status === FORBIDDEN) { setListedCases([]); setPhase('denied'); return; }
      if (page.status !== 0) { setPhase('unavailable'); return; }
      setListedCases(previous => {
        const seen = new Set(previous.map(row => row.key));
        const additions = page.cases.filter(row => {
          if (seen.has(row.key)) return false;
          seen.add(row.key);
          return true;
        });
        return [...previous, ...additions].slice(0, MAX_DISPLAY_CASES);
      });
      setCursor(kind === 'manual' ? page.nextCursor : null);
      setAutomaticHasMore(kind === 'automatic' && page.nextCursor !== null);
      setPhase('ready');
    }).catch(() => { if (id === generation.current) setPhase('error'); });
  };

  const current = forCommunity === communityId;
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
      {phase === 'sending' && source === 'manual' ? t('admin.bot.moderationLog.loading') : t('admin.bot.moderationLog.load')}
    </Button>
    {phase === 'denied' ? <p role="note">{t('admin.bot.moderationLog.denied')}</p> : null}
    {phase === 'error' ? <p role="alert">{t('admin.bot.moderationLog.error')}</p> : null}
    {current && phase === 'unavailable' ? <p role="status">{t('admin.bot.moderationLog.responseUnavailable')}</p> : null}
    {current && phase === 'ready' && cases.length === 0 && (source === 'manual' ? cursor === null : !automaticHasMore)
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
