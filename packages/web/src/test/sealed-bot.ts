// A test Bot behind sealed commands for a FakeClient (Bot v2 Task 21b): the page's `sendBotCommand` calls reach `respond` as
// the v1 operation they run, and the answers arrive in the `botAnswers` slice as the worker publishes them.
import type { SliceTypes } from '@dilla/client-core';
import { sealedBotFake, type SealedCall, type SealedReply } from '../../../client-core/src/testing/sealed-bot.ts';
import type { FakeClient } from './fake-client.ts';

export type { SealedCall, SealedReply };

/** Installs the test Bot on `fake` (after its handler is set): the community's catalog offers `offered` (v1 operations). */
export function installSealedBot(fake: FakeClient, communityId: string, respond: (call: SealedCall) => SealedReply | Promise<SealedReply>,
  options: { offered?: readonly number[] } = {}) {
  const bot = sealedBotFake(respond, options);
  fake.set(`botCatalog:${communityId}`, bot.get(`botCatalog:${communityId}`) as SliceTypes[`botCatalog:${string}`]);
  fake.set('botAnswers', []);
  bot.subscribe('botAnswers', () => { fake.set('botAnswers', [...(bot.get('botAnswers') as SliceTypes['botAnswers'] & unknown[])]); });
  const prior = fake.handler;
  fake.handler = command => bot.call(command) ?? prior(command);
  return bot;
}
