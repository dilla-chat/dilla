// The desktop user panel at the bottom left (web3.8, item 13; R23-R25): composition only. The ui UserPanel draws it;
// the mute and deafen are the call's `selfMute`/`selfDeafen` (the session's in a call, the `voice.self` setting outside one).
// The avatar and name block opens the status menu (the account-wide presence choice), Discord-style. Signal §5.4: there is no gear
// (settings is the top bar's). Task 11 (the prototype's statusLabel): the line under the name is "in {room}" while this device is
// in a call, else the person's custom status, else the presence label.
import { useState } from 'react';
import type { PresenceChoice } from '@dilla/client-core';
import { UserPanel as UiUserPanel } from '@dilla/ui';
import { useCore } from '../core/context.tsx';
import { t } from '../strings/index.ts';
import { voiceShortcutText } from './voice/shortcuts.ts';
import type { CallView } from './voice/useCall.tsx';

/** The mute and deafen shortcuts as the tooltips write them; the platform cannot change during a session. */
const SHORTCUTS = voiceShortcutText();
const CHOICES: readonly PresenceChoice[] = ['online', 'idle', 'dnd', 'invisible'];

export function UserPanel({ name, username, choice, call, ptt = false, statusMsg, inRoom = null }: {
  name: string; username: string; choice: PresenceChoice; call: Pick<CallView, 'selfMute' | 'selfDeafen' | 'selfMuted' | 'selfDeafened'>;
  /** The account's custom status ("packing rope"); absent or empty, the presence label stands. */
  statusMsg?: string;
  /** The room of this device's call while it is in one ("harbour", "the call with ada"). */
  inRoom?: string | null;
  /** Push-to-talk mode: the mic carries the PTT tag, and the avatar rings while the person is heard. */
  ptt?: { held: boolean } | false;
}): React.JSX.Element {
  const client = useCore();
  const [error, setError] = useState(false);
  const line = inRoom !== null ? t('members.inChannel', { channel: inRoom })
    : statusMsg !== undefined && statusMsg !== '' ? statusMsg : t(`presence.${choice}`);
  return (
    <>
      <UiUserPanel name={name} username={username} line={line} muted={call.selfMuted} deafened={call.selfDeafened} shortcuts={SHORTCUTS}
        labels={{ region: t('panel.region'), mute: t('voice.mute'), deafen: t('voice.deafen') }}
        status={{
          current: choice, label: t('panel.status', { status: t(`presence.${choice}`) }),
          items: CHOICES.map(id => ({ id, label: t(`presence.${id}`) })),
          onChoose: id => {
            setError(false);
            void client.call({ m: 'setPresenceChoice', choice: id as PresenceChoice }).catch(() => setError(true));
          },
        }}
        {...(ptt !== false ? { ptt: { tag: t('layout.ptt.tag'), description: t('settings.voice.ptt'), held: ptt.held } } : {})}
        onMute={call.selfMute} onDeafen={call.selfDeafen} />
      {error ? <p className="dw-panel-error" role="alert">{t('presence.error')}</p> : null}
    </>
  );
}
