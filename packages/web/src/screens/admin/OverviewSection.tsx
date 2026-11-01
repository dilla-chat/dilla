import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { Button, ConfirmDialog, Segmented, SettingsRow, TextField } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { useSlice } from '../../core/use-slice.ts';
import { useSettingsEdit, type SettingsEditState } from '../../settings/edit-state.tsx';
import { t, type StringKey } from '../../strings/index.ts';
import { SERVER_NAME_MAX_BYTES, nameProblem } from '../Create.tsx';
import { forbidden, nfc, refused, statusIs, useRun, type SectionProps } from './shared.ts';

/** The inactive timeouts the prototype offers, in seconds, with its short labels (Task 11, Q7). */
const TIMEOUTS = [60, 300, 900, 1800, 3600] as const;

/**
 * Server name and AFK timeout. Their baselines stay independent so a partial save retries only what failed. Task 11 (Jonas, Q7;
 * the prototype's Overview): the server's name under the heading, one card with the name row (dilla's own), the inactive
 * timeout as a segmented choice, and Leave server, which an owner is told to hand over first.
 */
export function OverviewSection({ communityId, serverName, announce, owner = false, onGone }: SectionProps & { owner?: boolean; onGone?: () => void }): React.JSX.Element {
  const client = useCore();
  const base = useId();
  const fieldId = `${base}-name`;
  const [name, setName] = useState(serverName);
  const [nameBase, setNameBase] = useState(serverName);
  const [localError, setLocalError] = useState<string | null>(null);
  const [afkFailure, setAfkFailure] = useState(false);
  const { busy, error: actionError, run, reset: resetAction } = useRun();
  const afk = useSlice(`afk:${communityId}`);
  const channels = useSlice(`channels:${communityId}`);
  const [afkTimeout, setAFKTimeout] = useState(300);
  const [afkBase, setAFKBase] = useState<number | null>(null);
  const [afkLoadError, setAFKLoadError] = useState(false);
  const afkEdited = useRef(false);
  const shownName = useRef(serverName);
  const nameChanged = nfc(name) !== nfc(nameBase);
  const timeoutChanged = afkBase !== null && afkTimeout !== afkBase;
  const dirty = nameChanged || timeoutChanged;

  useEffect(() => {
    let cancelled = false;
    void client.call({ m: 'getAFKSettings', communityId }).then(value => {
      if (cancelled) return;
      if (typeof value !== 'object' || value === null || !('timeoutSeconds' in value) || typeof value.timeoutSeconds !== 'number') throw new Error('invalid AFK settings');
      setAFKBase(value.timeoutSeconds);
      if (!afkEdited.current) setAFKTimeout(value.timeoutSeconds);
      setAFKLoadError(false);
    }).catch(() => { if (!cancelled) setAFKLoadError(true); });
    return () => { cancelled = true; };
  }, [client, communityId, afk]);
  useEffect(() => {
    const before = shownName.current;
    shownName.current = serverName;
    setNameBase(serverName);
    setName(current => current === before ? serverName : current);
  }, [serverName]);

  const save = useCallback(async (): Promise<boolean> => {
    if (busy || !dirty) return false;
    if (nameChanged) {
      const problem = nameProblem(name, SERVER_NAME_MAX_BYTES);
      if (problem !== null) {
        setLocalError(t(problem === 'empty' ? 'admin.overview.error.empty' : problem === 'long' ? 'admin.overview.error.long' : 'admin.overview.error.refused'));
        return false;
      }
    }
    setLocalError(null); setAfkFailure(false);
    const savingName = name.trim();
    const shouldRename = nameChanged;
    const shouldSaveTimeout = timeoutChanged;
    let renamed = false;
    const ok = await run(async () => {
      if (shouldRename) {
        await client.call({ m: 'renameCommunity', communityId, name: savingName });
        renamed = true;
        setNameBase(savingName);
      }
      if (shouldSaveTimeout) await client.call({ m: 'setAFK', communityId, timeoutSeconds: afkTimeout });
    }, e => {
      if (renamed) {
        setAfkFailure(true);
        announce(t('workspace.overview.partial'));
        return t('admin.afk.error', { code: e.code });
      }
      if (shouldSaveTimeout) setAfkFailure(true);
      return shouldSaveTimeout && !shouldRename ? t('admin.afk.error', { code: e.code })
        : statusIs(e, 409) ? t('admin.overview.error.conflict')
          : forbidden(e) ? t('admin.overview.error.forbidden')
            : refused(e) ? t('admin.overview.error.refused')
              : t('admin.overview.error.other', { code: e.code });
    });
    if (ok) {
      setNameBase(savingName);
      setAFKBase(afkTimeout);
      afkEdited.current = false;
      setAfkFailure(false);
    }
    return ok;
  }, [busy, run, afkTimeout, announce, client, communityId, dirty, name, nameChanged, timeoutChanged]);
  const reset = useCallback(() => {
    if (busy) return;
    setName(nameBase);
    if (afkBase !== null) setAFKTimeout(afkBase);
    afkEdited.current = false;
    setLocalError(null); setAfkFailure(false); resetAction();
  }, [busy, resetAction, afkBase, nameBase]);
  const state = useMemo<SettingsEditState>(() => ({ dirty, busy, canSave: dirty, save, reset }), [dirty, busy, save, reset]);
  useSettingsEdit(state);

  const error = localError ?? actionError;
  const [leaving, setLeaving] = useState(false);
  const [ownerNote, setOwnerNote] = useState(false);
  const leaveRun = useRun();
  const leave = () => {
    void leaveRun.run(() => client.call({ m: 'leaveCommunity', communityId }), e => t('admin.leave.error.other', { code: e.code }))
      .then(ok => { if (ok) { announce(t('admin.left', { server: serverName })); setLeaving(false); onGone?.(); } });
  };
  void channels;

  return <section className="dw-settings-section" aria-labelledby={`${base}-title`}>
    <h2 id={`${base}-title`} tabIndex={-1}>{t('admin.overview.title')}</h2>
    <p>{serverName}</p>
    <SettingsRow id={`${base}-name-row`} icon="server" label={t('admin.overview.name')} hint={t('admin.overview.hint')}>
      <div className="dw-overview-field">
        <TextField id={fieldId} label={t('admin.overview.name')} value={name}
          onChange={value => { setName(value); setLocalError(null); setAfkFailure(false); resetAction(); }}
          error={afkFailure ? undefined : error ?? undefined} required autoComplete="off" spellCheck={false} disabled={busy} />
      </div>
    </SettingsRow>
    <SettingsRow id={`${base}-afk-row`} icon="moon" label={t('admin.overview.afk')} hint={t('admin.overview.afkHint')}>
      <Segmented id={`${base}-afk`} label={t('admin.overview.afk')} value={String(afkTimeout)} disabled={afkBase === null || busy}
        options={TIMEOUTS.map(n => ({ value: String(n), label: t(`admin.overview.afk.${n}` as StringKey) }))}
        onChange={value => { afkEdited.current = true; setAFKTimeout(Number(value)); setAfkFailure(false); resetAction(); }} />
      {afkLoadError ? <p role="alert">{t('admin.afk.loadError')}</p> : null}
      {afkFailure && error ? <p role="alert">{error}</p> : null}
    </SettingsRow>
    <SettingsRow id={`${base}-leave-row`} icon="log-out" label={t('admin.overview.leave')} hint={t('admin.overview.leaveHint')}>
      <Button variant="danger" onClick={() => { if (owner) setOwnerNote(true); else { leaveRun.reset(); setLeaving(true); } }}>{t('admin.leave.confirm')}</Button>
      {ownerNote ? <p role="status">{t('admin.leave.owner', { server: serverName })}</p> : null}
    </SettingsRow>
    <ConfirmDialog open={leaving} title={t('admin.leave.title', { server: serverName })} body={t('admin.leave.body', { server: serverName })}
      confirmLabel={t('admin.leave.confirm')} busyLabel={t('admin.leave.busy')} cancelLabel={t('admin.cancel')}
      busy={leaveRun.busy} error={leaveRun.error} attempt={leaveRun.attempt} onConfirm={leave} onClose={() => { if (!leaveRun.busy) setLeaving(false); }} />
  </section>;
}
