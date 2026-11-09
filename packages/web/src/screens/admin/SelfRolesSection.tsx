// Server Settings → Self-service roles: managers approve eligible roles; members opt into approved roles.
import { useEffect, useId, useRef, useState } from 'react';
import { BotOperationUncertainError, createBotOperations, decode, encode, fromHex, toHex, type RoleSummary } from '@dilla/client-core';
import { Button, SettingsRow } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { errorOf, type UiError } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { t } from '../../strings/index.ts';
import { PERM } from '@dilla/client-core';
import { type SectionProps } from './shared.ts';

type SelfRole = { id: string; name: string; position: number; selfSelected: boolean };
type Installation = { communityId: string; state: number; capabilities: number };
type PendingPanel = { requestId: string; channelId: string; roleIds: string[]; uploadUncertain: boolean };
const requestId = (): string => [...crypto.getRandomValues(new Uint8Array(16))].map(b => b.toString(16).padStart(2, '0')).join('');
/** A sealed answer's v1 status, as the code it stands for (client-core operations' STATUS_OF_CODE, reversed). */
const STATUS_CODE: Readonly<Record<number, string>> = { 1: 'E_INVALID', 2: 'E_FORBIDDEN', 3: 'E_BOT_STALE', 4: 'E_CONFLICT', 5: 'E_UNAVAILABLE',
  6: 'E_BUSY', 7: 'E_NOT_FOUND', 8: 'E_UPGRADE_REQUIRED', 9: 'E_INTERNAL' };
const SELF_ROLE_BITS = PERM.viewChannel | PERM.sendMessages | PERM.attachFiles | PERM.addReactions | PERM.readHistory
  | PERM.connect | PERM.speak | PERM.video | PERM.screenShare;

function errorMessage(error: UiError): string {
  if (error.code === 'E_FORBIDDEN' || error.status === 403) return t('admin.bot.error.forbidden');
  if (error.status === 409 || error.code === 'E_CONFLICT') return t('admin.bot.error.conflict');
  return t('admin.bot.error.other', { code: error.code });
}

export function SelfRolesSection({ communityId, announce }: SectionProps): React.JSX.Element {
  const client = useCore();
  const base = useId();
  const roles = useSlice(`roles:${communityId}`);
  const access = useSlice(`permissions:${communityId}`);
  // Bot v2 Task 22: a role panel is posted by the Bot, into a Bot channel only.
  const botChannels = useSlice(`botCatalog:${communityId}`)?.botChannels ?? [];
  const channels = (useSlice(`channels:${communityId}`) ?? []).filter(channel => channel.kind === 0 && botChannels.includes(channel.id));
  const [selfRoles, setSelfRoles] = useState<SelfRole[]>([]);
  const [loadedFor, setLoadedFor] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [installation, setInstallation] = useState<Installation | null>(null);
  const [installationLoaded, setInstallationLoaded] = useState(false);
  const [panelChannel, setPanelChannel] = useState('');
  const [emojis, setEmojis] = useState<Record<string, string>>({});
  const [pendingPanel, setPendingPanel] = useState<PendingPanel | null>(null);
  const [panelNotice, setPanelNotice] = useState<string | null>(null);
  const generation = useRef(0);
  const currentCommunity = useRef(communityId);
  currentCommunity.current = communityId;

  useEffect(() => {
    let live = true;
    setInstallation(null); setInstallationLoaded(false); setPendingPanel(null); setPanelNotice(null);
    setBusy(false);
    void client.call({ m: 'getBotInstallation', communityId }).then(value => {
      if (!live) return;
      const row = value as Installation | null;
      setInstallation(row?.communityId === communityId ? row : null);
      setInstallationLoaded(true);
    }).catch(() => { if (live) setInstallationLoaded(true); });
    return () => { live = false; };
  }, [client, communityId]);

  useEffect(() => {
    const read = ++generation.current;
    setSelfRoles([]); setLoadedFor(null); setError(null);
    void client.call({ m: 'listSelfRoles', communityId }).then(value => {
      if (read !== generation.current) return;
      setSelfRoles(Array.isArray(value) ? value as SelfRole[] : []); setLoadedFor(communityId);
    }).catch((e: unknown) => {
      if (read !== generation.current) return;
      setError(errorMessage(errorOf(e))); setLoadedFor(communityId);
    });
    const revision = generation;
    return () => { revision.current++; };
  }, [client, communityId]);

  const configure = (roleIds: string[]) => {
    if (busy || loadedFor !== communityId || !access?.manageRoles) return;
    const read = generation.current;
    setBusy(true); setError(null);
    void client.call({ m: 'configureSelfRoles', communityId, requestId: requestId(), roleIds }).then(() => {
      if (read !== generation.current) return;
      const selected = new Set(roleIds);
      setSelfRoles(current => [
        ...current.filter(row => selected.has(row.id)),
        ...(roles ?? []).filter(role => selected.has(role.id) && !current.some(row => row.id === role.id))
          .map(role => ({ id: role.id, name: role.name, position: role.position, selfSelected: false })),
      ]);
      announce(t('admin.bot.selfRoles.saved'));
    }).catch((e: unknown) => { if (read === generation.current) setError(errorMessage(errorOf(e))); })
      .finally(() => { if (read === generation.current) setBusy(false); });
  };

  const select = (roleId: string, enabled: boolean) => {
    if (busy || loadedFor !== communityId) return;
    const read = generation.current;
    setBusy(true); setError(null);
    void client.call({ m: 'selectSelfRole', communityId, roleId, requestId: requestId(), enabled }).then(() => {
      if (read === generation.current) setSelfRoles(current => current.map(row => row.id === roleId ? { ...row, selfSelected: enabled } : row));
    }).catch((e: unknown) => { if (read === generation.current) setError(errorMessage(errorOf(e))); })
      .finally(() => { if (read === generation.current) setBusy(false); });
  };

  const publishPanel = async (retry = false): Promise<void> => {
    if (busy || loadedFor !== communityId || !access?.manageRoles || pendingPanel !== null && !retry) return;
    const roleIds = pendingPanel?.roleIds ?? selfRoles.filter(role => emojis[role.id]?.trim() !== '').map(role => role.id);
    const channelId = pendingPanel?.channelId ?? panelChannel;
    const mappings: [string, Uint8Array, string][] = roleIds.map(roleId => {
      const role = selfRoles.find(row => row.id === roleId)!;
      return [emojis[roleId].trim(), fromHex(roleId), role.name];
    });
    const emojiValues = mappings.map(row => row[0]);
    const encodedMappings = mappings.length === 0 ? new Uint8Array() : encode(mappings);
    if (channelId === '' || mappings.length === 0 || mappings.some(row => row[0] === '' || new TextEncoder().encode(row[0]).length > 32
      || /[\u0000-\u001f\u007f]/u.test(row[0]) || new TextEncoder().encode(row[2]).length > 100)
      || new Set(emojiValues).size !== emojiValues.length || encodedMappings.length > 2200) {
      setError(t('admin.bot.rolePanel.invalid')); return;
    }
    const current = pendingPanel ?? { requestId: requestId(), channelId, roleIds, uploadUncertain: false };
    const requestCommunity = communityId;
    const requestGeneration = generation.current;
    const isCurrent = (): boolean => currentCommunity.current === requestCommunity && generation.current === requestGeneration;
    setBusy(true); setError(null); setPanelNotice(null);
    // Bot v2 Task 22: the panel is the sealed app operation 131 (v1 31, revision 0). Its answer is the accepted message
    // [channel, seq]; no answer in time keeps the request, and Retry resends the same id (the Bot answers it again).
    setPendingPanel({ ...current, uploadUncertain: false });
    setPanelNotice(t('admin.bot.rolePanel.pending'));
    let result: { status: number; data: Uint8Array };
    try {
      result = await createBotOperations(client, requestCommunity).request({ operation: 31, args: [channelId, encodedMappings], revision: '0',
        requestId: current.requestId });
    } catch (e: unknown) {
      if (!isCurrent()) return;
      if (e instanceof BotOperationUncertainError) {
        setPendingPanel({ ...current, uploadUncertain: true });
        setPanelNotice(t('admin.bot.rolePanel.uncertain'));
      } else {
        setPendingPanel(null); setPanelNotice(null);
        setError(t('admin.bot.rolePanel.error', { code: errorOf(e).code }));
      }
      setBusy(false);
      return;
    }
    if (!isCurrent()) return;
    try {
      if (result.status !== 0) throw new Error('bot rejected role panel');
      const accepted = decode(result.data);
      if (!Array.isArray(accepted) || accepted.length !== 2 || !(accepted[0] instanceof Uint8Array) || accepted[0].length !== 16
        || typeof accepted[1] !== 'bigint' || accepted[1] < 1n || accepted[1] > 0x7fff_ffff_ffff_ffffn
        || toHex(accepted[0]) !== current.channelId) throw new Error('invalid accepted role panel identity');
      const roleOptions = current.roleIds.map(roleId => ({ emoji: emojis[roleId].trim(), roleId }));
      await client.call({ m: 'registerRolePanel', communityId: requestCommunity, requestId: current.requestId, channelId: current.channelId,
        messageSeq: accepted[1].toString(), options: roleOptions });
      if (!isCurrent()) return;
      setPendingPanel(null); setPanelNotice(t('admin.bot.rolePanel.published')); announce(t('admin.bot.rolePanel.published'));
    } catch (e: unknown) {
      if (!isCurrent()) return;
      setPendingPanel(null); setPanelNotice(null);
      setError(t('admin.bot.rolePanel.error', { code: result.status !== 0 ? STATUS_CODE[result.status] ?? 'E_UNKNOWN' : errorOf(e).code }));
    } finally {
      if (isCurrent()) setBusy(false);
    }
  };

  const candidates: RoleSummary[] = (roles ?? []).filter(role => role.position > 0 && (role.allow & ~SELF_ROLE_BITS) === 0
    && (role.deny & ~SELF_ROLE_BITS) === 0 && (access?.owner === true || role.position < (access?.top ?? 0)));
  const approved = new Set(selfRoles.map(row => row.id));
  const panelEnabled = installationLoaded && installation?.communityId === communityId && installation.state === 3
    && (installation.capabilities & 4) !== 0;

  return <section className="dw-settings-section" aria-labelledby={`${base}-title`}>
    <h2 id={`${base}-title`} tabIndex={-1}>{t('admin.bot.selfRoles.title')}</h2>
    {loadedFor !== communityId ? <p aria-busy="true">{t('admin.bot.loading')}</p> : null}
    {loadedFor === communityId && access?.manageRoles ? <fieldset disabled={busy}>
      <legend>{t('admin.bot.selfRoles.configure')}</legend>
      {candidates.map(role => <label key={role.id}><input type="checkbox" checked={approved.has(role.id)}
        onChange={event => configure(event.currentTarget.checked ? [...approved, role.id] : [...approved].filter(id => id !== role.id))} /> {role.name}</label>)}
    </fieldset> : null}
    {loadedFor === communityId && selfRoles.length > 0 ? <fieldset disabled={busy}>
      <legend>{t('admin.bot.selfRoles.select')}</legend>
      {selfRoles.map(role => <label key={role.id}><input type="checkbox" checked={role.selfSelected}
        onChange={event => select(role.id, event.currentTarget.checked)} /> {role.name}</label>)}
    </fieldset> : null}
    {loadedFor === communityId && access?.manageRoles ? <section aria-labelledby={`${base}-panel-title`}>
      <h3 id={`${base}-panel-title`}>{t('admin.bot.rolePanel.title')}</h3>
      <p>{t('admin.bot.rolePanel.help')}</p>
      {!panelEnabled ? <p role="note">{t('admin.bot.rolePanel.requiresBot')}</p> : null}
      {channels.length === 0 ? <p role="note">{t('admin.bot.rolePanel.noChannels')}</p> : null}
      {selfRoles.map(role => <SettingsRow key={role.id} id={`${base}-emoji-${role.id}`} label={`${t('admin.bot.rolePanel.emoji')} ${role.name}`}>
        <input aria-label={`${t('admin.bot.rolePanel.emoji')} for ${role.name}`} value={emojis[role.id] ?? ''} maxLength={32}
          onChange={event => { const value = event.currentTarget.value; setEmojis(current => ({ ...current, [role.id]: value })); }} disabled={!panelEnabled || pendingPanel !== null} />
      </SettingsRow>)}
      <SettingsRow id={`${base}-panel-channel`} label={t('admin.bot.rolePanel.channel')}>
        <select aria-label={t('admin.bot.rolePanel.channel')} value={panelChannel} onChange={event => setPanelChannel(event.currentTarget.value)}
          disabled={!panelEnabled || pendingPanel !== null}>
          <option value="">{t('admin.bot.rolePanel.chooseChannel')}</option>{channels.map(channel => <option key={channel.id} value={channel.id}>{channel.name}</option>)}
        </select>
      </SettingsRow>
      {pendingPanel?.uploadUncertain ? <Button onClick={() => { void publishPanel(true); }}>{t('admin.bot.rolePanel.retry')}</Button>
        : <Button aria-disabled={!panelEnabled || pendingPanel !== null || busy || undefined} onClick={() => { void publishPanel(); }}>
          {busy ? t('admin.bot.rolePanel.publishing') : t('admin.bot.rolePanel.publish')}
        </Button>}
      {panelNotice === null ? null : <p role="status">{panelNotice}</p>}
    </section> : null}
    {loadedFor === communityId && error === null && selfRoles.length === 0 ? <p>{t('admin.bot.selfRoles.empty')}</p> : null}
    {error === null ? null : <p role="alert">{error}</p>}
  </section>;
}
