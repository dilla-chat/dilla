import { describe, expect, it, vi } from 'vitest';
import { privateResults } from './private-results.ts';
const row = { at: 1, status: 'pending' as const, text: 'Saving your reminder…', provider: 'bot' as const };
describe('privateResults', () => {
  it('isolates conversations, accounts and clients and retains the same store', () => {
    const client = {}; const store = privateResults(client, 'ada', 'general');
    store.add(row);
    expect(privateResults(client, 'ada', 'general')).toBe(store);
    expect(store.rows()).toHaveLength(1);
    expect(privateResults(client, 'ada', 'other').rows()).toEqual([]);
    expect(privateResults(client, 'bea', 'general').rows()).toEqual([]);
    expect(privateResults({}, 'ada', 'general').rows()).toEqual([]);
  });
  it('updates and dismisses with stable snapshots and subscription cleanup', () => {
    const store = privateResults({}, 'ada', 'general'); const listener = vi.fn(); const stop = store.subscribe(listener);
    const id = store.add(row); const snapshot = store.rows();
    expect(store.rows()).toBe(snapshot);
    store.update(id, { status: 'success', text: 'Saved.' });
    expect(store.rows()[0]).toMatchObject({ id, status: 'success', text: 'Saved.' });
    expect(snapshot[0].status).toBe('pending');
    store.dismiss(id); expect(store.rows()).toEqual([]); expect(listener).toHaveBeenCalledTimes(3);
    stop(); store.add(row); expect(listener).toHaveBeenCalledTimes(3);
  });
});
