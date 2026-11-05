import { KeyboardTable } from '@dilla/ui';
import { t } from '../../strings/index.ts';
import { isApplePlatform } from '../voice/shortcuts.ts';

/** README §5.9: a chord such as '⌘+⇧+M' is drawn as one keycap per key. */
const caps = (chord: string): string[] => chord.split(/\+(?=.)/);
/** The keycaps of a chord for a platform: ⌘ and ⌥ on macOS, Ctrl and Alt on Windows and Linux (the top bar's "Ctrl K" and the call bar's "Ctrl⇧M" follow the same rule). */
export function platformKeys(chord: string, apple: boolean): string[] {
  return caps(chord).map(k => (apple ? k : k === '⌘' ? 'Ctrl' : k === '⌥' ? 'Alt' : k));
}
/**
 * Settings → Keyboard. Fix round 2 (Jonas): the prototype's five rows first, with its labels and keycaps (⌘ on a Mac,
 * Ctrl elsewhere: platformKeys), then dilla's other shortcuts in the same style: the second search chord and the
 * open DM's call (Ruling Z). Esc closes the topmost window (a dialog, a sheet, Settings); Ctrl+End jumps the open conversation
 * to its newest message (Conversation.tsx), except in a text field, where it moves the caret.
 */
export function Keyboard({ apple = isApplePlatform() }: { apple?: boolean } = {}): React.JSX.Element {
  const caps = (chord: string): string[] => platformKeys(chord, apple);
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
