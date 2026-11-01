import { useState } from 'react';
import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
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
afterEach(() => vi.useRealTimers());
it.each([false, true])('reads exact pending request IDs and ACKs an opaque lease blocked=%s', async blocked => {
  vi.useFakeTimers();
  const client = new FakeClient();
  client.set(`permissions:${communityId}`, { loaded: true, manageCommunity: true, manageRoles: false, manageChannels: false,
    kickMembers: false, banMembers: false, createInvite: false, owner: false, top: 0, everyoneId: '', overwritesLoaded: true });
  let response = false; let rejectAck = true;
  client.handler = command => {
    if (command.m === 'humanInteractionRequest') return Promise.resolve({ requestId: command.requestId, upload: 'accepted' });
    if (command.m === 'readBotInteractionResponses') return Promise.resolve(response ? { through: 'f'.repeat(32), blocked, responses: [{
      deliveryId: '1', requestId: client.callsOf('humanInteractionRequest')[0].requestId, communityId, operation: 2,
      channelId: null, revision: '1', status: 0, data: new Uint8Array(),
    }] } : { through: null, blocked: false, responses: [] });
    if (command.m === 'ackBotInteractionResponses' && rejectAck) return Promise.reject(new Error('offline'));
    return Promise.resolve(null);
  };
  render(<CoreProvider client={client}><BotSettingsProvider communityId={communityId}><Fixture /></BotSettingsProvider></CoreProvider>);
  await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Start' })); await Promise.resolve(); });
  expect(screen.getByText('pending')).toBeInTheDocument();
  expect(client.callsOf('readBotInteractionResponses')).toHaveLength(1);
  expect(client.callsOf('readBotInteractionResponses')[0].requestIds).toEqual([client.callsOf('humanInteractionRequest')[0].requestId]);
  await act(async () => { await vi.advanceTimersByTimeAsync(749); });
  expect(client.callsOf('readBotInteractionResponses')).toHaveLength(1);
  response = true;
  await act(async () => { await vi.advanceTimersByTimeAsync(1); });
  expect(screen.getByText('pending')).toBeInTheDocument();
  rejectAck = false;
  await act(async () => { await vi.advanceTimersByTimeAsync(750); });
  expect(screen.getByText('confirmed')).toBeInTheDocument();
  expect(client.callsOf('ackBotInteractionResponses').every(call => call.through === 'f'.repeat(32))).toBe(true);
});
