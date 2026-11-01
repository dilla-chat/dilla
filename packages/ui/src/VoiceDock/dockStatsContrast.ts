/**
 * Ruling Y (prototype parity, Jonas 2026-10-27): the voice dock's stats line (`.d-voice-dock__where`) is the prototype's --fg-3
 * #767D89 on the --live tint, 4.38:1 in signal. Storybook's own axe run skips color-contrast on that element only; every
 * other rule still runs on it, and scripts/story-a11y.ts (the CI gate) applies the exception in the signal theme alone, so
 * light and high-contrast stay fully checked there.
 */
import { CONVERSATION_CONTRAST_NOT } from '../../scripts/prototype-contrast.ts';

// Task 7: these stories compose a conversation too, so the conversation's Ruling Y elements stay off the rule here as well.
export const DOCK_STATS_CONTRAST = {
  a11y: { config: { rules: [{ id: 'color-contrast', selector: `*:not(.d-voice-dock__where):not(.d-voice-dock__where *)${CONVERSATION_CONTRAST_NOT}` }] } },
} as const;
