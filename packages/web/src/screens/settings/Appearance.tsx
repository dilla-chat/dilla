import { useEffect, useRef, useState } from 'react';
import { Banner, Icon, Segmented, SettingsRow, type IconName } from '@dilla/ui';
import { isThemePref, readTheme, writeTheme, type ThemePref } from '../../prefs.ts';
import { t, type StringKey } from '../../strings/index.ts';
import { applyPreference } from '../../theme.ts';
import { useCore } from '../../core/context.tsx';
import { useSlice } from '../../core/use-slice.ts';

const TITLE_ID = 'appearance-title';
/** The theme row's tile glyph (README §5.9 icon tiles; task 10). */
const THEME_ICON: IconName = 'eye';
const THEMES: readonly { value: ThemePref; key: StringKey }[] = [
  { value: 'system', key: 'appearance.theme.system' },
  { value: 'signal', key: 'appearance.theme.signal' },
  { value: 'light', key: 'appearance.theme.light' },
  { value: 'high-contrast', key: 'appearance.theme.contrast' },
];

/**
 * Settings → Appearance (L-TS-26, Q24): the theme. The choice applies at once and is kept in the page's own
 * IndexedDB record, which main.tsx reads before the first paint; a refused write still leaves the theme applied
 * for this page's life.
 */
export function Appearance(): React.JSX.Element {
  const client = useCore();
  const settings = useSlice('settings') ?? {};
  const [value, setValue] = useState<ThemePref>('system');
  const [pending, setPending] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  // A choice made before the stored preference arrives wins over it.
  const chosen = useRef(false);
  useEffect(() => {
    let live = true;
    void readTheme().then(stored => { if (live && !chosen.current) setValue(stored); });
    return () => { live = false; };
  }, []);

  const change = (next: string) => {
    if (!isThemePref(next)) return;
    chosen.current = true;
    setValue(next);
    setFailed(false);
    setPending('theme');
    applyPreference(window, next);
    void writeTheme(next).catch(() => { setFailed(true); })
      .finally(() => setPending(current => current === 'theme' ? null : current));
  };
  const setPreference = (key: 'density' | 'reduceMotion', next: string) => {
    setFailed(false);
    setPending(key);
    client.call({ m: 'setSetting', key, value: next }).catch(() => { setFailed(true); })
      .finally(() => setPending(current => current === key ? null : current));
  };

  return (
    <section className="dw-settings-section" aria-labelledby={TITLE_ID}>
      <h2 id={TITLE_ID} tabIndex={-1}>{t('appearance.title')}</h2>
      {/* Walk 23: the row's visible label is for the eye only; the radio group carries the name, so a screen reader hears "theme" once. */}
      <div className="d-settings-row">
        <span className="d-settings-row__icon" aria-hidden="true"><Icon name={THEME_ICON} size={15} /></span>
        <div className="d-settings-row__text"><span className="d-settings-row__label" aria-hidden="true">{t('appearance.theme.label')}</span></div>
        <div className="d-settings-row__control dw-appearance-theme">
          <Segmented id="appearance-theme" label={t('appearance.theme.label')} value={value} onChange={change}
            options={THEMES.map(o => ({ value: o.value, label: t(o.key) }))} />
        </div>
      </div>
      <SettingsRow id="appearance-density-row" icon="layout-grid" label={t('workspace.density')}>
        <Segmented id="appearance-density" label={t('workspace.density')} value={settings.density === 'compact' ? 'compact' : 'regular'}
          onChange={next => setPreference('density', next)} options={[
            { value: 'regular', label: t('workspace.density.regular') }, { value: 'compact', label: t('workspace.density.compact') },
          ]} />
      </SettingsRow>
      <SettingsRow id="appearance-motion-row" icon="activity" label={t('workspace.motion')}>
        <Segmented id="appearance-motion" label={t('workspace.motion')} value={settings.reduceMotion === 'on' || settings.reduceMotion === 'off' ? settings.reduceMotion : 'system'}
          onChange={next => setPreference('reduceMotion', next)} options={[
            { value: 'system', label: t('workspace.motion.system') }, { value: 'on', label: t('workspace.motion.on') }, { value: 'off', label: t('workspace.motion.off') },
          ]} />
      </SettingsRow>
      {pending !== null ? <p role="status">{t('workspace.preference.pending')}</p> : null}
      {failed ? <Banner tone="danger">{t('workspace.preference.error')}</Banner> : null}
    </section>
  );
}
