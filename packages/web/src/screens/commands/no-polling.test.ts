import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

const read = (relative: string): string => readFileSync(new URL(relative, import.meta.url), 'utf8');

// Bot v2: state follows pushed hints (client-core bot/hints.ts); nothing in the Bot UI reads the Bot on a timer.
describe('Bot UI code', () => {
  it.each(['./PollCard.tsx', '../voice/music.tsx', './UtilityCommandProvider.tsx'])('no setInterval remains in %s', (file) => {
    expect(read(file)).not.toMatch(/setInterval/u);
  });
});
