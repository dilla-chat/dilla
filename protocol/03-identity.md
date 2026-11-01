# 03 — Identity

## Keys

Per user:

- **UMK** (user master key): Ed25519. Generated at signup on the first device. The private key is
  stored **only** in the recovery-key-encrypted header (`06-backup-archive.md`) and, transiently,
  on the device performing signup or recovery. It signs exactly one thing: the SSK.
- **SSK** (user signing key): Ed25519, signed by the UMK. The private key lives on every `native`
  device of the user and never on a `browser` device, with one exception: a `browser` device that
  performs its own signup ("Browser-rooted signup" below). It signs device credentials and the
  device list.

Per device:

- **DSK** (device signing key): Ed25519. It is the MLS leaf signature key. Generated on the device;
  the private key never leaves it. Stored in the OS keystore where one exists (Keychain, DPAPI,
  libsecret or KWallet, Android Keystore, iOS Secure Enclave-backed keychain); on the `browser` tier,
  in the device's encrypted store (the MLS library's signature-key table), whose 256-bit key is
  random and is kept in IndexedDB only wrapped by a non-extractable WebCrypto key.

Signature domains (the message signed is the domain string in UTF-8 followed by the fields, with
no separators):

- `sig_umk_ssk = Ed25519.sign(UMK_priv, "dilla ssk v1" || ssk_pub)`
- `sig_ssk_dev = Ed25519.sign(SSK_priv, "dilla dsk v1" || device_id || dsk_pub || kind || tier || signer_tier)`
  where `kind`, `tier` and `signer_tier` are one byte each, unsigned (`kind`: 0 user, 1 bot; `tier`:
  0 native, 1 browser; `signer_tier`: 0 native, 1 browser, 2 provisional — see "Pairing"). For a
  browser device signed by a one-shot recovery-key entry, the signer is still the SSK
  (reconstituted from the recovered root object in memory and zeroised afterwards) and
  `signer_tier` in the credential is 1.

Instance key rotation: a new instance signing key is announced with
`sig_old(new_pub || "dilla instance rotate v1")`; clients accept the `GroupContextExtensions`
proposal that replaces `external_senders` only when that signature verifies under the previous key.

### Browser-rooted signup

A `browser` device may create an account without any `native` device. It performs, in order:

1. Generate `UMK`, `SSK`, its `DSK`, a random `device_id` and `RK` ("Recovery"). Compute
   `sig_umk_ssk` and `sig_ssk_dev` with `kind = 0`, `tier = 1`, `signer_tier = 1`, seal the root
   object (`06-backup-archive.md`, "Header") under `K_header`, then erase `UMK_priv`, `RK` and
   `K_header`. `SSK_priv` and `K_backup` are kept only in the device's encrypted store, in one
   pending signup record.
2. Show `RK` and require the acknowledgement that "Recovery" describes, before anything is sent.
3. Register (`09-http-api.md`, `POST /v1/accounts`) with a credential whose `user_id` is 16 zero
   bytes: the instance mints the `user_id`, and `sig_ssk_dev` does not cover it.
4. Replace `user_id` in the credential with the minted one (no signature changes), sign device list
   version 1 naming this device, seal the first state object under `K_backup`, and delete the
   pending signup record, which erases `SSK_priv` and `K_backup`. Then publish the device list.

From step 4 on no device of the account holds `SSK_priv`: adding or revoking a device needs `RK`
("Recovery") or a `native` device. A device that is interrupted between steps 3 and 4 resumes from
the pending record; a device that loses its store in that window has registered an account it
cannot complete, and the user starts a new signup.

### Instance keys

An instance holds two long-lived secrets: the **external-sender signing key**, an Ed25519 keypair
whose public half is the `external_senders` GroupContext extension of every `text` and `call` group,
and the **franking key** `K_frank`, 32 bytes used as the HMAC-SHA256 key of `04-envelope-and-franking.md`
§ Franking. Both are generated once by `dillad init` from the platform CSPRNG and are stored, with
every key they have replaced, in `instances.key_history` as deterministic CBOR:

```
key_history = [
  v,             ; uint, = 1
  entries        ; [+ entry], oldest first
]

entry = [
  kind,          ; uint: 0 external-sender Ed25519 signing key, 1 franking key
  key_id,        ; bstr 16
  public,        ; bstr: the 32-byte Ed25519 public key for kind 0, an empty bstr for kind 1
  secret,        ; bstr 32: the Ed25519 seed for kind 0, K_frank for kind 1
  created,       ; uint, unix seconds
  retired        ; uint or null: unix seconds, null while the entry is current
]
```

`instances.external_sender_key_id` and `instances.franking_key_id` name the one entry of each kind
whose `retired` is null. Rotation appends an entry and sets the previous entry's `retired`; a
retired franking key is kept so that a report franked under it can still be verified, and a retired
signing key is kept so that the rotation signature of this section verifies under it. The history is
instance secret material: it is in a backup (`06-backup-archive.md`, "the instance keys") and it is
never served over `/v1`.

## Credential

Every leaf uses an MLS `basic` credential whose `identity` is the deterministic CBOR encoding of:

```
[
  v,             ; uint, = 1
  umk_pub,       ; bstr, 32
  user_id,       ; bstr, 16
  device_id,     ; bstr, 16
  kind,          ; uint: 0 user, 1 bot
  tier,          ; uint: 0 native, 1 browser
  signer_tier,   ; uint: 0 native, 1 browser, 2 provisional (pairing only; see "Pairing")
                 ;   (tier of the device or entry that signed this device)
  ssk_pub,       ; bstr, 32
  sig_umk_ssk,   ; bstr, 64
  sig_ssk_dev    ; bstr, 64
]
```

The leaf's `signature_key` MUST equal the `dsk_pub` covered by `sig_ssk_dev`; the verifier
extracts both the credential identity and DSK from the current authenticated MLS tree leaf,
reconstructs the signed message from them, and compares an authenticated application sender with
that same leaf. A worker cannot supply either value for the trust verdict. A leaf is
accepted only if:

1. both signatures verify;
2. `umk_pub` equals the pinned UMK for `user_id` on this instance (trust-on-first-use with a
   loud, undismissable alert on change), or no pin exists yet; a key that is neither the pinned
   UMK nor one a validated signed list presented (for example a key whose list the instance
   never serves) fails this rule as `E_CREDENTIAL`. The own account is never trusted on first
   use: a device pins its own user's UMK, verified, from its own credential (at signup, at
   enrolment and whenever it opens its store), and drops any own signed list it cached under
   another UMK, so the own list is read and validated again. A leaf of the own user under any
   other UMK fails this rule as `E_UMK_CHANGED`, and a signed list under another UMK for the own
   user is refused (`E_UMK_CHANGED`) and changes no pin. The device's own leaf is not judged;
3. `dsk_pub` for `device_id` appears, unrevoked, in the newest **signed device list** the verifier
   has seen for this user (below);
4. `dilla_binding` checks of `01-groups.md` pass for the group;
5. `tier` in the credential equals the `tier` of that `device_id`'s entry in the newest signed
   device list.

Failing 1 or 4 refuses the leaf (`E_CREDENTIAL`); failing 2 is `E_UMK_CHANGED`; failing 3 is
`E_DEVICE_REVOKED` when the newest list names that `device_id` and `dsk_pub` with `revoked_at` set,
and `E_DEVICE_UNLISTED` otherwise; failing 5 is `E_TIER_MISMATCH`. An incomplete, rolled-back, gapped or
substituted list history is `E_DEVICE_LIST_STALE` and leaves the leaf pending. A proposed `Add`
whose leaf fails is left out of this device's commit (never committed, and never a refusal that
stalls the device's commits); one whose history is still unknown is refused until it is fetched.
A fetched history that fails the candidate leaf (rules 1 to 3 or 5, also for a user this device
has never cached) is that leaf's verdict, kept by its leaf identity, so the `Add` is then left
out rather than refused again; an incomplete or substituted history still only refuses.
A bad leaf already merged by a remote commit stays in MLS but is quarantined, and it blocks sends
in each group still containing it, as a changed UMK does, until a validated `Remove` eliminates
it from that group (fail closed for confidentiality: a member must not encrypt to a leaf it could
not validate). Its committer is not blocked or quarantined; the audit names who added the leaf.
Receiving is unaffected, and a leaf whose history is merely unknown (first sight) stays pending
and blocks nothing. A leaf that later validates against a newer signed list ends its own failed
verdict. A changed UMK blocks sends in each group still containing its
leaf until exact-key verification or a validated `Remove` eliminates that leaf from that group.
The Remove does not approve the new key or unblock any other group. A remote member can repeatedly
rotate a leaf's key to trigger this refusal against an honest sender.
The pin is one record per user: the pinned UMK, whether it was verified, and at most one pending
changed UMK. A validated signed list under another UMK makes that UMK the pending change: its
leaves are `E_UMK_CHANGED`, the pinned UMK's leaves are pending (their new messages wait; nothing
they sent before is retracted), and the list revokes no device of the pinned UMK until exact-key
verification approves it. A later list back under the pinned UMK ends the pending change; a list
no newer than the one cached, validated through a leaf of the pinned UMK (one SSK both UMKs
signed can name devices of both), is the cached list itself seen again: it ends nothing
and leaves the changed UMK the cached list's key, still open to verification. A
leaf's send block is never released by a later list: only exact-key verification of that leaf's
UMK, or a `Remove` of that leaf from that group (whoever commits it), releases it. The block is
checked again when a queued message is encrypted.
A message whose sender leaf is pending or failed (unlisted, a changed UMK, or a UMK no validated
list presented) is not shown, and its MLS-authenticated plaintext is kept on the device. It
becomes readable only when that exact leaf, still the sender's current leaf, later validates: a
newer signed list names it, or exact-key verification approves its UMK (the messages a changed
key sent while the change was pending wait for that approval). It is never shown under a key
the user did not approve, and deleting it discards what was kept.

The instance binds a new leaf to a device too, before any client sees it: a leaf's
`signature_key` must be the `dsk_pub` its device registered (`POST /v1/accounts`,
`POST /v1/devices`; the key the device's sessions are established under). No flow changes a
registered `dsk_pub`, so the key is compared as it stands at the check. The delivery service applies
this on every path a leaf enters a group by: a published KeyPackage, an `Add`, an external join or
resync, the one leaf a group registration adopts, and every leaf of a tree heal's reseed adopts
(`02-delivery-service.md`, "KeyPackage directory" and invariants 1, 4, 5 and 11; `ProposeAdd` takes
only a directory KeyPackage bound this way, invariant 6). Once in a group a leaf keeps its key: an
`Update` or an UpdatePath that changes it is refused by clients and the delivery service alike
(`01-groups.md`, "Client policy for proposals from members"). It is the instance's check, made
against its own device records; the client checks above, made against the signed device list, do
not depend on it.

### Account kind attestation

A credential's `kind` is signed by its own user's SSK, so it is that user's claim. The instance
states the account's kind (`users.kind`: `0` user, `1` bot) in an **account-kind attestation**,
signed with its current external-sender key (§ Instance keys), the key every client pins:

```
message     = "dilla account kind v1" || CBOR([1, instance_id(bstr16), user_id(bstr16), kind(uint)])
attestation = [
  v,             ; uint, = 1
  key_id,        ; bstr 16: the external-sender key's key_id
  kind,          ; uint: 0 user, 1 bot
  sig            ; bstr 64: Ed25519.sign(external_sender_priv, message)
]
```

`instance_id` keeps an attestation from being replayed on another instance and `user_id` from
another account. The attestation binds **no key** (Ruling AW): an account's kind never changes, so
the statement holds for every key the account ever presents, including a UMK the user moved to
without proving it to the instance. Key trust — pins, UMK change detection, the person's
verification (§ Credential, § Safety number) — is separate and unchanged. There is no expiry;
Ed25519 signatures are deterministic, so an account's attestation is the same bytes on every read
and the instance signs it when served, for every account it holds (those created before wire 4
included), with nothing stored. It travels as the fifth element of the
`GET /v1/users/{user_id}/device-list` head on a wire-4 session (`09` § Accounts and devices).

A client verifies an attestation **before it stores it**, under its pinned instance key, its own
`instance_id` and the user id it was served for; it needs no pin for the user. One that does not
verify, or is malformed, is stored nowhere and never replaces a stored one that verified, and it is
treated exactly like a withheld one: the history read still proceeds, so a key change is still
recorded and nothing that waits on that read (the removal feed) stalls. Only a verified attestation
is stored (one per user) and only it sets the per-instance "attestation seen" flag. **In addition
to** the rules of § Credential, the client judges every leaf of that user against it:

- the stored attestation verifies and its `kind` equals the credential's `kind`: the leaf passes
  (whatever key the leaf is under);
- it verifies and `kind` differs: the leaf fails with `E_CREDENTIAL` (a member whose credential
  claims to be a Bot, or the reverse);
- no verifying attestation for that user: once the client has stored **any** attestation from its
  instance, the leaf stays pending (`E_DEVICE_LIST_STALE`, fail closed — whether or not the person
  verified the key); before that, the earlier rule applies (the credential's kind is not checked by
  the core).

A tombstoned account (`DELETE /v1/accounts/me`) is served no attestation (its wire-4 head is the
four elements). A client that verified the account's attestation before the deletion keeps it, so
the account's remaining leaves keep their verdict until the deletion's `Remove`s land; a client that
never did leaves them pending.

`key_id` names the external-sender key that signed, for diagnostics; nothing the signature covers
binds it, so it must not be trusted to select or rotate keys.

The flag is one-way: a client that has seen wire 4 from its instance never again accepts a leaf
without an attestation, whatever version a later session negotiates, and a store holding it cannot
be opened by an older client (app schema 13 is refused as newer). Until the first attestation is
stored — a client that has only ever negotiated wire 2 or 3 with this instance — the core keeps the
earlier rule; that downgrade window closes on the first wire-4 device-list read. The instance can
withhold an attestation (the leaf stays pending, fail closed) but not forge one without its key; it
can, with its key, attest any kind, so the attestation moves the check into every client's trust
decision with signed evidence, not out of the instance's hands (`08-threat-model.md`). Clients that
show a sender as a Bot still require the server's member row of account kind 1 as well
(`02` § Roles, credential-kind rule).

## Device list

The user's devices are published as a hash-chained, SSK-signed list. The DS stores and serves it;
it cannot forge it. Deterministic CBOR:

```
[
  v,           ; uint, = 1
  user_id,     ; bstr, 16
  version,     ; uint, monotonically increasing from 1
  prev_hash,   ; bstr, 32: SHA-256 of the previous list's full 6-element CBOR encoding;
               ;           32 zero bytes for version 1
  entries,     ; array of [device_id (bstr 16), dsk_pub (bstr 32), tier (uint), added_at (uint),
               ;           revoked_at (uint or null)]
  sig_ssk      ; bstr, 64: Ed25519.sign(SSK_priv, "dilla devices v1" || the deterministic CBOR
               ;           encoding of the 5-element array [v, user_id, version, prev_hash, entries])
]
```

Rules: a verifier keeps the newest signed `version` it has validated per user and refuses a
rollback, gap, invalid `prev_hash` or signature. The requested user and the `user_id` of every
signed list in the history must equal the user extracted from the current authenticated MLS leaf.
The list entry must bind that leaf's `device_id`, DSK and tier. A leaf whose `device_id` is absent
from the newest list is `E_DEVICE_UNLISTED`, one whose entry has `revoked_at` set is
`E_DEVICE_REVOKED`, and no message from either is rendered. Revocation is therefore cryptographic
and does not depend on the instance. A message held for the sender's list (`E_DEVICE_LIST_STALE`)
takes its leaf's failed verdict as its reason once the leaf is judged. It stays held, so a later
list that validates the leaf still releases it, except for `E_DEVICE_REVOKED`: a revoked device's
messages are final, their retained ciphertext is discarded, and a later list that names the
device unrevoked releases none of them (it validates only what the device sends after). A later
quarantine (including a pinned UMK's leaf that a verified changed UMK's list does not hold)
retracts every previously accepted display row attributable to that leaf in that group, refolds
dependent edits, reactions and replies, removes derived search rows, and
invalidates older timeline and search slices before display. Old quarantined rows stay neutral
after verification unless retained ciphertext is freshly authenticated against the validated leaf.

A verifier first reads the unpaged signed candidate head, then fetches ascending history after
its cached signed version (`GET /v1/users/{user_id}/device-list?after=n`, `09`), or from version 1
on first sight. It validates every consecutive signature and hash link, and requires the final
signed bytes and hash to equal the candidate head. The final list must verify under the leaf's
`ssk_pub` also when the history is only the cached head: a leaf under another key whose device a
cached list names, served that unchanged head, fails rule 2 (`E_CREDENTIAL`) and changes no cached
list or pin. It re-reads the unpaged head after paging;
a changed head allows one bounded retry. An unchanged exact cached head needs no history fetch.
The history is at most 1024 versions and 4 MiB. Only version 1 may carry a zero `prev_hash`.
An instance can withhold newer history even on first sight: trust on first use cannot detect this
freeze, and the UI must not call first sight verified.

## Custody by tier

| secret | native device | browser device |
|---|---|---|
| `UMK_priv` | never (only in the recovery header) | never, except transiently at a one-shot recovery-key entry (it signs the SSK binding of the new device's credential and is zeroised at once) |
| `SSK_priv` | yes | never, except during its own browser-rooted signup (pending signup record only), and transiently at a one-shot recovery-key entry, zeroised afterwards |
| `DSK_priv` | yes, in the OS keystore | yes, in the encrypted store (its key wrapped in IndexedDB) |
| `K_backup` (archive key) | yes | only if the user enables "history in browser sessions" (default off); and during its own browser-rooted signup (pending signup record only); and transiently at a one-shot recovery-key entry, zeroised afterwards |
| MLS group state | yes | yes (OPFS) |

A browser device can therefore decrypt and send in groups it belongs to, but cannot enrol other
devices or publish device lists, except during its own browser-rooted signup and at a one-shot
recovery-key entry, when it holds `SSK_priv` transiently and zeroises it. Peers show a `web` tag on
members whose message came from a `browser`-tier leaf.

## Pairing

Enrolling a new device is a two-leaf `pairing` group without external senders. The
`04-envelope-and-franking.md` envelope format is NOT used in pairing groups; the only application
message sent in one is the pairing payload defined below.

**Pairing QR payload.** The new device generates its DSK and encodes, as a QR code, Crockford
base32 of the deterministic CBOR `[v = 1, device_id (bstr 16), dsk_pub (bstr 32), umk_pub (bstr 32)]`
— `umk_pub` is the user's own pinned UMK, carried so the authorising device can confirm it. When a
camera is unavailable, the **fingerprint** shown for manual comparison is the first 12 Crockford
base32 characters of `SHA-256(dsk_pub)`.

**Provisional credential.** To open the pairing group, the new device presents the same 10-element
identity array as any credential ("Credential" above), with `signer_tier = 2` (provisional),
`ssk_pub` = 32 zero bytes, `sig_umk_ssk` = 64 zero bytes, `sig_ssk_dev` = 64 zero bytes, and
`umk_pub` = the UMK carried in the pairing QR payload. This credential is accepted only as the
second leaf of a `pairing` group whose `dilla_binding.target_id` equals the credential's
`device_id`; a DS session opened with a provisional credential may publish only one KeyPackage, for
that group, and may receive only that group's Welcome and handshakes. Any other use is rejected
(`E_PROVISIONAL_OUTSIDE_PAIRING`).

1. The new device generates its DSK, opens a DS session with its provisional credential, and shows
   the pairing QR payload (or the fingerprint).
2. The authorising device (which holds `SSK_priv`) creates the pairing group with
   `dilla_binding.kind = 2` and `target_id = new device_id`, and adds the new device's KeyPackage.
   It MUST refuse to continue unless the tree has exactly two leaves and the peer's leaf signature
   key hashes to the scanned fingerprint.
3. Both devices compute the **SAS** from the MLS `epoch_authenticator` of the epoch after the Add
   (below) and the user confirms the same digits appear on both screens.
4. The authorising device sends exactly one application message in the pairing group, the
   **pairing payload**: deterministic CBOR `[v = 1, credential (bstr: the SSK-signed identity CBOR
   of the new device), ssk_priv (bstr 32 or null), k_backup (bstr 32 or null), pins (array of
   [user_id, umk_pub, first_seen, verified] or null)]`. For a `browser` device, `ssk_priv` and
   `pins` are null, and `k_backup` is null too unless the user opted in to "history in browser
   sessions".
5. The authorising device publishes a new signed device list including the new device, then both
   devices delete the pairing group.

## Recovery

At signup the client generates a 256-bit **recovery key** `RK` and shows it once, in two forms the
user may write down: 24 BIP-39 English words (BIP-39 checksum, 256-bit entropy) or 52 Crockford
base32 characters in 13 groups of 4 (no checksum). 256 bits split into 5-bit Crockford groups is 52
characters, not 64 — an arithmetic slip in an earlier draft of the design spec, noted here so it is
not re-litigated. The client MUST require the user to acknowledge that the key was written down
before continuing; it MUST NOT offer a copy button on that screen.
The client shows `RK` before it registers the account and never stores it: the device performing
signup keeps only the root object sealed under `K_header` and, after registration, the state object
sealed under `K_backup`.

Derived keys: `K_header = HKDF-SHA256(salt = "", IKM = RK, info = "dilla header v1", L = 32)` and
`K_backup = HKDF-SHA256(salt = "", IKM = RK, info = "dilla archive v1", L = 32)`.

The **root object** (`06-backup-archive.md`, "Header") holds `UMK_priv` and `SSK_priv` under
`K_header`; it is written once, at signup, and never replaced (the instance stores it insert-only, `06`
"Header"); recovery reads it and writes nothing to it, because `UMK_priv` is never on a device
otherwise. The **state object** holds the device list and the UMK
pin table under `K_backup`; any native device rewrites it on every change. Recovering on a new
device with only `RK`: derive `K_header` and `K_backup`, fetch and decrypt the root object, enrol
the new device by signing its credential with the recovered `SSK_priv`, fetch and decrypt the state
object, publish a new device list revoking any device the user no longer holds, then restore
history from the archive under `K_backup` (`06-backup-archive.md`, "Restore"). A user who loses
every device **and** `RK` loses their history; the instance cannot help, and the onboarding copy
says so.

## Safety number

Two users compare a 60-digit safety number derived from both UMKs:

```
digits = decimal(SHA-256(min(umk_a, umk_b) || max(umk_a, umk_b)))   ; min/max by byte order
safety_number = the first 60 digits of `digits` left-padded with zeros to 78 digits
displayed as 12 groups of 5
```

The **SAS** for pairing (and for optional call verification) is derived from the MLS
`epoch_authenticator` (32 bytes): `sas = the first 30 digits of decimal(epoch_authenticator)
left-padded to 78 digits`, displayed as 6 groups of 5.

## Vectors

`vectors/identity.json`. An implementation conforms when it reproduces `safety_number.digits`,
`sas.digits`, `recovery_key.base32`, `recovery_key.k_header`, `recovery_key.k_backup` and
`credential_identity.cbor`, and when it verifies `credential_identity.fields.sig_umk_ssk` against
`umk_pub` over `"dilla ssk v1" || ssk_pub`, and `sig_ssk_dev` against `ssk_pub` over
`"dilla dsk v1" || device_id || dsk_pub || kind || tier || signer_tier`.

`account_kind` holds the account-kind attestation of § Account kind attestation for a user and a
bot (`cases`): an implementation conforms when it reproduces each case's `message` from
`instance_id`, `user_id` and `kind`, and its `attestation` bytes from `key_id` and the
deterministic Ed25519 signature under `instance_priv` (an Ed25519 seed, test material), and
verifies `signature` against `instance_pub`.

`credential_identity` also carries `umk_priv`, `ssk_priv`, `dsk_priv` and `dsk_pub`. Those are
Ed25519 seeds chosen so the file is reproducible from a fixed input; they are test material and
protect nothing. `dsk_pub` is published because it is covered by `sig_ssk_dev` but is not itself a
field of the credential array: a verifier takes it from the MLS leaf's `signature_key`.

## Error codes

`E_CREDENTIAL`, `E_UMK_CHANGED`, `E_DEVICE_UNLISTED`, `E_DEVICE_REVOKED`, `E_DEVICE_LIST_STALE`, `E_PAIRING_LEAVES`,
`E_PAIRING_FINGERPRINT`, `E_TIER_MISMATCH`, `E_PROVISIONAL_OUTSIDE_PAIRING`.
