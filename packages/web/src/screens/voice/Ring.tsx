// Incoming DM rings (flow 06, decision 6, R1–R8): the banner at the top of the main landmark, answer and decline, and the
// desktop notification of each ring that may raise one. The ring sound is useSounds'. A ring never moves focus.
import { useEffect, useLayoutEffect, useRef } from 'react';
import type { CoreClient, DmSummary, Ring } from '@dilla/client-core';
import { RingBanner } from '@dilla/ui';
import { useDocumentVisible } from '../../core/use-visible.ts';
import { notificationCtor, ringNotification, type NotificationHandle } from '../../notify.ts';
import { t } from '../../strings/index.ts';

export interface RingFlowProps {
  presenceChoice?: string;
  client: Pick<CoreClient, 'call'>;
  rings: readonly Ring[] | undefined;
  dms: readonly DmSummary[] | undefined;
  settings: Readonly<Record<string, string>>;
  /** The name of the voice this device is in, for the busy text; null outside a call. */
  inCallWhere: string | null;
  /** Opens the DM (the notification's click) and joins its call (answer). */
  onOpen(channelId: string): void;
  onAnswer(channelId: string): void;
}

const key = (r: Ring) => `${r.callId}:${r.ringTs}`;

/**
 * Each ring's notification while it lasts and the page is hidden (flow 06): raised when the page hides during a ring,
 * closed when the ring ends or the page shows again, and all of them when the shell goes.
 */
function useRingNotifications(p: RingFlowProps): void {
  const open = useRef(new Map<string, NotificationHandle>());
  const live = useRef(p);
  live.current = p;
  const visible = useDocumentVisible();
  useEffect(() => {
    const rings = visible || p.presenceChoice === undefined || p.presenceChoice === 'dnd' ? [] : p.rings ?? [];
    const now = new Set(rings.map(key));
    for (const [k, note] of open.current) {
      if (now.has(k)) continue;
      note.close();
      open.current.delete(k);
    }
    for (const ring of rings) {
      const k = key(ring);
      if (open.current.has(k)) continue;
      const kind = p.dms?.find(d => d.id === ring.channelId)?.kind ?? 3;
      const note = ringNotification(ring, { Notification: notificationCtor(), settings: p.settings, kind, visible, presenceChoice: p.presenceChoice,
        focus: () => window.focus(), open: id => live.current.onOpen(id) });
      // A ring that may not notify is remembered too, so a later render never raises it.
      open.current.set(k, note ?? { onclick: null, close: () => undefined });
    }
  }, [p.rings, p.dms, p.settings, p.presenceChoice, visible]);
  useEffect(() => () => {
    for (const note of open.current.values()) note.close();
    open.current.clear();
  }, []);
}

/**
 * Hands focus to the main landmark when the banner goes while focus is on its buttons (a decline, an answer, or the ring
 * ending): its cleanup runs before React removes the banner's nodes, so focus never falls to the page (review I5).
 */
function FocusKeeper() {
  // Its own node: a child's layout effect runs before its parent's ref is attached, so the banner is found from here.
  const mark = useRef<HTMLSpanElement>(null);
  useLayoutEffect(() => {
    const el = mark.current?.closest('.dw-ring') ?? null;
    return () => {
      if (el === null || !el.contains(document.activeElement)) return;
      el.closest('main')?.focus();
    };
  }, []);
  return <span ref={mark} hidden />;
}

/** The ring banner (L-UI-70) and the ring's notifications; nothing renders without a ring. */
export function RingFlow(p: RingFlowProps): React.JSX.Element | null {
  useRingNotifications(p);
  const rings = p.rings ?? [];
  if (rings.length === 0) return null;
  const decline = (callId: string) => {
    const ring = rings.find(r => r.callId === callId);
    if (ring !== undefined) p.client.call({ m: 'callDecline', callId, ringTs: ring.ringTs }).catch(() => undefined);
  };
  const answer = (callId: string) => {
    const ring = rings.find(r => r.callId === callId);
    if (ring !== undefined) p.onAnswer(ring.channelId);
  };
  return (
    <div className="dw-ring" data-doc-state="call-ring">
      <FocusKeeper />
      <RingBanner rings={rings.map(r => ({
        callId: r.callId, busy: r.busy, name: r.callerName,
        text: r.busy && p.inCallWhere !== null ? t('call.ring.busy', { name: r.callerName, where: p.inCallWhere })
          : t('call.ring.incoming', { name: r.callerName }),
      }))} labels={{ region: t('call.ring.region'), answer: t('call.ring.answer'), decline: t('call.ring.decline') }}
        onAnswer={answer} onDecline={decline} />
    </div>
  );
}
