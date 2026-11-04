import { createElement as h, Fragment } from 'react';
import { Button } from './Button.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { OnboardingFrame } from '../OnboardingFrame/OnboardingFrame.tsx';
import '../OnboardingFrame/OnboardingFrame.css';
import '../Dialog/Dialog.css';
import '../SettingsRow/SettingsRow.css';
import '../SettingsFrame/SettingsFrame.css';
import '../InviteLink/InviteLink.css';
import '../PhoneHeader/PhoneHeader.css';
import '../CallControls/CallControls.css';
import '../CallBar/CallBar.css';
import '../RingBanner/RingBanner.css';
import '../RecoveryKey/RecoveryKey.css';
import '../ManageList/ManageList.css';

const button = (props: Partial<Parameters<typeof Button>[0]> = {}, children = 'Save') => h(Button, { children, ...props });
// Keep each shell in this audit's normal flow; the real phone shell is fixed to the viewport.
const shell = (child: ReturnType<typeof h>) => h('div', { className: 'd-app-shell', 'data-layout': 'phone', style: { position: 'relative', height: 'auto' } }, child);
const wrap = (className: string, child: ReturnType<typeof h>) => h('div', { className }, child);
const auth = h(OnboardingFrame, {
  title: 'Sign in to midgard.gg', stepLabel: 'Step 1 of 4', children: h('p', null, 'Welcome back.'),
  footer: h(Fragment, null,
    button({ variant: 'accent', type: 'submit' }, 'Continue'),
    wrap('d-onboarding-frame__nav', button({ variant: 'ghost' }, 'Back'))),
});
// Literal expectations from Dilla Signal Prototype.dc.html: auth 80, confirm 553, generic 706,
// invite 743, phone header 224, voice controls 453–459. Small is the existing component API tier. CallBar/RingBanner are app-only surfaces.
export const phonePrototypeCases = [
  { name: 'generic button outside the shell', content: button(), selector: '.d-btn', height: 32 },
  { name: 'generic button inside the phone shell', content: shell(button()), selector: '.d-btn', height: 32 },
  { name: 'small button inside the phone shell', content: shell(button({ size: 'sm' })), selector: '.d-btn', height: 22 },
  { name: 'icon danger confirmation', content: shell(wrap('d-dialog__footer', button({ variant: 'danger', children: h('span', null, h(Icon, { name: 'user-x', size: 14 }), 'Kick') }))), selector: '.d-btn', height: 34 },
  { name: 'confirmation Cancel', content: shell(wrap('d-dialog__footer', button({ variant: 'ghost' }, 'Cancel'))), selector: '.d-btn', height: 34 },
  { name: 'sign-in Continue', content: auth, selector: '.d-btn[data-variant="accent"]', height: 40 },
  // Continue and Back share one real auth frame, avoiding duplicate main landmarks in a single story.
  { name: 'onboarding navigation', content: auth, fixtureIndex: 5, selector: '.d-onboarding-frame__nav .d-btn', height: 24 },
  { name: 'settings row action', content: h('div', { className: 'd-settings-frame', 'data-layout': 'phone', style: { position: 'static', width: 'auto', height: 'auto' } }, wrap('d-settings-row__control', button())), selector: '.d-btn', height: 32 },
  { name: 'invite action', content: shell(wrap('d-invite-link__row', button({ variant: 'accent' }, 'Copy'))), selector: '.d-btn', height: 36 },
  { name: 'phone header action', content: shell(wrap('d-phone-header', wrap('d-phone-header__actions', button({ variant: 'ghost' }, 'Threads')))), selector: '.d-btn', height: 28 },
  { name: 'voice bar control', content: shell(wrap('d-call-controls--bar d-voice-toolbar', button({ className: 'd-voice-toolbar__control' }, 'Mute'))), selector: '.d-btn', height: 40 },
  { name: 'app call strip control', content: shell(wrap('d-call-bar', button())), selector: '.d-btn', height: 44 },
  { name: 'recovery Save as file', content: wrap('d-recovery-key', button({ className: 'd-recovery-key__print' }, 'Save as file')), selector: '.d-btn', height: 34 },
  { name: 'invite Revoke', content: wrap('d-manage-list__trail', button({ variant: 'danger' }, 'Revoke')), selector: '.d-btn', height: 30 },
  { name: 'app incoming ring action', content: shell(wrap('d-ring-banner__actions', button())), selector: '.d-btn', height: 32 },
];
