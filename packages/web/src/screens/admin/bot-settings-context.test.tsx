import { useState } from 'react';
import { act, fireEvent, render, screen } from '@testing-library/react';
import { expect, it } from 'vitest';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { installSealedBot } from '../../test/sealed-bot.ts';
import { BotSettingsProvider, useBotInteractionOperation } from './bot-settings-context.tsx';
const communityId = '11'.repeat(16);
function Fixture() {
  const controller = useBotInteractionOperation('test');
  const [result, setResult] = useState('idle');
  return <><button onClick={() => {
    const id = controller!.startOperation('test', { communityId, operation: 2, args: [], revision: null });
    setResult('pending');
    if (id) void controller!.waitOperation('test', id).then(value => setResult(value?.acked ? 'confirmed' : 'failed'));
  }}>Start</button><p>{result}</p></>;
}
// Task 22: a shared operation is a sealed command: the Bot's answer comes back to the send, with nothing to poll or ACK.
it('sends a shared operation as its sealed app operation and confirms it on the Bot\'s answer', async () => {
  const client = new FakeClient();
  client.set(`permissions:${communityId}`, { loaded: true, manageCommunity: true, manageRoles: false, manageChannels: false,
    kickMembers: false, banMembers: false, createInvite: false, owner: false, top: 0, everyoneId: '', overwritesLoaded: true });
  let answer: (() => void) | null = null;
  const bot = installSealedBot(client, communityId, () => new Promise(resolve => { answer = () => resolve({ status: 0, revision: '1' }); }));
  render(<CoreProvider client={client}><BotSettingsProvider communityId={communityId}><Fixture /></BotSettingsProvider></CoreProvider>);
  await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Start' })); await Promise.resolve(); });
  expect(screen.getByText('pending')).toBeInTheDocument();
  expect(bot.calls.map(c => [c.sealed, c.operation, c.revision])).toEqual([[102, 2, '0']]);
  await act(async () => { answer?.(); await Promise.resolve(); await Promise.resolve(); });
  expect(await screen.findByText('confirmed')).toBeInTheDocument();
});
