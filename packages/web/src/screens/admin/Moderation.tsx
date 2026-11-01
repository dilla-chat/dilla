// Flow 08: kick and ban. Each is a ConfirmDialog driven by one worker command; every refusal is said by its code.
import { useEffect, useRef, useState } from 'react';
import { ConfirmDialog, Select, TextField } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { errorOf } from '../../core/errors.ts';
import { t } from '../../strings/index.ts';

export const REASON_MAX_BYTES = 512;
/** Ban lengths in seconds, as the select offers them; `forever` sends null. */
export const BAN_LENGTHS = { hour: 3600, day: 86400, week: 604800, month: 2592000, forever: null } as const;
export type BanLength = keyof typeof BAN_LENGTHS;
const LENGTH_KEYS: readonly BanLength[] = ['hour', 'day', 'week', 'month', 'forever'];
const TIMEOUT_LENGTHS = { tenMinutes: 600, hour: 3600, day: 86400, clear: null } as const;
type TimeoutLength = keyof typeof TIMEOUT_LENGTHS;
const TIMEOUT_KEYS: readonly TimeoutLength[] = ['tenMinutes', 'hour', 'day', 'clear'];
const requestId = (): string => [...crypto.getRandomValues(new Uint8Array(16))].map(b => b.toString(16).padStart(2, '0')).join('');

const UTF8 = new TextEncoder();
// Control characters other than line feeds and tabs.
const BAD_CONTROL = /[\u0000-\u0008\u000b\u000c\u000d\u000e-\u001f\u007f-\u009f]/;

/** Why the instance would refuse `reason`; null when it would not. */
export function reasonProblem(reason: string): 'long' | 'control' | null {
  if (UTF8.encode(reason).length > REASON_MAX_BYTES) return 'long';
  if (BAD_CONTROL.test(reason)) return 'control';
  return null;
}

export interface ModTarget { userId: string; name: string }
export interface ModFlowProps {
  open: boolean;
  communityId: string;
  serverName: string;
  target: ModTarget | null;
  onClose(): void;
  /** Runs after the instance accepted; the pane closes the dialog. */
  onDone(): void;
  /** The shell's polite announcer. */
  announce(text: string): void;
}

/** The guards of Create.tsx: one command at a time (a ref), and a session that ends when the dialog closes. */
function useGuarded(open: boolean) {
  const running = useRef(false);
  const session = useRef(0);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    if (open) return;
    session.current += 1;
    running.current = false;
    setBusy(false);
    setError(null);
  }, [open]);
  return { running, session, error, busy, attempt, setError, setBusy, setAttempt };
}

export function KickFlow(props: ModFlowProps): React.JSX.Element {
  const client = useCore();
  const g = useGuarded(props.open);
  const vars = { name: props.target?.name ?? '', server: props.serverName };

  const confirm = () => {
    const target = props.target;
    if (target === null || g.running.current) return;
    g.running.current = true;
    g.setBusy(true);
    g.setError(null);
    const at = g.session.current;
    void (async () => {
      try {
        await client.call({ m: 'kickMember', communityId: props.communityId, userId: target.userId });
        if (g.session.current === at) { g.running.current = false; g.setBusy(false); }
        props.announce(t('mod.kick.done', { name: target.name }));
        if (g.session.current === at) props.onDone();
      } catch (e) {
        const code = errorOf(e).code;
        const message = code === 'E_FORBIDDEN' ? t('mod.kick.error.forbidden', vars)
          : code === 'E_NOT_FOUND' ? t('mod.kick.error.gone', vars) : t('mod.error.other', { code });
        // The error and the end of busy land in one update (ConfirmDialog's contract).
        if (g.session.current === at) { g.setError(message); g.setAttempt(a => a + 1); }
        if (g.session.current === at) { g.running.current = false; g.setBusy(false); }
      }
    })();
  };

  return (
    <ConfirmDialog open={props.open} person={vars.name === '' ? undefined : vars.name} title={t('mod.kick.title', vars)} body={t('mod.kick.body', vars)} confirmLabel={t('mod.kick.confirm')}
      busyLabel={t('mod.kick.busy')} cancelLabel={t('mod.cancel')} busy={g.busy} error={g.error} attempt={g.attempt}
      onConfirm={confirm} onClose={() => props.onClose()} />
  );
}

export function BanFlow(props: ModFlowProps): React.JSX.Element {
  const client = useCore();
  const g = useGuarded(props.open);
  const [reason, setReason] = useState('');
  const [length, setLength] = useState<BanLength>('day');
  const name = props.target?.name ?? '';
  const vars = { name, server: props.serverName };

  useEffect(() => {
    if (!props.open) { setReason(''); setLength('day'); }
  }, [props.open]);

  const confirm = () => {
    const target = props.target;
    if (target === null || g.running.current) return;
    const problem = reasonProblem(reason);
    if (problem !== null) {
      g.setError(t(problem === 'long' ? 'mod.ban.error.reasonLong' : 'mod.ban.error.reasonControl'));
      g.setAttempt(a => a + 1);
      return;
    }
    g.running.current = true;
    g.setBusy(true);
    g.setError(null);
    const at = g.session.current;
    void (async () => {
      try {
        await client.call({ m: 'banMember', communityId: props.communityId, userId: target.userId, reason, seconds: BAN_LENGTHS[length] });
        if (g.session.current === at) { g.running.current = false; g.setBusy(false); }
        props.announce(t('mod.ban.done', { name: target.name }));
        if (g.session.current === at) props.onDone();
      } catch (e) {
        const err = errorOf(e);
        // The reason was checked above, so a 400 (or the worker's own refusal) can only be about the length.
        const message = err.code === 'E_FORBIDDEN' ? t('mod.ban.error.forbidden', vars)
          : err.status === 400 && err.code === 'E_INVALID_REQUEST' ? t('mod.ban.error.clock')
            : t('mod.error.other', { code: err.code });
        if (g.session.current === at) { g.setError(message); g.setAttempt(a => a + 1); }
        if (g.session.current === at) { g.running.current = false; g.setBusy(false); }
      }
    })();
  };

  return (
    <ConfirmDialog open={props.open} person={vars.name === '' ? undefined : vars.name} title={t('mod.ban.title', vars)} body={t('mod.ban.body', vars)} confirmLabel={t('mod.ban.confirm')}
      busyLabel={t('mod.ban.busy')} cancelLabel={t('mod.cancel')} busy={g.busy} error={g.error} attempt={g.attempt}
      onConfirm={confirm} onClose={() => props.onClose()}>
      <TextField id="mod-ban-reason" label={t('mod.ban.reason')} hint={t('mod.ban.reasonHint', { name })} value={reason}
        onChange={v => { setReason(v); g.setError(null); }} disabled={g.busy} />
      <Select label={t('mod.ban.duration')} value={length} onChange={v => setLength(v as BanLength)} disabled={g.busy}
        options={LENGTH_KEYS.map(k => ({ value: k, label: t(`mod.ban.duration.${k}`) }))} />
    </ConfirmDialog>
  );
}

export function TimeoutFlow(props: ModFlowProps): React.JSX.Element {
  const client = useCore();
  const g = useGuarded(props.open);
  const [length, setLength] = useState<TimeoutLength>('tenMinutes');
  const vars = { name: props.target?.name ?? '', server: props.serverName };
  useEffect(() => { if (!props.open) setLength('tenMinutes'); }, [props.open]);

  const confirm = () => {
    const target = props.target;
    if (target === null || g.running.current) return;
    g.running.current = true;
    g.setBusy(true);
    g.setError(null);
    const at = g.session.current;
    const duration = TIMEOUT_LENGTHS[length];
    const command = duration === null
      ? { m: 'clearMemberTimeout' as const, communityId: props.communityId, userId: target.userId, requestId: requestId() }
      : { m: 'setMemberTimeout' as const, communityId: props.communityId, userId: target.userId,
          requestId: requestId(), expires: String(Math.floor(Date.now() / 1000) + duration) };
    void (async () => {
      try {
        await client.call(command);
        if (g.session.current === at) { g.running.current = false; g.setBusy(false); }
        props.announce(t('mod.timeout.done', { name: target.name }));
        if (g.session.current === at) props.onDone();
      } catch (e) {
        const err = errorOf(e);
        const message = err.code === 'E_FORBIDDEN' ? t('mod.timeout.error.forbidden', vars)
          : err.status === 400 && err.code === 'E_INVALID_REQUEST' ? t('mod.timeout.error.clock')
            : t('mod.error.other', { code: err.code });
        if (g.session.current === at) { g.setError(message); g.setAttempt(a => a + 1); g.running.current = false; g.setBusy(false); }
      }
    })();
  };

  return (
    <ConfirmDialog open={props.open} person={vars.name === '' ? undefined : vars.name} title={t('mod.timeout.title', vars)} body={t('mod.timeout.body', vars)} confirmLabel={t('mod.timeout.confirm')}
      busyLabel={t('mod.timeout.busy')} cancelLabel={t('mod.cancel')} busy={g.busy} error={g.error} attempt={g.attempt}
      onConfirm={confirm} onClose={() => props.onClose()}>
      <Select label={t('mod.timeout.duration')} value={length} onChange={value => setLength(value as TimeoutLength)} disabled={g.busy}
        options={TIMEOUT_KEYS.map(key => ({ value: key, label: t(`mod.timeout.duration.${key}`) }))} />
    </ConfirmDialog>
  );
}

export function WarnFlow(props: ModFlowProps): React.JSX.Element {
  const client = useCore();
  const g = useGuarded(props.open);
  const [reason, setReason] = useState('');
  const request = useRef<string | null>(null);
  const vars = { name: props.target?.name ?? '', server: props.serverName };
  useEffect(() => { if (!props.open) { setReason(''); request.current = null; } }, [props.open]);
  const confirm = () => {
    const target = props.target;
    if (target === null || g.running.current) return;
    const problem = reasonProblem(reason);
    if (problem !== null || reason.trim() === '') {
      g.setError(t(problem === 'long' ? 'mod.warn.error.reasonLong' : problem === 'control' ? 'mod.warn.error.reasonControl' : 'mod.warn.error.reasonEmpty'));
      g.setAttempt(a => a + 1); return;
    }
    request.current ??= requestId();
    g.running.current = true; g.setBusy(true); g.setError(null);
    const at = g.session.current;
    void (async () => {
      try {
        const result = await client.call({ m: 'warnMember', communityId: props.communityId, userId: target.userId,
          requestId: request.current!, reason }) as { evidence: 'accepted' | 'pending' };
        if (g.session.current === at) { g.running.current = false; g.setBusy(false); }
        props.announce(result.evidence === 'accepted' ? t('mod.warn.done', vars) : t('mod.warn.evidencePending', vars));
        if (g.session.current === at) props.onDone();
      } catch (e) {
        const err = errorOf(e);
        const message = err.code === 'E_FORBIDDEN' ? t('mod.warn.error.forbidden', vars) : t('mod.error.other', { code: err.code });
        if (g.session.current === at) { g.setError(message); g.setAttempt(a => a + 1); g.running.current = false; g.setBusy(false); }
      }
    })();
  };
  return <ConfirmDialog open={props.open} person={vars.name === '' ? undefined : vars.name} title={t('mod.warn.title', vars)} body={t('mod.warn.body', vars)} confirmLabel={t('mod.warn.confirm')}
    busyLabel={t('mod.warn.busy')} cancelLabel={t('mod.cancel')} busy={g.busy} error={g.error} attempt={g.attempt}
    onConfirm={confirm} onClose={() => props.onClose()}>
    <TextField id="mod-warn-reason" label={t('mod.warn.reason')} value={reason} onChange={value => { setReason(value); g.setError(null); }} disabled={g.busy || request.current !== null} />
  </ConfirmDialog>;
}
