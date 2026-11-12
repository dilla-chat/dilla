import { expect, it, vi } from 'vitest';
import { privateResults, PRIVATE_RESULT_TTL_MS } from './private-results.ts';
const row = { at: 1, status: 'pending' as const, text: 'Saving your reminder…', provider: 'bot' as const };
it('stamps a finished row, leaves a pending or held one unstamped, and restamps on a status change', () => {
  vi.useFakeTimers(); vi.setSystemTime(1000);
  try {
    const store = privateResults({}, 'ada', 'settled');
    const id = store.add(row);
    expect(store.rows()[0]?.settledAt).toBeUndefined();
    vi.setSystemTime(5000); store.update(id, { status: 'success' });
    expect(store.rows()[0]?.settledAt).toBe(5000);
    vi.setSystemTime(9000); store.update(id, { text: 'again' });
    expect(store.rows()[0]?.settledAt).toBe(5000);
    store.update(id, { status: 'failed' });
    expect(store.rows()[0]?.settledAt).toBe(9000);
    store.update(id, { hold: true });
    expect(store.rows()[0]?.settledAt).toBeUndefined();
    vi.setSystemTime(12000); store.update(id, { hold: false });
    expect(store.rows()[0]?.settledAt).toBe(12000);
    const done = store.add({ ...row, status: 'cancelled' });
    expect(store.rows().find(r => r.id === done)?.settledAt).toBe(12000);
    expect(PRIVATE_RESULT_TTL_MS).toBe(60_000);
  } finally { vi.useRealTimers(); }
});
