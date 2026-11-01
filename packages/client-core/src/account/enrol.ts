/** The second-browser enrolment (L-TS-21): host login, a pending registration, the sealed objects, the recovery key, list v+1; and the upload of sealed objects on every ready. */
import { CoreError, type CorePort, type Id } from '../core-port';
import { DillaHttpError } from '../http/errors';
import type { Routes } from '../http/routes';
import type { Session } from './session';
import { adoptServedList, publishDeviceList } from './signup';
import { chainRow } from '../state/trust';

export interface EnrolFetched { root: Uint8Array; state: Uint8Array; listBody: Uint8Array }

export class Enrol {
  private assertion: string | null = null;
  /** What the held assertion still owes (web3.6 `needs`): 0 nothing, 1 an app code or a backup code, 2 a passkey or a
   *  backup code. It is the assertion's debt, not the account's factor_owed flag. */
  private assertionOwes: 0 | 1 | 2 = 0;
  constructor(private readonly deps: { core: CorePort; routes: Routes; session: Session; now(): number }) {}

  /** Spends the held assertion when it owes what `want` asks: 'settled' nothing (the registration), 'code' an app code
   *  (needs 1), 'owed' anything (a backup code, which settles needs 1 and 2 alike). */
  private take(want: 'settled' | 'code' | 'owed'): string {
    const owes = this.assertionOwes;
    const fits = want === 'settled' ? owes === 0 : want === 'code' ? owes === 1 : owes !== 0;
    if (this.assertion === null || !fits) throw new Error('E_NO_ASSERTION');
    const assertion = this.assertion;
    this.assertion = null;
    this.assertionOwes = 0;
    return assertion;
  }

  private hold(assertion: string, owes: 0 | 1 | 2): void {
    this.assertion = assertion;
    this.assertionOwes = owes;
  }

  async login(username: string, password: string): Promise<{ needs: 0 | 1 | 2 }> {
    this.assertion = null;
    this.assertionOwes = 0;
    const answer = await this.deps.routes.passwordLogin(username, password);
    this.hold(answer.assertion, answer.needs);
    return { needs: answer.needs };
  }

  /** A discoverable passkey login (web3.6): needs 0 when the authenticator verified the user, else 1 (a code follows).
   *  A refused finish leaves any assertion held from an earlier login in place. */
  async passkey(ceremonyId: Id, response: string): Promise<{ needs: 0 | 1 }> {
    const answer = await this.deps.routes.passkeyLoginFinish(ceremonyId, response);
    this.hold(answer.assertion, answer.needs);
    return { needs: answer.needs };
  }

  async totp(code: string): Promise<void> {
    const assertion = this.take('code');
    const answer = await this.deps.routes.totpVerify(assertion, code);
    this.hold(answer.assertion, 0);
  }

  /** A backup code settles an assertion owing needs 1 or 2; a refused code has spent it (the person logs in again). */
  async backupCode(code: string): Promise<void> {
    const assertion = this.take('owed');
    const answer = await this.deps.routes.recoveryVerify(assertion, code);
    this.hold(answer.assertion, 0);
  }

  async register(instanceId: Id): Promise<{ userId: Id }> {
    const assertion = this.take('settled');
    const { core, routes } = this.deps;
    const id = core.identity();
    let deviceId: Id;
    if (id.phase === 0) deviceId = core.enrolBegin(instanceId).deviceId;
    else if (id.phase === 3 && id.userId === null && id.deviceId !== null) deviceId = id.deviceId;
    else throw new CoreError('E_CORE_STATE', 'register needs phase 0 or an unregistered enrolment');
    const { nonce } = await routes.postChallenge(deviceId);
    const login = new TextEncoder().encode(assertion);
    let body: Uint8Array;
    try { body = core.enrolSessionSign(nonce, login); }
    finally { login.fill(0); }
    const answer = await routes.postSessionPending(deviceId, body);
    if (answer.scope !== 1) throw new Error('E_SESSION_SCOPE');
    core.sessionStore({ token: answer.token, expires: answer.expires, idleExpires: answer.idleExpires });
    core.enrolRegistered(answer.userId);
    return { userId: answer.userId };
  }

  async fetch(userId: Id): Promise<EnrolFetched> {
    const { routes } = this.deps;
    const root = await routes.getBackup(0);
    if (root === null) throw new Error('E_NO_BACKUP');
    const state = await routes.getBackup(1);
    const list = await routes.getDeviceList(userId);
    if (list === null) throw new Error('E_NO_BACKUP');
    // The core decodes the four-element list body; a wire-4 head's attestation is no part of it.
    return { root: root.object, state: state?.object ?? new Uint8Array(0), listBody: chainRow(list.raw) };
  }

  async complete(recoveryKey: string, fetched: EnrolFetched, username: string): Promise<void> {
    const { core, routes, session } = this.deps;
    const { stateSealed, interrupted } = core.enrolComplete({ recoveryKey, rootSealed: fetched.root, stateSealed: fetched.state,
      listBody: fetched.listBody, username, now: BigInt(Math.floor(this.deps.now() / 1000)) });
    if (interrupted !== null) await publishInterrupted(core, routes, interrupted);
    try { await publishDeviceList(core, routes); }
    catch (err) {
      if (err instanceof Error && err.message === 'E_DEVICE_UNLISTED') throw new Error('E_LIST_RACE');
      // REGISTRATION-DEVICES-02: a 401 here (the session did not re-establish either) is a row the instance
      // revoked after the registration: replaced by another registration of the account.
      if (err instanceof DillaHttpError && err.status === 401) throw new Error('E_SIGNIN_EVICTED');
      throw err;
    }
    // WORKER-WEB-01: the pending token from register is not kept past the list that names this browser, so a
    // reload or a failed establish after this point finds no session and ensure() establishes an enrolled one.
    core.sessionClear();
    if (!(await session.establish())) throw new Error('E_SIGNIN_EVICTED');
    const instance = await routes.getInstance();
    core.instanceSignerKeyPin(instance.instanceId, instance.externalSenderPub);
    await routes.putBackup(1, stateSealed);
    core.stateSealedUploaded();
  }

  reset(): void {
    this.assertion = null;
    this.assertionOwes = 0;
    if (this.deps.core.identity().phase === 3) this.deps.core.enrolReset();
  }
}

/** BACKUPS-RECOVERY-02: the list another device signed and did not publish, which the core found in the state object
 *  and signed this browser's list on, goes to the instance first. A 409 is a list there already (the next PUT meets
 *  a fork); a 401 is this row replaced (E_SIGNIN_EVICTED). Any other failure starts the sign-in over (E_LIST_RACE):
 *  this browser's list cannot be published without it, and the instance still holds the state object that names it. */
async function publishInterrupted(core: CorePort, routes: Routes, body: Uint8Array): Promise<void> {
  const userId = core.identity().userId;
  if (userId === null) throw new CoreError('E_CORE_NO_IDENTITY', '');
  try { await routes.putDeviceList(userId, body); }
  catch (err) {
    if (err instanceof DillaHttpError && err.status === 409) return;
    if (err instanceof DillaHttpError && err.status === 401) throw new Error('E_SIGNIN_EVICTED');
    throw new Error('E_LIST_RACE');
  }
}

export async function refreshOwnDeviceList(core: CorePort, routes: Routes, userId: Id):
  Promise<{ version: bigint; listed: boolean }> {
  return adoptServedList(core, routes, userId, await routes.getDeviceList(userId));
}

const same = (a: Uint8Array, b: Uint8Array): boolean => a.length === b.length && a.every((value, i) => value === b[i]);
const confirmedRoots = new WeakMap<CorePort, Uint8Array>();

export async function ensureBackups(core: CorePort, routes: Routes): Promise<void> {
  const rows = await routes.listBackups();
  const sealed = core.sealedObjects();
  if (sealed.root !== null) {
    if (rows.some((row) => row.kind === 0)) {
      if (!same(confirmedRoots.get(core) ?? new Uint8Array(0), sealed.root)) {
        const stored = await routes.getBackup(0);
        if (stored === null || !same(stored.object, sealed.root)) throw new Error('E_ROOT_MISMATCH');
        confirmedRoots.set(core, sealed.root.slice());
      }
    } else {
      try { await routes.putBackup(0, sealed.root); }
      catch (err) {
        // 409: a root is stored. 410 E_PRUNED: an operator purged these bytes, which tombstones them while the
        // root stays served (protocol/09 Admin); either way the stored root is the one to compare.
        const stored409 = err instanceof DillaHttpError && err.status === 409;
        const pruned = err instanceof DillaHttpError && err.status === 410 && err.code === 'E_PRUNED';
        if (!stored409 && !pruned) throw err;
        const stored = await routes.getBackup(0);
        if (stored === null || !same(stored.object, sealed.root)) throw new Error('E_ROOT_MISMATCH');
      }
      confirmedRoots.set(core, sealed.root.slice());
    }
  }
  if (sealed.state === null) return;
  const listed = rows.some((row) => row.kind === 1);
  // The repair of a stored object that is behind or junk is repairBackupState's, after the own list is refreshed.
  if (!listed || !sealed.stateUploaded) {
    await routes.putBackup(1, sealed.state);
    core.stateSealedUploaded();
  }
}

/** BACKUPS-RECOVERY-03, at each ready once the own list is refreshed: a stolen session can replace the state object
 *  with junk, and past list v1 the core refuses an unopenable one (the rollback floor), so every recovery-key action
 *  would fail. This browser holds no K_backup and cannot open the instance's copy; when its own sealed state carries
 *  the list it accepted as the newest, a stored object with other bytes is behind that list or junk, and it uploads
 *  its own. A browser whose own state is older leaves the object to the device that published the newest list.
 *  Resolves whether it uploaded. */
export async function repairBackupState(core: CorePort, routes: Routes): Promise<boolean> {
  const own = core.sealedObjects().state;
  if (own === null) return false;
  const stored = await routes.getBackup(1);
  if (stored !== null && same(stored.object, own)) return false;
  if (stored !== null && !core.stateSealedCurrent()) return false;
  await routes.putBackup(1, own);
  core.stateSealedUploaded();
  return true;
}
