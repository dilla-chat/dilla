// Signal prototype's fixed, ordered emoji set (gap90).
import { t, type StringKey } from './strings/index.ts';

const EMOJI = ["👍", "👎", "😂", "❤️", "🔥", "🎉", "😮", "😢", "😡", "🙏", "👀", "💯", "✅", "❌", "🛡️", "⚔️", "🏹", "🪓", "🍻", "🍕", "🎮", "🎵", "🌊", "⛵", "🐉", "👑", "💀", "🤝", "😎", "🤔", "🫡", "🧭"] as const;

export const REACTION_EMOJI: readonly { emoji: string; key: StringKey }[] = Object.freeze(EMOJI.map((emoji, i) => ({
  emoji, key: `shell.emoji.${String(i + 1).padStart(2, '0')}` as StringKey,
})));

export function reactionName(emoji: string): string {
  const item = REACTION_EMOJI.find(e => e.emoji === emoji);
  if (item !== undefined) return t(item.key);
  return reactionDisplay(emoji) || t('shell.message.unknownReaction');
}

export function reactionDisplay(emoji: string): string {
  return emoji.replace(/[\u0000-\u001f\u007f-\u009f\u061c\u200e-\u200f\u2028-\u202e\u2066-\u206f]/gu, '').trim();
}
