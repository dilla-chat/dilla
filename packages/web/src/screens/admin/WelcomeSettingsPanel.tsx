import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { Button, SettingsRow } from '@dilla/ui';
import { useSettingsEdit, type SettingsEditState } from '../../settings/edit-state.tsx';
import { t } from '../../strings/index.ts';
import { decodeWelcomeSettings, encodeWelcomeSettings, type WelcomeSettingsValue } from './welcome-settings.ts';
import { useBotDraftState, useBotInteractionOperation } from './bot-settings-context.tsx';

type Upload = 'accepted' | 'pending' | 'rejected' | 'unknown';
type WelcomeResponse = {
  deliveryId: string;
  requestId: string;
  communityId: string;
  operation: number;
  channelId: string | null;
  status: number;
  revision: string | null;
  data: Uint8Array;
  sourceSeq: string;
};
export interface WelcomeSettingsGateway {
  request(input: { communityId: string; channelId: null; requestId: string; operation: 1 | 2;
    args: readonly (string | number | Uint8Array | null)[]; revision: string | null }): Promise<{ requestId: string; upload: Upload }>;
  read(communityId: string, operations: readonly number[], requestIds: readonly string[]): Promise<{ responses: WelcomeResponse[]; through: string | null; blocked: boolean }>;
  ack(communityId: string, through: string): Promise<null>;
}

interface WelcomeSettingsPanelProps {
  communityId: string;
  serverName: string;
  channels: { id: string; name: string }[];
  enabled: boolean;
  canManage: boolean;
  gateway: WelcomeSettingsGateway;
}

interface PendingRequest {
  communityId: string;
  requestId: string;
  operation: 1 | 2;
  saved?: WelcomeSettingsValue;
}

const newRequestId = (): string => [...crypto.getRandomValues(new Uint8Array(16))]
  .map(byte => byte.toString(16).padStart(2, '0')).join('');
const validRevision = (value: string | null): value is string => value !== null && /^(0|[1-9][0-9]{0,18})$/u.test(value)
  && BigInt(value) <= 0x7fff_ffff_ffff_ffffn;

export function WelcomeSettingsPanel({ communityId, serverName, channels, enabled, canManage, gateway }: WelcomeSettingsPanelProps): React.JSX.Element {
  const base = useId();
  const dirtyRef = useRef(false);
  const sharedOperations = useBotInteractionOperation('welcome');
  const sharedPending = sharedOperations?.operation('welcome') ?? null;
  const [config, setConfig] = useBotDraftState<WelcomeSettingsValue | null>(communityId, 'welcome.config', null);
  const [revision, setRevision] = useBotDraftState<string | null>(communityId, 'welcome.revision', null);
  const [selectedChannel, setSelectedChannel] = useBotDraftState(communityId, 'welcome.selectedChannel', '');
  const [template, setTemplate] = useBotDraftState(communityId, 'welcome.template', 'Welcome {member} to {server}!');
  const [loaded, setLoaded] = useBotDraftState(communityId, 'welcome.loaded', false);
  const [mustReload, setMustReload] = useBotDraftState(communityId, 'welcome.mustReload', false);
  const [pending, setPending] = useState<PendingRequest | null>(null);
  const [ackThrough, setAckThrough] = useState<{ communityId: string; through: string } | null>(null);
  const [ackRetry, setAckRetry] = useState(0);
  const generation = useRef(0);
  const currentCommunity = useRef(communityId);
  const previousCommunity = useRef(communityId);
  const sendingRequest = useRef<string | null>(null);
  useLayoutEffect(() => {
    currentCommunity.current = communityId;
    generation.current += 1;
    sendingRequest.current = null;
  }, [communityId]);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const unavailable = !enabled || !canManage || channels.length === 0;
  const operationBusy = sharedPending !== null && (sharedPending.phase === 'sending' || sharedPending.phase === 'waiting');
  const channelIdsKey = channels.map(channel => channel.id).join(',');
  const fallbackChannel = channels[0]?.id ?? '';
  const configuredDraft = config !== null && config.channelId === selectedChannel
    && config.template === template && config.serverName === serverName;

  const request = useCallback(async (operation: 1 | 2, saved?: WelcomeSettingsValue): Promise<void> => {
    if (sharedOperations !== null) {
      const args: readonly (string | number | Uint8Array | null)[] = operation === 1 ? [5] : [5, encodeWelcomeSettings(saved!)];
      const requestId = sharedOperations.startOperation('welcome', { communityId, operation, args,
        revision: operation === 1 ? null : revision, ...(saved === undefined ? {} : { metadata: saved }) });
      if (requestId === null) setError(t('admin.bot.welcomeSettings.uploadFailed'));
      else { setError(null); setNotice(t('admin.bot.welcomeSettings.waiting')); }
      return;
    }
    if (sendingRequest.current !== null || pending?.communityId === communityId || unavailable) return;
    const requestId = newRequestId();
    const requestGeneration = generation.current;
    sendingRequest.current = requestId;
    setError(null);
    setNotice(null);
    try {
      const args: readonly (string | number | Uint8Array | null)[] = operation === 1
        ? [5]
        : [5, encodeWelcomeSettings(saved!)];
      const upload = await gateway.request({ communityId, channelId: null, requestId, operation, args,
        revision: operation === 1 ? null : revision });
      if (requestGeneration !== generation.current || currentCommunity.current !== communityId) return;
      if (upload.requestId !== requestId || upload.upload === 'rejected') {
        setError(t('admin.bot.welcomeSettings.uploadFailed'));
        return;
      }
      setPending({ communityId, requestId, operation, ...(saved === undefined ? {} : { saved }) });
      setNotice(t(upload.upload === 'accepted' ? 'admin.bot.welcomeSettings.waiting' : 'admin.bot.welcomeSettings.uploadUncertain'));
    } catch {
      if (requestGeneration === generation.current && currentCommunity.current === communityId) {
        setError(t('admin.bot.welcomeSettings.uploadFailed'));
      }
    } finally {
      if (sendingRequest.current === requestId) sendingRequest.current = null;
    }
  }, [sharedOperations, communityId, revision, setError, setNotice, pending, unavailable, gateway, currentCommunity, generation,
    sendingRequest, setPending]);

  useEffect(() => {
    if (sharedOperations === null || sharedPending === null) return;
    if (sharedPending.phase === 'failed') { setError(t('admin.bot.welcomeSettings.uploadFailed')); return; }
    if (sharedPending.error === 'read') setError(t('admin.bot.welcomeSettings.readFailed'));
    const response = sharedPending.response;
    if (response === null || !sharedPending.acked) return;
    try {
      if (response.status !== 0 || !validRevision(response.revision)) {
        setError(t(response.status === 4 ? 'admin.bot.welcomeSettings.conflict' : 'admin.bot.welcomeSettings.responseFailed'));
        if (response.status === 4) { setMustReload(true); }
      } else if (sharedPending.operation === 1) {
        const saved = decodeWelcomeSettings(response.data);
        setConfig(saved);
        if (!dirtyRef.current) {
          setSelectedChannel(saved?.channelId ?? fallbackChannel);
          setTemplate(saved?.template ?? 'Welcome {member} to {server}!');
        } setRevision(response.revision);
        setLoaded(true); setMustReload(false); setNotice(null); setError(null);
      } else {
        const empty = decodeWelcomeSettings(response.data);
        if (empty !== null) throw new Error('unexpected welcome save response data');
        const saved = sharedPending.metadata as WelcomeSettingsValue | undefined;
        if (saved === undefined) throw new Error('missing welcome save payload');
        setConfig(saved); setRevision(response.revision); setLoaded(true); setMustReload(false);
        setNotice(t('admin.bot.welcomeSettings.saved')); setError(null);
      }
    } catch {
      setError(t('admin.bot.welcomeSettings.responseFailed'));
    } finally {
      sharedOperations.clearOperation('welcome');
    }
  }, [fallbackChannel, sharedOperations, sharedPending, setConfig, setLoaded, setMustReload, setRevision, setSelectedChannel, setTemplate]);

  useEffect(() => {
    if (previousCommunity.current === communityId) return;
    previousCommunity.current = communityId;
    setConfig(null);
    setRevision(null);
    setSelectedChannel('');
    setTemplate('Welcome {member} to {server}!');
    setLoaded(false);
    setMustReload(false);
    setPending(null);
    setAckThrough(null);
    setAckRetry(0);
    setError(null);
    setNotice(null);
  }, [communityId, setConfig, setRevision, setSelectedChannel, setTemplate, setLoaded, setMustReload]);

  useEffect(() => {
    if (pending === null || pending.communityId !== communityId) return;
    let canceled = false;
    let timer: number | undefined;
    const poll = async (): Promise<void> => {
      try {
        const page = await gateway.read(communityId, [1, 2], [pending.requestId]);
        if (canceled) return;
        const response = page.responses.find(row => row.requestId === pending.requestId && row.communityId === pending.communityId
          && row.operation === pending.operation && row.channelId === null);
        if (response !== undefined) {
          if (response.status === 0 && validRevision(response.revision)) {
            if (pending.operation === 1) {
              const saved = decodeWelcomeSettings(response.data);
              setConfig(saved);
              if (!dirtyRef.current) {
                setSelectedChannel(saved?.channelId ?? fallbackChannel);
                setTemplate(saved?.template ?? 'Welcome {member} to {server}!');
              }
              setRevision(response.revision);
              setLoaded(true);
              setMustReload(false);
              setNotice(null);
            } else if (pending.saved !== undefined) {
              const empty = decodeWelcomeSettings(response.data);
              if (empty !== null) throw new Error('unexpected welcome save response data');
              setConfig(pending.saved);
              setRevision(response.revision);
              setLoaded(true);
              setMustReload(false);
              setNotice(t('admin.bot.welcomeSettings.saved'));
            }
            setPending(null);
          } else {
            setPending(null);
            setError(t(response.status === 4 ? 'admin.bot.welcomeSettings.conflict' : 'admin.bot.welcomeSettings.responseFailed'));
            if (response.status === 4) { setMustReload(true); }
          }
          // Acknowledge in an effect after React commits the visible result.
          const ownResponse = (row: WelcomeResponse): boolean => row.requestId === pending.requestId
            && row.communityId === pending.communityId && row.operation === pending.operation && row.channelId === null;
          if (page.through !== null && ownResponse(response) && page.responses.every(ownResponse)) {
            setAckRetry(0);
            setAckThrough({ communityId: pending.communityId, through: page.through });
          }
        }
        if (!canceled && pending !== null) timer = window.setTimeout(() => { void poll(); }, 750);
      } catch {
        if (!canceled) {
          setError(t('admin.bot.welcomeSettings.readFailed'));
          timer = window.setTimeout(() => { void poll(); }, 1500);
        }
      }
    };
    void poll();
    return () => { canceled = true; if (timer !== undefined) window.clearTimeout(timer); };
  }, [communityId, channelIdsKey, fallbackChannel, gateway, pending, setConfig, setLoaded, setMustReload, setRevision, setSelectedChannel, setTemplate]);

  useEffect(() => {
    if (ackThrough === null) return;
    let canceled = false;
    let retryTimer: number | undefined;
    void gateway.ack(ackThrough.communityId, ackThrough.through).then(() => {
      if (!canceled) setAckThrough(current => current?.communityId === ackThrough.communityId
        && current.through === ackThrough.through ? null : current);
    }).catch(() => {
      if (!canceled) retryTimer = window.setTimeout(() => {
        setAckRetry(attempt => attempt + 1);
      }, 1500);
    });
    return () => { canceled = true; if (retryTimer !== undefined) window.clearTimeout(retryTimer); };
  }, [ackThrough, ackRetry, gateway]);

  const load = (): void => { void request(1); };
  const submitSave = useCallback(async (): Promise<boolean> => {
    if (mustReload || revision === null || selectedChannel === '' || template.trim() === '' || new TextEncoder().encode(template).length > 2000) {
      setError(t('admin.bot.welcomeSettings.invalid'));
      return false;
    }
    const saved = { channelId: selectedChannel, template, serverName };
    if (sharedOperations === null) { await request(2, saved); return false; }
    const requestId = sharedOperations.startOperation('welcome', { communityId, operation: 2,
      args: [5, encodeWelcomeSettings(saved)], revision, metadata: saved });
    if (requestId === null) return false;
    setError(null); setNotice(t('admin.bot.welcomeSettings.waiting'));
    const result = await sharedOperations.waitOperation('welcome', requestId);
    return result?.response?.status === 0;
  }, [mustReload, revision, selectedChannel, template, serverName, sharedOperations, communityId, request, setError, setNotice]);

  const dirty = loaded && (config === null || !configuredDraft);
  dirtyRef.current = dirty;
  const canSave = dirty && !unavailable && revision !== null && !mustReload && selectedChannel !== ''
    && template.trim() !== '' && new TextEncoder().encode(template).length <= 2000 && !operationBusy;
  const editState = useMemo<SettingsEditState>(() => ({ dirty, busy: operationBusy, canSave, save: submitSave, reset: () => {
    setSelectedChannel(config?.channelId ?? fallbackChannel); setTemplate(config?.template ?? 'Welcome {member} to {server}!');
    if (config === null) setLoaded(false);
  } }), [dirty, operationBusy, canSave, submitSave, config, fallbackChannel, setLoaded, setSelectedChannel, setTemplate]);
  const managed = useSettingsEdit(editState);

  return <section aria-labelledby={`${base}-title`}>
    <h3 id={`${base}-title`}>{t('admin.bot.welcomeSettings.title')}</h3>
    <p>{t('admin.bot.welcomeSettings.help')}</p>
    {!enabled ? <p role="note">{t('admin.bot.welcomeSettings.requiresCapability')}</p> : null}
    {!canManage ? <p role="note">{t('admin.bot.welcomeSettings.readOnly')}</p> : null}
    {channels.length === 0 ? <p role="note">{t('admin.bot.welcomeSettings.noChannels')}</p> : null}
    {!loaded ? <Button aria-disabled={unavailable || pending !== null || operationBusy || undefined} onClick={load}>
      {pending?.operation === 1 || sharedPending?.operation === 1 && operationBusy ? t('admin.bot.welcomeSettings.loading') : t('admin.bot.welcomeSettings.load')}
    </Button> : <>
      <SettingsRow id={`${base}-channel`} label={t('admin.bot.welcomeSettings.channel')}>
        <select aria-label={t('admin.bot.welcomeSettings.channel')} value={selectedChannel} disabled={unavailable || pending !== null || operationBusy}
          onChange={event => setSelectedChannel(event.currentTarget.value)}>
          <option value="">{t('admin.bot.welcomeSettings.chooseChannel')}</option>
          {channels.map(channel => <option key={channel.id} value={channel.id}>{channel.name}</option>)}
        </select>
      </SettingsRow>
      <SettingsRow id={`${base}-template`} label={t('admin.bot.welcomeSettings.template')} layout="stacked">
        <textarea aria-label={t('admin.bot.welcomeSettings.template')} value={template} maxLength={2000}
          disabled={unavailable || pending !== null || operationBusy} onChange={event => setTemplate(event.currentTarget.value)} />
        <p>{t('admin.bot.welcomeSettings.tokens')}</p>
      </SettingsRow>
      {!managed ? <Button aria-disabled={unavailable || pending !== null || operationBusy || revision === null || mustReload || undefined}
        onClick={() => void submitSave()}>
        {pending?.operation === 2 || sharedPending?.operation === 2 && operationBusy ? t('admin.bot.welcomeSettings.saving') : t('admin.bot.welcomeSettings.save')}
      </Button> : null}
      <Button aria-disabled={pending !== null || operationBusy || undefined} onClick={load}>{t('admin.bot.welcomeSettings.reload')}</Button>
    </>}
    {sharedPending?.phase === 'waiting' ? <p role="status" aria-live="polite">{t(sharedPending.uploadStatus === 'accepted'
      ? 'admin.bot.welcomeSettings.waiting' : 'admin.bot.welcomeSettings.uploadUncertain')}</p>
      : notice !== null ? <p role="status" aria-live="polite">{notice}</p> : null}
    {sharedPending !== null && sharedPending.response === null && sharedPending.phase === 'waiting'
      && sharedPending.uploadStatus !== 'accepted' && sharedPending.uploadStatus !== 'pending'
      ? <Button onClick={() => sharedOperations?.retryOperation('welcome')}>{t('admin.bot.welcomeSettings.retry')}</Button> : null}
    {error !== null ? <p role="alert">{error}</p> : sharedPending?.error === 'read' ? <p role="alert">{t('admin.bot.welcomeSettings.readFailed')}</p> : null}
    {configuredDraft && loaded ? <p>{t('admin.bot.welcomeSettings.configured')}</p> : null}
  </section>;
}
