// The messages between the page and the core worker (L-TS-09).
import type { CallKeys, CallStatsReport, SliceName, WorkerError } from '../state/types';

export type Command =
  | { m: 'start' }
  | { m: 'signupBegin' }
  // → { communityId, joinError }; with factorOwed: true (and no join yet) when the account stops at factor-setup.
  | { m: 'signupSubmit'; invite: string; username: string; display: string; password: string | null; recoveryKeyAcknowledged: true }
  | { m: 'signupReset' }
  | { m: 'resetDevice' }
  | { m: 'joinCommunity'; invite: string }
  // v0.1.0-web3.1, ready only. Names are checked as the instance checks them: 1..255 UTF-8 bytes for a server, 1..100 for a
  // channel, no control character, not blank. createChannel adds a top-level channel the server cannot read.
  | { m: 'createCommunity'; name: string }                                              // → { communityId }; listed before the ret
  | { m: 'createChannel'; communityId: string; kind: 'text' | 'voice' | 'announcement'; name: string; parentId?: string;
      private?: { roleIds: string[] }; announce?: { roleIds: string[] } }   // → { channelId }; listed before the ret
  | { m: 'createInvite'; communityId: string; maxUses: number; ttlSeconds: number }     // → InviteCreatedResult
  | { m: 'selectCommunity'; communityId: string }
  // Ready only → null: GET /v1/communities again; the communities slice is published before the answer (the page looks for
  // a server whose create answer was lost, with the socket still up).
  | { m: 'refreshCommunities' }
  | { m: 'getBotInstallation'; communityId: string }
  | { m: 'getBotCommandStatus'; communityId: string }
  | { m: 'getBotSpotifyConfig'; communityId: string }
  | { m: 'patchBotProfile'; communityId: string; display: string }
  // Typed future request contract only. The worker refuses it until human kind-3 interaction groups are authenticated.
  | { m: 'listModerationCases'; communityId: string; requestId: string; offset: 0 }
  | { m: 'listManualModerationEvidence'; communityId: string; requestId: string; cursor: string | null }
  | { m: 'humanInteractionRequest'; communityId: string; channelId: string | null; requestId: string; operation: number;
      args: readonly (string | number | Uint8Array | null)[]; revision: string | null; stepUp?: string }
  | { m: 'readBotInteractionResponses'; communityId: string; operations: readonly number[]; requestIds?: readonly string[] }
  | { m: 'ackBotInteractionResponses'; communityId: string; through: string }
  | { m: 'readHumanInteractionResponses'; communityId: string; requestIds?: readonly string[] }
  | { m: 'ackHumanInteractionResponses'; communityId: string; through: string }
  | { m: 'getBotJoinRole'; communityId: string }
  | { m: 'configureBotJoinRole'; communityId: string; requestId: string; roleId: string; installationId: string }
  | { m: 'restartBot'; communityId: string; installationId: string }
  | { m: 'setBotMusicSettings'; communityId: string; installationId: string; volume: number; idleMinutes: number }
  | { m: 'listSelfRoles'; communityId: string }
  | { m: 'configureSelfRoles'; communityId: string; requestId: string; roleIds: string[] }
  | { m: 'selectSelfRole'; communityId: string; roleId: string; requestId: string; enabled: boolean }
  | { m: 'registerRolePanel'; communityId: string; requestId: string; channelId: string; messageSeq: string;
      options: readonly { emoji: string; roleId: string }[] }
  | { m: 'configureBotInstallation'; communityId: string; requestId: string; expectedRevision: string | null; capabilities: number; stepUp: string }
  | { m: 'disableBotInstallation'; communityId: string; requestId: string; expectedRevision: string }
  | { m: 'beginBotSpotifyAttempt'; communityId: string; stepUp: string; clientId: string }
  | { m: 'callbackBotSpotifyAttempt'; communityId: string; attemptId: string; state: string; code: string }
  | { m: 'cancelBotSpotifyAttempt'; communityId: string; attemptId: string }
  | { m: 'openChannel'; channelId: string }
  | { m: 'closeChannel'; channelId: string }
  | { m: 'loadEarlier'; channelId: string }
  // web3.7 public threads. One open thread per channel; it is read again whenever the channel's timeline is.
  | { m: 'openThread'; channelId: string; rootMsgId: string }    // ready → null; slice thread:<channel>, published before the ret
  | { m: 'closeThread'; channelId: string }                       // ready → null; thread:<channel> = null
  | { m: 'loadThreads'; channelId: string }                       // ready → null; slice threads:<channel>
  | { m: 'send'; channelId: string; text: string; replyTo?: string | null; attachments?: readonly string[]; threadId?: string | null }   // → { msgId }; threadId: a held type-0 root (web3.7)
  | { m: 'retrySend'; msgId: string }
  | { m: 'discardSend'; msgId: string }
  | { m: 'signInBegin' }                                         // needs-signup → signin-login
  // web3.8 (R6): the password and the factor first, the recovery key last. The login and the factor never register: the
  // worker holds the settled assertion, and signInKey registers it after the key's form check, back to back with the
  // fetch, the enrolment and the list PUT (REGISTRATION-DEVICES-02). The login, the code and the passkey carry no key
  // (pre-flight C-3); only signInKey and signInBackupCode do.
  | { m: 'signInLogin'; username: string; password: string }    // → { needs }: 1 signin-totp, 2 signin-passkey, 0 signin-key
  | { m: 'signInTotp'; code: string }                            // signin-totp → signin-key
  // web3.6 (flow 10). The ceremony runs in the page (navigator.credentials); the worker holds no WebAuthn state.
  | { m: 'signInPasskeyBegin' }                                  // signin-login|totp|passkey → { ceremonyId (32 hex), options (JSON) }
  | { m: 'signInPasskey'; ceremonyId: string; response: string } // → { needs }: 1 signin-totp, 0 signin-key
  // A backup code is held in the page and sent only with the key, so an assertion that expired meanwhile spends none.
  | { m: 'signInBackupCode'; code: string; recoveryKey: string }      // signin-totp|passkey → ready; a refusal: signin-login
  | { m: 'factorSetupDone'; invite: string | null }              // factor-setup → ready → { communityId, joinError }
  // web3.6 (flow 10): factor management. The secret, the uri, the codes and the tokens travel only in these returns: the
  // worker keeps none of them, and the factors slice holds none. Every state-changing one publishes `factors` before it resolves.
  | { m: 'refreshFactors' }                                      // ready|factor-setup → null; slice factors
  | { m: 'factorTotpBegin'; stepup: string | null }              // ready|factor-setup → { secret, uri }
  | { m: 'factorTotpConfirm'; code: string }                     // ready|factor-setup → { backupCodes }
  | { m: 'factorPasskeyBegin'; stepup: string | null }           // ready|factor-setup → { ceremonyId (32 hex), options (JSON) }
  | { m: 'factorPasskeyFinish'; ceremonyId: string; response: string; name: string } // ready|factor-setup → { backupCodes }
  | { m: 'stepUp'; method: 'password' | 'totp' | 'backup'; secret: string }          // ready → { token }
  | { m: 'stepUpPasskeyBegin' }                                  // ready → { ceremonyId, options }
  | { m: 'stepUpPasskeyFinish'; ceremonyId: string; response: string }               // ready → { token }
  | { m: 'removeFactor'; stepup: string; kind: 'totp' | 'passkey'; credId: string | null } // ready → null
  | { m: 'freshSession' }                                         // ready → null; mints a new session from the device key (a first password needs one under the reauth window)
  | { m: 'changePassword'; old: string | null; next: string; stepup: string | null } // ready → null; slice factors (hasPassword). Neither password is kept
  | { m: 'regenerateBackupCodes'; stepup: string }               // ready → { backupCodes }
  | { m: 'signInKey'; recoveryKey: string }                      // signin-key → ready; registers when nothing is registered yet
  | { m: 'signInCancel' }                                        // signin-* → needs-signup
  | { m: 'refreshDevices' }                                      // ready: GET /v1/devices + own list → slice devices
  | { m: 'revokeDevice'; deviceId: string; recoveryKey: string | null } // ready: another device; the key is required for a listed device, ignored (null) for an unlisted live row
  | { m: 'renameDevice'; deviceId: string; label: string | null }  // ready: PATCH /v1/devices/{id}; the label is trimmed, 1..64 characters, no control character; null clears → slice devices
  | { m: 'signOutRevoke'; recoveryKey: string }                  // ready: this device → store wiped → cleared
  | { m: 'forgetBrowser' }                                       // ready (and revoked, pre-flight ruling d): sessions deleted, store wiped → cleared
  | { m: 'markRead'; channelId: string }                         // ready
  | { m: 'setPresenceChoice'; choice: 'online' | 'idle' | 'dnd' | 'invisible' }
  | { m: 'updateProfile'; display?: string; statusMsg?: string; bio?: string }   // ready: a field left out is left alone, '' clears it → null
  | { m: 'presenceActivity' }
  | { m: 'getAFKSettings'; communityId: string }
  | { m: 'setAFK'; communityId: string; timeoutSeconds: number }
  | { m: 'afkActivity'; communityId: string; channelId: string; exempt?: boolean }
  | { m: 'afkAway'; communityId: string }                         // ready → null: PUT …/afk/away (AW4); nothing clears away
  | { m: 'afkRead'; communityId: string; timeoutMs?: number }                       // ready → null once a fresh GET …/afk landed (AW5 fix 1); E_UNAVAILABLE if it failed
  | { m: 'setSetting'; key: string; value: string | null }       // ready; null deletes; key must match L-TS-25, value its allowed set
  | { m: 'openDm'; userId: string }                              // ready → { channelId }
  | { m: 'editMessage'; channelId: string; msgId: string; text: string }                  // ready → null (type 1)
  | { m: 'deleteMessage'; channelId: string; msgId: string; seq: string }                // ready → null
  | { m: 'moderateRemoveMessage'; channelId: string; msgId: string; seq: string; reason: string; excerpt: string } // ready → null; signed removal then op61 evidence
  | { m: 'resolveManualRemoval'; proofRequestId: string } // ready → reconcile feed and retry the exact durable signed proof
  | { m: 'react'; channelId: string; msgId: string; emoji: string; on: boolean }          // ready → null (type 3, or 4 when off)
  | { m: 'pin'; channelId: string; msgId: string; on: boolean }                           // ready → null (type 5, or 6 when off)
  | { m: 'loadPins'; channelId: string }                                                  // ready → null; slice pins:<channel>
  | { m: 'closePins'; channelId: string }                                                 // ready → null
  | { m: 'attachFiles'; channelId: string; files: readonly File[] }                       // ready → { trayIds }; slice tray:<channel>
  | { m: 'checkpointFiles'; channelId: string }                                          // ready → File[] selected in this channel
  | { m: 'discardAttachment'; channelId: string; trayId: string }                         // ready → null
  | { m: 'openAttachment'; channelId: string; seq: string; index: number; thumb: boolean } // ready → { blob, name, mime }
  // E8: the GIF picker. The Giphy key is a per-server setting. The worker holds the session, so it searches, fetches and
  // saves the key; a GIF crosses back as a Blob and the page sends it with attachFiles, so it is an ordinary attachment,
  // sealed like any file, and no browser contacts Giphy. The key only ever goes up: no return, slice or error carries it.
  | { m: 'gifStatus'; communityId: string }                                               // ready → { on, rating }
  | { m: 'gifSetKey'; communityId: string; key: string }                                  // ready → null (a server manager)
  | { m: 'gifClearKey'; communityId: string }                                             // ready → null (a server manager)
  | { m: 'gifSetRating'; communityId: string; rating: 'g' | 'pg' | 'pg-13' | 'r' }        // ready → null (a server manager)
  | { m: 'gifSearch'; communityId: string; q: string; offset: number }                    // ready → { items: [{ id, title, width, height }], next }
  | { m: 'gifFetch'; communityId: string; gifId: string; variant: 'preview' | 'full' }     // ready → { blob } (image/gif)
  | { m: 'refreshTrust' } // ready: bounded signed-list refresh
  | { m: 'verifyContact'; userId: string; umk: string }
  // ready → string: the digits for exactly `umk` (64 lowercase hex), the key the dialog will hand to verifyContact
  // (changedUmk ?? umk); E_DEVICE_LIST_STALE when it is no longer the core's comparison key. Without `umk` (until the page
  // sends it) the worker binds the core's current comparison key.
  | { m: 'safetyNumber'; userId: string; umk: string }
  | { m: 'search'; scope: 'channel' | 'community' | 'all'; id: string | null; query: string;
    cursor: { token: string } | null }
  | { m: 'typing'; channelId: string; on: boolean }
  | { m: 'voiceSelf'; callId: string; flags: number }
  // web-3 (L-TS-50). The page never sends callJoin directly: startPageCall (L-WEB-50) sends it after subscribing to
  // onCallKeys. "Answer" is callJoin with the DM's channel id. vdec: '' or up to three of vp8, h264, vp9, no repeats.
  | { m: 'callJoin'; channelId: string; vdec: string }                                   // → CallJoinResult (no key bytes)
  // 'lost' (WB client I3): the page's room ended on its own (its connection gave up, or the room closed); the worker
  // leaves as for 'user' and ends in phase removed with E_DISCONNECTED rather than a silent idle.
  | { m: 'callLeave'; reason: 'user' | 'pagehide' | 'switch' | 'lost' }                  // → null
  | { m: 'callShare'; on: boolean }                                                      // → null
  | { m: 'callRefresh' }                                                                 // → { token, iceServers }
  | { m: 'callStats'; report: CallStatsReport }                                          // → null
  // Signal task 8c: this device's RTT to the SFU in whole ms (0..5000), or null to clear it (paused, reconnecting).
  | { m: 'callRtt'; ms: number | null }                                                  // → boolean (false: refused, not stored)
  // fix/firefox-misrouted-audio: the page's media layer notes a case Diagnostics should show; code is one of CALL_NOTES.
  | { m: 'callNote'; code: 'E_MEDIA_MISROUTE' }                                         // → null
  | { m: 'loadVoice'; communityId: string | null; channelId: string | null }            // exactly one id → null
  | { m: 'callDecline'; callId: string; ringTs: number }                                 // ringTs: op 53's expires_ts → null
  | { m: 'renameCommunity'; communityId: string; name: string }
  | { m: 'deleteCommunity'; communityId: string }           // dropped before the ret; no removal notice
  | { m: 'leaveCommunity'; communityId: string }            // same
  | { m: 'transferOwnership'; communityId: string; userId: string }
  | { m: 'createCategory'; communityId: string; name: string }   // → { channelId }
  | { m: 'updateChannel'; channelId: string; name: string | null; topic: string | null; parent: string; slowmode: number | null }  // parent: 'keep'|'top'|32-hex; slowmode: null keeps it, else a SLOW_STEPS value
  | { m: 'moveChannel'; channelId: string; direction: 'up' | 'down' }
  | { m: 'deleteChannel'; channelId: string }
  | { m: 'kickMember'; communityId: string; userId: string }
  | { m: 'banMember'; communityId: string; userId: string; reason: string; seconds: number | null }  // 3600..3_153_600_000
  | { m: 'unbanMember'; communityId: string; userId: string }
  | { m: 'getMemberTimeout'; communityId: string; userId: string } // → Unix-second timestamp string|null
  | { m: 'setMemberTimeout'; communityId: string; userId: string; requestId: string; expires: string } // expires is Unix seconds
  | { m: 'clearMemberTimeout'; communityId: string; userId: string; requestId: string }
  | { m: 'warnMember'; communityId: string; userId: string; requestId: string; reason: string }
  | { m: 'loadBans'; communityId: string }      // slice bans:<cid>
  | { m: 'loadInvites'; communityId: string }   // slice invites:<cid>, live only
  | { m: 'revokeInvite'; communityId: string; inviteId: string }
  | { m: 'createRole'; communityId: string; name: string; color: number; hoist: boolean; mentionable: boolean; allow: number }  // → { roleId }
  | { m: 'updateRole'; communityId: string; roleId: string; name: string | null; color: number | null; hoist: boolean | null;
      mentionable: boolean | null; allow: number | null }
  | { m: 'moveRole'; communityId: string; roleId: string; direction: 'up' | 'down' }
  | { m: 'deleteRole'; communityId: string; roleId: string }
  | { m: 'grantRole'; communityId: string; userId: string; roleId: string }    // members and access are read again before the ret
  | { m: 'revokeRole'; communityId: string; userId: string; roleId: string }
  | { m: 'setOverwrite'; channelId: string; kind: 0 | 1; targetId: string; allow: number; deny: number }   // 0/0 → DELETE (404 = done)
  | { m: 'setChannelPrivate'; channelId: string; private: boolean }
  | { m: 'resign'; nonce: string };
export type ToWorker = { t: 'call'; id: number; command: Command };
export type FromWorker =
  | { t: 'ret'; id: number; ok: true; value: unknown }
  | { t: 'ret'; id: number; ok: false; error: WorkerError }
  | { t: 'slice'; name: SliceName; rev: number; value: unknown }
  | { t: 'released'; instance: string; nonce: string }
  // L-TS-52: posted with [keys.baseKey] transferred, only to the page that owns the call; never a ret, never a slice.
  | { t: 'callKeys'; callId: string; keys: CallKeys };
/** Not part of ToWorker. Honoured only by a worker started with { testHooks: true }, which only
 *  packages/client-core/harness/worker.ts does; src/worker/entry.ts (what createCoreWorker() loads) passes false. */
export type TestHook = { t: 'test'; op: 'gateway-stop' | 'gateway-start' | 'pause-fail-once' };
export const LOCK_PREFIX = 'dilla-core:';        // + instance id hex
const HEX_ID = /^[0-9a-f]{32}$/;

function exactNotice(v: unknown, kind: 'released' | 'takeover', instance: string, nonce: string): boolean {
  if (!isRecord(v) || !HEX_ID.test(instance) || !HEX_ID.test(nonce)) return false;
  const keys = Object.keys(v);
  return keys.length === 3 && keys.includes('t') && keys.includes('instance') && keys.includes('nonce')
    && v.t === kind && v.instance === instance && v.nonce === nonce;
}
export const isReleaseNotice = (v: unknown, instance: string, nonce: string): boolean => exactNotice(v, 'released', instance, nonce);
export const isTakeoverNotice = (v: unknown, instance: string, nonce: string): boolean => exactNotice(v, 'takeover', instance, nonce);

function isRecord(data: unknown): data is Record<string, unknown> {
  return typeof data === 'object' && data !== null;
}

/** A call envelope: t 'call', a non-negative safe integer id and a command object with a string m. The
 *  command's own fields are checked by the controller. */
export function isToWorker(data: unknown): data is ToWorker {
  if (!isRecord(data) || data.t !== 'call') return false;
  const { id, command } = data;
  return typeof id === 'number' && Number.isSafeInteger(id) && id >= 0 && isRecord(command) && typeof command.m === 'string';
}

export function isTestHook(data: unknown): data is TestHook {
  return isRecord(data) && data.t === 'test' && (data.op === 'gateway-stop' || data.op === 'gateway-start' || data.op === 'pause-fail-once');
}
