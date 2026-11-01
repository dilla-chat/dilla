import { t, type StringKey } from '../../strings/index.ts';
import { utility } from '../../strings/utility.ts';
export type UtilityCopyKey = keyof typeof utility & StringKey;
export function utilityText(key: UtilityCopyKey, vars?: Record<string, string | number>): string {
  return t(key, vars);
}
