import { KeyboardTable } from '@dilla/ui';
import { t } from '../../strings/index.ts';

/** README §5.9: a chord such as 'Control/⌘+Shift+M' is drawn as one keycap per key. */
const caps = (chord: string): string[] => chord.split(/\+(?=.)/);
/**
 * Settings → Keyboard. Fix round 2 (Jonas): the prototype's five rows first, with its labels and keycaps (⌘ stands for Control
 * off a Mac too, as the app reads either), then dilla's other shortcuts in the same style: the second search chord and the
 * open DM's call (Ruling Z). Esc closes the topmost window (a dialog, a sheet, Settings); Ctrl+End jumps the open conversation
 * to its newest message (Conversation.tsx), except in a text field, where it moves the caret.
 */
export function Keyboard(): React.JSX.Element {
  return <section className="dw-settings-section" aria-labelledby="keyboard-title"><h2 id="keyboard-title" tabIndex={-1}>{t('keyboard.title')}</h2><p>{t('signalSettings.keyboard.body')}</p>
    <KeyboardTable caption={t('keyboard.title')} orLabel={t('keyboard.or')} rows={[
      { action: t('signalSettings.keyboard.search'), keys: caps(t('signalSettings.keyboard.searchKeys')) },
      { action: t('signalSettings.keyboard.mute'), keys: caps(t('signalSettings.keyboard.muteKeys')) },
      { action: t('signalSettings.keyboard.deafen'), keys: caps(t('signalSettings.keyboard.deafenKeys')) },
      { action: t('signalSettings.keyboard.close'), keys: caps(t('signalSettings.keyboard.closeKeys')) },
      { action: t('signalSettings.keyboard.jump'), keys: caps(t('signalSettings.keyboard.jumpKeys')) },
      { action: t('signalSettings.keyboard.searchSlash'), keys: caps(t('signalSettings.keyboard.searchSlashKeys')) },
      { action: t('keyboard.dmCallAction'), keys: caps(t('signalSettings.keyboard.dmCallKeys')) },
    ]} />
  </section>;
}
