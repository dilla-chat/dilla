import type { BotStatus } from '@dilla/ui';
export interface PrivateResult { id: string; at: number; status: BotStatus; text: string; provider: 'builtin' | 'bot' }
interface Store {
  rows: () => readonly PrivateResult[];
  add: (row: Omit<PrivateResult, 'id'>) => string;
  update: (id: string, patch: Partial<PrivateResult>) => void;
  dismiss: (id: string) => void;
  subscribe: (listener: () => void) => () => void;
}
interface Entry { store: Store; viewed(): boolean }
// No persistence or transport. Active feed subscriptions pin their conversations; other entries are an LRU of fifty.
const clients = new WeakMap<object, Map<string, Map<string, Entry>>>();
export function privateResults(client: object, account: string, conversation: string): Store {
  let accounts = clients.get(client);
  if (!accounts) { accounts = new Map(); clients.set(client, accounts); }
  let conversations = accounts.get(account);
  if (!conversations) { conversations = new Map(); accounts.set(account, conversations); }
  const memory = conversations;
  const trim = (viewing?: string) => {
    for (const [key, entry] of memory) {
      if (memory.size <= 50) break;
      if (key !== viewing && !entry.viewed()) memory.delete(key);
    }
  };
  let entry = memory.get(conversation);
  if (!entry) {
    let rows: readonly PrivateResult[] = [];
    const listeners = new Set<() => void>();
    const publish = (next: readonly PrivateResult[]) => { rows = next; for (const listener of listeners) listener(); };
    entry = { viewed: () => listeners.size > 0, store: {
      rows: () => rows,
      add: row => { const id = crypto.randomUUID(); publish([...rows, { ...row, id }].sort((a, b) => a.at - b.at)); return id; },
      update: (id, patch) => { publish(rows.map(row => row.id === id ? { ...row, ...patch, id } : row).sort((a, b) => a.at - b.at)); },
      dismiss: id => { publish(rows.filter(row => row.id !== id)); },
      subscribe: listener => { listeners.add(listener); return () => { listeners.delete(listener); trim(); }; },
    } };
  }
  memory.delete(conversation); memory.set(conversation, entry);
  trim(conversation);
  return entry.store;
}
