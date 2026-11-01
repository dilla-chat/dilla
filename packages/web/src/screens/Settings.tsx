import { SettingsFrame, PhoneLayoutProvider } from '@dilla/ui';
import { routeOf, useRoute, type ServerSettingsSection, type SettingsSection } from '../router.ts';
import { phoneNow, usePhoneLayout } from '../layout.ts';
import { t } from '../strings/index.ts';
import { Appearance } from './settings/Appearance.tsx';
import { Devices } from './settings/Devices.tsx';
import { Profile } from './settings/Profile.tsx';
import { SignInFactors } from './settings/SignInFactors.tsx';
import { useAnnouncer } from './announcer.ts';
import { Notifications } from './settings/Notifications.tsx';
import { Privacy } from './settings/Privacy.tsx';
import { Diagnostics } from './settings/Diagnostics.tsx';
import { Keyboard } from './settings/Keyboard.tsx';
import { Voice } from './settings/Voice.tsx';
import { useSlice } from '../core/use-slice.ts';
import { useCore } from '../core/context.tsx';
import { errorOf } from '../core/errors.ts';
import { SafetyCompare } from '@dilla/ui';
import { useEffect, useRef, useState, type ReactNode } from 'react';
import type { TrustSummary } from '@dilla/client-core';
import type { StringKey } from '../strings/index.ts';
import { SettingsNavigation } from '../settings/SettingsNavigation.tsx';
import { availableSettings, SETTINGS_REGISTRY, type SettingsSection as UserSettingsSection } from '../settings/registry.ts';
import { useSettingsFocus } from '../settings/use-settings-focus.ts';
import { SettingsEditProvider, useSettingsWorkspace } from '../settings/edit-state.tsx';

/** Why a comparison cannot go on: never display a raw code inside the trust ceremony. */
type CompareError = 'privacy.loadFailed' | 'privacy.keyMoved' | 'privacy.verifyFailed' | 'privacy.noShared';
function compareError(code: string, step: 'load' | 'verify'): CompareError {
  if (code === 'E_DEVICE_LIST_STALE' || code === 'E_UMK_CHANGED') return 'privacy.keyMoved';
  if (code === 'E_CORE_NOT_FOUND') return 'privacy.noShared';
  return step === 'load' ? 'privacy.loadFailed' : 'privacy.verifyFailed';
}
function compareState(record: TrustSummary): 'first-seen' | 'changed' | 'verified' | 'unlisted' {
  if (record.changedUmk !== null || record.blocked === 'E_UMK_CHANGED') return 'changed';
  return record.unlisted === true ? 'unlisted' : record.verified ? 'verified' : 'first-seen';
}

export interface SettingsProps {
  activity?: ReactNode; activityVisible?: boolean;
  /** The selected server, whose group follows "You" in the nav (Signal task 10 fix round 1); null with none. */
  communityId?: string | null;
}

/** Section metadata shared with the phone's You tab. */
export const NAV: readonly { id: SettingsSection; key: StringKey }[] = SETTINGS_REGISTRY.filter(entry => entry.scope === 'user')
  .map(entry => ({ id: entry.id as SettingsSection, key: entry.labelKey }));

/** Full user settings workspace. Only the active section is mounted. */
export function Settings({ activity, activityVisible, communityId = null }: SettingsProps = {}): React.JSX.Element {
  const [route] = useRoute();
  if (route.name !== 'settings') return <></>;
  return <SettingsEditProvider><SettingsWorkspaceScreen activity={activity} activityVisible={activityVisible} communityId={communityId} /></SettingsEditProvider>;
}

function SettingsWorkspaceScreen({ activity, activityVisible, communityId = null }: SettingsProps): React.JSX.Element {
  const [route, navigate] = useRoute();
  const phone = usePhoneLayout();
  const client = useCore();
  const records = useSlice('trust') ?? [];
  const diagnostics = useSlice('diagnostics');
  const account = useSlice('account');
  const workspace = useSettingsWorkspace();
  const { text: announcement, announce } = useAnnouncer();
  const [comparison, setComparison] = useState<{ record: TrustSummary; umk: string; number: string | null; error: CompareError | null } | null>(null);
  const [anchor, setAnchor] = useState<string | null>(null);
  const focusHeading = useRef(false);
  const section = route.name === 'settings' ? route.section : null;
  // README §5.10 (Signal task 10 fix round 1): no index page on a phone either; /settings opens Profile, as the prototype's
  // You tab does, with the nav's chips above it.
  const activeSection: UserSettingsSection = section ?? 'profile';
  const contentRef = useSettingsFocus(activeSection, anchor);
  const compareUser = route.name === 'settings' ? route.compare ?? null : null;
  const compared = useRef<string | null>(null);

  useEffect(() => {
    if (comparison !== null || !focusHeading.current) return;
    focusHeading.current = false;
    contentRef.current?.querySelector<HTMLElement>('[data-privacy-heading]')?.focus();
  }, [comparison, contentRef]);
  useEffect(() => {
    if (section !== null) return;
    setAnchor(null);
    setComparison(null);
  }, [section]);

  const failCompare = (userId: string, umk: string, error: CompareError) =>
    setComparison(current => current?.record.userId === userId && current.umk === umk ? { ...current, error } : current);
  const compare = (record: TrustSummary) => {
    const umk = record.changedUmk ?? record.umk;
    setComparison({ record, umk, number: null, error: null });
    void client.call({ m: 'safetyNumber', userId: record.userId, umk }).then(result => {
      const live = client.get('trust')?.find(row => row.userId === record.userId);
      if (typeof result !== 'string') { failCompare(record.userId, umk, 'privacy.loadFailed'); return; }
      if ((live?.changedUmk ?? live?.umk) !== umk) { failCompare(record.userId, umk, 'privacy.keyMoved'); return; }
      setComparison(current => current?.record.userId === record.userId && current.umk === umk ? { ...current, number: result } : current);
    }).catch((e: unknown) => failCompare(record.userId, umk, compareError(errorOf(e).code, 'load')));
  };
  useEffect(() => {
    if (compareUser === null) { compared.current = null; return; }
    if (compared.current === compareUser) return;
    const record = records.find(row => row.userId === compareUser);
    if (record === undefined) return;
    compared.current = compareUser;
    compare(record);
  });

  if (route.name !== 'settings') return <></>;
  const { from } = route;
  const select = (id: string, targetAnchor: string | null = null) => {
    if (!availableSettings('user').some(entry => entry.id === id)) return;
    setAnchor(targetAnchor);
    if (id !== 'privacy') setComparison(null);
    const next = { name: 'settings', section: id as SettingsSection, from } as const;
    if (phoneNow()) void navigate(next, false);
    else void navigate(next, true);
  };
  const selectServer = (id: string) => {
    if (communityId === null) return;
    setComparison(null);
    const next = { name: 'serverSettings', communityId, section: id as ServerSettingsSection, from } as const;
    void navigate(next, !phoneNow());
  };
  const close = () => {
    setComparison(null);
    void navigate(from === null ? { name: 'root' } : routeOf(from), true);
  };
  const nav = <SettingsNavigation active={{ scope: 'user', id: activeSection }} communityId={communityId} onUser={id => select(id)} onServer={selectServer} />;
  const sectionContent = activeSection === 'profile' ? <Profile />
    : activeSection === 'devices' ? <Devices />
      : activeSection === 'signin' ? <SignInFactors announce={announce} live={announcement} />
        : activeSection === 'notifications' ? <Notifications communityId={communityId} />
          : activeSection === 'appearance' ? <Appearance />
            : activeSection === 'voice' ? <Voice />
              : activeSection === 'privacy' ? <Privacy records={records} onCompare={compare} />
                : activeSection === 'keyboard' ? <Keyboard />
                  : <Diagnostics data={diagnostics ?? { connection: 'offline', generation: null, instance: '', devices: 0, errors: [] }}
                    instanceName={account?.instance?.name ?? null} writeText={text => navigator.clipboard?.writeText(text) ?? Promise.resolve()} />;

  return <PhoneLayoutProvider value={phone}><SettingsFrame open title={t('settings.title')} closeLabel={t('settings.close')} onClose={close}
    nav={nav} phonePage="section"
    contentRef={contentRef} wide={activeSection === 'privacy' || activeSection === 'diagnostics'}
    overlays={workspace?.confirmation} activity={activity} activityVisible={activityVisible} footer={workspace?.footer}>
    {sectionContent}

    <SafetyCompare open={comparison !== null} contactName={comparison?.record.name ?? ''}
      state={comparison === null ? 'first-seen' : compareState(comparison.record)} approvedUmk={comparison?.umk ?? null}
      qrUserId={comparison?.record.userId} groups={comparison?.number?.split(' ') ?? []}
      canVerify={comparison !== null && comparison.record.shared && comparison.record.unlisted !== true && comparison.number !== null && comparison.error !== 'privacy.keyMoved'}
      error={comparison?.error ? t(comparison.error) : null} docState="trust-compare"
      labels={{ title: t('privacy.title'), copy: t('privacy.copy'), verify: t('privacy.verify'), close: t('dialog.close'), loading: t('privacy.loading'),
        instructions: t('privacy.instructions'), confirm: t('privacy.confirm'), details: t('privacy.keyDetails'), showQr: t('privacy.showQr'), hideQr: t('privacy.hideQr'), qrLabel: t('privacy.qrLabel'), firstSeen: t('privacy.firstSeen', { name: comparison?.record.name ?? '' }),
        states: { 'first-seen': t('privacy.firstSeenLabel'), changed: t('privacy.changed'), verified: t('privacy.verifiedLabel'), unlisted: t('privacy.unlisted') } }}
      onCopy={() => { if (comparison?.number) void navigator.clipboard?.writeText(comparison.number); }}
      onVerify={umk => {
        if (comparison?.umk !== umk || comparison.number === null) return;
        const { userId } = comparison.record;
        void client.call({ m: 'verifyContact', userId, umk }).then(() => {
          focusHeading.current = true;
          setComparison(current => current?.record.userId === userId && current.umk === umk ? null : current);
        }).catch((e: unknown) => failCompare(userId, umk, compareError(errorOf(e).code, 'verify')));
      }} onClose={() => setComparison(null)} />
    {activeSection === 'signin' ? null : <p className="d-sr-only" role="status" aria-live="polite" data-testid="settings-announcer">{announcement}</p>}
  </SettingsFrame></PhoneLayoutProvider>;
}
