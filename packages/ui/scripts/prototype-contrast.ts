/**
 * Ruling Y (prototype parity, Jonas 2026-10-27), task 7: the conversation's prototype-exact colours below 4.5:1 in signal.
 * `selector` is the element exactly, for scripts/story-a11y.ts, which turns colour-contrast off for it in signal only and
 * still runs every other rule on it. `simple` is a one-class selector Storybook's own a11y pass (.storybook/preview.tsx,
 * every theme) can put in a `:not()`; the runner above keeps light and high-contrast strict. No imports: Storybook bundles it.
 */
export const CONVERSATION_PROTOTYPE_CONTRAST: readonly { selector: string; simple: string; why: string }[] = [
  { selector: '.d-message-row__gutter', simple: '.d-message-row__gutter', why: 'the time gutter in #5C636E on bg (3.28:1)' },
  { selector: '.d-message-row__time', simple: '.d-message-row__time', why: 'a phone row\'s time beside the name in #636A75 (3.65:1)' },
  { selector: '.d-composer[data-empty="true"] .d-composer__send', simple: '.d-composer__send', why: 'the empty composer\'s send in #5C636E on #171B22 (2.84:1)' },
  { selector: '.d-mention-list [aria-selected="true"] .d-mention-list__secondary', simple: '.d-mention-list__secondary', why: 'the picked suggestion\'s handle in #767D89 on #171B22 (4.16:1)' },
  { selector: '.d-code-block__count', simple: '.d-code-block__count', why: 'a code block\'s line count in #636A75 on bg-3 (3.4:1)' },
  { selector: '.d-code-block [data-tok="com"]', simple: '[data-tok="com"]', why: 'a code comment in #6C737E on bg-2 (4.06:1)' },
  // Task 8 (README §5.6): a call bar keycap is the prototype's 10px at opacity 0.6 (signal only; CallControls.css); on a toggled-on
  // mute or deafen (--danger on the 0.1 danger tint) that is about 2.7:1. It is decorative (aria-hidden): the same shortcuts are
  // the user panel's mute and deafen aria-keyshortcuts, and the tooltip's text below a 1240px window.
  { selector: '.d-call-controls--bar .d-voice-toolbar__keycap', simple: '.d-voice-toolbar__keycap', why: 'a call bar keycap, --danger at 0.6 on the danger tint (2.7:1)' },
  // Task 6b's ruling (Jonas 2026-10-27, prototype parity): the voice dock's stats in --fg-3 on the --ok tint (4.38:1). Listed here,
  // not only in story-a11y's dock stories, because the e2e axe runs (e2e/web/support/axe.ts) see the real dock beside a DM call.
  { selector: '.d-voice-dock__stats', simple: '.d-voice-dock__stats', why: 'the voice dock stats in #767D89 on the --ok tint (4.38:1)' },
  // Task 11 (Jonas, Q2): a DM row's sub (its person's status) in #767D89; on the selected row's #171B22 that is 4.16:1. It is
  // aria-hidden (the row carries it as its description).
  { selector: '.d-chrow[aria-current="page"] .d-chrow__sub', simple: '.d-chrow__sub', why: 'the selected DM row\'s status in #767D89 on #171B22 (4.16:1)' },
  // Task 8b (README §6.6): the dock's now-playing row says "Now playing · 0:37 / 3:34" in the prototype's #767D89 on the same tint.
  { selector: '.d-np--compact .d-np__sub', simple: '.d-np__sub', why: 'the dock player\'s state line in #767D89 on the --ok tint (4.38:1)' },
  // Task 8b: the open queue's current row keeps the prototype's #767D89 number, requester and duration on its 0.08 accent tint.
  { selector: '.d-np__item[data-current] .d-np__n', simple: '.d-np__n', why: 'the current queue row\'s number in #767D89 on the accent tint (4.2:1)' },
  { selector: '.d-np__item[data-current] .d-np__by', simple: '.d-np__by', why: 'the current queue row\'s requester in #767D89 on the accent tint (4.2:1)' },
  { selector: '.d-np__item[data-current] .d-np__dur', simple: '.d-np__dur', why: 'the current queue row\'s duration in #767D89 on the accent tint (4.2:1)' },
  // Task 9 (README §5.7): the members aside's status line is the prototype's #767D89 (--fg-3); on the open member's row (the --hover
  // fill #14171C, the prototype's p.bg while the card is open) that is 4.33:1. Light and high-contrast keep their accessible pairs.
  { selector: '.d-member-list__row[data-open] .d-member-list__line2', simple: '.d-member-list__line2', why: 'the open member row\'s status line in #767D89 on #14171C (4.33:1)' },
  // Task 10b (README §5.9): the sensitivity meter's scale (−100 dB / −50 / 0 dB) is the prototype's 9.5px #636A75 on bg (3.65:1).
  // It is aria-hidden: the threshold slider's value text and the readouts beside the pill carry the numbers.
  { selector: '.d-sens__tick', simple: '.d-sens__tick', why: 'the sensitivity meter scale in #636A75 on bg (3.65:1)' },
  // Not listed because axe cannot see it: a code block's line numbers, the prototype's #4A5059 on bg-2 (2.4:1), are CSS-generated
  // (`::before` counters) and decorative (never part of the code's text). Light and high-contrast draw them in fg-3 / #C8C8C8.
];

/** The `:not()` chain Storybook's a11y config appends to its color-contrast selector. */
export const CONVERSATION_CONTRAST_NOT = CONVERSATION_PROTOTYPE_CONTRAST.map(e => `:not(${e.simple})`).join('');
