# 02 — Delivery service

## Roles

The instance acts as the MLS Delivery Service (RFC 9750 §5) for every group it hosts:

1. **KeyPackage directory**: stores KeyPackages per device, serves one on request, consumes
   ordinary packages on use, never consumes the last-resort package. It stores a package only if
   its credential names the publishing session's `device_id` and `user_id` and its leaf's
   `signature_key` is the device's **registered key** (the `dsk_pub` it registered with
   `POST /v1/accounts` or `POST /v1/devices`, the key its sessions are established under); a
   package for another device is `403 E_FORBIDDEN`, any other mismatch `422 E_COMMIT_INVALID`
   (`rule = "key_package"`), and nothing of the request is stored. The credential's `kind` must
   fit the publishing account (the **credential-kind rule**, below).

   **Credential-kind rule.** A leaf credential's `kind` (`03-identity.md` § Credential) is signed by
   its own user's SSK, so it is the account's claim, not a fact. The DS binds it to the account
   (`users.kind`) wherever a new leaf reaches it: a published KeyPackage, a directory package
   before the DS spends it on its own `Add` (one that fails is deleted, as an unbound one is), every
   `Add` a commit applies (invariant 4), the leaf an external commit creates (invariant 5's joiner
   and a resync), the one leaf a group is registered with, every leaf of a reseeded tree and the
   joiner of a replayed resync (heal). A **human** account (`kind 0`) presents only a human-kind
   credential; a **Bot** account (`kind 1`) presents exactly the credential its installation
   verified (`10-community-bot.md`), which enrolment verified as Bot-kind. A credential whose kind
   does not decode, or that names no account of the instance, fails closed. The refusal is the
   entry point's own (`422 E_COMMIT_INVALID` with its rule: `key_package`, `add_key_package`,
   `external_joiner`; `400 E_INVALID_REQUEST` at registration). Clients still never trust a
   sender's credential `kind` alone: a sender is shown as a Bot only when the server's member row
   is of account kind 1 as well, and from wire 4 a client's core also judges every leaf's kind
   against the instance-signed account-kind attestation (`03` § Account kind attestation).
2. **Sequencer**: accepts exactly one Commit per epoch per group and totally orders handshake and
   application messages per group (`seq`, a per-group unsigned counter starting at 1).
3. **GroupInfo and tree store**: keeps the latest committer-signed GroupInfo (without the ratchet
   tree) and the ratchet tree it maintains itself from the handshake stream.
4. **External sender**: issues Add and Remove proposals bound to the permission system, and
   `GroupContextExtensions` proposals only for its own key rotation.
5. **Structural validator**: maintains an MLS `PublicGroup` per group (the public state: tree,
   epoch, extensions, leaf credentials) and validates every Proposal, Commit and GroupInfo
   structurally before accepting it. It never holds a group secret and cannot decrypt.

## Device sessions

Every endpoint in this document requires a **device session**. A device session is issued to a
`(user_id, device_id)` pair only against a live proof of possession of that device's `DSK_priv`.

1. `POST /v1/devices/{device_id}/sessions/challenge` — no authentication. The instance answers
   `201 [nonce(bstr 32), expires(uint)]`. The nonce is 32 CSPRNG bytes, single use, 60-second TTL,
   held in memory, deleted on read. The answer is identical for an unknown `device_id`.
2. `POST /v1/devices/{device_id}/sessions` — body
   `[nonce(bstr 32), purpose(uint), sig(bstr 64), registration(array)|credential(bstr)|null, login(bstr|null)]`, where

   ```
   sig = Ed25519(DSK_priv,
           "dilla session v1"    ; 16 bytes UTF-8
        || instance_id           ; 16 bytes
        || device_id             ; 16 bytes
        || nonce                 ; 32 bytes
        || purpose)              ; 1 byte: 0 session, 1 renew, 2 provisional
   ```

   The instance answers
   `201 [token(tstr), scope(uint), user_id(bstr 16), device_id(bstr 16), expires(uint), idle_expires(uint), generation(uint)]`.
   The establish body is at most 8192 bytes. `login`, when present, is an enrolment assertion
   (`09` § Auth ceremonies) sent as the token's UTF-8 bytes in a bstr. Element 3 must then be
   `[device_id(bstr 16), dsk_pub(bstr 32), tier(uint 0|1), signer_tier(uint 0|1), credential(bstr, 1 to 2048 bytes)]`;
   without `login`, element 3 is ignored whatever its type. For a device with no row, the array's
   `device_id` must match the path, both tiers must be `browser` (the `03` pairing ceremony is the
   native path), and `sig` must verify under its `dsk_pub`. The assertion is spent after these
   checks and before the list, cap and rate checks: a refusal after the spend needs a fresh login.
   Its user must be active and have published a device list. The new unverified row stores the
   opaque credential and receives a `pending` session. An unlisted row expires 24 hours after
   creation, revoking its pending sessions. A registration is never refused while the user has an
   unlisted live row: at 8 live devices, or past 3 live rows created in the last hour (a live first
   device included; revoked and expired rows do not count), it revokes the oldest unlisted live
   row, whatever its age, with its sessions and gateway connections, and is admitted. Only 8 live
   devices that the newest list all names refuse, with `403 E_FORBIDDEN`. At registration the
   instance cannot tell the owner from a holder of the password alone (the recovery key that tells
   them apart is used after registration), so a per-user refusal a password holder could keep
   saturated would lock the owner's recovery out; instead the owner's new row is exposed only
   between its registration and the list `PUT` that names it, and each replacement costs a host
   login, metered per source address by the auth ceremonies' `login` bucket, and an establish,
   metered per source address and per `device_id`. `POST /v1/devices` applies the same cap, rate
   and expiry but never evicts: past the cap it refuses `403 E_FORBIDDEN` and past the rate
   `429 E_RATE_LIMITED`, because its enrolled caller pays no host login and can free a slot itself,
   and an evicting route would let a stolen enrolled session replace the owner's new row at will.
   For the same reason the key-less `DELETE /v1/devices/{device_id}` refuses (`409`) an unlisted
   row younger than 10 minutes unless the caller's session is that device's own, and
   `DELETE /v1/devices/{device_id}/sessions` refuses (`409`) another device's sessions of such a
   row; both spend the device session's write bucket.
3. The **token** is 32 bytes from the platform CSPRNG, base64url without padding, stored only as
   `SHA-256(token)`. It is sent as `Authorization: Bearer <token>` on HTTP and in the gateway's
   `IDENTIFY` frame. There is no cookie and therefore no CSRF surface on `/v1`.
4. **Scope** is `0 enrolled`, `1 pending`, `2 provisional`. An establish with `login` is `pending`;
   purpose 2 is `provisional`, after the same list is read: an unlisted row past its 24 hours is
   revoked and answers `401 E_UNAUTHENTICATED` whatever the purpose, and a browser row that was
   never verified, under a user with a list (what assertion registration creates), is refused
   purpose 2 with `401`, since `03` pairing is the native path. Otherwise the newest list
   verified against `users.ssk_pub` decides:
   no list or an unrevoked entry naming the device's `device_id` and `dsk_pub` together gives
   `enrolled`; any other device, one whose key a list entry names under another `device_id`
   included, is unlisted and gets `pending`. Wherever the instance asks whether a device is listed
   (the scope, the 24-hour expiry, the cap's replacement, the key-less `DELETE /v1/devices/{id}`
   and invariant 4) it compares this pair. A list that fails verification, a stored list that
   cannot be decoded included, gives `401 E_UNAUTHENTICATED` (only an absent list is "no list"); a verifier or
   instance fault gives `503 E_UNAVAILABLE`, so a transient failure is retryable. Scope is fixed
   at mint: a pending device becomes enrolled by establishing again after a list names it.
   `enrolled` reaches every endpoint subject to ordinary ACL; `pending` reaches only its own two
   backup reads (`06-backup-archive.md`), its own device-list GET and PUT, and the session routes
   needing no session. Other routes answer `403 E_FORBIDDEN`, and the gateway's `IDENTIFY` with a
   pending or provisional session's token, sent in the frame, as a bearer or through a ticket, is
   refused like a token that does not resolve (`E_UNAUTHENTICATED`, close `4003`), as is a
   `RESUME` whose session token is one (`invalid_session`). `provisional` reaches only
   KeyPackage publication for one `pairing` group and that group's Welcome and handshakes;
   outside them the answer is `E_PROVISIONAL_OUTSIDE_PAIRING`.
5. **Lifetime** is a sliding 30 days for `native` devices and 7 days with a 12-hour idle window for
   `browser` devices, renewed by `purpose = 1`. At most **8** live sessions exist per device; the
   oldest is evicted.
6. **Revocation.** The instance verifies every list published with
   `PUT /v1/users/{user_id}/device-list` before storing it: `blob` is the six-element signed list of
   `03-identity.md` § Device list, whose `version`, `prev_hash` and `sig_ssk` must equal the body's
   `version`, `prev_hash` and `ssk_signature` (`400 E_INVALID_REQUEST`); its signature must verify
   under `users.ssk_pub` and it must name the publishing user (`400 E_INVALID_REQUEST`); its
   `version` must be the stored newest's plus one with `prev_hash = SHA-256` of the newest's blob,
   or `1` with 32 zero bytes when none is stored (`409 E_INVALID_REQUEST`). Accepting a list that
   revokes a device of the publishing user MUST mark the device revoked and delete its session rows
   in the transaction that stores the list, then close its gateway connections and propose its
   `Remove` in every group it holds a leaf in; the publishing device may revoke itself. Entries for
   another user's devices are ignored. Setting `users.disabled_at` does the same for every device of
   that user. Every instance process that
   serves the gateway also re-reads each ready connection's session row once per
   `gateway.heartbeat_interval`, and closes a connection whose session no longer resolves (deleted,
   pruned or past its expiry) with close `4004 session_revoked`, which is not resumable. A session
   deleted by a process that holds no handle on the gateway (the `dillad admin` command line) thus
   ends its live connections within one heartbeat interval, not instantly. A store that cannot
   answer is not a revocation: the connection stays and the next tick asks again.
7. The sole exception to the proof rule above is `POST /v1/accounts`, which creates the device and
   its first session in the same transaction: the device's key is the one being registered, so there
   is no prior key to prove possession of. Every later session for that device goes through the
   challenge. `POST /v1/devices` proves the key it registers the way item 2's registration does:
   its body carries a nonce from item 1's challenge for the new `device_id` and `sig` over item 2's
   preimage with purpose `0`, by the `dsk_pub` being registered (`403 E_FORBIDDEN` otherwise; the
   nonce is spent either way). Both device creation routes refuse a `dsk_pub` that a live device of
   the same user already holds (`409 E_INVALID_REQUEST`), so a stolen session can register no row
   under a key it does not hold, and no row shares another's listing.
   A `device_id` is unique across the instance: `POST /v1/accounts`, `POST /v1/devices`, and
   assertion registration refuse a device id already held by another user with
   `409 E_INVALID_REQUEST`, even if that row is no longer listed.
8. A session whose account owes a second factor (`09` § Second factors), whatever its scope,
   reaches only `09`'s `F` and `P` routes (each still within its own scope); every other route,
   the `E or V` ones included, answers `403 E_FACTOR_REQUIRED`, and the gateway refuses it as it
   refuses a `pending` session (close 4003).

Refusals: `401 E_UNAUTHENTICATED` for an absent, replayed or expired nonce, a bad signature, an
unknown device without a registration assertion, an unknown or spent assertion, a user with no
list on registration, or a list that fails verification; `400 E_INVALID_REQUEST` for a malformed
registration array or a tier other than `browser`; `403 E_FORBIDDEN` for a disabled account or a
cap of entirely listed devices; `429 E_RATE_LIMITED` per source address and per `device_id` (the
per-user enrolment rate replaces a row and never refuses); `503 E_UNAVAILABLE` for an instance
list-verifier fault.

## API

Every `/v1` request and response body is deterministic CBOR, a fixed-position array, served and
accepted as `Content-Type: application/cbor`; MLS objects are `bstr`. Path identifiers are 32
lowercase hex characters (`^[0-9a-f]{32}$`). All endpoints require a device session
(`§ Device sessions`).

| Method and path | Auth | Request | Response | Errors |
|---|---|---|---|---|
| `POST /v1/groups` | E | `[group_id(bstr16), binding(bstr), group_info(bstr), ratchet_tree(bstr)]` | `201 [group_id, next_seq]` | `E_BINDING_INVALID`, `E_MODE_READABLE`, `E_GROUP_EXISTS`, `E_FORBIDDEN`, `E_INVALID_REQUEST` |
| `GET /v1/groups/{id}/info` | E | — | `[epoch, group_info, tree_hash, next_seq]` | `E_NOT_FOUND` |
| `GET /v1/groups/{id}/tree` | E | — | `[epoch, ratchet_tree, tree_hash]` | `E_NOT_FOUND` |
| `GET /v1/groups/{id}/handshakes?from=&limit=` | E | — | `[[seq, epoch, kind, sender, blob]]` | `E_NOT_FOUND`, `E_PRUNED` |
| `POST /v1/groups/{id}/commit` | E | `[epoch, commit(bstr), group_info(bstr), welcomes([[device_id(bstr16), blob(bstr)]]), ratchet_tree(bstr\|null)]` | `[seq, epoch]` | `E_COMMIT_CONFLICT`, `E_COMMIT_REQUIRED`, `E_COMMIT_INVALID`, `E_FORBIDDEN`, `E_LEAF_NOT_CURRENT`, `E_INVALID_REQUEST`, `E_RATE_LIMITED` |
| `POST /v1/groups/{id}/proposal` | E | `[epoch, proposal(bstr)]` | `[seq]` | `E_COMMIT_INVALID`, `E_FORBIDDEN`, `E_INVALID_REQUEST`, `E_REMOVE_PENDING` |
| `POST /v1/groups/{id}/message` | E | `[epoch, private_message(bstr), class(uint)]` (§ Upload class) | `[seq, franking_tag(bstr32), recv_ts]` | `E_COMMIT_REQUIRED`, `E_LEAF_NOT_CURRENT`, `E_TOO_LARGE`, `E_COMMITMENT_INVALID`, `E_FORBIDDEN`, `E_RATE_LIMITED`, `E_INVALID_REQUEST`, `E_NOT_FOUND` |
| `POST /v1/groups/{id}/resync` | E | `[external_commit(bstr), group_info(bstr)]` | `[seq, epoch]` | `E_COMMIT_INVALID`, `E_FORBIDDEN`, `E_REMOVE_PENDING` (freeze-exempt, invariant 5) |
| `POST /v1/groups/{id}/fork-report` | E | `[epoch, seq, reason(tstr)]` | `202 []` | `E_NOT_FOUND` |
| `POST /v1/keypackages` | E or V | `[packages([bstr]), last_resort(bstr\|null)]` | `201 [count]` | `E_COMMIT_INVALID`, `E_TOO_LARGE` |
| `GET /v1/devices/{device_id}/keypackage` | E | — | `[blob, last_resort(uint), kp_ref(bstr)]` | `E_NOT_FOUND`, `E_RATE_LIMITED` |
| `GET /v1/groups/{id}/messages?from=&limit=` | E | — | `[[seq, epoch, uploader_device, blob\|null, commitment\|null, franking_tag, recv_ts, deleted(uint), class(uint\|null)]]` | `E_PRUNED` |
| `GET /v1/groups/{id}/heal` | E | — | `[epoch, next_seq, generation, need_from_seq]` | `E_NOT_FOUND` |
| `GET /v1/groups/{id}/heal-state` | E | — | `[epoch, next_seq, generation, need_from_seq, needs_heal(uint)]` | `E_NOT_FOUND`; `needs_heal` is 0 or 1 and reports the restored epoch-unknown marker to an enrolled current member |
| `POST /v1/groups/{id}/heal` | E | `[group_info(bstr), tail([[seq,epoch,kind,sender,blob]]), ratchet_tree(bstr\|null)]` | `[epoch, next_seq]` | `E_COMMIT_INVALID`, `E_FORBIDDEN`, `E_REMOVE_PENDING` |
| `GET /v1/welcomes?after=&limit=` | E or V | — | `[[welcome_id, group_id, epoch, commit_seq, blob, ratchet_tree, tree_hash]]` | — |
| `DELETE /v1/welcomes/{welcome_id}` | E or V | — | `204` | `E_NOT_FOUND` |
| `DELETE /v1/groups/{id}/messages/{seq}` | E | — | `204` | `E_NOT_FOUND`, `E_NOT_UPLOADER` |
| `POST /v1/groups/{id}/cursor` | E | `[last_seq, last_epoch]` | `204` | `E_NOT_FOUND` |
| `GET /v1/groups/{id}/proposals` | E | — | `[[ref, kind, target_leaf\|null, blob, void(uint)]]` | `E_NOT_FOUND` |

Two rules the table encodes: a Welcome is **fetched without being consumed** — the `DELETE` is what
marks it delivered, because `StagedWelcome::new_from_welcome` consumes the key material even when
the client then fails — and the Welcome response carries the ratchet tree **as of the welcoming
epoch**, because dilla Welcomes carry no tree and the live tree has moved on.

`DELETE /v1/groups/{id}/messages/{seq}` is member-only: a caller whose device is not a current member of the group is `404 E_NOT_FOUND`, the answer every member-only group route gives, so the route tells no device outside the group whether it exists. A member that is not the uploading user is then `403 E_NOT_UPLOADER`; any member device of the uploading user may delete. A `seq` the group holds no message at is `404 E_NOT_FOUND`. An accepted delete keeps the row's `seq`, `epoch`, `uploader_device`, `commitment`, `franking_tag` and `recv_ts`, drops its ciphertext and answers `204`. An accepted delete fans `message.deleted [seq, deleted_at]` (op 21) to every member device of the group, the deleting device included, with replay.

The `group_id`, `epoch` and `tree_hash` served with a Welcome (on `GET /v1/welcomes` and in the
`mls.welcome` frame) are the instance's own: the group the commit was uploaded to, the epoch the
instance's public group reached by merging that commit, and the hash of that public group's tree
after the merge. The committer supplies only `[device_id, blob]` per Welcome and cannot choose them.
A joining client verifies the Welcome's group context against all three before it joins
(`01-groups.md`, client join rule 5).

A commit's `welcomes` are addressed: each `device_id` must be a device the commit's own applied Add
proposals add, and a commit that addresses a Welcome to any other device is refused with
`E_COMMIT_INVALID` and `rule = "welcome_addressee"` — otherwise any committer could queue opaque
blobs to arbitrary devices, telling each the id and epoch of a group it was never added to. A commit
carries at most 256 Welcomes (`01`'s `MAX_ADDS`); more is `E_INVALID_REQUEST`. The instance answers
membership and the epoch from the path, the session and the head of the body before it reads the
rest, and a device has one commit upload in flight at a time (`E_RATE_LIMITED` for a second).

A `POST /v1/groups` that registers the `text` group of a channel with more eligible users than the
creator is followed by the instance's Add proposals for their devices (`01-groups.md`, "Joining":
creating a private channel), at most 256 per commit. The instance issues them only **after** the
`201` has been written: the registrant never waits for them, and no `mls.handshake` carrying one
of them, and no `mls.commit_needed` asking the creator to commit them, is sent before the answer
that tells the creator the group exists. The answer and the frames travel on different connections,
so a client can still read a frame for a group whose `201` it has not yet read; it holds such a
frame until the registration's answer arrives rather than discarding it.

`commitment` in the `GET /v1/groups/{id}/messages` items is the stored value of `C`, read by the DS
from `private_message.authenticated_data` at upload time; it is not a separate client-supplied
field.

`GET /v1/instance` and `GET /v1/instance/limits` are dillad's own surface, not the delivery
service's — `02` freezes with "DS API v1" at the end of W5 while the instance document must stay
editable — and are defined in `protocol/09-http-api.md`'s `## Instance` section instead.

## Gateway frames

Every frame is the deterministic-CBOR fixed-position array

```
frame = [ op, n, group_id, payload ]
  op       ; uint, the opcode below. The dotted names are documentation labels only.
  n        ; uint. S→C: the per-session replay sequence, starting at 1; 0 means the frame is
           ;   not replayable. C→S: the client's correlation id, echoed in ERROR; 0 = no reply.
  group_id ; bstr 16, or null when the frame is not group-scoped.
  payload  ; array, fixed positions, shape determined by op.
```

Two counters exist and are never conflated: the per-group delivery-service **`seq`**, which is
durable and appears inside payloads, and the per-session replay **`n`**, which lives only in memory
and only in element 1.

### Connecting

A client opens the gateway with an HTTP/1.1 WebSocket upgrade, `GET /gateway`, on the instance's
own origin, and offers the subprotocol `dilla.v1`; the instance echoes `dilla.v1` and never any
other value. Messages are binary and carry one frame each; compression is not negotiated.

The connection's session is, in this order: the `session_token` of `identify` when it is not
empty; else the upgrade's `Authorization: Bearer <token>`; else a **gateway ticket**. A client that
cannot set request headers (a browser's `WebSocket`) mints a ticket with `POST /v1/gateway/ticket`
(`09-http-api.md` § Sessions endpoints: single use, valid 30 seconds), offers it as a second
subprotocol beside the first — `new WebSocket(url, ["dilla.v1", "dilla.ticket.<ticket>"])` — and
sends `identify` with an empty `session_token`. The upgrade spends the ticket whether or not the
connection then identifies, and the ticket is never echoed, so the credential does not appear in
the response.

An upgrade whose `Origin` header names a host other than the request's `Host` (compared with the
port, case-insensitively) and matches none of the instance's `http.trusted_origins` (default
`dilla://app`) is refused with `403`; a request without `Origin` is not refused on this rule. A
client served by the instance from its own origin therefore needs no configuration.

### Control (0–15) — `group_id = null`, `n = 0`

Two exceptions to the heading, and only two. `ready` (3) and `resumed` (4) are replayable and
carry `n ≠ 0`: a resumed connection whose `resumed` frame was dropped has no record of the replay
window it was given, and `ready`'s per-group digest is what a client with a refused resume heals
from. `commit_ack` (12) is group-scoped and carries a `group_id`; it is the one client opcode that
does.

| op | label | dir | payload |
|---|---|---|---|
| 0 | `hello` | S→C | `[wire_versions([uint]), e2ee_versions([uint]), media_versions([uint]), heartbeat_ms(uint), max_frame_bytes(uint), instance_id(bstr 16), generation(uint), backoff_ms(uint), backoff_jitter_ms(uint)]` — the last two are invariant 7's back-off window: a device that is not sent `mls.commit_needed` waits `backoff_ms + random(0..backoff_jitter_ms)` before volunteering a commit. `wire_versions` is `[3,2]`: wire 3 adds the community bot capability in `10`, while wire 2 remains supported outside bot-enabled communities. Both versions require every upload to name its class (§ Upload class), and `GET /v1/instance` advertises the same list (`09` § Instance) |
| 1 | `identify` | C→S | `[session_token(tstr), wire_version(uint), e2ee_version(uint), media_version(uint), caps(uint)]` — each version is one the `hello` offered; any other, wire version 1 included, is an `error` naming `E_VERSION` and close 4006 (`version_unsupported`) |
| 2 | `resume` | C→S | `[session_token(tstr), resume_token(bstr 32), generation(uint), last_n(uint)]` |
| 3 | `ready` | S→C | `[device_id(bstr 16), user_id(bstr 16), generation(uint), resume_token(bstr 32), wire_version(uint), e2ee_version(uint), media_version(uint), keypackages_remaining(uint), groups([[group_id(bstr 16), epoch(uint), last_seq(uint), proposals_outstanding(uint)]])]` — `epoch` and `last_seq` are the device's own acknowledged cursor for the group (`POST …/cursor`; `0, 0` before the first), not the group head |
| 4 | `resumed` | S→C | `[replayed_from(uint), replayed_to(uint), resume_token(bstr 32)]` — the token rotates on every `resumed`, so the frame that announces a resume also issues the credential for the next one; a client that keeps the token from `ready` after resuming is refused |
| 5 | `invalid_session` | S→C | `[resumable(uint), reason(tstr)]` |
| 6 | `heartbeat` | C→S | `[last_n(uint), active(uint)]` |
| 7 | `heartbeat_ack` | S→C | `[server_ts(uint)]` |
| 8 | `reconnect` | S→C | `[reason(tstr), after_ms(uint)]` |
| 9 | `error` | S→C | `[cid(uint), code(tstr), detail(tstr)]` |
| 10 | `subscribe` | C→S | `[group_ids([bstr 16])]` |
| 11 | `unsubscribe` | C→S | `[group_ids([bstr 16])]` |
| 12 | `commit_ack` | C→S | `[round(uint)]`, `group_id` set — the acknowledgement invariant 7 counts lost rounds against |

On a successful resume, the server sends the new `resumed` frame before the
replayed frames. If the requested durable acknowledgement is `last_n`, its range
is `replayed_from = last_n + 1` through `replayed_to`, inclusive, and the new
`resumed` has `n = replayed_to + 1`. An empty replay range has
`replayed_from = replayed_to + 1`. The client treats this initial `resumed` as a
barrier: it records the rotated token but does not acknowledge that frame's `n`
until every preceding replay entry has been durably handled. Replay entries
arrive in increasing, contiguous order within the announced range; live entries
start after the barrier. A missing entry or inconsistent range requires fresh
identification and authoritative catch-up, not an acknowledgement across the gap.
An older `ready` or `resumed` encountered inside the replay range is a historical
control entry: it does not replace the newly rotated token or begin another replay
window. This ordering also permits a connection interrupted during replay to use
the new token with its last durable acknowledgement.

### Delivery service (16–31) — `group_id` set

| op | label | `n` | payload |
|---|---|---|---|
| 16 | `mls.handshake` | ≠ 0 | `[seq(uint), epoch(uint), kind(uint), sender(uint\|null), blob(bstr)]` |
| 17 | `mls.commit_needed` | 0 | `[epoch(uint), proposal_refs([bstr]), deadline_ms(uint), round(uint)]` |
| 18 | `mls.epoch_changed` | ≠ 0 | `[epoch(uint), seq(uint)]` |
| 19 | `message.ct` | ≠ 0 | `[seq(uint), epoch(uint), uploader_device(bstr 16), blob(bstr), franking_tag(bstr 32), recv_ts(uint), class(uint\|null)]` |
| 20 | `mls.welcome` | ≠ 0 | `[welcome_id(uint), epoch(uint), commit_seq(uint), blob(bstr), ratchet_tree(bstr), tree_hash(bstr 32)]` |
| 21 | `message.deleted` | ≠ 0 | `[seq(uint), deleted_at(uint)]` |

`mls.commit_needed` carries `deadline_ms` **relative to the moment the frame is handed to the
connection's writer**; the instance re-bases it at that moment and the client starts its timer on
receipt. It is `n = 0` and therefore never replayed: an election that was decided while the client
was away is stale by definition, and the instance re-elects.

`message.ct` is fanned out to **every** member device including the uploader's, so the per-group
`seq` stream is dense on every device.

`mls.welcome` is a convenience, not the delivery: every Welcome is durably queued and served by
`GET /v1/welcomes`. The instance sends the frame only when the Welcome blob and the welcoming
epoch's ratchet tree fit the `max_frame_bytes` it advertised in `hello` — a frame above it would
close the joiner's connection rather than reach it, and a real group's tree (620 KiB at 1,500
leaves) routinely does not fit. When no `mls.welcome` arrives, a device that expects to be added —
it published KeyPackages, or it is completing a pairing — polls `GET /v1/welcomes`, whose items carry
the same tree.

### Non-E2EE and interactions (32–47) — `n ≠ 0`

| op | label | payload |
|---|---|---|
| 32 | `message.plain` | `[channel_id(bstr 16), seq(uint), sender(bstr 16), envelope(bstr), franking_tag(bstr 32), edited(uint), deleted(uint)]` |
| 33 | `interaction` | `[interaction_id(bstr 16), bot_user_id(bstr 16), kind(uint), blob(bstr)]` |

`message.plain` carries a server-readable channel's post, edit or delete (`09` § Readable
channels). It is fanned out to every live connection of every user who may view the channel and
is still a member of its community, the author's own included, so the channel's `seq` stream is
dense on every device of its audience; `group_id` is null.

### Ephemeral (48–63) — `n = 0`, never replayed, never persisted

| op | label | dir | payload |
|---|---|---|---|
| 48 | `presence` | S→C | `[user_id(bstr 16), status(uint), since_ts(uint), status_msg(tstr)]` |
| 49 | `typing` | S→C | `[channel_id(bstr 16), user_id(bstr 16), device_id(bstr 16), typing(uint 0\|1), expires_ts(uint)]` |
| 50 | `voice_state` | S→C | `[user_id(bstr 16), device_id(bstr 16), call_id(bstr 16), flags(uint)]` |
| 51 | `typing_set` | C→S | `[channel_id(bstr 16), typing(uint 0\|1)]` |
| 52 | `voice_self` | C→S | `[call_id(bstr 16), flags(uint 0..3)]` |
| 53 | `call_ring` | S→C | `[call_id(bstr 16), channel_id(bstr 16), user_id(bstr 16), state(uint 0..2), expires_ts(uint)]` |
| 54 | `community_changed` | S→C | `[community_id(bstr 16)]` |
| 55 | `afk_away` | S→C | `[community_id(bstr 16), user_id(bstr 16), since_ts(uint)]` |
| 56 | `app_build` | S→C, wire ≥5 | `[entry(tstr)]` |
| 57 | `presence_activity` | C→S, wire ≥5 | `[]` |
| 58 | `call_stats` | C→S, wire ≥5 | `[call_id(bstr 16), report(array)]` |

`app_build` follows ready/resumed and names the entry from the verified, currently served web
handler. It is at most 512 characters, matches `/assets/[A-Za-z0-9_./-]+.js`, and contains no `..`.
It is a hint: the page verifies `/app-version.json` once, deduplicates notices and in-flight
checks, and defers automatic reload throughout a live or recovering call. The existing draft/file
checkpoint precedes any automatic update. No staged file or unverified directory is advertised.
The current web app never polls versions periodically, including before login and on older or
disconnected sockets. Startup, foreground, online and reconnect still reconcile. An unresolved
new hint retries with bounded exponential backoff; successful verification stops retries.
Malformed hints are ignored. Frames
56–58 never consume replay numbers or appear in the replay ring.

`presence_activity` contains no client identity, input details or timestamp. It is accepted only
on a ready wire-5 human enrolled session, revalidated from the connection's stored token hash,
with no factor debt or revocation. It shares one allowance per device per five seconds with the
HTTP activity route, across sockets and reconnects. Duplicate input is silently coalesced. The
allowance cache admits at most 8192 devices in a five-second window and refuses excess work
without refreshing presence. No activity is queued for reconnect. Heartbeat's legacy `active`
field is ignored for presence; current clients send zero. Wire 5 starts Idle until real input;
legacy connections retain their initial Online behavior. Ten minutes without accepted input
means Idle; DND and invisible choices retain precedence. AFK voice admission/speech/activity
and away-roster rules are unchanged.

`call_stats` carries the existing six-element report from `09` § Call stats, with payload at most
1 KiB. The current connection supplies identity and session authority. Processing uses eight
workers and at most 128 device slots globally (running slots included), one running and one
replaceable latest sample per device. Malformed/oversized frames are rejected before admission;
coalesced, congested, expired (older than ten seconds) or cancelled samples are discarded. A
worker has a two-second processing budget. Disconnect cancels queued/running work; each queued
sample revalidates the current bearer session and the shared acceptance service rechecks the
live call, current leaf, view/connect permissions, barred-device state and protocol authority.
Only accepted reports spend the same atomic five-second device allowance used by HTTP. A
reserved device cannot run another acceptance concurrently. No sample is persisted or replayed.
Clients send at most one current sample every ten seconds, drop when WebSocket bufferedAmount
exceeds 64 KiB, and drop the sample if the ready wire-5 path is unavailable. The current web
app has no HTTP fallback for activity or coarse stats; an ambiguous send is never repeated. The page keeps only one pending stats read or
worker request, and its local UI measurements retain their independent cadence.

Presence status values are `0` Offline, `1` Online, `2` Idle and `3` Do Not Disturb.
Invisible is an account choice, exposed to other members as Offline. The frame is sent only to
current members of a shared community and to the account's own devices. A fresh connection reads
the authenticated community presence snapshot; ephemeral frames are never replayed.

All ephemeral frames have `n=0` and `group_id=null`; a client may put a `cid` only in the
frame header, never in the payload. The server accepts `typing_set` only from a member currently
allowed to view that channel, with at most one update per `(device,channel)` every 3 seconds and
at most 64 active typing channels per device. It holds typing for 10 seconds, advertises the
server expiry as `expires_ts`, and checks the channel's current audience both at dispatch and
fan-out. It never persists or replays typing. Unknown, hidden or no-longer-viewable channels
return `E_NOT_FOUND`; malformed ids, element counts and flags return `E_FRAME_SHAPE`; throttling
returns `E_RATE_LIMITED`. A malicious member's invalid typing is dropped for that sender and
does not disconnect an honest recipient. `voice_self` requires membership in the live call and
its channel's current audience; otherwise it returns `E_NOT_FOUND` without revealing existence.
Its flag bit 0 is self-mute and bit 1 is self-deaf; the server maps them to bits 1 and 2 of
server-sourced `voice_state`, combined with the room state. Membership in the live call means
the instance's SFU lists the device in the call's room, which can be later than the client's own
join; a `voice_self` sent before then is refused and not kept. A client therefore keeps its last
`voice_self` per call and sends it again while its own `voice_state` for that call lacks those
self bits, at most 3 times per join, and sends one made while offline once after it reconnects.

`voice_state` is produced by the instance alone, from what its SFU reports about a call's room
(`09` § Voice), and goes to every device of every user who may view the call's channel — for a DM
or group DM, its participants. `call_id` is the call's id. `flags` is a bit set:

| bit | name | set while |
|---|---|---|
| 0 | `in_call` | the device is in the call's room |
| 1 | `self_mute` | the device's own mute state from `voice_self` |
| 2 | `self_deaf` | the device's own deafen state from `voice_self` |
| 3 | `video` | the device publishes a camera track |
| 4 | `screen` | the device publishes a screen-share track |

`flags` 0 means the device has left the call; when a call ends, every device still in it is sent
one. Every other bit is reserved and 0. The SFU's reports can arrive late or not at all, so
`voice_state` is advisory: it never decides who may join or publish.

A client that shows who is in voice reads the snapshot first (`GET /v1/channels/{id}/call` or
`GET /v1/communities/{id}/calls`, `09` § Voice) and then applies, in arrival order, the
`voice_state` frames it received while the read was in flight. A frame for a device the snapshot
does not list adds it; a frame with `flags` 0 removes it. Ops 50 and 52 are otherwise unchanged.

`call_ring` is how a DM or group-DM call rings the other participants (`09` § Voice, "Ringing").
The instance alone sends it; it is never persisted or replayed, and `group_id` is null and `n` 0.
`state` is:

| state | meaning | `user_id` |
|---|---|---|
| 1 | ringing | the caller |
| 0 | ring over: answered by one of the recipient's devices, declined by the recipient, the call ended, or 30 seconds passed | the caller |
| 2 | declined: sent to every device of the caller | the participant who declined |

`expires_ts` is the unix second at which the ring ends: `now + 30` when the ring starts, repeated
unchanged in that ring's state 0 and state 2 frames. It is a second on the instance's clock and
the ring's identity (the decline echoes it); a client never compares it with its own clock, and
times a ring's 30 seconds from the arrival of its state 1, which the instance sends once, as the
ring starts. A start that **opens** a call (`201`) on a DM
or group DM rings every other participant who has no device in the call, sending state 1 to each
of that user's online devices; a `200` start of a live call rings no one, and there is at most one
ring per (call, user). A user's ring stops (state 0 to every device of that user) at the first of:
a device of that user is admitted to the call's room, the call ends, 30 seconds pass on the
instance clock, or the user declines (`POST /v1/calls/{call_id}/decline`). A caller whose device
leaves an otherwise empty room ends the call, which stops every ring.

The instance rings a given (caller user, recipient user) pair at most once per 15 seconds and at
most 20 times per rolling hour, across all channels. A start beyond the bound still opens the call
but sends that recipient no `call_ring`. The bound is keyed by the pair only, never by the
recipient alone, so no third party can spend a recipient's budget and silence honest callers.

A client's decoder refuses a malformed `call_ring`, or one whose `state` is above 2, as
`E_FRAME_SHAPE` and drops it without disconnecting. Sent by a client, any op 53 is `E_FRAME_TYPE`
at the instance, as every S→C opcode is.

`community_changed` tells the members of a community that its structure changed. The instance
sends it, after the change commits, to every live connection of every member of the community
when a channel of it is created, updated or deleted (`09` § Channels) and when the community is
patched (`09` § Communities). It is also sent after a member joins, leaves, is kicked or is
banned; after a role is created, changed, deleted, granted or revoked; after a channel overwrite is
put or deleted; and after a community is deleted (to its members at that moment). The user a
leave, kick or ban removed is sent it once, after the commit, to every live connection; a ban of a
non-member sends nothing to that user. No one else is sent it. It names the
community and nothing else: a client that lists the community reads its channels and members
again through the routes that check its permissions, and may wait a moment so that a burst of
changes costs one read. It is never persisted or replayed, so a client also reads the lists on
`ready`. A client's decoder refuses a `community_changed` of any other shape as `E_FRAME_SHAPE`
and drops it without disconnecting; it ignores one for a community it does not list. Sent by a
client, any op 54 is `E_FRAME_TYPE` at the instance.

`afk_away` keeps a community's away roster current (`09` § Voice, the AFK channel). A member who
is away is listed under the community's AFK channel and has no voice connection: no device of that
user is in any call. `since_ts` is the unix second the member became away in that community, or `0`
when the member is no longer away there. The instance sends it, after the stored roster changed, to
every live connection of every user who may view the community's AFK channel, and only when a row
was actually set or cleared: marking a member who is already away, or clearing one who is not,
sends nothing. It is never persisted or replayed, so a client reads the roster from
`GET /v1/communities/{id}/afk` (its fifth element) on `ready` and whenever it reads the community
again. The instance may serve that read before a change it announces while the read is in flight,
so a client keeps each `afk_away` it receives during the read and applies them, in the order
received, to the roster of the NEWEST read once that read lands (a set no newer than the `since`
held for that member is stale and ignored). When no read is in flight and the client already holds
a roster, a frame applies straight to that roster. With no read in flight and no roster yet, it
drops the frame, since its next read is served later. A client's decoder refuses an `afk_away` of
any other shape as `E_FRAME_SHAPE` and drops it without disconnecting; it ignores one for a
community it does not list. A client that predates op 55 ignores it as an unknown
server-to-client opcode in this range. Sent by a client, any op 55 is `E_FRAME_TYPE` at the
instance.

Opcodes 64–127 are reserved for future `wire_version`s; 128 and above are never used. An opcode
outside the negotiated `wire_version` is a hard error (`E_FRAME_TYPE`), never ignored. A frame of
the wrong element count for its opcode is `E_FRAME_SHAPE`; a frame that fails the deterministic
decoder is `E_FRAME_CBOR`; a frame over `max_frame_bytes` is `E_FRAME_LIMIT`. A client frame whose
opcode is not group-scoped but that carries a non-null `group_id` is `E_FRAME_SHAPE`, and so is a
group-scoped client frame whose `group_id` is null — the second rule is what stops `commit_ack`
being silently dropped rather than refused.

### Close codes (RFC 6455 private range)

| code | name | resumable |
|---|---|---|
| 4000 | `unknown_error` | yes |
| 4001 | `not_identified` | no |
| 4002 | `decode_error` | no |
| 4003 | `unauthenticated` | no |
| 4004 | `session_revoked` | no |
| 4006 | `version_unsupported` | no |
| 4007 | `invalid_n` | no |
| 4008 | `rate_limited` | yes |
| 4009 | `session_timeout` | no |
| 4010 | `going_away` | yes |
| 4100 | `client_hello_timeout` | — |
| 4101 | `client_heartbeat_timeout` | — |
| 4102 | `client_undecodable` | — |
| 4109 | `client_too_large` | — |

Codes 4100–4199 are reserved for closes initiated by a client. An instance never sends one and
attaches no meaning to one it receives: a browser cannot close a socket with 1002 or 1009
(`WebSocket.close` accepts only 1000 and 3000–4999), so a client that gives up on a connection
names its reason here. The instance's own codes stay in 4000–4099.

A structured failure is sent as an `error` frame **before** the close, because the close reason is
capped at 123 bytes.

### Handshake records

`kind` is a uint with exactly three values: **0 proposal, 1 commit, 2 external_commit**. A Welcome
is not a handshake — it has its own wire format, its own frame and its own endpoints.

`sender` is the **leaf index** (uint) of the sending member, or **null** when the sender is the
instance's external sender. An instance additionally records the sending `device_id` beside the
handshake, resolved through its own member table; that record is not on the wire.

### Upload class

An application message upload names its **upload class**, the third element of
`POST /v1/groups/{id}/message`: **0** for a post (envelope type 0) and **1** for anything else
(types 1–6: an edit, a delete, a reaction or a pin, which receivers fold). The instance cannot
read the envelope, so the class is what lets it enforce a channel's posting rules on end-to-end
encrypted messages without throttling the folds. It stores the class with the message and relays
it as the 9th element of a `GET /v1/groups/{id}/messages` row and the 7th of `message.ct`, `null`
for a row a two-element legacy upload stored before wire version 2. A class other than the uint 0
or 1 (a null third element included), or a body of any length but 3, is `400 E_INVALID_REQUEST`:
the two-element body is refused, and so is every other length. A client reads a row of eight
elements and a `message.ct` of six, from an instance before the class, as carrying a null class; a
class other than 0, 1 or null is a shape error (`E_FRAME_SHAPE` for the frame).

The host's two rules apply to text groups of community channels only; direct messages, group DMs
and call groups are unchecked. They are decided under the group's lock, after the current-leaf
check and before the message is stored:

- A class-0 upload needs `send_messages` in the channel (`09` § Permissions), else
  `403 E_FORBIDDEN`.
- A class-0 upload within the channel's `slowmode_seconds` of the user's last class-0 upload into
  any text group of the channel, from any of their devices, is `429 E_RATE_LIMITED` with
  `retry_after_ms` the time left, unless the user holds `bypass_slowmode`, which exempts slow mode
  only, never the `send_messages` rule above. The instance keeps the
  time of that last post as a durable mark per channel and user, raised by every accepted class-0
  upload and by nothing else, so a deleted message still counts, and so does one retention has
  since removed.

A class-1 upload is never checked for `send_messages` nor slowed. Whatever the class, an upload
into the text group of a community channel that has been deleted is `403 E_FORBIDDEN`, and an
upload into a closed group of any kind (a deleted channel's, or one a mode change or a restore
replaced) is `404 E_NOT_FOUND`, as for a group that does not exist, before the freeze and the
current-leaf check.

Receivers drop a row whose class disagrees with its decrypted type (`04` § Semantics).

## Invariants

Each invariant has a chaos scenario in `dilla-testkit` named after it.

1. **Registration.** A group is registered with its `dilla_binding`. The DS refuses a `text` group
   for a channel whose visibility is `invite` or `discoverable`, or whose mode is `readable`
   (`403 E_MODE_READABLE`). `call` groups exist for every voice session regardless of the channel's
   text mode. Registration is also gated on the registering session's user: a `text` or `call`
   group bound to a community channel needs that channel to be of the binding's `community_id` and
   of a kind that carries the group (a `text` group on a text channel, a `call` group on a voice
   channel) and the user to hold, in that channel (`09` § Permissions, overwrites applied),
   `view_channel` for a `text` group and `view_channel` and `connect` for a `call` group; a
   community `text` or `call` group whose target is not a live channel of that community is
   refused (`target_id` is the channel id for both kinds, `01-groups.md` § dilla_binding), and so
   is every Add to, and join of, such a group once its channel is deleted: permissions held
   community-wide never stand in for the channel's overwrites. A `text` or `call` group with no
   `community_id` (a DM or group DM) must name a live DM or group DM as its `target_id` and needs
   the user to be one of its participants (`09` § DMs). A binding that names no such target is
   `400 E_BINDING_INVALID`; a user who may not register it is `403 E_FORBIDDEN`. A channel or DM
   carries one `text` group and one `call` group (`01-groups.md` § Group kinds): a registration
   whose target already has an open group of the same kind is `409 E_GROUP_EXISTS`, decided under
   a lock on the target so two first registrations cannot both pass, with one exception, invariant
   11's re-creation: while every open group of the target is epoch-unknown, the channel owner's
   device (the community owner; any participant of a DM) may register its replacement. `pairing`
   and `interaction` groups are not channel groups and are not gated here. A registration adopts
   exactly one leaf, the registering device's own: the uploaded tree holds one leaf (counted in
   the tree, a leaf whose credential is no dilla identity included), whose credential names the
   session's device and user, whose `signature_key` is that device's registered key, and whose
   device is not revoked and is in its user's newest signed device list — the checks an external
   joiner's leaf passes, the ACL half being the registration gate above. Anything else is
   `400 E_INVALID_REQUEST` and nothing is stored. Every other member enters through a commit
   (invariant 4) or an external join (invariant 5). The group context's `external_senders`
   extension must be the one `01-groups.md` § External senders gives the group's kind, because the
   instance governs a group only through it (every external `Add` and `Remove` is verified against
   it): a `text` or `call` group carries exactly one entry, whose `signature_key` is this instance's
   current external-sender key (`external_sender_pub` of `GET /v1/instance`) and whose credential is
   `basic` with the identity `[1, "instance", instance_id]` of this instance; a `pairing` or
   `interaction` group carries none. Anything else is `400 E_BINDING_INVALID` and nothing is
   stored. Heal's reseed (invariant 11) adopts an uploaded tree under the same rule.
2. **Tree service.** The DS keeps a `PublicGroup` per group. Committers upload a GroupInfo
   **without** the ratchet tree; the DS serves the tree from its own `PublicGroup`, and a joiner
   MUST verify `tree_hash` in the GroupInfo against the served tree before joining.
3. **One commit per epoch.** The first valid Commit for epoch `n` wins; a later one for the same
   epoch gets `409 E_COMMIT_CONFLICT` with the winning commit and the current outstanding proposals.
4. **Commit validity.** A Commit is accepted only if: it is signed by a current leaf — the leaf of
   the device uploading it (`403 E_FORBIDDEN` otherwise) — or is a valid external commit; it
   references every outstanding non-void DS proposal (invariant 6), except that an outstanding
   non-void DS `Remove` of leaf `L` is also satisfied when the commit applies any other `Remove`
   of `L` — a member's own `Remove`, which the clause on member-originated `Remove`s below measures
   against its proposer — while `L` still holds the device the DS `Remove` recorded (OpenMLS
   commits only the later of two `Remove`s of one leaf in the committer's queue, so a committer
   holding the DS's first commits the member's; the device removed is the same). The DS deletes a
   `Remove` satisfied this way with the commit, as it does a referenced one, and never re-issues it;
   it contains no `Update` from the committer; its UpdatePath leaf node, if a member commit carries
   one, and every `Update` it applies carry the credential that leaf holds before the commit, byte
   for byte, and that leaf's `signature_key`; an external commit by a device that already holds a
   leaf (a resync) removes that leaf — a device holds at most one leaf, read from the credentials
   of the tree before the commit — and carries the removed leaf's credential, byte for byte
   (`01-groups.md`, "Client policy for proposals from members"; a member `Update` proposal
   that changes either is refused on `POST /v1/groups/{id}/proposal` the same way, and neither is
   stored or fanned out; these refusals are `422 E_COMMIT_INVALID` with `rule = "structural"`,
   because the `PublicGroup`'s own processing refuses the message, except on an external commit,
   where the rule is the joiner's, `rule = "external_joiner"`); it applies no
   `GroupContextExtensions` proposal, by value or by reference, from any sender: a group's context
   extensions never change after creation (`01-groups.md`, "Client policy for proposals from
   members"), so its `external_senders` stays the one invariant 1 checked at registration (a
   `422 E_COMMIT_INVALID` with `rule = "structural"`, refused by the `PublicGroup`'s own processing;
   a member's standalone `GroupContextExtensions` proposal is refused on
   `POST /v1/groups/{id}/proposal` the same way, and a heal's replayed tail that carries one is
   refused with `rule = "tail"`, invariant 11); every member-originated
   `Remove` targets its proposer's
   own user (the committer's for a `Remove` the commit carries, the proposing member's for a member
   `Remove` proposal it references — how a member leaves, `01-groups.md`), the proposer being the
   sender the `PublicGroup` authenticated; every `Add` carries a leaf whose `signature_key` is the
   registered key of the device its credential names, and a
   credential whose user is eligible under the channel's
   ACL (for a community group, the same permission invariant 1 asks of a registrant, resolved
   through `09` § Permissions; for a DM or group DM, being one of its participants; for any other
   group, being in it already) and whose device the
   newest signed device list the DS holds names, by its `device_id` and DSK in one unrevoked
   entry (§ Device sessions item 4); the `PublicGroup` validates
   it structurally; and the uploaded GroupInfo's epoch is `n + 1`, and its `group_id` and
   `tree_hash` are those of the group the commit merges to (`rule = "group_info"`), since that
   GroupInfo is what every device that resyncs or joins builds its external commit from until the
   next commit. Otherwise `422 E_COMMIT_INVALID`.
5. **Freeze.** While any DS proposal is outstanding for a group, application messages get
   `425 E_COMMIT_REQUIRED`, and external commits get `425 E_COMMIT_REQUIRED` too — **unless no member
   device is online**, in which case the external commit is accepted, the outstanding proposals are
   re-issued for the new epoch, and `mls.commit_needed` goes to the joiner. After an external
   commit that omitted DS proposals (this exception, or the resync below), the non-void ones are
   re-issued for the new epoch and the freeze stays; a DS `Remove` that a member commit satisfied
   under invariant 4 without referencing it is deleted with that commit and never re-issued. `POST /v1/groups/{id}/resync` is exempt from this freeze: an own-leaf external
   commit is how a device that has fallen out of the epoch returns, and making it wait on a
   membership commit deadlocks exactly the device that cannot act. Two guards apply: the resync is
   refused with `409 E_REMOVE_PENDING` when the resyncing device is the target of an outstanding non-void
   instance Remove, and the instance re-issues its outstanding proposals for the new epoch
   immediately afterwards. A resync's new leaf, like a joiner's, must name the uploading device and
   user and carry that device's registered key as its `signature_key`, and the resync must remove
   the leaf the device holds (invariant 4: one device, one leaf) (`422 E_COMMIT_INVALID`,
   `rule = "external_joiner"`). `POST /v1/groups/{id}/resync` from a device that holds **no** leaf in the
   group is not a resync but a join (`01-groups.md`, "Joining"): it is gated by the channel ACL
   (`E_NOT_FOUND` to a device the ACL does not admit, as every read answers it), it is held by the
   freeze above, and the joiner's leaf must be its own device with its DSK in its user's newest
   signed device list (invariant 4's Add clause). A device the ACL admits may read
   `GET /v1/groups/{id}/info` and `/tree` to build that external commit.
6. **Void.** Before proposing, the DS validates a KeyPackage (lifetime not expired, capabilities
   include `0xF001`, not consumed, and bound to its device as the KeyPackage directory binds an
   upload: its credential names the device and the device's user and its leaf `signature_key` is
   the device's registered key — a directory package that is not, one stored before that binding,
   is deleted from the directory unspent and the device's next package is taken, and a device left
   with none gets the same refusal as a device whose directory is empty) and a Remove target (leaf
   still present). A DS proposal older
   than its TTL — 30 seconds in `call` groups, 24 hours in `text` groups — is marked **void**; a
   Commit MAY omit void proposals. In `call` groups the DS sweeps the call groups that have DS
   proposals outstanding every 5 seconds, at most 256 of them per sweep in turn, so a call proposal
   is void within one sweep rotation after its 30 s TTL (30–35 s while at most 256 call groups have
   work; a group whose sweep fails is retried on the next sweep and holds up no other), and a voided DS `Remove` whose
   target leaf is still present is re-issued with the same `action_id` instead of being dropped: in
   a call group a void lifts the freeze but leaves the member decrypting media, and only a commit
   removes it. A DS `Remove` records the device holding its leaf when it is issued, and is re-issued
   only while that same device still holds the leaf: MLS reuses blank leaves, and a `Remove` re-aimed
   by index alone would remove whoever joined there since. A DS `Remove` that recorded no device
   (one written before devices were recorded) is re-issued only within the epoch it was issued in,
   and never across epochs. A DS proposal re-issued in the epoch it was voided in is the same
   proposal — the external sender's signature is deterministic, so its bytes and its reference are
   the voided one's — and is re-armed (no longer void, issued now with a fresh TTL, its original
   `action_id` kept), not duplicated; a re-issue that fails leaves the voided proposal as it was. A
   DS `Remove` that names a device rather
   than a leaf (a device that left a call, a kick, or the committer election's removal of a
   candidate) resolves the leaf under the group lock and is
   dropped when the device holds no leaf or when a non-void DS `Remove` of that leaf is already
   outstanding. A DS `Remove` that names a leaf is likewise dropped while a non-void DS `Remove` of
   that leaf is outstanding; one whose leaf was read before the group lock (the inactivity sweep's,
   a text-group kick's) names the device it expects too, and is refused like a `Remove` of a leaf
   that is gone when another device holds the leaf by then. The DS also offers a `Remove` bound to
   one membership — the device and the epoch at which it took the leaf — which is refused as well
   when that device holds the leaf from a later epoch, having left and come back. A leaf keeps the
   epoch its device took it at for as long as that device holds it; a leaf another device takes,
   and the leaf an external commit (a resync or an external join) puts its joiner on, start at that
   commit's epoch, whether the joiner lands at its old index or another. A member proposal never
   cancels, voids, blocks or replaces a DS proposal: a
   member's own `Remove` is not mandatory for a commit, does not freeze the group and starts no
   election, so it never counts as the DS's while it is only proposed. The DS issues its `Remove`
   of a leaf regardless of the member's own `Remove` of it — a commit then applies one of the two
   and leaves the other unreferenced, and invariant 4 counts the DS's as satisfied when the one
   applied is the member's — and refuses a member's own `Remove` of a leaf whose DS `Remove` is
   outstanding with `409 E_REMOVE_PENDING`, which the member reads as "I am being removed": it
   withdraws the proposal and does not retry. A member that uploads (`POST /proposal`) a proposal
   the DS already holds — the same bytes, so the same reference, sender and epoch; typically a retry
   of an upload whose answer was lost — is answered as the first upload was (the same `[seq]`;
   nothing is queued, logged or fanned out again) when the DS's copy is that member's own
   proposal. If that copy had gone void it is re-armed as a DS proposal is (live again, issued now
   with a fresh TTL, the same `action_id`): the member is asking for the same thing again. Only a
   reference that names an instance proposal or another sender's proposal — which no conforming
   client can produce, since the reference covers the sender — is refused, with
   `409 E_REMOVE_PENDING`, and the DS's copy is left as it is.
   The underlying action is
   retried with a fresh KeyPackage, or dropped if the target leaf is already gone. Before proposing an Add the DS also checks invariant
   4's device-list clause (the device's DSK is in its user's newest signed device list) and leaves
   an unlisted device unproposed, its KeyPackage unspent. An outstanding DS Add whose device is
   revoked or quarantined, or whose user is no longer eligible under the channel ACL, is marked void
   at once — no Commit could carry it, and none could omit it while it was non-void — and it is
   dropped rather than re-issued. When every instance proposal of a group has gone
   void, the freeze lifts even if no member device is online. A device is **online** while it holds
   a gateway connection in state `ready` whose last liveness mark — `ready`, `resumed` or a
   `heartbeat` frame — is newer than the instance's `session_idle_close` window (default 90 s, with
   clients beating every 30 s). A device whose only session is inside the resume grace window has no
   connection and is therefore not online.
7. **Committer election.** `mls.commit_needed` goes to the lowest-index online device, bot devices
   first; other devices back off `300 ms + random(0..300 ms)`; a 2-second watchdog nudges the next
   candidate; after three lost rounds the failing device is removed by a DS Remove. A lost round
   counts against a device only if that device acknowledged the task — the frame reaching the
   connection's writer is not an acknowledgement. The acknowledgement is the `commit_ack` frame
   (opcode 12), group-scoped, whose payload echoes the `round` of the `mls.commit_needed` it
   answers. Three acknowledged-and-lost rounds remove the device; unacknowledged rounds only
   advance the election. For a kept device, the delay before re-election doubles on each kept
   cycle: 2 s, 4 s, 8 s, up to 60 s. It resets when the group's epoch advances or the device
   acknowledges and commits. The exception to removal is the group's last member that could
   commit: when every other current member is barred (quarantined or revoked) or already targeted
   by an outstanding non-void DS `Remove`, the device is kept and its lost rounds are reset. It is
   elected again when it is online and work remains, with the backoff above. Removing it would
   leave nobody able to commit, and the group would stay frozen until its proposals expire. A
   device that an outstanding non-void DS `Remove` is removing is never elected — it cannot commit
   its own removal — and when it is the only candidate online nobody is elected.

   The member list an election walks is also the list every group-scoped frame (`message.ct`,
   `message.deleted`, `mls.handshake`) fans out to. The instance holds it, with each device's leaf
   index, in memory, writes it after every registration, accepted commit and heal, and rebuilds it
   from its member table for every open group when it starts, before it accepts a connection. A
   restarted instance therefore delivers group frames and elects committers without waiting for a
   group's next commit. Frames fanned out while a device had no connection are not replayed to a
   fresh `identify`; a client catches each group up over HTTP after every `ready`.
8. **Current-leaf sends.** Application messages are accepted only from a device session whose
   leaf is in the current `PublicGroup` (`403 E_LEAF_NOT_CURRENT`). The DS reads the franking
   commitment `C` from `private_message.authenticated_data` and MUST reject an upload whose
   `authenticated_data` is not exactly 32 bytes (`422 E_COMMITMENT_INVALID`); it then computes and
   stores the franking tag `T` over that value (`04-envelope-and-franking.md`, "Franking").
9. **Fork handling.** A member that cannot process an accepted Commit reports it
   (`POST /fork-report`) and resyncs to the DS head by external commit. Three distinct reports
   against one Commit quarantine the committer: a flag on the device and a DS Remove of the leaf
   the committer's device holds now (not the leaf it committed from, which a newcomer may hold by
   then; nothing is proposed when the device holds no leaf). The instance's periodic reconcile
   proposes that `Remove` again for as long as the quarantined device holds a leaf with no DS
   `Remove` of it outstanding, and does the same for a revoked device, in every `text` and `call`
   group (DMs included): the kinds that carry the instance as an external sender (`01` § External
   senders). In `pairing` and `interaction` groups, which carry none, the instance proposes nothing;
   their members remove a revoked device themselves (`01` § External senders: a member's `Remove`
   of its own user's leaf, a device revocation).
10. **Retention.** Handshakes are kept 30 days. Retention has two independent halves. **Delivery
    retention** is how long the instance keeps an object so that a device that was away can still
    fetch it: handshakes 30 days; application ciphertext until every member device's cursor has
    passed it or 30 days, whichever comes first; Welcomes 30 days; KeyPackages until consumed or
    until their MLS lifetime expires. **Archival retention** is the community's own policy for
    application ciphertext that every cursor has already passed (default: indefinite), recorded per
    message in `expires`, where an absent value means "retained". An instance MAY shorten either
    half per community policy; it MUST NOT lengthen the handshake window beyond 30 days without also
    lengthening client-side past-epoch retention, which this version does not allow. Cursors are per
    device. Only a **delivery** deletion moves the `E_PRUNED` high-water of a stream; an archival
    deletion never does. `expires` need not grow with `seq` (a community that shortens its policy
    makes newer messages expire before older ones), so a high-water raised to an expired `seq`
    would stand above messages that still exist and refuse a catch-up the instance can serve. A
    catch-up across an archivally deleted message simply does not return it.
11. **Restore.** `dillad restore` bumps the instance `generation`; every group becomes
    epoch-unknown and every response carries the new generation. A member heals a group by
    `POST /v1/groups/{id}/heal`, uploading its member-signed GroupInfo together with its handshake
    tail (at most **64** items). The instance rebuilds its `PublicGroup` from its own restored
    state blob and replays the tail through it; it adopts the result only when its own stored epoch
    is **≤** the uploaded GroupInfo's epoch, the tail replays cleanly, the GroupInfo's epoch
    **equals the replayed group state epoch**, and its `tree_hash` equals the rebuilt tree's.
    Epoch equality matters even when an empty commit leaves the tree hash unchanged. When the
    instance holds no usable state blob for the
    group, the healing member additionally uploads the ratchet tree in the same request — the one
    upload where the tree is allowed, because the instance has none — and the instance reseeds from
    it; RFC 9420 §12.4.3.3's signed `tree_hash` ties that tree to the member-signed GroupInfo, and
    every leaf of it must pass invariant 4's `Add` clause (the device known, owned by the
    credential's user, not revoked, keyed by its registered key, in its user's newest signed device
    list, its user eligible under the ACL; `422 E_COMMIT_INVALID` with `rule = "add_key_package"` or
    `"add_acl"`), and the tree may hold no leaf whose credential is no dilla identity
    (`rule = "reseed"`). Three more conditions bind a reseed: the healing device must hold a leaf
    of the group as the instance restored it (its own member record; otherwise `403 E_FORBIDDEN`,
    as for any heal by a non-member); the uploaded tree's `dilla_binding` must name the group the
    instance holds — the same `kind`, `community_id` and `target_id` as the binding stored with
    the group (`rule = "reseed"`); and its `external_senders`, as the tree stands after the
    replayed tail, must pass invariant 1's rule for the group's kind (`rule = "reseed"`). The
    reseed requires the uploaded tree's epoch to equal the restored epoch. An ahead tree is refused
    even with a nonempty tail: its missing handshake history cannot be verified and published to
    other members from that tree alone. A device that is the target
    of an outstanding non-void DS `Remove` at the restored epoch (`409 E_REMOVE_PENDING`, as for a
    resync), or that is quarantined (`403 E_FORBIDDEN`), cannot heal. The replay does not apply invariant 4's first clause
    or invariant 5 (the restored queue describes the backup's epoch); instead, after a heal that
    moves the epoch, the instance re-issues the restored epoch's outstanding DS proposals for the
    healed epoch when they still apply — a `Remove` whose recorded device still holds its leaf, onto
    that device with the same `action_id`; an `Add` still eligible and not yet a member — and drops
    the rest. A proposal whose re-issue fails is dropped too, and so is everything at the restored
    epoch if the instance stops between adopting the heal and re-issuing. What re-creates a dropped
    proposal is its own source, not the heal: the instance's periodic reconcile re-proposes the
    `Remove` of every leaf whose device is quarantined or revoked and of every leaf whose user the
    channel ACL no longer admits (both in `text` and `call` groups, DMs included); the inactivity sweep
    re-proposes an inactivity `Remove` while the device stays away; a group DM's next membership
    sync re-proposes its participants' `Remove`s and `Add`s, and a channel's next membership change
    or admission its `Add`s; a committer-election `Remove` is not re-created (the device is elected
    and charged again only if it keeps losing rounds). A heal that leaves the epoch where it was
    leaves them as they are. If no heal succeeds within
    24 hours the group is closed and re-created by the channel owner's device. All non-last-resort
    KeyPackages are purged on restore. Live calls end.

### Testkit scenario DSL

The chaos scenarios are `.scn` files in `testkit/scenarios/`: one statement per line, `#` comments.
They run against `dilla-testkit`'s in-memory delivery service, or against a running instance when
the file says `ds <url>` or the runner is given `--ds <url>` (which wins). This list is informative;
the parser in `testkit/src/scenario/parse.rs` is the reference.

Base vocabulary: `instance`, `client`, `sync`, `group`, `join`, `external_join`, `send`,
`expect_decrypts`, `remove`, `go_offline`, `go_online`, `expect_reject <code> <statement>`.

Added for the remote delivery service:

- `ds <url>` — run against the instance at `url`; only before the first `client`.
- `kick <actor> <target>` — the instance proposes removing `target`'s device on `actor`'s authority.
- `advance_clock <duration>` — move the instance clock by `30s`, `5m`, `24h` or `90d`.
- `expect_frame <op> [field=value …]` — the last client to act received that frame, by its label above.
- `expect_425 <statement>` — the statement is refused `425 E_COMMIT_REQUIRED`.
- `snapshot <name>` — the test host snapshots the instance's state under `name`, through `dillad
  backup`'s own code.
- `restore_snapshot <name>` — the test host restores it through `dillad restore`'s own code and
  restarts the instance, which finishes the restore at start as `dillad serve` does.
- `commit <actor> [<group>]` — the actor commits for the current epoch of every group it is in, or
  of that one group. `commit <actor> <group> member_removes_last` queues a member's own `Remove`
  behind the instance's `Remove` of the same leaf first, so the commit applies the member's
  (invariant 4); the step fails when the actor holds no such pair.
- `leave <client> <group>` — the client posts its own `Remove` proposal (`01-groups.md`, how a
  member leaves); another member's commit applies it. A second `leave` in the same epoch re-uploads
  the same proposal and is answered as the first (re-armed if it had gone void). When the instance
  is already removing the leaf, the refusal (invariant 6, `409 E_REMOVE_PENDING`, matched on the
  code) is the client being
  removed, not a failure: the client withdraws its proposal and the step passes. The testkit's
  in-memory delivery service issues no instance proposals and so cannot answer that refusal; it
  refuses the statement (as it does `kick`) rather than let a leave scenario pass without it.
- `join_many <group> <count>` — `count` new clients join, at most 256 Adds per commit.
- `expect_decrypts_all <actor>` — everything the actor received since its last such assertion decrypts.
- `expect_quarantined <actor>` — the instance reports the actor's device quarantined (invariant 9).
- `expect_closed <group>` — the instance reports the group closed (invariant 11).
- `resync <client> <group>` — the client drops its copy of the group and returns by an own-leaf
  external commit (`POST /resync`, invariant 9).
- `fork_report <client> <group>` — the client reports the last commit it received for the group as
  one it cannot process (`POST /fork-report`, invariant 9).
- `heal <client> <group>` — the client uploads its GroupInfo and handshake tail to a restored
  instance (`POST /heal`, invariant 11).
- `ack_commit <client>` — the client acknowledges its latest `mls.commit_needed` with `commit_ack`
  and does nothing else (invariant 7).
- `admit <group> <client>` — the instance proposes adding the client's device (invariant 6).

Probes for invariants 1, 4 and 8, which break exactly one rule each:

- `expect_reject <code> rule=<rule> <statement>` — as `expect_reject`, and the `E_COMMIT_INVALID`
  must name that invariant-4 rule (`member_remove_scope`, `outstanding_proposals`,
  `external_joiner`, …).
- `client <name> device_list=none|revoked` — the client's user publishes no signed device list, or
  one whose entry for the device is revoked (default `signed`).
- `external_join <client> <group> [as=<uploader>] [leaf_key=fresh]` and
  `resync <client> <group> [as=<uploader>] [leaf_key=fresh]` — the client's external commit is uploaded under the
  uploader's session, or its leaf carries a signature key that is not the device's DSK.
- `mark_revoked <client>` — the test host marks the device revoked in the store and leaves its
  session, the window a revocation can race.
- `send_bad_commitment <client> <group> <len>` — a message whose `authenticated_data` is `len`
  bytes rather than 32 (invariant 8).
- `channel <target> [visibility=private|invite|discoverable] [mode=e2ee|readable]
  [members=<client>,…]` — the test host records the channel invariant 1 checks a `text` group
  against; with `members=` it also stores a community-less channel with those clients' users as its
  members, whose group the instance populates once it is registered.

Against an instance, `join … via=welcome` is the protocol's own join: the instance proposes the Add
and a member commits it, because a member's own Add is refused by every receiver in a `text` or
`call` group (`01-groups.md`, "Client policy for proposals from members").

## Errors

Every `/v1` refusal is an HTTP status plus a body that is the deterministic-CBOR fixed-position
array

```
error = [
  code,            ; tstr, one of the codes below
  detail,          ; tstr, human text; may be empty; a client MUST NOT parse it
  retry_after_ms   ; uint or null; present only for E_RATE_LIMITED and E_COMMIT_REQUIRED
]
```

served as `Content-Type: application/cbor`. Codes are stable strings and are the only thing a
client switches on. Four refusals carry additional fixed-position elements appended after
`retry_after_ms`, and only those four:

```
E_COMMIT_CONFLICT : [code, detail, null, winning_commit(bstr), proposals([bstr])]
E_COMMIT_REQUIRED : [code, detail, retry_after_ms, proposals([bstr])]
E_COMMIT_INVALID  : [code, detail, null, rule(tstr)]
E_VERSION         : [code, detail, null, wire([uint]), e2ee([uint]), media([uint])]
```

| HTTP | code | meaning | client action |
|---|---|---|---|
| 400 | `E_INVALID_REQUEST` | malformed body, malformed identifier, wrong array length | do not retry unchanged |
| 400 | `E_REMOVAL_PROOF_EXPIRED` | a moderation-removal proof is outside its acceptance window and no receipt exists for its request ID | sign a fresh proof with the same request ID and retry; an exact prior receipt is returned before this check |
| 400 | `E_BINDING_INVALID` | `dilla_binding` absent, wrong version, or not matching the object; or a registered group's `external_senders` not the one its kind carries (invariant 1) | do not retry |
| 400 | `E_VERSION` | no version in common; the extra elements list what the instance supports | reconnect with a supported version, or tell the user to update |
| 400 | `E_ENVELOPE_SHAPE` | the envelope is not a 9-element deterministic CBOR array, or a field has the wrong type or length | do not retry unchanged |
| 400 | `E_ENVELOPE_TYPE` | the type byte names no envelope type this version knows | do not retry unchanged |
| 400 | `E_ENVELOPE_LIMIT` | a field or list exceeds a `protocol/04` § Limits bound | shrink the field and resend |
| 401 | `E_UNAUTHENTICATED` | no device session, or an expired or revoked one | establish a session (`§ Device sessions`) |
| 401 | `E_PASSKEY_UNKNOWN` | the passkey is not registered on this instance | use another sign-in |
| 403 | `E_FORBIDDEN` | the session is authenticated but not permitted | none |
| 403 | `E_CHANNEL_MODE` | the operation is not allowed for this channel's mode | use the surface the mode does allow |
| 403 | `E_FACTOR_REQUIRED` | the session's account owes a second factor | add one (`09` § Second factors) |
| 403 | `E_LEAF_NOT_CURRENT` | the sending device's leaf is not in the current tree | resync by external commit; show "you are no longer a member" if refused |
| 403 | `E_MODE_READABLE` | text groups are not allowed for this channel | none; the channel is server-readable |
| 403 | `E_NOT_UPLOADER` | only the uploading user may delete this object | none |
| 403 | `E_PROVISIONAL_OUTSIDE_PAIRING` | a `provisional` session reached anything but its one `pairing` group | finish enrolment, then establish an `enrolled` session |
| 403 | `E_STEP_UP` | the step-up token is missing, spent, expired, another session's, or its secret was wrong | confirm again |
| 403 | `E_TARGET_NO_FACTOR` | the user the request names holds no second factor and the community requires one | none; tell the actor |
| 404 | `E_NOT_FOUND` | no such group, device, message or blob | none |
| 409 | `E_GROUP_EXISTS` | this `group_id` is already registered | mint a new `group_id` |
| 409 | `E_COMMIT_CONFLICT` | another commit won this epoch | discard the pending commit, process the winner, retry |
| 409 | `E_REMOVE_PENDING` | the instance is already removing this leaf (invariant 6); the member's own `Remove` of it is refused. Also, defensively, an uploaded proposal whose reference names an instance proposal or another sender's (no conforming client sends one; a member's re-upload of its own proposal is answered as the first). Also a resync or a heal by the target of an outstanding non-void instance `Remove` (invariant 5's first guard) | you are being removed: withdraw the proposal, do not retry; a refused resync may be tried again once a member has committed the `Remove` (the device then holds no leaf and joins afresh) or the group has closed |
| 409 | `E_CALL_FULL` | the call already has `livekit.max_voice_participants` devices | show "This call is full"; the user may retry later |
| 409 | `E_CALL_SHARERS_FULL` | the call already has `livekit.max_publishers` devices sharing camera or screen | show that sharing is full; retry after a sharer stops |
| 409 | `E_CALL_NOT_ALONE` | `DELETE /v1/calls/{call_id}?if_alone=1`: the caller's device is not the call group's only current leaf | the call goes on; just leave it |
| 410 | `E_PRUNED` | delivery retention has deleted a row of the requested stream at or above `from` (`from` is the first `seq` wanted; the instance records the highest `seq` delivery retention deleted from each stream, so the answer is exact; an archival deletion is not recorded, invariant 10) | resync; mark older messages "undecryptable (too old)" |
| 410 | `E_INVITE_INVALID` | the invite is expired, exhausted or revoked | none |
| 413 | `E_TOO_LARGE` | the object exceeds the instance limit | split or attach |
| 422 | `E_COMMIT_INVALID` | structural or policy failure; `rule` names the clause | do not retry unchanged; resync if behind |
| 422 | `E_COMMITMENT_INVALID` | `private_message.authenticated_data` is not exactly 32 bytes | recompute `C` and resend |
| 425 | `E_COMMIT_REQUIRED` | outstanding DS proposals must be committed first | commit them (or wait for `mls.commit_needed`), then resend |
| 429 | `E_RATE_LIMITED` | a token bucket is empty | wait `retry_after_ms`; a `Retry-After` header in whole seconds carries the same value |
| 500 | `E_INTERNAL` | an unexpected server fault; the detail is always empty | retry with backoff |
| 503 | `E_UNAVAILABLE` | the instance could not take a resource the request needs within its bound (a call whose SFU work is stuck) | wait `retry_after_ms`, then retry |
| 507 | `E_STORAGE_FULL` | the instance or the user's quota is exhausted | none |

A **syntactically invalid** identifier in a path is `400 E_INVALID_REQUEST`; a syntactically valid
but unknown one is `404 E_NOT_FOUND`.

That is **30 rows**: this file's original eighteen plus `E_VERSION`, `E_PROVISIONAL_OUTSIDE_PAIRING`
and `E_INTERNAL` (deviation ID12), plus the four Plan 2 consumes — `E_ENVELOPE_SHAPE`,
`E_ENVELOPE_TYPE`, `E_ENVELOPE_LIMIT` and `E_CHANNEL_MODE` (deviation ID12 as amended; controller
ruling 2026-09-24). The first three are Plan 1a's own Go code — `server.CodeVersion`,
`Sessions.Middleware` and `WriteError`'s fallback — and `scripts/check-protocol-docs.mjs`'s
`checkErrorVocabulary` diffs this table against `internal/server/errors.go` in both directions, so
the three could not have been left out without CI failing.

Four more came later. `E_REMOVE_PENDING` is invariant 6's refusal of a member's own `Remove` of a
leaf the instance is already removing (plan dilla-media task 9); the only other proposals it
refuses are ones no conforming client sends (a reference naming an instance proposal or another
sender's), never a member's re-upload of its own proposal. `E_CALL_FULL` and
`E_CALL_SHARERS_FULL` are the call routes' capacity refusals (`09` § Voice, plan dilla-media
task 10): the first answers the advisory participant count at token time, the second the publisher
lease. `E_UNAVAILABLE` is a call route or the signalling proxy that could not take the call's
serialisation within its bound (`09` § Voice). None of the last three is a delivery-service code.
`E_CALL_NOT_ALONE` came with web-3: the conditional end of a call (`09` § Voice), refused unless
the caller's device is the call group's only current leaf, decided under the group lock.
`E_FACTOR_REQUIRED`, `E_TARGET_NO_FACTOR` and `E_STEP_UP` came with web3.6, the required second
factor: an account that owes one (§ Device sessions item 8), a `require_mod_2fa` grant to a user
who holds none (`09` § Communities), and a factor change without a valid step-up. Each is 403 and
distinct, so a client tells the three apart by code, never by detail. `E_PASSKEY_UNKNOWN` (401)
came with them: a passkey login whose credential this instance does not hold (`09` § Second
factors), distinct from `E_UNAUTHENTICATED` so the client can say which. None is a
delivery-service code.

The four Plan 2 consumes are declared **here**, in the one task that owns the vocabulary, even though their
only call sites are Plan 2's. The three `E_ENVELOPE_*` codes are 400 and mirror the Rust envelope
module's own codes and the `protocol/vectors/` reject corpus one-for-one: a server-readable
channel's instance **is** a receiver in `protocol/04`'s sense, so it refuses exactly what a client
refuses and a client switches on one vocabulary. `E_CHANNEL_MODE` is **403**, not 400 — the request
is well formed and the session is authenticated; the operation is simply not allowed for the
channel's mode (an earlier draft of Plan 2 gave it 400 in a table row and 403 at its call site; the
controller resolved it to 403). Declaring all four here is what keeps
`scripts/check-protocol-docs.mjs` green from Plan 1's first commit onward, because that script
diffs the table against `internal/server/errors.go` in both directions and a code in one and not
the other fails CI.

`E_TOO_LARGE` fires at **131072 bytes** of MLS ciphertext on `POST /v1/groups/{id}/message`. The
gateway's inbound read limit is **16384 bytes** (`gateway.read_limit_bytes`) because no inbound
frame carries ciphertext; the outbound `max_frame_bytes` advertised in `hello` is
`max_ciphertext_bytes + 512` = **131584**, and a client sets its own WebSocket read limit from it.

## Retention

Defaults: handshakes 30 days; application ciphertext per invariant 10; KeyPackages until consumed
or their MLS lifetime expires (clients set 90 days); GroupInfo latest only; `PublicGroup` state
current epoch only, with the last 64 handshakes retained for restore. Retention has two independent
halves. **Delivery retention** is how long the instance keeps an object so that a device that was
away can still fetch it: handshakes 30 days; application ciphertext until every member device's
cursor has passed it or 30 days, whichever comes first; Welcomes 30 days; KeyPackages until
consumed or until their MLS lifetime expires. **Archival retention** is the community's own policy
for application ciphertext that every cursor has already passed (default: indefinite), recorded per
message in `expires`, where an absent value means "retained". An instance MAY shorten either half
per community policy; it MUST NOT lengthen the handshake window beyond 30 days without also
lengthening client-side past-epoch retention, which this version does not allow. Attachment blobs
(`09` § Blobs) have no delivery half: they follow the community's archival retention only (default:
indefinite) and never the 30-day window, because a device restoring an archive needs attachments
far older than that.

Cursors: a device's cursor advances only on an **explicit client acknowledgement**, never on
fan-out — a frame put on a writer queue is not a delivery. The prune floor is the minimum cursor
over devices that are not revoked, not disabled and seen within the 90-day inactivity window; a
device outside that set does not hold the floor.
