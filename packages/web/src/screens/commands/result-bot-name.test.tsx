import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { FakeClient } from '../../test/fake-client.ts';
import { useResultBotName } from './result-bot-name.ts';
afterEach(() => vi.useRealTimers());
it.each([false, true])('no Bot: reads once and never polls over 90 seconds (visible=%s)', async visible => {
  const client = new FakeClient();
  renderHook(() => useResultBotName(client, 'community', [], visible));
  await waitFor(() => expect(client.callsOf('getBotCommandStatus')).toHaveLength(1));
  vi.useFakeTimers(); await act(() => vi.advanceTimersByTimeAsync(90_000));
  expect(client.callsOf('getBotCommandStatus')).toHaveLength(1);
});
it('resolves the current name without capabilities, reads installation changes, and does not poll hidden results', async () => {
  const client = new FakeClient(); let bot = 'one';
  client.handler = () => Promise.resolve({ botUserId: bot, installationId: bot });
  const { result, rerender } = renderHook(({ members }) => useResultBotName(client, 'community', members, false), {
    initialProps: { members: [{ userId: 'one', kind: 1, username: 'bot', display: 'Freja' }] },
  });
  await waitFor(() => expect(result.current).toBe('Freja'));
  vi.useFakeTimers(); await act(() => vi.advanceTimersByTimeAsync(90_000));
  expect(client.callsOf('getBotCommandStatus')).toHaveLength(1);
  bot = 'two'; rerender({ members: [{ userId: 'two', kind: 1, username: 'bot', display: 'Glenn II' }] });
  await act(async () => { await Promise.resolve(); });
  expect(result.current).toBe('Glenn II');
  expect(client.callsOf('getBotCommandStatus')).toHaveLength(2);
});
it('retries an unknown installed Bot only while its result is visible, and stops after resolving the name', async () => {
  const client = new FakeClient(); client.handler = () => Promise.resolve({ botUserId: 'installed' });
  const { rerender, result } = renderHook(({ visible, members }) => useResultBotName(client, 'community', members, visible), {
    initialProps: { visible: false, members: [] as { userId: string; kind: number; display: string }[] },
  });
  await waitFor(() => expect(client.callsOf('getBotCommandStatus')).toHaveLength(1));
  vi.useFakeTimers(); await act(() => vi.advanceTimersByTimeAsync(90_000));
  expect(client.callsOf('getBotCommandStatus')).toHaveLength(1);
  rerender({ visible: true, members: [] });
  await act(() => vi.advanceTimersByTimeAsync(30_000));
  expect(client.callsOf('getBotCommandStatus')).toHaveLength(2);
  rerender({ visible: true, members: [{ userId: 'installed', kind: 1, display: 'Freja' }] });
  await act(async () => { await Promise.resolve(); });
  expect(result.current).toBe('Freja');
  const reads = client.callsOf('getBotCommandStatus').length;
  await act(() => vi.advanceTimersByTimeAsync(90_000));
  expect(client.callsOf('getBotCommandStatus')).toHaveLength(reads);
});
