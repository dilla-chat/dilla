import { readdirSync, readFileSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// Bot v2: state follows pushed hints (client-core bot/hints.ts); nothing in the Bot UI or the Bot adapters reads the Bot on a
// timer. Every non-test file under these directories is scanned for setInterval.
const root = resolve(process.cwd(), '..');   // packages/, as vitest runs from packages/web
const DIRS = ['web/src/screens/commands', 'web/src/screens/voice', 'client-core/src/voice', 'client-core/src/bot'];

/** The only intervals allowed, each a local clock or a check of the page's own state that sends no request to the Bot. */
const ALLOWED: Record<string, string> = {
  'web/src/screens/commands/BotCommandDialogs.tsx': 'closes the dialog when the page binding it was opened for changes (reads page state only)',
  'web/src/screens/commands/builtin.ts': '50 ms local check of the voice session state while /join waits (reads page state only)',
  'web/src/screens/voice/useCall.tsx': '1 s local tick that shows a suspended device as a gap in the call (no request)',
};

function sources(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return sources(path);
    return /\.(ts|tsx)$/u.test(entry.name) && !/\.test\.(ts|tsx)$/u.test(entry.name) && !/\.fixture\.ts$/u.test(entry.name) ? [path] : [];
  });
}

describe('Bot UI and adapter code', () => {
  it('no setInterval remains outside the allow-list of local clocks', () => {
    const found = DIRS.flatMap((dir) => sources(join(root, dir)))
      .filter((file) => /setInterval/u.test(readFileSync(file, 'utf8')))
      .map((file) => relative(root, file));
    expect(found.filter((file) => ALLOWED[file] === undefined)).toEqual([]);
  });

  it('every allow-list entry still needs its exception', () => {
    for (const file of Object.keys(ALLOWED)) expect(readFileSync(join(root, file), 'utf8')).toMatch(/setInterval/u);
  });
});
