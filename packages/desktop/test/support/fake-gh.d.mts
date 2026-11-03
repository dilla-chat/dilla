export interface FakeGhCall { command: string; argv: string[]; stdio: string[]; stdin: Buffer }
export function goodPosture(): Record<string, any> & { rules: { type: string; ruleset_id?: number; parameters?: unknown }[] };
export function fakeGh(overrides?: Record<string, unknown>): { state: Record<string, any>; calls: FakeGhCall[]; spawn: (command: string, argv: string[], options?: { stdio?: unknown }) => unknown };
