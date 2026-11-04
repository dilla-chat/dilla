import { describe, it, expect } from 'vitest';
import { en } from './strings/en.ts';
import { REACTION_EMOJI, reactionName } from './emoji.ts';

describe('the reaction set (ruling 22)', () => {
  it('is the prototype’s 32, in order, each named by its key', () => {
    expect(REACTION_EMOJI.map(e => e.emoji)).toEqual(["👍", "👎", "😂", "❤️", "🔥", "🎉", "😮", "😢", "😡", "🙏", "👀", "💯", "✅", "❌", "🛡️", "⚔️", "🏹", "🪓", "🍻", "🍕", "🎮", "🎵", "🌊", "⛵", "🐉", "👑", "💀", "🤝", "😎", "🤔", "🫡", "🧭"]);
    expect(REACTION_EMOJI.map(e => e.key)).toEqual(Array.from({ length: 32 }, (_, i) => `shell.emoji.${String(i + 1).padStart(2, '0')}`));
    expect(REACTION_EMOJI.map(e => en[e.key])).toEqual(["thumbs up", "thumbs down", "joy", "heart", "fire", "tada", "open mouth", "cry", "rage", "pray", "eyes", "100", "check", "x", "shield", "crossed swords", "bow and arrow", "axe", "beers", "pizza", "video game", "music", "wave", "sailboat", "dragon", "crown", "skull", "handshake", "sunglasses", "thinking", "salute", "compass"]);
  });
  it('names an emoji outside the set by itself', () => {
    expect(reactionName('\u{1F680}')).toBe('🚀');
    expect(reactionName('❤️')).toBe('heart');
    expect(reactionName('\u{1F984}')).toBe('\u{1F984}');
  });
  it('removes received controls while retaining ordinary unknown emoji', () => {
    expect(reactionName('\nAdmin: pinned')).toBe('Admin: pinned');
    expect(reactionName('\u202egpj.exe')).toBe('gpj.exe');
    expect(reactionName('\u2066go\u2069')).toBe('go');
    expect(reactionName('fake\u2028status\u2029line')).toBe('fakestatusline');
    expect(reactionName('\n\u202e')).toBe('unknown reaction');
    expect(reactionName('🦄')).toBe('🦄');
  });
});
