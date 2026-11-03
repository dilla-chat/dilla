// The worker's slice publisher (L-TS-08): every value is a deep-frozen copy, replaced whole, and
// published with a revision that rises by one per slice name; an unchanged value is not published.
import type { SliceName, SliceTypes } from './types';

interface Held { rev: number; value: unknown; json: string }

function deepFreeze<T>(value: T): T {
  if (typeof value === 'object' && value !== null) {
    for (const inner of Object.values(value)) deepFreeze(inner);
    Object.freeze(value);
  }
  return value;
}

export class SliceStore {
  private readonly held = new Map<SliceName, Held>();
  private readonly nonemptyTrays = new Set<SliceName>();

  constructor(private readonly publish: (name: SliceName, rev: number, value: unknown) => void) {}

  set<N extends SliceName>(name: N, value: SliceTypes[N]): void {
    // No slice value carries a bigint (L-TS-08), so JSON is an exact equality test.
    const json = JSON.stringify(value);
    const current = this.held.get(name);
    if (current !== undefined && current.json === json) return;
    const rev = (current?.rev ?? 0) + 1;
    const frozen = deepFreeze(structuredClone(value));
    this.held.set(name, { rev, value: frozen, json });
    if (name.startsWith('tray:')) {
      if ((value as SliceTypes[`tray:${string}`]).length > 0) this.nonemptyTrays.add(name);
      else this.nonemptyTrays.delete(name);
      this.set('pendingAttachments', this.nonemptyTrays.size > 0);
    }
    this.publish(name, rev, frozen);
  }

  get<N extends SliceName>(name: N): SliceTypes[N] | undefined {
    return this.held.get(name)?.value as SliceTypes[N] | undefined;
  }

  rev(name: SliceName): number {
    return this.held.get(name)?.rev ?? 0;
  }
}
