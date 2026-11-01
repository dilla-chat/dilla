import { describe, expect, it } from 'vitest';
import { CLASSIC_COMMAND_PAYLOADS, formatClassicCommandMessage } from './classic';

describe('classic command chat content', () => {
  it.each([
    ['shrug', '¯\\\\\\_(ツ)\\_/¯'],
    ['tableflip', '(╯°□°)╯︵ ┻━┻'],
    ['unflip', '┬─┬ ノ( ゜-゜ノ)'],
  ] as const)('preserves the classic %s payload', (command, expected) => {
    expect(CLASSIC_COMMAND_PAYLOADS[command]).toBe(expected);
    expect(formatClassicCommandMessage(command)).toBe(expected);
    expect(formatClassicCommandMessage(command, '')).toBe(expected);
  });

  it('separates optional text by one space and preserves its Unicode and line breaks', () => {
    expect(formatClassicCommandMessage('tableflip', '東京\nagain')).toBe('(╯°□°)╯︵ ┻━┻ 東京\nagain');
  });
});
