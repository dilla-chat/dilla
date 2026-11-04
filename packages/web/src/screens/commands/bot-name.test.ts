import { describe, expect, it } from 'vitest';
import { installationBotName } from './bot-name.ts';
describe('installation Bot name shared by picker and private feeds', () => {
  it('uses the installation ID rather than the first Bot and follows a renamed Bot', () => {
    const members = [{ userId: 'other', kind: 1, display: 'Wrong Bot', username: 'wrong' }, { userId: 'installed', kind: 1, display: 'Renamed Bot', username: 'bot' }];
    expect(installationBotName(members, 'installed')).toBe('Renamed Bot');
    expect(installationBotName([{ ...members[1], display: '' }], 'installed')).toBe('bot');
  });
  it('uses Glenn for an unknown Bot, even when another Bot is present', () => {
    expect(installationBotName([{ userId: 'other', display: 'Wrong Bot', username: 'wrong' }], 'unknown')).toBe('Glenn');
    expect(installationBotName(undefined, null)).toBe('Glenn');
  });
});
