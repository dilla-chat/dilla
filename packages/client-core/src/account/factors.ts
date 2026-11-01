// Wave web3.6 (flow 10): managing second factors. Every client gate is advisory: the instance decides each request and a
// refusal reaches the page with its own code. A command that changes what the account holds publishes the `factors` slice
// before it resolves (contract 5); a refused step-up, a missing row or a conflict reads the factors again and rethrows,
// so the page shows what the instance holds. The TOTP secret, the otpauth URI, the backup codes and the step-up tokens
// are returned to the caller and never put in a slice, a log or a store.
import { fromHex, toHex } from '../hex';
import { DillaHttpError } from '../http/errors';
import { PASSKEY_NAME_MAX_BYTES } from '../http/routes';
import type { Factors, Routes } from '../http/routes';
import type { FactorsState, SliceName, SliceTypes } from '../state/types';
import type { Command } from '../worker/protocol';

export type FactorsRoutes = Pick<Routes, 'getFactors' | 'totpEnroll' | 'totpConfirm' | 'passkeyRegisterBegin' | 'passkeyRegisterFinish'
  | 'stepUp' | 'stepUpPasskeyBegin' | 'stepUpPasskeyFinish' | 'removeFactor' | 'regenerateBackupCodes' | 'changePassword'>;

export interface FactorsDeps {
  routes: FactorsRoutes;
  /** Whether the worker is in a phase that holds the factors slice (ready or factor-setup). */
  holdsFactors(): boolean;
  publish<K extends SliceName>(name: K, value: SliceTypes[K]): void;
}

type FactorCommand = Extract<Command, { m: 'refreshFactors' | 'factorTotpBegin' | 'factorTotpConfirm' | 'factorPasskeyBegin'
  | 'factorPasskeyFinish' | 'stepUp' | 'stepUpPasskeyBegin' | 'stepUpPasskeyFinish' | 'removeFactor' | 'regenerateBackupCodes' | 'changePassword' }>;
export type FactorMethod = FactorCommand['m'];

/** What the slice shows once the account is forgotten. */
export const NO_FACTORS: FactorsState = { owed: false, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: false };

/** The factors slice from GET /v1/auth/factors: ids in hex, nothing secret. */
export function factorsState(f: Factors): FactorsState {
  return { owed: f.owed, totp: f.totp === null ? null : { confirmedAt: f.totp },
    passkeys: f.passkeys.map((k) => ({ id: toHex(k.credId), name: k.name, created: k.created, lastUsed: k.lastUsed })),
    backupCodesLeft: f.backupCodesLeft, hasPassword: f.hasPassword,
    // Task 11 (Q7): present only when the server knows it.
    ...(f.passwordChanged !== undefined && f.passwordChanged !== null ? { passwordChanged: f.passwordChanged } : {}) };
}

const ID = /^[0-9a-f]{32}$/;
const SIX_DIGITS = /^\d{6}$/;
export const BACKUP_CODE = /^[A-Z2-7]{26}$/;
/** A credential id: 1..1023 bytes in lowercase hex (2..2046 characters). */
const CRED_ID = /^(?:[0-9a-f]{2}){1,1023}$/;
const CONTROL = /\p{Cc}/u;
const UTF8 = new TextEncoder();
const RESPONSE_MAX_BYTES = 65_536;

/** A backup code as typed: dashes and spaces dropped, upper-cased. */
export const normaliseBackupCode = (code: string): string => code.replace(/[-\s]/g, '').toUpperCase();

const stepupProblem = (v: unknown): string | null => typeof v === 'string' && v !== '' ? null : 'stepup is empty';
const optionalStepupProblem = (v: unknown): string | null => v === null ? null : stepupProblem(v);
const codeProblem = (v: unknown): string | null => typeof v === 'string' && SIX_DIGITS.test(v) ? null : 'code is not six digits';
const ceremonyProblem = (v: unknown): string | null => typeof v === 'string' && ID.test(v) ? null : 'ceremonyId is not 32 lowercase hex';
function responseProblem(v: unknown): string | null {
  return typeof v === 'string' && v !== '' && UTF8.encode(v).length <= RESPONSE_MAX_BYTES ? null : 'response is not a string of 1..65536 bytes';
}
function nameProblem(v: unknown): string | null {
  const bad = 'name must be 1 to 64 characters';
  if (typeof v !== 'string' || CONTROL.test(v)) return bad;
  const points = [...v].length;
  if (points < 1 || points > 64) return bad;
  // The route also bounds the name in bytes, so a name of wide characters is refused here, not by the route.
  return UTF8.encode(v).length <= PASSKEY_NAME_MAX_BYTES ? null : `name is longer than ${String(PASSKEY_NAME_MAX_BYTES)} bytes`;
}

/** The synchronous input checks of the factor commands: an E_BAD_INPUT reason, or null. */
export const FACTOR_VALIDATORS: Record<FactorMethod, ((c: Record<string, unknown>) => string | null) | undefined> = {
  refreshFactors: undefined,
  factorTotpBegin: (c) => optionalStepupProblem(c.stepup),
  factorTotpConfirm: (c) => codeProblem(c.code),
  factorPasskeyBegin: (c) => optionalStepupProblem(c.stepup),
  factorPasskeyFinish: (c) => ceremonyProblem(c.ceremonyId) ?? responseProblem(c.response) ?? nameProblem(c.name),
  stepUp: (c) => {
    if (c.method !== 'password' && c.method !== 'totp' && c.method !== 'backup') return 'method is not password, totp or backup';
    if (typeof c.secret !== 'string' || c.secret === '') return 'secret is empty';
    if (c.method === 'totp') return codeProblem(c.secret);
    if (c.method === 'backup' && !BACKUP_CODE.test(normaliseBackupCode(c.secret))) return 'backup code is not 26 letters and digits';
    return null;
  },
  stepUpPasskeyBegin: undefined,
  stepUpPasskeyFinish: (c) => ceremonyProblem(c.ceremonyId) ?? responseProblem(c.response),
  removeFactor: (c) => {
    const bad = stepupProblem(c.stepup);
    if (bad !== null) return bad;
    if (c.kind !== 'totp' && c.kind !== 'passkey') return 'kind is not totp or passkey';
    if (c.kind === 'totp') return c.credId === null ? null : 'credId is for a passkey';
    return typeof c.credId === 'string' && CRED_ID.test(c.credId) ? null : 'credId is not 2 to 2046 lowercase hex';
  },
  regenerateBackupCodes: (c) => stepupProblem(c.stepup),
  changePassword: (c) => {
    if (typeof c.next !== 'string' || c.next === '') return 'the new password is empty';
    if (c.old !== null && (typeof c.old !== 'string' || c.old === '')) return 'the old password is empty';
    return optionalStepupProblem(c.stepup);
  },
};

export class FactorsController {
  constructor(private readonly deps: FactorsDeps) {}

  /** Reads the factors and publishes them; rejects when the read does. */
  async refresh(): Promise<null> {
    if (!this.deps.holdsFactors()) return null;
    const factors = await this.deps.routes.getFactors();
    // A late answer after a wipe or a re-attach is not this account's to publish.
    if (this.deps.holdsFactors()) this.deps.publish('factors', factorsState(factors));
    return null;
  }

  /** The re-read after a commit: the server has already acted, so a failed read must not hide the result. The slice stays
   *  as it was and the page can call refreshFactors. */
  private async reread(): Promise<void> {
    try { await this.refresh(); } catch { /* the commit stands */ }
  }

  async totpBegin(stepup: string | null): Promise<{ secret: string; uri: string }> {
    const { secret, uri } = await this.guarded(() => this.deps.routes.totpEnroll(stepup));
    return { secret, uri };
  }

  async totpConfirm(code: string): Promise<{ backupCodes: string[] }> {
    const backupCodes = await this.guarded(() => this.deps.routes.totpConfirm(code));
    await this.reread();
    return { backupCodes };
  }

  async passkeyBegin(stepup: string | null): Promise<{ ceremonyId: string; options: string }> {
    const c = await this.guarded(() => this.deps.routes.passkeyRegisterBegin(stepup));
    return { ceremonyId: toHex(c.ceremonyId), options: c.options };
  }

  async passkeyFinish(ceremonyId: string, response: string, name: string): Promise<{ backupCodes: string[] }> {
    const done = await this.guarded(() => this.deps.routes.passkeyRegisterFinish(fromHex(ceremonyId), response, name));
    await this.reread();
    return { backupCodes: done.backupCodes };
  }

  /** A wrong secret is the person's to correct at the dialog: no re-read, the factors did not change. */
  async stepUp(method: 'password' | 'totp' | 'backup', secret: string): Promise<{ token: string }> {
    const kind = method === 'password' ? 0 : method === 'totp' ? 1 : 2;
    const { token } = await this.deps.routes.stepUp(kind, method === 'backup' ? normaliseBackupCode(secret) : secret);
    return { token };
  }

  async stepUpPasskeyBegin(): Promise<{ ceremonyId: string; options: string }> {
    const c = await this.deps.routes.stepUpPasskeyBegin();
    return { ceremonyId: toHex(c.ceremonyId), options: c.options };
  }

  async stepUpPasskeyFinish(ceremonyId: string, response: string): Promise<{ token: string }> {
    const { token } = await this.deps.routes.stepUpPasskeyFinish(fromHex(ceremonyId), response);
    return { token };
  }

  async removeFactor(stepup: string, kind: 'totp' | 'passkey', credId: string | null): Promise<null> {
    await this.guarded(() => this.deps.routes.removeFactor(stepup, kind === 'totp' ? 0 : 1, credId === null ? null : fromHex(credId)));
    await this.reread();
    return null;
  }

  async regenerateBackupCodes(stepup: string): Promise<{ backupCodes: string[] }> {
    const backupCodes = await this.guarded(() => this.deps.routes.regenerateBackupCodes(stepup));
    await this.reread();
    return { backupCodes };
  }

  /** The old password is the step-up where the account has one (a wrong one is 403 E_FORBIDDEN, a throttle 429: the dialog's to
   *  show, no re-read); a first password on an account holding a factor takes a step-up token instead. Neither password is kept:
   *  they pass through this call and the route, and the answer carries nothing. */
  async changePassword(old: string | null, next: string, stepup: string | null): Promise<null> {
    await this.guarded(() => this.deps.routes.changePassword(old, next, stepup));
    await this.reread();
    return null;
  }

  /** A refused step-up (403 E_STEP_UP), a missing row (404) or a conflict (409) means the page's picture of the account is
   *  stale: the factors are read again, best effort, and the refusal is rethrown unchanged. */
  private async guarded<T>(call: () => Promise<T>): Promise<T> {
    try {
      return await call();
    } catch (e) {
      if (e instanceof DillaHttpError && (e.status === 404 || e.status === 409 || (e.status === 403 && e.code === 'E_STEP_UP'))) {
        await this.reread();
      }
      throw e;
    }
  }
}
