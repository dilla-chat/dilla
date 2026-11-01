import { describe, expect, it } from 'vitest';
import type { MemberSummary } from '@dilla/client-core';
import { communityBotMember, isCommunityBot } from './bot-member.ts';

const GLENN = '01J00000000000000000000001';
const ADA = '01J00000000000000000000002';
const members: MemberSummary[] = [
  { userId: GLENN, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] },
  { userId: ADA, username: 'ada', display: 'Ada', kind: 0, roleIds: [] },
];

describe('communityBotMember', () => {
  it('names a member only when the server lists it as a Bot account', () => {
    expect(communityBotMember(members, GLENN)?.username).toBe('glenn');
    expect(communityBotMember(members, ADA)).toBeUndefined();
    expect(communityBotMember(members, null)).toBeUndefined();
    expect(communityBotMember(undefined, GLENN)).toBeUndefined();
    expect(isCommunityBot(members, GLENN)).toBe(true);
    expect(isCommunityBot(members, ADA)).toBe(false);
  });
});
