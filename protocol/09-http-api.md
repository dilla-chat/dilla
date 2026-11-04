# 09 — HTTP API (non-delivery-service)

## Scope

This document is owned by `dillad`, not by the delivery-service contract. `02-delivery-service.md`
freezes with "DS API v1" at the end of W5; this document does **not** freeze then — it covers the
instance document and limits, accounts, devices, sessions, invites, auth ceremonies, communities and
content, rate limits and the `users.flags` bit layout, all of which stay editable as dillad grows.

The built-in community bot's wire-3 installation, event, role, moderation and
provider-setup endpoints are specified in [10-community-bot.md](10-community-bot.md).

## Encoding

One body encoding: deterministic CBOR, fixed-position arrays, served and accepted as
`Content-Type: application/cbor` on every `/v1` request and response that has a body. A request
whose `Content-Type` is not `application/cbor` is `415`; a body that fails to decode as
deterministic CBOR is `400 E_INVALID_REQUEST`. Blob bodies are the one exception: raw octets,
`Content-Type: application/octet-stream`.

## Identifiers

Path identifiers are 32 lowercase hex characters (`^[0-9a-f]{32}$`); `{blob_id}` is 64 lowercase
hex. Both are validated before the database or the filesystem is touched. A syntactically malformed
identifier is `400 E_INVALID_REQUEST`; a well-formed but unknown one is `404 E_NOT_FOUND`.

## Sessions

Wire-3 session capability negotiation uses authenticated
`POST /v1/sessions/negotiate`, with request `[[wire_versions], [e2ee_versions],
[media_versions]]` and response `[wire,e2ee,media]`. Each offered list contains
1..8 distinct positive uints. The highest common versions are bound to this
bearer session; a missing intersection is `400 E_VERSION` and changes nothing.
Gateway identify/resume records its selected versions with the same binding.
Negotiation admits enrolled humans and live installation-bound bot sessions in
enrolling, joining or ready installation state. These bot sessions use enrolled
device scope 0 with `10`'s additional route restrictions; negotiating versions
does not grant installation or feature authority.
See `10`'s Session capability and compatibility section for bot upgrade gates;
request headers never establish negotiated capability.

Every endpoint in this document except the ones marked `—` or `R` requires a device session exactly as
`02-delivery-service.md`'s `§ Device sessions` defines: `Authorization: Bearer <token>` resolved to
a `sessions` row by `SHA-256(token)`; no cookie and therefore no CSRF surface on `/v1`. The `Auth`
column below uses these scopes:

| scope | meaning |
|---|---|
| `E` | an `enrolled` session |
| `P` | `pending` also accepted |
| `V` | `provisional` also accepted |
| `A` | an `enrolled` session whose user holds the instance-admin flag |
| `F` | an `enrolled` session whose account owes a second factor is also accepted |
| `B` | an enrolled managed-bot session bound to its live installation, account and device; additional phase/capability restrictions are in `10` |
| `R` | an operator-registered bot-runtime credential, distinct from device sessions and instance administration |
| `—` | no session required |

An account owes a second factor until it holds a confirmed TOTP secret or a passkey registered
for this instance's `rp_id` (`02` § Device sessions item 8, § Second factors). Every `E` route
not marked `E, F` answers such an account's sessions `403 E_FACTOR_REQUIRED`, as do the `E or V`
routes, for a `provisional` session too; the `P` routes admit it, and the gateway refuses it
(close `4003`). A table without an `Auth` column says its scope in the sentence
before it ("every route below is `E`").

## Accounts and devices

| Method and path | Auth | Request | Response |
|---|---|---|---|
| `POST /v1/accounts` | — | `[code(tstr), username(tstr), display(tstr), umk_pub(bstr32), ssk_pub(bstr32), sig_umk_ssk(bstr64), password(tstr\|null), device(array)]` where `device = [device_id(bstr16), dsk_pub(bstr32), tier(uint), signer_tier(uint), credential(bstr)]`; a non-empty `password` has at least 12 characters (Unicode code points), else `400 E_INVALID_REQUEST` and nothing is stored | `[user_id(bstr16), device_id(bstr16), token(tstr), expires(uint)]` |
| `GET /v1/accounts/me` | E, F | — | `[user_id, username, display, kind, flags, created, presence_choice, status_msg, bio]` |
| `PATCH /v1/accounts/me/presence` | E | `[choice(uint)]` | `204`; choice: 1 Online, 2 Idle, 3 Do Not Disturb, 4 Invisible |
| `POST /v1/accounts/me/presence/activity` | E | — | `204`; refreshes this live device's activity time |
| `PATCH /v1/accounts/me` | E | `[display(tstr\|null), status_msg(tstr\|null), bio(tstr\|null)]` | `204`; a `null` leaves that field as it is and `""` clears it; `400` when a given field is refused, and then nothing is written (below) |
| `DELETE /v1/accounts/me` | E (step-up) | `[]` | `204` — credential purge, Removes from every group, tombstone keeping `username` |
| `POST /v1/devices` | E | `[device_id, dsk_pub, tier, signer_tier, credential, nonce(bstr32), sig(bstr64)]` | `[device_id]`; `403 E_FORBIDDEN` when `sig` does not prove possession of `dsk_pub` (`02` § Device sessions item 7) and, `device cap reached`, at 8 live devices; `429 E_RATE_LIMITED` with `retry_after_ms` past 3 live rows created in the hour; it never evicts; `409 E_INVALID_REQUEST` when `device_id` exists or a live device of the user holds `dsk_pub` |
| `GET /v1/devices` | E | — | `[[device_id, tier, signer_tier, verified_at, revoked_at, last_seen, label\|null]]` |
| `PATCH /v1/devices/{device_id}` | E | `[label\|null]` | `204` names the device (null clears the name); the label is under the shared text rule below (NFC, trimmed, 1 to 64 characters, one line, no control, bidirectional or other invisible character), and the label is not part of the signed device list, so the server can see and change it; else `400 E_INVALID_REQUEST`; the device may be the caller's own or another live device of the same user; `404 E_NOT_FOUND` for another user's, an unknown or a removed (revoked) device; `403 E_FACTOR_REQUIRED` while the session owes a second factor; spends the device session's write bucket |
| `DELETE /v1/devices/{device_id}` | E | — | `204` for an unlisted row — revokes, deletes sessions, closes sockets; `409 E_INVALID_REQUEST` for a listed device (its `device_id` and `dsk_pub` in one unrevoked entry of the newest list), which is revoked by a signed device list, and for an unlisted row created less than 10 minutes ago unless the caller's session is that device's own; `404 E_NOT_FOUND` for another user's device |
| `PUT /v1/users/{user_id}/device-list` | E or P (own list only for P) | `[version(uint), blob(bstr), ssk_signature(bstr64), prev_hash(bstr32)]` | `204`; `400 E_INVALID_REQUEST` when `blob`'s outer elements disagree with the body or the list does not verify; `409 E_INVALID_REQUEST` when `version` is not the newest plus one or `prev_hash` does not chain |
| `GET /v1/users/{user_id}/device-list` | E or P (own list only for P) | — | `[version, blob, ssk_signature, prev_hash]`; on a wire-4 session `[version, blob, ssk_signature, prev_hash, attestation]`, the account-kind attestation of `03` § Account kind attestation (a `500 E_INTERNAL` when the instance cannot sign it, never a head without it) |
| `GET /v1/users/{user_id}/device-list?after=N` | E or P (own list only for P) | — | `[[version, blob, ssk_signature, prev_hash]]` of the stored versions greater than `N`, ascending, at most 64; `[]` when none; a malformed `N` is `400` |

For trust validation, the unpaged `GET /v1/users/{user_id}/device-list` supplies a signed
candidate head. Its first four elements are the history row the pages carry (the chain compares
those); a wire-4 head's fifth element, the account-kind attestation, is no part of the chain and is
handed to the core before the user's history is ingested. History pages after `N` contain at most 64 ascending versions; a page has no
authoritative head or `lastVersion`. If the candidate is the exact cached signed head, compare
its bytes and hash and skip paging; a lower version is rollback. Otherwise fetch from version 1
on first sight, or after the cached signed version, checking every signature, user, consecutive
version and `prev_hash`. Reject an empty or short page before the candidate, a gap, an extra
version, or final signed bytes/hash different from the candidate. Read the unpaged head again
after paging; if it changed, retry once within 1024 versions and 4 MiB total or leave the leaf
untrusted. A 403, 404 or malformed body also leaves it untrusted. A server that withholds all
newer history can freeze first-sight trust; the client cannot prove freshness from these routes.

`POST /v1/accounts` is the sole exception to the device-session proof rule: it creates the device
and its first session in the same transaction, because the device's key is the one being
registered and there is no prior key to prove possession of. `POST /v1/devices` is not an
exception: `nonce` comes from `POST /v1/devices/{device_id}/sessions/challenge` for the new
`device_id`, and `sig` is the establish signature over it (`02` § Device sessions item 2's
preimage, purpose `0`) by the private half of the `dsk_pub` being registered.

The `credential` of the `device` sub-array is stored as sent (1 to 8192 bytes) after the instance
has verified it (both signatures over this `dsk_pub`, canonical CBOR) and bound it to the request:
it names this `device_id`, `tier` and `signer_tier`, the human kind, and the request's `umk_pub`,
`ssk_pub` and `sig_umk_ssk`; anything else is `400 E_INVALID_REQUEST`. `POST /v1/devices` binds its
credential the same way to the session's account: the account's own kind (`users.kind`), its
`umk_pub`, `ssk_pub` and `sig_umk_ssk`, this device, and a `user_id` that is the session's or the
zero placeholder (`02` § Roles, credential-kind rule). Because the instance mints `user_id` in this response,
a registering client cannot know it when it builds the credential: it sends the credential with
16 zero bytes as `user_id`, and after the response rebuilds its MLS credential with the minted
`user_id` without signing anything again (`sig_ssk_dev` does not cover `user_id` and
`sig_umk_ssk` covers `ssk_pub` only, `03` § Keys). Every leaf the device creates carries the
rebuilt credential; the placeholder never appears in a group.

A device created through a host login is registered by `POST /v1/devices/{device_id}/sessions`
with a registration array and assertion (`02` § Device sessions item 2). The establish body is at
most 8192 bytes and its credential at most 2048 bytes. Both that route and `POST /v1/devices`
keep at most 8 live devices per user and at most 3 live rows created per user in any hour (a live
first device counts; revoked and expired rows do not). For assertion registration neither limit
refuses while the user has an unlisted live row: past either, the oldest unlisted live row is
revoked, whatever its age, and the registration is admitted (`02` § Device sessions item 2); a cap
of 8 listed devices refuses with `403 E_FORBIDDEN`, and it never answers `429` for the per-user
rate. `POST /v1/devices` never evicts: past the cap it answers `403 E_FORBIDDEN` (`device cap
reached`) and past the rate `429 E_RATE_LIMITED` with `retry_after_ms`, since its enrolled caller
pays no host login and can free a slot itself, and an evicting route would let a stolen enrolled
session replace a recovering owner's new row. An unlisted row and its pending sessions expire after
24 hours. An owner with an enrolled session can also remove an unlisted row with
`DELETE /v1/devices/{device_id}` once the row is 10 minutes old; a younger one only its own session
removes (`409` otherwise), so a stolen enrolled session cannot cut a recovering owner's row before
the list `PUT` that names it. The `/v1/devices` routes spend the device session's read (`GET`)
and write (`POST`, `PATCH`, `DELETE`) buckets.

A device's `label` is the owner's own name for it. It is not part of the signed device list: the
instance stores it readable, like a display name, and could read or change it. `GET /v1/devices` returns it only to the owner, for the owner's own devices, so no other user ever
sees it. A client shows it with the first characters of the `device_id` beside it, the name isolated from the text
around it (`<bdi>`), and names a device by both wherever a person acts on it (removal); it treats the label as a
hint, never as a statement about which key the device holds.

**The text rule.** A display name (at sign-up, in `POST /v1/accounts`, and in `PATCH /v1/accounts/me`), a member `nick`, a
`status_msg` and a `bio` share one rule: valid UTF-8, NFC, counted in characters, trimmed of White_Space, and not only
whitespace or combining marks. A line break is refused except in a `bio` (where `\r\n` becomes `\n`). The rule refuses:
a control character; every Default_Ignorable_Code_Point (the bidi controls, the zero-width characters, the soft hyphen,
the Hangul fillers, variation selectors, tag characters, the invisible operators and the unassigned ones among them);
U+2028, U+2029, U+2800 and U+FFF9 to U+FFFB; an unassigned code point (General_Category Cn, noncharacters included), a
private-use one and a surrogate; and more than 8 combining marks in a row. Four things are kept by context: U+200C and
U+200D where RFC 5892 Appendix A.1 and A.2 allow them (after a virama; a ZWNJ between joining letters, so Persian, Kurdish
and Sinhala text keeps working; a ZWJ joining two emoji, which are an Extended_Pictographic with an emoji presentation),
U+FE0E and U+FE0F directly after an Emoji code point, U+180B to U+180D and U+180F directly after a Mongolian letter, and
the three RGI emoji tag sequences (the England, Scotland and Wales flags). Every character property comes from the tables
of one Unicode version (17.0.0), generated by `scripts/gen-profile-text-tables.mjs` from pinned inputs, so a client agrees
with the instance whatever its own Unicode version. Only a new or changed text is checked: **a display name stored before
this rule is kept as it is** and still reads back, and a client may change any other field without sending it.

`PATCH /v1/accounts/me` writes the profile (EOD E6; schema 22, migration `00022_profile`). `display`
is at most 64 characters, `status_msg` at most 190 and one line, `bio` at most 500 with line breaks. Each given field is
checked by the text rule before anything is written, and one `UPDATE` writes only the fields given, so
two devices that change different fields do not lose each other's edit. The route spends the device session's write bucket
like the presence writes. The server stores the text **readable**, like the display
name: it is not end-to-end encrypted, and anyone who shares a community with the account reads it. The account has no
avatar. `GET /v1/accounts/me` and each row of the member list answer `status_msg` and `bio` as the last two elements (`""`
when unset). A client written for the seven-element `me` row or the seven-element member row reads the extra elements as absent.

One row above describes more than any released instance does. It is recorded here so a client
plans against what an instance answers, not against what the table would otherwise promise:

- `DELETE /v1/accounts/me` tombstones the account keeping its `username`, disables it, and deletes
  every session of every device with their sockets closed. It does **not** yet purge the credential
  rows (password PHC, TOTP secret, recovery-code hashes, WebAuthn credentials, OIDC identity) or
  remove the user from its MLS groups. The tombstone stops every one of those credentials
  authenticating immediately; the stored secrets survive until the credential deletes and the
  group-removal path land, and this paragraph goes when they do.

`blob` is the full six-element signed list of `03` § Device list. The instance verifies it and acts
on its revocations (`02` § Device sessions item 6).

## Sessions endpoints

| Method and path | Auth | Request | Response |
|---|---|---|---|
| `POST /v1/devices/{device_id}/sessions/challenge` | — | `[]` | `201 [nonce(bstr32), expires(uint)]` |
| `POST /v1/devices/{device_id}/sessions` | — | `[nonce, purpose, sig, registration\|credential\|null, login\|null]` | `201 [token, scope, user_id, device_id, expires, idle_expires, generation]` |
| `DELETE /v1/devices/{device_id}/sessions` | E, F | — | `204` (all sessions of that device, sockets closed); `409 E_INVALID_REQUEST` for another device's unlisted row created less than 10 minutes ago; `404 E_NOT_FOUND` for another user's device; spends the device session's write bucket (`429 E_RATE_LIMITED`) |
| `POST /v1/gateway/ticket` | E | `[]` | `201 [ticket(tstr), expires(uint)]` — single use, 30 s |
| `POST /v1/sessions/negotiate` | E | `[[wire_versions], [e2ee_versions], [media_versions]]` | `[wire,e2ee,media]`; highest common versions persisted on this bearer session |

The full session issuance and lifetime rules — scopes, lifetimes, revocation — are
`02-delivery-service.md`'s `§ Device sessions`; this table only locates the routes.
`DELETE /v1/devices/{device_id}/sessions` keeps the grace of the key-less
`DELETE /v1/devices/{device_id}`: another device's sessions of an unlisted row younger than 10
minutes are not removed (`409`), and the caller's own device always is. Without it a stolen enrolled
session polling `GET /v1/devices` cut a recovering owner's pending session on sight, before the list
`PUT` that names the new row. A listed device's sessions, and an unlisted row's after 10 minutes,
are removed by any enrolled session of the user. A client whose own list is unpublished accepts a
`pending` answer when it re-establishes after such a cut; a second `401` on the list `PUT` is the
row's eviction, not a revocation.
An assertion registration with a native `tier` or `signer_tier` is `400 E_INVALID_REQUEST`:
assertion registration is for browser devices; `03`'s pairing ceremony is the native path.

## Community bot installation

These routes are the installation subset of `10-community-bot.md`. That document
defines the tuples, phase transitions, idempotent receipts, capability approval
and recovery semantics. Human and bot device sessions require negotiated wire 3;
runtime credentials use the explicit wire 3 provisioning contract. An ordinary
human bearer cannot call an `R` route, and a runtime credential grants no general
instance-admin authority. A `B` endpoint still applies enrolled-session middleware
before checking its managed-bot binding; it is not an exception for humans who owe
a second factor.

| Method and path | Auth | Request | Response |
|---|---|---|---|
| `GET /v1/communities/{community}/bot/command-status` | E (human, current community member, wire v3) | — | `null` when no active installation; otherwise `[1,installation_id,bot_user_id|null,state,revision]`; no-store; omits configuration, capabilities and credentials |
| `GET /v1/communities/{community}/bot` | E (human, manage_community) | — | installation or 404 |
| `PATCH /v1/communities/{community}/bot/profile` | E (human, manage_community, wire v3) | `[display_name]` | `[normalized_display_name]`; changes only the installed bot account's display name and not its handle |
| `PUT /v1/communities/{community}/bot` | E (human, manage_community and capability approval) | `[request_id,expected_revision\|null,capabilities,step_up]` | installation; 201 new, 200 existing. Without `v=2` only bits 0–7 may be sent and Polls/Reminders (bits 8, 9) keep their value, both on for a new installation (10-community-bot.md) |
| `POST /v1/communities/{community}/bot/disable` | E (human, manage_community) | `[request_id,expected_revision]` | installation |
| `DELETE /v1/communities/{community}/bot` | E (human, manage_community) | `[request_id,expected_revision]` | 204 |
| `POST /v1/communities/{community}/bot/restart` | E (human, manage_community, wire v3) | `[installation_id]` | `[restart_requested,restart_handled]`; coalesces while a request is pending; audited (protocol/10 § Restart) |
| `PUT /v1/communities/{community}/bot/music-settings` | E (human, manage_community, wire v3) | `[installation_id,volume,idle_minutes]` | widened installation; volume 25/50/75/100, idle 5/15/30 minutes |
| `GET /v1/bot-runtime/installations` | R | — | `[installations]`; at most 100, after cursor is an installation id; `v=2` returns the widened installation (protocol/10) |
| `PUT /v1/bot-runtime/installations/{installation}/status` | R (owning runtime) | `[version\|null,restart_handled]` | 204 |
| `POST /v1/bot-runtime/installations/{installation}/claim` | R | `[request_id]` | `[installation,claim_id,claim_expires]` |
| `POST /v1/bot-runtime/installations/{installation}/enroll` | R | `[request_id,claim_id,umk_pub,ssk_pub,sig_umk_ssk,device]` | `[bot_user,bot_device,username]`; no bearer token |
| `PUT /v1/bot-runtime/installations/{installation}/credential` | B (enrolling; completed retries in joining/ready) | `[request_id,expected_revision,credential]` | installation |
| `POST /v1/bot-runtime/installations/{installation}/ready` | B (joining; completed retry in ready) | `[request_id,expected_revision]` | installation |
| `POST /v1/bot-runtime/installations/{installation}/keypackages` | B (joining/ready) | `[operation_id,expected_generation,upload_body]` | `201 [count]` |
| `GET /v1/bot-runtime/installations/{installation}/keypackages` | B (joining/ready) | — | `[generation,server_time,ordinary_active_count,has_current_fallback]` |
| `GET /v1/bot-runtime/installations/{installation}/keypackages/{operation}` | B (joining/ready) | — | `[expected_generation,upload_sha256,count]` or 404 |
| `POST /v1/bot-runtime/installations/{installation}/groups` | B (joining/ready) | `[operation_id,expected_generation,registration_body]`; kind-3 adds `admission_request_id` | `201` new / `200` exact replay: registration receipt |
| `GET /v1/bot-runtime/installations/{installation}/groups/{operation}` | B (joining/ready) | — | registration receipt or 404 |
| `GET /v1/bot-runtime/installations/{installation}/interaction-admissions/{request}/group-absence` | B (ready, matching live admission receipt) | — | strict CBOR boolean; true only when the receipt's exact group ID is absent |
| `GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/info` | B (ready, exact current registered kind-3 interaction group) | — | `[epoch,group_info,tree_hash32,next_seq]` |
| `GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/handshakes?from=N&limit=L` | B (ready, exact current registered kind-3 interaction group) | — | handshake rows, same format and bounds as ordinary group sync |
| `GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/messages?from=N&limit=L` | B (ready, exact current registered kind-3 interaction group) | — | application-message rows, same format and bounds as ordinary group sync |
| `POST /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/cursor` | B (ready, exact current registered kind-3 interaction group) | `[last_seq,last_epoch]` | 204; advances this Bot device's cursor monotonically |
| `GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/heal` | B (ready, exact current registered kind-3 interaction group, including after restore) | — | `[epoch,next_seq,generation,need_from_seq,needs_heal]`; `needs_heal` is uint 0 or 1 |
| `POST /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/heal` | B (ready, exact current registered kind-3 interaction group) | `[group_info(bstr),tail([[seq,epoch,kind,sender,blob]]),ratchet_tree(bstr\|null)]` | `[epoch,next_seq]`; proof and heal-window rules match ordinary group heal |
| `POST /v1/bot-runtime/installations/{installation}/moderation/actions` | B (ready, warnings/timeouts capability, owning installation) | `[request_id,group_id,source_seq,action,rules,timeout_ms,expires]` | `[action_id,community_revision,expires|null]` |
| `GET /v1/bot-runtime/installations/{installation}/moderation/removals/{request}` | B (ready, removal capability, owning installation) | — | `[action_id,community_id,group_id,actor_user_id,actor_device_id,target_seq,removal_revision,accepted_at]`; accepted signed removal only |
| `POST /v1/bot-runtime/installations/{installation}/interaction-admissions/context` | B (joining/ready) | `[request_id,group_id,participant_user_id,participant_device_id]` | `[snapshot_payload,signature]`; read-only signed context v1, max 160 KiB |
| `GET /v1/groups/{id}/interaction-admission-context` | E (enrolled human session bound to the accepted participant device) | — | `[current_context_payload,signature]`; read-only current kind-3 context v1, max 160 KiB; requires current membership and exact authenticated two-leaf group state |
| `POST /v1/groups/{group}/automod-grants/arm` | E (human, current interaction participant, `manage_community`, fresh step-up) | `[request_id,community_id,expected_revision,request_expires,config_hash32,step_up]` | `[opaque_grant,expires_unix]`; one minute maximum; grant binds the current interaction group, participant user/device, operation 63, revision, expiry and config digest |
| `POST /v1/bot-runtime/installations/{installation}/automod-grants/redeem` | B (ready installation) | `[opaque_grant,community_id,group_id,request_id,expected_revision,request_expires,actor_user_id,actor_device_id,config_hash32]` | `[1,receipt_id]`; exact retries return the same receipt; changed tuple or config digest is refused |

Managed Bot synchronization of private kind-3 interaction groups uses only the four
installation-scoped routes above. Each read and cursor update validates the exact
accepted registration, current two-leaf MLS state, Ready installation, Bot session,
participant membership/device, signed device lists and current eligibility while
holding the group fence through the database operation. Generic group read routes
remain limited to kinds 0 and 1; Bot interaction routes do not grant group writes.

Managed Bots use this scoped publication route, with the durable retry and
generation rules in `10`; their generic `POST /v1/keypackages` is refused. Human
publication is unchanged. Receipts prove historical acceptance, not current
directory availability, and remain subject to current installation authority.
The collection GET is a read-only current inventory snapshot with server Unix
time in seconds; it never consumes a package. Its fallback flag is uint 0 or 1.

Managed Bots register groups only through the installation-scoped routes above;
their generic `POST /v1/groups` is refused with `403 E_FORBIDDEN`. Existing
non-kind-3 Bot registration keeps its three-field outer tuple; an initial kind-3
interaction adds the nonzero admission request ID as field four. Human group
registration and its four-field CBOR tuple remain unchanged. `registration_body`
is a bstr of at most 65,505 bytes (the whole CBOR request is capped at 64 KiB)
containing the exact deterministic-CBOR encoding of
`[group_id(bstr16),binding(bstr),group_info(bstr),ratchet_tree(bstr)]`; the
server strictly decodes and validates it without reconstructing its bytes.
`operation_id` is a nonzero bstr16 and `expected_generation` is a uint no greater
than signed-int64 max. A fresh request is accepted only when that generation is
current; mismatch is `409 E_INVALID_REQUEST` (`expected_generation is stale`)
and writes nothing. New acceptance is
`201 [operation_id,group_id,registration_sha256,accepted_generation,
initial_epoch,next_seq]`. The hash is
`SHA-256(ASCII("dilla bot group registration v1") || 0x00 || registration_body)`.
`next_seq` is the exact initial registration response value (1), and
`initial_epoch` is the registered GroupInfo epoch. The acceptance receipt and
group row, public MLS state and initial member rows commit atomically.

The durable receipt is scoped by installation and operation and binds user,
device, community, group, body hash, acceptance generation, initial epoch and
`next_seq`. A matching retry returns `200` with the original six-field receipt,
even after later epochs or generation changes. Reuse with another group, body or
expected generation is `409 E_INVALID_REQUEST`; a different operation colliding
with a registered group is `409 E_GROUP_EXISTS`, without group metadata. Receipt
lookup requires the current B session to match the path installation and returns
the same receipt or `404 E_NOT_FOUND`; another installation's receipt is
indistinguishable from absence. Receipt lookup and historical replay require a
live installation-bound session in joining or ready state. Receipts survive
disablement and normal backup/restore and are not deleted when an installation
is disabled.

Before native group creation, the Go coordinator durably stores the operation id
and original `expected_generation`; retries reuse that generation, never a host
clock or newly sampled generation. It also retains the exact canonical
registration-body bytes returned by the native intent. After restart, the
coordinator compares the receipt's operation, group, expected/accepted
generation and body hash with that intent before calling
`bot_group_registered(operation_id,group_id,next_seq)`. Absence is conclusive
nonacceptance only when the original generation is still current, the group id
has no registered row, and managed bots have no alternate registration path. If
generation has changed, absence is `unknown`: pause that group and retain the
intent for reconciliation. A retry's stale-generation refusal does not resolve
the earlier ambiguous request. Never infer discard or create a replacement group
from a missing receipt after restore.

Managed Bot sends use the attempt and recovery contract in `10`:

| Method and path | Auth | Request | Response |
|---|---|---|---|
| `POST /v1/groups/{group}/bot/messages` | B | `[operation_id,attempt_id,predecessor_attempt_id\|null,expected_generation,epoch,private_message,class]` | twelve-field send receipt |
| `GET /v1/communities/{community}/bot/receipts/{operation}?attempt=ID` | B | — | send receipt or 404 |
| `POST /v1/communities/{community}/bot/receipts/{operation}/resolve` | B | `[attempt_id,predecessor_attempt_id\|null,group_id,expected_generation,epoch,class,ciphertext_sha256]` | send receipt |

These routes require current managed identity and group access. New acceptance
also requires current send permission. Recovery returns historical acceptance,
durable nonacceptance or uncertainty after restore; a missing receipt alone never
permits another ciphertext. The existing human message upload API is unchanged.

## Community bot runtime and human-facing routes

These additional routes are mounted by `dillad`; the event, moderation-removal
and role delegation semantics are specified in `10-community-bot.md`. `B`
requires a live installation-bound managed-bot session and the route's phase,
community and capability checks. `E` requires an enrolled human session; listed
permission checks still apply.

| Method and path | Auth | Request | Response |
|---|---|---|---|
| `GET /v1/communities/{community}/bot/actors/{actor}/authority` | B (ready) | — | `[effective_permission_bits, is_owner(uint), is_administrator(uint)]`; 404 if the community, installation or actor is absent |
| `GET /v1/communities/{community}/bot/actors/{actor}/channels/{channel}/authority` | B (ready, channel visible to bot) | — | `[community_id,actor_id,channel_id,effective_permission_bits]` for the current human member; 403/404 if installation, membership or visibility checks fail; no-store |
| `GET /v1/communities/{community}/bot/events?after=N&limit=L` | B | — | `[events, next_seq, high_water, floor]`; `after` is exclusive, default limit 100, maximum 250 |
| `PUT /v1/communities/{community}/bot/events/ack` | B | `[through]` | `204` |
| `POST /v1/groups/{group}/moderation/removals` | E or B | `[proof_bstr, signature64]` | `[action_id, removal_revision]` |
| `GET /v1/groups/{group}/moderation/removals?after=N&limit=L` | E or B | — | `[removal_rows, next_revision]` |
| `GET /v1/communities/{id}/self-roles` | E (community member) | — | `[[role_id, role_name, position, selected(uint)]]` |
| `PUT /v1/communities/{id}/self-roles/policy` | E (manage_roles) | `[request_id, role_ids]` | `[request_id, accepted_count]` |
| `PUT /v1/communities/{id}/self-roles/{role_id}` | E (community member) | `[request_id, enabled(uint 0 or 1)]` | `204` |
| `PUT /v1/communities/{id}/role-panels` | E (manage_roles) | `[request_id, channel_id, message_seq, options]`; each option is `[emoji, role_id]` | `[request_id, option_count]` |
| `PUT /v1/communities/{id}/bot-join-role` | E (manage_roles) | `[request_id, role_id, expected_installation_id]`; zero `role_id` clears the approval | `[request_id, installation_id]`; configuration only, no immediate role grant; 409 if the active installation changed |
| `GET /v1/communities/{id}/bot-join-role` | E (manage_roles) | Empty | `[role_id/null, installation_id]` for a ready installation; 404 when none is ready |
| `GET /v1/communities/{community}/bot/members/{member}/episode` | B (ready, wire v3, current installed bot/member) | Empty | `[member_id, episode_id]` for the current membership episode; 404 when the member is absent |
| `POST /v1/communities/{community}/bot/join-role-grants` | E (ready Bot session, wire v3, roles capability) | `[request_id, member_id, episode_id]` | `[request_id, installation_id, community_id, member_id, episode_id, role_id]` immutable grant receipt; 204 when the episode is current but no join role is configured; 404 for a stale episode; repeated identical requests return the same receipt |
| `PUT /v1/communities/{id}/role-panels` | E (current manage_roles session; ready Roles-capable Bot) | `[request_id, channel_id, accepted_message_seq, [[emoji, role_id]...]]` | `[request_id, accepted_option_count]`; requires an accepted Bot send receipt for the channel and every role must remain approved and eligible; readable rows, when present, must match the receipt |
| `POST /v1/communities/{id}/role-panels/{channel_id}/{message_seq}/reaction` | E (current human channel member; after an accepted encrypted event) | `[group_id, event_seq, emoji, enabled(uint 0 or 1)]` | `204`; actor is the session user, and only the exact approved mapping for this registered panel may change that actor's reaction grant source |
| `PUT /v1/communities/{id}/members/{user_id}/timeout` | E (moderate_members) | `[request_id, expires_unix]` | `[request_id, expires_unix]` |
| `DELETE /v1/communities/{id}/members/{user_id}/timeout` | E (moderate_members) | `[request_id]` | `204` |
| `GET /v1/communities/{id}/members/{user_id}/timeout` | E (member or moderate_members) | — | `[expires_unix|null]` |
| `POST /v1/communities/{id}/members/{user_id}/warning` | E (moderate_members; actor must outrank target) | `[request_id]` | `[request_id, action_id, community_revision, accepted_at]` |
| `GET /v1/bot-runtime/installations/{installation}/moderation/warnings/{request}` | B (ready installation with warnings capability; owning session) | — | `[request_id, action_id, community_id, actor_user_id, actor_device_id, target_user_id, community_revision, accepted_at]`; authoritative accepted-action lookup for encrypted case attachment |

The role-panel action route is an explicit authenticated self-service action. The
client calls it only after the corresponding encrypted reaction upload has been
accepted; the server binds `event_seq` to the current session device's accepted
application-message row in the named open channel group. MLS plaintext remains
opaque to the server, so the request's emoji and enabled flag are the action
itself, not a server-verified interpretation of ciphertext. The server rechecks
the exact registered panel, Bot send receipt, current role approval, hierarchy,
and channel membership before adding a source; removal deletes only that
session user's matching panel source and remains available if policy approval
was later withdrawn.

The warning route writes an immutable warning action and audit metadata. It does
not accept or store a plaintext reason. The request ID is idempotent for the same
actor, device, community, and target; reusing it for another action conflicts.
The human client stages the reason in its encrypted local outbox before posting
the action, then sends operation 61 over the installation's encrypted interaction
group. The Bot attaches the case only when the authoritative receipt matches the
request, actor, device, community, and target. The encrypted case expires after
90 days; plaintext reason is never stored by the server.

Who may control the Bot's music is the role permission `control_music` (bit 25,
§ Permissions), not a per-installation list: the former DJ role routes
(`GET`/`PUT /v1/communities/{id}/music/dj-roles` and the Bot's
`GET /v1/bot-runtime/installations/{installation}/music/dj-roles`) are gone.
Migration 00048 gave `control_music` to every role the community's current,
non-removed installation had approved, then dropped the approval table; roles
approved only by a removed installation were not mapped.

Event acknowledgement advances
the installation's durable cursor; a cursor below the retained floor returns
`410 E_PRUNED`. Self-role policy is bounded to 250 role ids. A role panel must
reference a live readable-channel message authored by the installed bot, and
its options must be approved self-service roles.

## Invites

| Method and path | Auth | Request | Response |
|---|---|---|---|
| `GET /i/{code}` | — | — | `text/html`, or CBOR when the request asks for `application/cbor`: `[instance(tstr), community_id(bstr16\|null), expires(uint), community_name(tstr\|null)]`. Never mutates; `Cache-Control: no-store`, `Referrer-Policy: no-referrer` |
| `POST /v1/invites/redeem` | — | `[code(tstr)]` | `[invite_id(bstr16), community_id(bstr16\|null), grants_admin(uint)]` |
| `POST /v1/communities/{id}/invites` | E | `[max_uses(uint), ttl_seconds(uint), grants_admin(uint)]` | `201 [invite_id(bstr16), code(tstr), url(tstr), expires(uint)]` |
| `GET /v1/communities/{id}/invites` | E | — | `[[invite_id, community_id\|null, created_by\|null, grants_admin, max_uses, used_count, created, expires_at, revoked_at\|null]]`, newest first, revoked and spent invites included, never a code |
| `DELETE /v1/invites/{id}` | E | — | `204` |

Errors: `410 E_INVITE_INVALID` when the invite is expired, exhausted or revoked.

- A **community invite** is an invite whose `community_id` is set. Minting and listing need
  `create_invite` community-wide (§ Permissions) and membership of the community (`404 E_NOT_FOUND`
  otherwise, as for every community route); `403 E_FORBIDDEN` without the permission.
  `max_uses` is 1–1000, or 0 for any number of uses ("∞"); `ttl_seconds` is 1–2 592 000 (thirty
  days), or 0 for an invite that never expires. An open-ended invite (either 0) also needs
  `manage_community` (owners and administrators hold it): it is a standing way in that only a
  manager can take back, so a member who holds only `create_invite` gets `403 E_FORBIDDEN` and may
  still make the finite ones. `grants_admin` is 0 or 1, and 1 is refused (`403 E_FORBIDDEN`) to
  anyone who is not an instance admin, because whoever registers with the invite becomes one; a
  `grants_admin` invite is always finite (`400 E_INVALID_REQUEST` with a 0). Anything else out of
  range is `400 E_INVALID_REQUEST`. On the wire 0 keeps its meaning everywhere an invite is read:
  the mint's `expires`, the list's `max_uses` and `expires_at`, and the landing CBOR's `expires`
  say 0 for "never" and "∞". The instance stores an open-ended invite with `max_uses = 1000` and
  an `unlimited_uses` flag, and "never" as the largest signed 64-bit second, so a reader that
  ignores the flag still stops at the old bounds. The HTML landing page reads "Never expires" and
  "No use limit". Revoking stays as it is: the creator or a `manage_community` holder.
- `code` is the invite's only plaintext, returned once, by the mint. The instance stores its
  SHA-256 and the audit row (`invite.create`) names it by the first four bytes of that hash in
  hex. `url` is the instance's base URL followed by `/i/` and the code.
- **One use per person.** `POST /v1/accounts` with a community invite spends one use, and the
  instance records which account that use registered. The first successful `join` by that account
  that presents the same invite spends no second use: it consumes the record instead, in the
  join's transaction, so a new person costs an invite one use and `max_uses = 10` admits ten
  people. A join without the invite (of an open community, say) leaves the record in place. The
  invite must still be unrevoked and unexpired (`410 E_INVITE_INVALID` otherwise). The record is
  the instance's own, written by the sign-up; no request field claims it. Once it is consumed, a
  join by that account with the invite (after a `leave`, say) spends a use as any other account's
  join does, so a used-up invite admits nobody who did not sign up with it. An invite without a
  community is spent by the sign-up alone and leaves no record.
- `DELETE` revokes: the caller must be a member of the invite's community and either its creator
  or hold `manage_community`. Revoking a revoked invite is a no-op that keeps the first time; an
  unknown invite, one without a community and one of a community the caller is not in are all
  `404`. Each revocation writes an `invite.revoke` audit row.
- `GET /i/{code}` of a community invite names the community; an invite of a deleted community
  answers as an unknown one. A code is accepted as a person types it: hyphens and spaces stripped,
  case ignored, and `0`, `8` and `1` read as `O`, `B` and `I` or `L`.
- `GET /i/{code}`, `POST /v1/invites/redeem` and `POST /v1/communities/{id}/join` share one
  bucket, `limits.rate.invite_per_second` (0.1) with `invite_burst` (5), keyed by the client
  address, so a join is not a way round the limit on guessing codes. A refusal is `429
  E_RATE_LIMITED`; the landing page carries `Retry-After` too.

## Auth ceremonies

| Method and path | Auth | Request | Response |
|---|---|---|---|
| `POST /v1/auth/password/login` | — | `[username(tstr), password(tstr)]` | `[assertion(tstr), needs(uint)]`, or `[assertion(tstr), needs(uint), passkey(uint)]` when `needs` is not 0 (`needs`, `passkey`: § Second factors) |
| `POST /v1/auth/password` | E (step-up) | `[old(tstr\|null), new(tstr), stepup(tstr\|null)]`; the third element may be left out (a null `stepup`) | `204`; a `new` shorter than 12 characters (Unicode code points) is `400 E_INVALID_REQUEST`, answered before `old` or `stepup` is checked; a stored password set before this minimum still logs in; a wrong `old` is `403 E_FORBIDDEN`, counted on the step-up ledgers (§ Second factors), so past their free attempts every attempt, the right one included, is `429 E_RATE_LIMITED`; `429` too, before any guess, while the client address's login failure budget is spent. A **first** password (the account has none, `old` is null) needs a session established within `auth.session.reauth_window` (`403 E_FORBIDDEN` otherwise) and, when the account holds a second factor, a step-up token, checked after the window check and before the new password is hashed (`403 E_STEP_UP` without a valid one) and spent once it is hashed, so a failed hash leaves it unspent; `stepup` is ignored when a password exists, where `old` is the step-up |
| `POST /v1/auth/totp/enroll` | E, F | `[stepup(tstr\|null)]` | `[secret(tstr), otpauth_url(tstr)]` (§ Second factors) |
| `POST /v1/auth/totp/confirm` | E, F | `[code(tstr)]` | `[backup_codes([tstr])]` (§ Second factors) |
| `POST /v1/auth/totp/verify` | — | `[assertion(tstr), code(tstr)]` | `[assertion(tstr)]`; accepts an assertion owing `needs` 1 only; `429 E_RATE_LIMITED` while the account is locked out (the assertion is spent) |
| `POST /v1/auth/recovery/verify` | — | `[assertion(tstr), code(tstr)]` | `[assertion(tstr)]`; accepts an assertion owing `needs` 1 or 2 |
| `POST /v1/auth/passkey/register/begin` | E, F | `[stepup(tstr\|null)]` | `[ceremony_id(bstr16), options(tstr)]` (the library's JSON, opaque to the client's CBOR layer) |
| `POST /v1/auth/passkey/register/finish` | E, F | `[ceremony_id(bstr16), response(tstr), name(tstr)]` | `[cred_id(bstr), backup_codes([tstr])]` (§ Second factors) |
| `POST /v1/auth/passkey/login/begin` | — | `[]` | `[ceremony_id(bstr16), options(tstr)]`; metered per address on its own `passkey_begin` bucket (`limits.rate.passkey_begin_per_second` / `_burst`, default 0.2 / 10) and per network (IPv4 /24, IPv6 /48) on `passkey_begin_net` (default 1 / 40), never on `login`: a begin spends no password attempt. `429 E_RATE_LIMITED` past either. The ceremony lives 120 s (the options' `timeout`). Past `auth.webauthn.max_open_logins` (default 10000) open discoverable ceremonies the oldest (by creation second) is evicted with the write, so a begin always succeeds; a finish on an evicted ceremony fails as an expired one does. Registration and step-up ceremonies are capped per user (`auth.webauthn.max_open_per_user`, default 5, oldest evicted). Expired ceremonies are pruned on the maintenance tick |
| `POST /v1/auth/passkey/login/finish` | — | `[ceremony_id(bstr16), response(tstr)]`, or `[ceremony_id(bstr16), response(tstr), assertion(tstr)]` binding it to a password or OIDC (first-factor) login | `[assertion(tstr), needs(uint 0\|1)]` (§ Second factors) |
| `GET /v1/auth/oidc/start` | — | — | `302` to the IdP, PKCE `S256`, state cookie `__Secure-dilla-oidc` scoped to this route only |
| `GET /v1/auth/oidc/callback` | — | — | `302` back to the client with a one-time `assertion` |

The `assertion` is a short-lived one-time **enrolment assertion**: it proves host login and is spent
by `POST /v1/devices/{device_id}/sessions`, sent as the token's UTF-8 bytes in a bstr (element 4).
With a registration array in element 3 it registers the device; either way the minted session is
`pending`. It is never a session token and never reaches `/v1` beyond that one endpoint.

### Second factors

Every human account holds a second factor: a TOTP authenticator, or a passkey. The routes below
enrol, use, list and remove them. Where a row repeats one of § Auth ceremonies, this one is the
full contract.

| Method and path | Auth | Request | Response |
|---|---|---|---|
| `POST /v1/auth/password/login` | — | `[username, password]` | `[assertion(tstr), needs(uint)]`: `0` nothing owed; `1` a TOTP code or a backup code (`/totp/verify`, `/recovery/verify`); `2` the account has passkeys and no TOTP: a passkey login, or a backup code. When `needs` is 1 or 2 a third element `passkey(uint)` follows: `1` when the account holds at least one passkey for this instance's `rp_id` (a passkey login would find one), else `0`. It is written only after the password verified, on an answer that already says a factor is owed; a refusal, and a login owing nothing, never carry it. An older server leaves it out, and a client reads its absence as `0` |
| `POST /v1/auth/totp/enroll` | E, F | `[stepup(tstr\|null)]` | `[secret(tstr), otpauth_url(tstr)]`; `403 E_STEP_UP` when the account holds a factor and `stepup` is missing or not valid; `403 E_FORBIDDEN` over a confirmed secret (unchanged) |
| `POST /v1/auth/totp/confirm` | E, F | `[code(tstr)]` | `[backup_codes([tstr])]`: `auth.totp.recovery_codes` of them (1–16, default 10) when the account held no unused code, else `[]` (the same for `register/finish`); clears `factor_owed` in the same transaction; `403 E_STEP_UP` when a factor appeared since an enroll that carried no step-up; `404 E_NOT_FOUND` with no pending enrolment (none, or already confirmed); `400 E_INVALID_REQUEST` for a code that does not match |
| `POST /v1/auth/recovery/verify` | — | `[assertion, code]` | `[assertion]`; accepts an assertion owing `needs` 1 **or 2**; `429 E_RATE_LIMITED` while the account is locked out (the assertion is spent) |
| `POST /v1/auth/passkey/register/begin` | E, F | `[stepup(tstr\|null)]` | `[ceremony_id(bstr16), options(tstr)]`; the user's credentials are excluded; `409 E_INVALID_REQUEST` when the account already holds 1024 passkeys (checked before the step-up token is spent) |
| `POST /v1/auth/passkey/register/finish` | E, F | `[ceremony_id, response(tstr), name(tstr)]` | `[cred_id(bstr), backup_codes([tstr])]`; `name` is 1–64 bytes of UTF-8 without control characters (`400` otherwise, the ceremony kept); an unknown, spent or expired ceremony, another account's, or a response that does not verify is `400 E_INVALID_REQUEST` (another account's ceremony is not spent); clears `factor_owed`; `403 E_STEP_UP` as for confirm; `409 E_INVALID_REQUEST` at 1024 passkeys |
| `POST /v1/auth/passkey/login/finish` | — | `[ceremony_id, response]` or `[ceremony_id, response, assertion(tstr)]` | `[assertion(tstr), needs(uint 0\|1)]`: 0 when the authenticator verified the user, else 1; `401 E_PASSKEY_UNKNOWN` for a passkey this instance does not hold. The optional third element binds the passkey to a first-factor login: it must be a live assertion that `/password/login` or the OIDC callback (browser or desktop) issued and that still owes a factor, else `401 E_UNAUTHENTICATED` before the ceremony runs, alike for unknown, expired, spent, complete and other assertions; a passkey of another account than that assertion's is `403 E_PASSKEY_OTHER_ACCOUNT` and issues nothing (counted as a failed login); a matching one spends the bound assertion. Without it nothing changes, so older clients are unaffected; a finish whose ceremony is absent (expired, evicted, spent or unknown) is `401 E_UNAUTHENTICATED` and is not counted on the address's login failure budget, while a present ceremony with an assertion that does not verify is; `429 E_RATE_LIMITED`, and no assertion, when it would owe 1 while the account is locked out. Each finish spends one `login` token, and a refused one is recorded on the address's login failure budget as a wrong password is, so a passkey is no way around the login throttle |
| `GET /v1/auth/factors` | E, F | — | `[owed(uint), totp(uint\|null), passkeys([[cred_id(bstr), name(tstr), created(uint), last_used(uint\|null)]]), backup_codes_left(uint), password(uint), password_changed(uint\|null)]`: `password_changed` is when the password was last changed (`POST /v1/auth/password`), null when unknown (set before migration 00052) or there is none; an account marked as owing that holds a factor is cleared first and answers `owed` 0 (§ Second factors) |
| `POST /v1/auth/stepup` | E | `[method(uint 0 password\|1 totp\|2 backup code), secret(tstr)]` | `201 [stepup(tstr), expires(uint)]`; a wrong secret is `403 E_STEP_UP`; `429 E_RATE_LIMITED` with `retry_after_ms` from the failure that crosses the step-up ledger's free attempts on, or while the client address's login failure budget is spent; `400 E_INVALID_REQUEST` for another method |
| `POST /v1/auth/stepup/passkey/begin` | E | `[]` | `[ceremony_id, options]` (this user's credentials only; UV required); `400 E_INVALID_REQUEST` for an account that holds no passkey |
| `POST /v1/auth/stepup/passkey/finish` | E | `[ceremony_id, response]` | `201 [stepup, expires]`; `403 E_STEP_UP` for a login's ceremony, another account's, or an assertion that does not verify; `429` as for `/stepup` |
| `POST /v1/auth/factors/remove` | E | `[stepup(tstr), kind(uint 0 totp\|1 passkey), cred_id(bstr\|null)]` | `204`; `409 E_INVALID_REQUEST` when it is the account's last factor; `404 E_NOT_FOUND` for a credential that isn't the caller's; `403 E_STEP_UP` |
| `POST /v1/auth/recovery/codes` | E | `[stepup(tstr)]` | `[backup_codes([tstr])]`; every earlier code stops working |

- **"Holds a factor"** means a confirmed TOTP secret, or at least one passkey registered for this
  instance's `rp_id`. Backup codes are not a factor. The wire and the server call them recovery
  codes; people are shown them as **backup codes**, and they are never the recovery key of
  `03-identity.md`.
- An account that holds no factor **owes** one (`factor_owed`, § Sessions). The TOTP confirm and
  the passkey finish write the factor, clear `factor_owed` and mint backup codes in one
  transaction under the account's lock.
- **`factor_owed` follows what the account holds.** At every start, before it serves, `dillad`
  reconciles it both ways: it marks every live human account that holds no factor and clears it
  from every one that holds one. A passkey counts only under the current `rp_id`, so changing
  `rp_id` marks an account whose only factor is a passkey, and putting it back (and restarting)
  clears it. `GET /v1/auth/factors` also clears it, and answers `owed` 0, when it finds the
  account holds a factor (checked and written in one transaction under the account's lock). An
  operator reset deletes the factors and sets the mark in one transaction under that lock, so
  neither clears a mark a reset set.
- **Step-up:** required by enroll, register/begin, remove and codes, and by a first password
  (`POST /v1/auth/password` with no password on the account), exactly when the caller's account
  holds a factor; a token is single-use, bound to the session that minted it, and lives 300 s;
  only these routes spend one. A first password needs it because a browser can establish a fresh
  session from its device key at will: the session's freshness alone would let a stolen browser
  set a password and then step up with it (method 0). The confirm and finish re-check the rule under the
  account's lock: when a factor appeared since a begin that carried no valid step-up, they write
  nothing and answer `403 E_STEP_UP`. The record of what a begin carried lives in memory, so after
  a restart the person confirms again. Step-up guesses (the step-up routes and the old password of
  `POST /v1/auth/password`) are counted on two step-up ledgers of their own, never on the account's
  password lockout: one per account, whatever the address, and one per account **and** client
  address. A request that ends before its guess is decided (a store or hasher failure) is not
  counted. A success refunds its own guess and clears only the per-address count; the
  account-wide count ages out over `auth.lockout.observation_window`, so no success, the owner's
  included, resets a thief's guesses. A step-up also respects the address's login failure budget
  (§ Rate limits), which its failures spend.
- **Limits:** an account holds at most 1024 passkeys for this `rp_id`, and
  `auth.totp.recovery_codes` is 1–16 (the client's decode bounds).
- **TOTP codes.** A code is accepted at the lowest unused counter within the skew window
  (`auth.totp.skew` time steps either side of now, default 1), and a counter at or below the last
  accepted one is refused, so each code signs in, confirms or steps up once.
- **A guess is counted before its secret is checked,** in one step with the lockout check, at
  every step-up route (`/stepup`, `stepup/passkey/finish`, the old password of
  `POST /v1/auth/password`) and at `/totp/verify` and `/recovery/verify`: concurrent guesses
  count against each other, so a burst in flight cannot all reach the comparison. The password
  login is bounded instead by its per-handle bucket.
- **The account lockout covers the second step:** a password or passkey login that still owes a
  factor does not clear the account's failed-login ledger; wrong codes at `/totp/verify` and
  `/recovery/verify` are counted on it; and while it is locked those routes answer `429`, as does a
  passkey login that would owe a code. The code that completes a login clears it.
- **A step-up passkey ceremony is not a login ceremony,** nor the reverse: `login/finish` refuses a
  step-up's ceremony (`401`), and `stepup/passkey/finish` refuses a login's or another account's
  (`403 E_STEP_UP`).
- **The last factor** cannot be removed (`409`), also when two devices remove the last two at once.
- **OIDC callback:** the assertion owes a factor whenever the account holds one (`needs` as for the
  password login).
- **Passkeys count alone only with user verification** (`auth.webauthn.user_verification`, default
  `required`), read from each assertion's own authenticator data.

## Communities and content

| Area | Routes |
|---|---|
| communities | `POST/GET /v1/communities`, `GET/PATCH/DELETE /v1/communities/{id}`, `GET /v1/communities/{id}/members`, `GET /v1/communities/{id}/members/{user_id}`, `DELETE /v1/communities/{id}/members/{user_id}`, `POST /v1/communities/{id}/join`, `POST /v1/communities/{id}/leave` |
| channels | `POST/GET /v1/communities/{id}/channels`, `GET/PATCH/DELETE /v1/channels/{id}`, `PUT /v1/channels/{id}/overwrites/{kind}/{target_id}`, `DELETE` the same, `GET /v1/channels/{id}/members`, `PUT/DELETE /v1/channels/{id}/members/{user_id}` |
| roles | `POST /v1/communities/{id}/roles`, `PATCH/DELETE /v1/roles/{id}`, `PUT/DELETE /v1/communities/{id}/members/{user_id}/roles/{role_id}` |
| bans | `PUT /v1/communities/{id}/bans/{user_id}`, `DELETE` the same, `GET /v1/communities/{id}/bans` |
| invites | `POST /v1/communities/{id}/invites`, `GET /v1/communities/{id}/invites`, `DELETE /v1/invites/{id}` |
| DMs | `POST /v1/dms` `[recipients([bstr16])]` → `[channel_id]`; `GET /v1/dms` |
| readable | `POST /v1/channels/{id}/messages` `[envelope(bstr)]` → `[seq, franking_tag, recv_ts]`; `GET /v1/channels/{id}/messages?from=`; `PATCH`/`DELETE /v1/channels/{id}/messages/{seq}`; `GET /v1/channels/{id}/search?q=&limit=&before=`; `PUT /v1/channels/{id}/read-state` |
| gifs | `GET /v1/communities/{id}/gifs`, `PUT/DELETE …/gifs/key`, `PUT …/gifs/rating`, `GET …/gifs/search`, `GET …/gifs/media/{gif_id}`; see § GIFs |
| blobs | `PUT/GET/HEAD/DELETE /v1/channels/{cid}/blobs/{blob_id}` (raw octets, `201` new / `200` already present, `422 E_INVALID_REQUEST` on a hash mismatch, `507 E_STORAGE_FULL`), `DELETE /v1/admin/blobs/{blob_id}`, `POST /v1/channels/{cid}/blobs/{blob_id}/confirm` |
| backups | `PUT /v1/backups/{kind}/{chunk_seq}` `[object(bstr)]` → `[blob_id, size]`, `GET /v1/backups`, `GET /v1/backups/{kind}/{chunk_seq}` → `[object, created]`, `DELETE` the same (`501` at wires 1 and 2); see § Backups |
| reports | `POST /v1/reports` `[group_id, seq, envelope(bstr), k_f(bstr32)]`, `GET /v1/reports`, `PATCH /v1/reports/{id}` |
| voice | `POST /v1/channels/{id}/calls` → `[call_id, group_id, livekit_url, token, ice_servers]`, `DELETE /v1/calls/{call_id}`, `GET /v1/channels/{id}/call` → `[group_id, call_id, participants]`, `GET /v1/communities/{id}/calls` → `[[channel_id, call_id, participants]]`, `POST /v1/calls/{call_id}/decline` `[expires_ts]` → `204` |
| admin | `GET /v1/admin/diagnostics`, `GET /v1/admin/audit`, `POST /v1/admin/users/{id}/disable` |

The blob delete rule: a `DELETE` on `/v1/channels/{cid}/blobs/{blob_id}` removes the **reference**,
only for the uploading user; the file is unlinked by the sweeper when the last reference is gone and
`unref_since` is older than `blobs.gc_grace`. Global removal is the admin verb and writes a
tombstone, because content addressing otherwise lets anyone re-`PUT` the same bytes.

### Communities

Every route below is `E`. A community a caller is not a member of answers `404 E_NOT_FOUND`
exactly as an unknown one does, so a non-member does not learn that it exists; a soft-deleted
community answers `404` to everyone.

| Method and path | Request | Response |
|---|---|---|
| `POST /v1/communities` | `[name(tstr), policy(bstr), min_account_age_seconds(uint), require_mod_2fa(uint)]` | `201 [community_id(bstr16), role_everyone(bstr16), policy_version(uint)]` |
| `GET /v1/communities` | — | `[[community_id(bstr16), name(tstr), owner(bstr16), policy_version(uint)]]`: the live communities the caller is a member of, ordered by `community_id`, unpaginated; `[]` for none |
| `GET /v1/communities/{id}` | — | `[community_id, owner, name, policy, policy_version, min_account_age_seconds, require_mod_2fa, created]` |
| `PATCH /v1/communities/{id}` | `[name(tstr\|null), policy(bstr\|null), min_account_age_seconds(uint\|null), require_mod_2fa(uint\|null)]` | `[policy_version(uint)]` |
| `DELETE /v1/communities/{id}` | — | `204` |
| `GET /v1/communities/{id}/members?after=` | — | `[[user_id, joined, nick, [role_id], username(tstr), display(tstr), kind(uint), status_msg(tstr), bio(tstr)]]`, at most 200 per page, ordered by `user_id`; `after` is the last `user_id` of the previous page |
| `GET /v1/communities/{id}/members/{user_id}` | — | `[user_id(bstr16), nick(tstr), username(tstr), display(tstr)]` for a current member; `404 E_NOT_FOUND` if the target is absent. The caller must also be a current member; names are read only from that community's current membership and the user's current account row |
| `GET /v1/communities/{id}/presence` | — | `[[user_id, status]]` for all current members; membership required; status 0 Offline, 1 Online, 2 Idle, 3 Do Not Disturb |
| `GET /v1/communities/{id}/permissions` | — | `[bits(uint), top(uint)]`: the caller's community-wide permissions (§ Permissions; no channel overwrite applied) and the position of the caller's highest role, `0` for `@everyone` alone and `2147483648` for the owner |
| `DELETE /v1/communities/{id}/members/{user_id}` | — | `204` |
| `POST /v1/communities/{id}/join` | `[invite(tstr\|null)]` | `[community_id]` |
| `POST /v1/communities/{id}/leave` | `[]` | `204` |
| `POST /v1/communities/{id}/owner` | `[user_id(bstr16)]` | `204`; `403` for anyone but the owner, `404` when the target is not a member, `400` for the caller themself, a bot account, or a disabled or deleted account |
| `PUT /v1/communities/{id}/members/{user_id}/roles/{role_id}` | `[]` | `204` |

- The creator is the **owner** and the first member, and the community starts with one role,
  `@everyone`, at position 0, whose id is `role_everyone`. The owner can neither leave nor be
  removed. `PATCH` needs `manage_community` (§ Permissions); `DELETE` is the owner's alone
  (`403 E_FORBIDDEN` for any other member). Removing a member is the **kick** of § Bans. Role
  grants are § Roles'. The owner may hand the community to another member (`POST …/owner`); then
  they are an ordinary member and may leave. They keep their roles, the hand-over writes a
  `community.transfer` audit row, and the members are sent `community_changed` (`02`).
- Any member reads their own permissions (`GET …/permissions`). A client uses it to offer only what the instance allows and still handles every refusal.
- `leave` removes the caller's leaves from every `text` and `call` group of the community's
  channels, as a kick does (§ Bans). `DELETE` closes every such group after the community and its
  channels are tombstoned.
- `name` is 1–255 bytes of UTF-8 with no control character. `require_mod_2fa` is 0 or 1.
  `min_account_age_seconds` is at most 3 153 600 000 (a century); 0 means no gate.
- `policy` is the **policy document** below, and `policy_version` starts at 1 and grows by one on
  every `PATCH` that carries a policy; a `PATCH` without one leaves it. Two concurrent policy
  changes cannot share a version: the loser is `409 E_INVALID_REQUEST` and re-reads.
- `join` refuses (`403 E_FORBIDDEN`) an account the community has banned (§ Bans; a ban whose
  `expires` has passed no longer refuses), a disabled or deleted account and an account younger
  than `min_account_age_seconds`. Joining a community the caller is already a member of is a no-op that
  answers `[community_id]`.
- `join` with `invite = null` on a community whose policy says `"join": "invite"` is
  `403 E_FORBIDDEN`. With a code it spends one use of that community's invite (§ Invites): a
  malformed code is `400 E_INVALID_REQUEST`, and an unknown, expired, exhausted or revoked one, or one
  minted for another community, is `410 E_INVITE_INVALID`. The gates above run first, so a join
  they refuse spends no use, and the use and the membership are written together. An open community
  spends an invite it is given all the same. `max_uses` holds under concurrent joins: exactly that
  many succeed.
  The one exception is the join that follows a sign-up with the same invite, which spends nothing
  (§ Invites, one use per person).
- `require_mod_2fa = 1` refuses (`403 E_TARGET_NO_FACTOR`) a grant of a role whose `allow` carries any
  moderation bit (manage messages, kick, ban, manage channels, manage roles, manage community,
  administrator) to a user who holds neither a confirmed TOTP secret nor a passkey for this
  instance's relying party.
- A member's `nick` is a **display name**, not a handle: free Unicode, NFC, at most 64 characters,
  under the text rule above (no control, bidi, zero-width or other invisible character). The handle rules do not apply to it. An empty `nick` means the
  member's own display name shows.
- `GET /v1/communities` lists the caller's own memberships and nothing else: a community the
  caller left or was removed from, or one that is soft-deleted, is not listed. It names no
  community in its path, so it has no `404`.
- A member row's `username`, `display`, `kind` (`0` user, `1` bot), `status_msg` and `bio` are the member's account
  fields, as `GET /v1/accounts/me` answers them to that user; a member whose account row is gone
  answers `""`, `""`, `0`, `""`, `""`. Only a member of the community reaches the route, so these names are
  visible exactly to the people the member shares the community with.

The **policy document** is a UTF-8 JSON object of at most 16 KiB. The instance stores the bytes the
owner sent and serves them back unchanged, but refuses (`400 E_INVALID_REQUEST`) a document that is
not one object, carries a key this table does not list, or has a value out of range. Every key is
optional; `{}` is every default.

| key | type | default | meaning |
|---|---|---|---|
| `join` | `"open"` or `"invite"` | `"open"` | whether a join needs a community invite |
| `screening` | bool | `false` | membership screening; stored and served, **not enforced** by this version |
| `retention_days` | uint ≤ 36500 | `0` | **archival** retention (`02` § Retention): days an application message every cursor has passed is kept; `0` keeps it indefinitely. In this version it is enforced for attachments only (§ Blobs expires a reference older than it); no message is deleted by it yet |
| `delivery_retention_days` | uint ≤ 30 | `0` | **delivery** retention: `0` is the instance's 30 days; a community may shorten it, never lengthen it. In this version it is stored and served but not enforced: every community gets the instance's 30 days |

### Channels

Every route below is `E`. A channel the caller may not view (§ Permissions: `view_channel` in that
channel, overwrites applied), a channel of a community the caller is not a member of included,
answers `404 E_NOT_FOUND` exactly as an unknown or deleted one does. Creating a channel needs
`manage_channels` community-wide; changing and deleting one need it in that channel
(`403 E_FORBIDDEN` otherwise).

| Method and path | Request | Response |
|---|---|---|
| `POST /v1/communities/{id}/channels` | `[kind(uint), mode(uint), visibility(uint), parent_id(bstr16\|null), name(tstr), topic(tstr), position(uint), slowmode_seconds(uint)[, overwrites]]`, `overwrites` = `[[kind(uint), target_id(bstr16), allow(uint), deny(uint)]]` | `201 [channel_id(bstr16), mode(uint), visibility(uint)]`; `404 E_NOT_FOUND` when `parent_id` names a category the caller cannot view, ahead of the `400` parent rules, as for a hidden channel |
| `GET /v1/channels/{id}` | — | `[channel_id, community_id(bstr16\|null), kind, mode, visibility, parent_id(bstr16\|null), name, topic, position, slowmode_seconds, seq, text_group_id(bstr16\|null)]`; `parent_id` is null when the caller cannot view the category |
| `GET /v1/channels/{id}/permissions` | — | `bits(uint)`: the caller's effective current permissions in the channel, after role and member overwrites (§ Permissions); `404 E_NOT_FOUND` when the caller cannot view the channel |
| `GET /v1/communities/{id}/channels` | — | `200 [[channel_id, kind, mode, visibility, parent_id(bstr16\|null), name, topic, position, slowmode_seconds, seq, text_group_id(bstr16\|null)]]`: the live channels the caller may view (`view_channel`, overwrites applied) by `position` then `channel_id`; a channel whose category the caller cannot view is listed with `parent_id` null; a category is listed only when the caller can view it; `404` for a non-member |
| `PATCH /v1/channels/{id}` | `[name(tstr\|null), topic(tstr\|null), mode(uint\|null), visibility(uint\|null), parent_id(bstr16\|null), position(uint\|null), slowmode_seconds(uint\|null)]` | `204`; `404 E_NOT_FOUND` when `parent_id` names a category the caller cannot view, ahead of the `400` parent rules, as for a hidden channel |
| `DELETE /v1/channels/{id}` | — | `204` |

| field | values |
|---|---|
| `kind` | `0` text, `1` voice, `2` category, `3` DM, `4` group DM. `3` and `4` are not created through a community (`400`) |
| `mode` | `0` end-to-end encrypted, `1` server-readable |
| `visibility` | `0` private, `1` invite, `2` discoverable |

- **Created with overwrites.** The optional ninth element gives the channel its overwrites
  (§ Roles) in the same transaction that creates it, so a channel can be private from its first
  moment: no member it excludes ever lists it, holds a `channel_members` row for it or is proposed
  to its groups. A body of any length but 8 or 9 is `400`. `overwrites` holds 1 to 100 entries
  (`400` otherwise), and each follows § Roles' overwrite rules against the caller's
  **community-wide** permissions, rank included; a second entry for the same `(kind, target_id)` is
  `400`. Three more rules hold:
  - the caller also needs `manage_roles` community-wide (`403`);
  - a category never takes overwrites at creation (`400`);
  - the creator must still see the channel with the overwrites applied (`400` otherwise). The one
    exception to the rank rule makes this possible for a non-owner: a kind `1` entry naming the
    creator themself, allowing `view_channel`, `send_messages` or both and denying none, is
    allowed, provided the creator holds every bit it allows (`403` otherwise). Any other entry on
    oneself, one that allows another bit or denies any, takes the rank rule (`403`). The exception
    exists only at creation: a later overwrite `PUT` on oneself takes the rank rule as always.
    Such an entry is an ordinary member overwrite afterwards. After a demotion it keeps only the
    bits it allows (seeing the channel, posting in it) for its holder, and only while they still
    hold `view_channel` community-wide (without it they hold no bit at all, § Permissions), until
    someone who outranks them removes it; and it goes with the membership like every member
    overwrite (§ Roles).

  The create's audit records `channel.create` and one `channel.overwrite.put` per entry, and
  `community_changed` (`02`) is sent once.

- **Announcement channels** (informative) have no kind of their own: the web client creates one as
  a text channel whose overwrites deny `send_messages` to `@everyone` and allow it to each role
  chosen to post, and labels any text channel whose `@everyone` overwrite denies `send_messages`
  as one. The instance enforces it like any other overwrite (§ Readable channels, and `02` § Upload
  class for an end-to-end encrypted one). A creator who is neither the owner nor an administrator
  keeps the right to post through a kind `1` entry on themself allowing `send_messages`, sent in
  the create (the exception above), since the `@everyone` deny would otherwise take it and an
  entry for their own role takes the rank rule.

- A channel whose visibility is `invite` or `discoverable` is **server-readable**. Creating one
  with `mode = 0` is not an error: the instance stores `mode = 1` and answers it, and the client
  labels the channel from the answer. A `PATCH` that would make a visible channel end-to-end
  encrypted is `400 E_INVALID_REQUEST`; make it private first. `mode` and `visibility` may not
  change in the same `PATCH` (`400`), because the order they were applied in would decide the
  outcome. This is the rule `02`'s invariant 1 enforces on registration: an MLS `text` group is
  refused for such a channel (`403 E_MODE_READABLE`), while a voice channel's `call` groups exist
  whatever its mode. A `PATCH` that takes an end-to-end-encrypted text channel out of end-to-end
  encryption (to `mode = 1`, or to a visibility that forces it) closes the channel's open `text`
  group after the change commits, and from then on no `Add` or join is admitted to it; its `call`
  group, if any, is untouched.
- A category is always top level. `parent_id` names a live category of the same community; a text
  or voice channel under a text channel, a nested category and a foreign or deleted parent are
  `400`. In a `PATCH`, null leaves the parent alone and the all-zero id moves the channel to the top
  level. Deleting a category moves its channels to the top level.
- `name` is 1–100 bytes and `topic` at most 1024 bytes of UTF-8, with no control character (a
  topic may carry line breaks and tabs). `position` is at most 2 147 483 647 and
  `slowmode_seconds` at most 21 600. A community's channels are ordered by `position`, ties broken
  by `channel_id` (bytewise), the same on every engine. Position `2^30` and above belongs to the
  built-in AFK channel (§ Voice): a `POST` or `PATCH` that puts any other channel there is `400
  E_INVALID_REQUEST`, a `PATCH` that names a position other than `2^30` for the AFK channel itself
  is `403 E_FORBIDDEN` `the AFK channel keeps its position`, and the next start moves any other
  channel found there (one an older release created) below `2^30`, keeping their order. When they do
  not fit after the highest position below `2^30`, it renumbers every channel of the community
  except the AFK channel `0..n-1` in that order instead.
- `seq` is the channel's own sequence, which the server-readable message path advances.
- `text_group_id` is the `group_id` of the channel's oldest open `text` group (`01` § Group kinds),
  an epoch-unknown one included (`02` invariant 11), for a channel that carries one: kind `0`, `3`
  or `4` in mode `0`. It is `null` while no such group is open — the client then registers one
  (`02`, `POST /v1/groups`) — and always `null` for a server-readable channel, a voice channel and
  a category. A client joins the group it names (`01` § Joining) and does not register another
  while it is not `null`.
- Deleting a community deletes its channels in the same transaction. Deleting a channel, or its
  community, closes the channel's open `text` and `call` groups (`02`) once the deletion has
  committed.

### Roles

Every route below is `E`, and every one needs `manage_roles` community-wide (for an overwrite, in
that channel), except the two `GET`s, which any member may read; a caller who is not a member of the
community answers `404 E_NOT_FOUND`.

| Method and path | Request | Response |
|---|---|---|
| `GET /v1/communities/{id}/roles` | — | `[[role_id(bstr16), name(tstr), color(uint), position(uint), allow(uint), deny(uint), hoist(uint), mentionable(uint)]]`: every role ordered by `position` then `role_id`, `@everyone` first; bits outside § Permissions are cleared |
| `GET /v1/communities/{id}/overwrites` | — | `[[channel_id, kind, target_id, allow, deny]]`: every overwrite of every channel (categories included) the caller may view, ordered by `channel_id`, `kind`, `target_id`; bits outside § Permissions are cleared |
| `POST /v1/communities/{id}/roles` | `[name(tstr), color(uint), position(uint), allow(uint), deny(uint), hoist(uint), mentionable(uint)]` | `201 [role_id(bstr16)]` |
| `PATCH /v1/roles/{id}` | the same seven fields, each `\|null`; null leaves a field alone | `204` |
| `DELETE /v1/roles/{id}` | — | `204` |
| `PUT /v1/communities/{id}/members/{user_id}/roles/{role_id}` | `[]` | `204` |
| `DELETE /v1/communities/{id}/members/{user_id}/roles/{role_id}` | — | `204`; `404` when the member does not hold the role |
| `PUT /v1/channels/{id}/overwrites/{kind}/{target_id}` | `[allow(uint), deny(uint)]` | `204`; a change that would hide the channel from its author, unless the author is the owner or holds `administrator`, is `400` |
| `DELETE /v1/channels/{id}/overwrites/{kind}/{target_id}` | — | `204`; `404` when there is no such overwrite; a change that would hide the channel from its author, unless the author is the owner or holds `administrator`, is `400` |

- **Rank.** A role's `position` orders it; higher is more senior. The owner is above every role;
  anyone else acts only on roles strictly below their own highest role. Creating, changing,
  moving, deleting, granting and revoking a role at or above it is `403 E_FORBIDDEN`.
- **No new authority.** A role's `allow` and `deny`, and an overwrite's, may name only bits the
  caller holds (for an overwrite, in that channel); a grant or revoke of a role that carries a bit
  the caller does not hold is refused the same way, and so is a grant or revoke of a role whose
  overwrite in any channel names a bit the caller does not hold in that channel (a role that opens
  a private channel the caller cannot see is not the caller's to hand out). `403 E_FORBIDDEN`.
- **`@everyone`** is the role at position 0, created with the community and held by every member.
  No other role may be created at or moved to position 0, `@everyone` is never moved, deleted,
  granted or revoked, and all of these are `400 E_INVALID_REQUEST`; its fields and bits may be
  changed like any other role's.
- `name` is 1–100 bytes of UTF-8 with no control character, `color` is at most `0xFFFFFF`
  (24-bit RGB), `position` is 1 to 2 147 483 647, `hoist` and `mentionable` are 0 or 1, and a
  community holds at most 250 roles. A bit outside § Permissions' table is `400`.
- `require_mod_2fa` (§ Communities) additionally gates a grant.
- **Overwrites.** `{kind}` is `0` (the target is a role of the channel's community) or `1` (the
  target is a member of it); anything else is `400`, and an unknown target is `404`. An overwrite
  may not allow and deny the same bit, nor carry a community-wide bit (§ Permissions), both `400`.
  A second `PUT` for the same target replaces the pair. A member's own overwrites (kind `1`) in
  every channel of the community are deleted with the membership, in the same transaction as a
  kick, a ban or a leave, so a member who rejoins starts without them.
- **No self-lockout.** A `PUT` or `DELETE` after which its author, neither the owner nor a holder
  of `administrator`, would no longer hold `view_channel` in the channel is `400`. It is checked
  after every rule above, so their `403` comes first, and an overwrite on oneself is no exception
  (the rank rule refuses it anyway).
- **Reading overwrites.** A client uses them with § Permissions to fold pins and `@everyone` (`04`).

### Bans

Every route below is `E`. A caller who is not a member of the community answers `404 E_NOT_FOUND`
exactly as an unknown community does. The three ban routes need `ban_members` community-wide and
the kick needs `kick_members` (§ Permissions); `403 E_FORBIDDEN` otherwise.

| Method and path | Request | Response |
|---|---|---|
| `PUT /v1/communities/{id}/bans/{user_id}` | `[reason(tstr), expires(uint\|null)]` | `204` |
| `DELETE /v1/communities/{id}/bans/{user_id}` | — | `204`; `404` when there is no such ban |
| `GET /v1/communities/{id}/bans` | — | `[[user_id, reason, by_user, created, expires(uint\|null), username(tstr), display(tstr)]]`, newest first; `display` is `""` for a deleted account, whose `username` stays (§ Accounts); both are `""` only if the account row is gone |
| `DELETE /v1/communities/{id}/members/{user_id}` (kick) | — | `204`; `404` when the target is not a member |

- **Rank.** Nobody may ban or kick the owner, and anyone but the owner acts only on a user whose
  highest role is strictly below their own; a caller who holds no role but `@everyone` acts on
  nobody, and nobody acts on themself. `403 E_FORBIDDEN`.
- A **ban** needs an account of this instance (`404` otherwise) but not a member: a ban may
  precede a join. It removes the membership, and `join` refuses the account while the ban stands.
  A second `PUT` replaces the reason, the moderator, the time and the expiry.
- `reason` is at most 512 bytes of UTF-8 with no control character but line breaks and tabs.
  `expires` is a unix second in the future and at most 3 153 600 000 seconds (a century) away, or
  null for a ban that stands until it is lifted; anything else is `400 E_INVALID_REQUEST`. A ban
  whose `expires` has passed no longer refuses a join but stays listed, as the moderation history,
  until it is lifted.
- A **kick** removes the membership and writes no ban: the user may join again at once. A kick,
  or a ban of a member, removes the membership and, after the commit, the instance sends
  `community_changed` (`02`) to the remaining members and once to the removed user.
- Lifting a ban re-adds the user to nothing; they join again.
- After a ban, a kick or a `leave` has committed, the instance, as the external sender (`02`
  § Roles), issues one `Remove` proposal for every live leaf of the user in every `text` and `call`
  group of the community's channels. Each freezes its group (`02` invariant 5) until a member
  commits it, so a user kicked while offline cannot read past the epoch that removes them; a
  `Remove` whose leaf is already gone is dropped (`02` invariant 6). The user's channel membership
  rows and their own (kind 1) channel overwrites for the community go in the same transaction as
  the membership, so a later rejoin starts from the roles and `@everyone` alone. A kind-1
  overwrite whose target is not a member can still be deleted (the target's rank counts as 0);
  writing one still needs a member. The `Remove`s do not depend
  on the request: a client that disconnects after the `204` (or before it) does not cut them short,
  and a `Remove` that is still never issued is proposed by the instance's sweep, which, a page of
  groups at a time, proposes a `Remove` for every live leaf of an open `text` or `call` group whose
  user the channel ACL no longer admits and that no outstanding instance `Remove` targets.
- Every ban, lift and kick writes an audit row naming the moderator (`ban.create`, `ban.delete`,
  `member.kick`).

### DMs

A DM (`kind = 3`, two participants) or group DM (`kind = 4`, three or more) is a channel with no
community: `community_id` is null, `mode` is always `0` (end-to-end encrypted) and `visibility`
`0` (private). Its participants are the only users who see it; to anyone else every route below
and `GET /v1/channels/{id}` answer `404 E_NOT_FOUND`, exactly as an unknown channel does. Every
route is `E`.

| Method and path | Request | Response |
|---|---|---|
| `POST /v1/dms` | `[recipients([bstr16])]` | `201 [channel_id(bstr16)]` new; `200 [channel_id]` the existing 1:1 DM |
| `GET /v1/dms` | — | `[[channel_id, kind(uint), members([bstr16])]]`, newest first, ties broken by `channel_id` |
| `GET /v1/channels/{id}/members` | — | `[[user_id(bstr16)]]`, ordered by `user_id` (bytewise) |
| `PUT /v1/channels/{id}/members/{user_id}` | — | `204` |
| `DELETE /v1/channels/{id}/members/{user_id}` | — | `204`; `404` when the user is not a participant |

- The participants of `POST /v1/dms` are the caller and `recipients`, duplicates dropped. Fewer
  than two (no recipient but the caller) is `400 E_INVALID_REQUEST`, and so is more than the
  instance's group-DM cap, `livekit.max_voice_participants` (so that every participant fits in the
  DM's call). An unknown account is `404`; a disabled or deleted one is `403 E_FORBIDDEN`
  (a disabled account is v1's block).
- A **1:1 DM** is idempotent: its `channel_id` is derived, not random, as the first 16 bytes of
  `SHA-256("dilla dm v1" || lo || hi)`, where `lo` and `hi` are the two user ids sorted bytewise
  and the label is its 11 ASCII bytes. Either participant opening it again, from either side, gets
  `200` and the same id, which is also the `target_id` of its `text` and `call` groups
  (`01` § dilla_binding). The id is a name, not a capability: it opens nothing to a
  non-participant. A group DM's `channel_id` is random, because its membership changes.
- A DM's `text` and `call` groups are registered by a participant (`02` invariant 1) with a
  binding whose `community_id` is null and whose `target_id` is the `channel_id`; the instance
  proposes the `Add` of every other participant's devices (below). A participant's client never
  `Add`s anyone to a DM's `text` group (`01` "Client policy for proposals from members"): a device
  enters it by the Welcome of the commit that carries the instance's `Add`, or by its own external
  commit.
- The member routes set a **group DM**'s participants; any participant may add or remove any
  other, and themself. A 1:1 DM's pair is fixed, and a community channel's membership follows its
  permissions: `PUT` and `DELETE` on either are `403 E_FORBIDDEN`. `GET` lists a community
  channel's members for anyone who may view it. Adding past the cap is `400`; adding an unknown
  account `404`, a disabled or deleted one `403`; adding a participant again changes nothing.
- After a participant is added or removed and the change has committed, the instance, as the
  external sender (`02` § Roles), proposes an `Add` for every device of every participant that
  holds an available KeyPackage and is not already in the DM's `text` group, and a `Remove` for
  every live leaf of a user who is no longer a participant in its `text` and `call` groups. An
  `Add` or `Remove` the instance already has outstanding is not proposed again. A device with no
  available KeyPackage is not proposed; it is added by a later change once it has published one.
- Opening a DM and every add and remove write an audit row (`dm.create`, `channel.member.add`,
  `channel.member.remove`).

### Readable channels

A server-readable text channel (`kind = 0`, `mode = 1`; every `invite` or `discoverable` channel
is one) carries its messages through the routes below instead of an MLS group. The instance
stores each envelope (`04`) as sent and indexes its text for search. Every route is `E`. A caller
who may not view the channel gets `404 E_NOT_FOUND`, as for an unknown channel; for any other
channel — an end-to-end encrypted one, a voice channel or a category — every route answers
`403 E_CHANNEL_MODE`, and an end-to-end encrypted channel's messages go through
`POST /v1/groups/{id}/message` (`02`).

| Method and path | Request | Response | Permission |
|---|---|---|---|
| `POST /v1/channels/{id}/messages` | `[envelope(bstr)]` | `[seq(uint), franking_tag(bstr 32), recv_ts(uint)]` | `send_messages`; `pin_messages` for a type 5 or 6 envelope, `add_reactions` for a type 3 or 4 |
| `GET /v1/channels/{id}/messages?from=&limit=` | — | `[[seq, sender(bstr16), envelope(bstr), franking_tag(bstr 32), created, edited\|null, deleted\|null]]` | `read_history` |
| `PATCH /v1/channels/{id}/messages/{seq}` | `[envelope(bstr)]` | `204` | the author, with `send_messages` |
| `DELETE /v1/channels/{id}/messages/{seq}` | — | `204` | the author only (`403 E_NOT_UPLOADER` for anyone else) |
| `GET /v1/channels/{id}/search?q=&limit=&before=` | — | `[[channel_id, seq, sender, snippet(tstr), score_micros(uint), created]]` | `read_history` |
| `PUT /v1/channels/{id}/read-state` | `[last_read_seq(uint)]` | `204` | `view_channel` |

- **Posting.** `seq` is the channel's own sequence (`GET /v1/channels/{id}` element 10), one step
  per message; `recv_ts` is the instance's clock and is the message's `created`. The instance is a
  receiver in `04`'s sense and refuses exactly what a client refuses, storing nothing partial: an
  envelope that is not a nine-element deterministic CBOR array, or any element of the wrong major
  type or length — `msg_id` and a non-null `thread_id` or `reply_to` 16 bytes, `k_f` 32, an
  attachment's `blob_id` 32, `key` 32 and `nonce` 12, an attachment a nine-element and a preview a
  four-element array — and an envelope of type 1 to 6 whose `reply_to` is null or that carries an
  attachment or a preview (`04` § Envelope) is `400 E_ENVELOPE_SHAPE`; a `type` above 6 is
  `400 E_ENVELOPE_TYPE`; and any bound of `04`'s § Envelope limits (`body` per type, 4 attachments,
  2 previews, `mime`, `name`, `thumb`, `url`, `title`, `description`, preview `image`) is
  `400 E_ENVELOPE_LIMIT`. The envelope of a `PATCH` is refused by the same rules. The body cap is
  96 KiB (§ Rate limits). Only a message's (type 0) or an edit's (type 1) `body` is search content,
  and only there does the instance count mentions: the number of distinct `<@…>` targets (a
  32-hex-digit user or role id, `everyone` or `here`), stored with the message for moderation.
- **Message references.** On a server-readable channel an envelope's `reply_to` and `thread_id`
  carry the target's channel `seq` as a big-endian uint64 in the low eight bytes, with the high
  eight bytes zero; in an end-to-end encrypted group they carry the target's `msg_id` unchanged,
  because there the server has no key to resolve (`04` § Envelope).
- **Envelope types.** A reaction (3, 4) needs `add_reactions` and a pin or unpin (5, 6) needs
  `pin_messages`; both are appended like a message, and clients fold them. An edit (1) or a delete
  (2) on `POST` is the alias of `PATCH` or `DELETE` on the `seq` its `reply_to` names: it is applied
  in place, not appended, and answers `204` with no body. A `reply_to` whose high eight bytes are
  not zero is `400 E_ENVELOPE_SHAPE`; a missing one is refused for every type 1 to 6 under Posting.
  An alias delete, being a `POST`, also needs `send_messages`.
- **Editing** is the author's alone (`403 E_FORBIDDEN` for anyone else) and takes a type 0 or 1
  envelope (`400 E_ENVELOPE_TYPE` otherwise). The instance re-franks the new envelope: `T` is
  recomputed over its `C`, the editing device and the edit's time, which the message records as
  `edited`, under the current franking key, and the key id is recorded with it. A tag over the
  replaced bytes could never verify.
- **Slow mode.** Inside `slowmode_seconds` of the caller's previous message in the channel, a post
  is `429 E_RATE_LIMITED` with `retry_after_ms` the time left; `bypass_slowmode` exempts the caller
  from slow mode only, never from `send_messages`. A
  deleted message still counts, so deleting and reposting does not reset the wait. Slow mode and
  `send_messages` apply to end-to-end encrypted posts too, through the upload class (`02` § Upload
  class): an end-to-end encrypted upload of class 1 (an edit, delete, reaction or pin) is neither
  checked for `send_messages` nor slowed; a two-element upload that names no class is refused
  (`400 E_INVALID_REQUEST`) from wire version 2.
- **Franking.** A readable envelope carries its `k_f` in the clear, so the instance computes `04`'s
  commitment `C` itself, unchanged. The tag `T` is `04`'s with two substitutions, because a
  readable channel has no MLS group: `channel_id` in place of `group_id`, and `0` for the epoch. The
  instance records which of its franking keys made the tag, so a report still verifies after a
  rotation.
- **Deleting** tombstones the message: its envelope becomes empty, it leaves the search index, and
  `GET` lists it with its `deleted` time; the franking tag stays for a report. Delete-for-everyone is
  the uploader's alone: anyone else, `manage_messages` or not, is `403 E_NOT_UPLOADER` (moderator
  deletion needs a signed moderation event, which this version does not define). An unknown or
  already deleted `seq` is `404`, and so is an edit of a deleted message. `{seq}` that is not a decimal uint
  is `400 E_INVALID_REQUEST`.
- **Live delivery.** Every accepted post, edit and delete is sent as `message.plain` (op 32, `02`)
  to every live connection of every user who may view the channel and is still a member of its
  community, the author included; `edited` and `deleted` are `1` on an edit's and a delete's frame,
  and a delete's `envelope` is empty.
- **Listing.** `from` is the first `seq` wanted (default: the first message); `limit` defaults to
  50 and is capped at 256.
- **Search.** `q` is words: a trailing `*` makes a word a prefix, text in double quotes is a phrase
  (`""` inside it is a quote), and a leading `-` excludes the word or phrase after it; every other
  character separates words. Matching folds case and diacritics (`haller` finds `håller`) and does
  not stem. A query with no word at all is `400 E_INVALID_REQUEST`; a query of exclusions only
  answers every message in scope without them. The scope is every server-readable text channel of
  `{id}`'s community the caller may view and read the history of. Hits are best first, ties broken
  by the newer `seq`; a `limit` outside 1 to 100 is 50; `before` returns only hits whose `seq`
  is below it. The snippet marks each match with `[` and `]`, and `score_micros` is the relevance
  score times 10^6, rounded, because deterministic CBOR carries no floats.
- **Read state** never moves backwards and never past the channel's newest `seq`: a lower value
  is ignored and a higher one is lowered to it.

### Blobs

An attachment's ciphertext (`04` § Envelope, `attachments`) is stored under its own SHA-256, `blob_id`, and
published into a channel by a **reference**. The reference is the access rule: a blob is read only
through a channel that holds a reference to it and that the caller may read. Every route is `E`,
works for a text channel of either mode, a voice channel and a DM, and answers `404 E_NOT_FOUND`
to a caller who may not view the channel, as for an unknown one; a category holds no attachments
(`403 E_FORBIDDEN`). `{blob_id}` is 64 lowercase hex characters (§ Identifiers). Bodies are raw
octets, `Content-Type: application/octet-stream`, never CBOR; the `PUT` answer and every refusal
are CBOR as everywhere else.

| Method and path | Request | Response | Permission |
|---|---|---|---|
| `PUT /v1/channels/{id}/blobs/{blob_id}` | the ciphertext | `201 [blob_id(bstr 32), size(uint)]` when the bytes are new, `200` with the same body when they were already stored | `attach_files` (`409 E_NOT_UPLOADER` when the channel's reference to the blob is another user's) |
| `GET /v1/channels/{id}/blobs/{blob_id}` | — | `200` the ciphertext, or `206` for a `Range` request | `read_history` |
| `HEAD /v1/channels/{id}/blobs/{blob_id}` | — | `200` with the `GET` headers and no body | `read_history` |
| `DELETE /v1/channels/{id}/blobs/{blob_id}` | — | `204` | `view_channel`, and the uploading user only (`403 E_NOT_UPLOADER` for anyone else) |
| `POST /v1/channels/{id}/blobs/{blob_id}/confirm` | — (no body) | `204` | `view_channel`, and the uploading user only (`403 E_NOT_UPLOADER` for anyone else) |

- **Uploading.** The instance hashes the body while it writes it and keeps it only when the digest
  equals `{blob_id}`; otherwise the answer is `422 E_INVALID_REQUEST` and nothing is stored. The
  body is hashed even when the blob is already stored: the upload is the proof that the caller
  holds the bytes, so knowing a `blob_id` is never enough to publish it into another channel, and
  a forward re-uploads. A body over `blobs.max_blob_bytes` is `413 E_TOO_LARGE`; a `Content-Type` other than
  `application/octet-stream` is `415 E_INVALID_REQUEST`; bytes an instance administrator removed
  are `410 E_PRUNED`, because content addressing would otherwise hand the removed name straight
  back. The reference a `PUT` creates is **pending**: it is read and deleted like any other, and
  its uploader confirms it before sending the message that names it. A channel holds one reference
  to a blob and it belongs to the user whose device made it: a `PUT` by that user into a channel
  that already holds their reference adds nothing and leaves it as it is, pending or confirmed
  (`200`); a `PUT` into a channel whose reference another user made is `409 E_NOT_UPLOADER` and
  leaves that reference as it is, so nobody else's upload can take over, or make expire, a
  reference a user confirmed or still has to. A forwarded attachment is therefore re-encrypted
  under a fresh key and re-uploaded as new bytes, never re-`PUT` as the same ciphertext. A
  client that uploads bytes for any reason — a forward, a restore, a readable-channel client, a
  test peer — confirms its reference; an unconfirmed reference expires after `blobs.pending_ttl`.
- **Quota.** `blobs.quota_bytes_per_user` bounds the ciphertext bytes of the distinct blobs a user
  references, from any of their devices, and of their backup objects (§ Backups); a blob in several
  channels counts once. An upload that
  would pass it is `507 E_STORAGE_FULL`, and so is any upload once it is reached; an upload that
  announces a `Content-Length` taking the user past the quota is refused before its body is read.
  `blobs.store_max_bytes` bounds the whole instance the same way (every stored blob counts,
  including one no channel references any more), `507 E_STORAGE_FULL` before the body is read.
- **Upload rate.** Each user may start `blobs.uploads_per_minute` uploads a minute and upload
  `blobs.upload_bytes_per_day` bytes a day (both refill continuously), attachment `PUT`s and the
  root backup `PUT` (§ Backups) drawing on the one budget; over either the answer is
  `429 E_RATE_LIMITED` with its `retry_after_ms`, before the body is read. Every byte the instance
  reads spends the day's budget, whether the upload is stored, refused or later deleted: deleting
  an attachment frees quota, never budget.
- **Reading.** `GET` answers `404 E_NOT_FOUND` whenever this channel holds no reference, even when
  the bytes exist, so the answer never says that an unreachable blob exists; `HEAD` has the same
  rule. The response carries `Content-Type: application/octet-stream`,
  `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff`,
  `Content-Security-Policy: default-src 'none'; sandbox`, `Cross-Origin-Resource-Policy:
  same-origin`, `Cache-Control: private, max-age=31536000, immutable`, `Accept-Ranges: bytes`,
  `ETag: "<blob_id hex>"` and `Repr-Digest: sha-256=:<base64 of blob_id>:`, and no
  `Last-Modified`. `Range`, `If-Range`, `If-Match` and `If-None-Match` follow RFC 9110; an
  unsatisfiable range is `416` with a plain-text body, without the `ETag` and `Cache-Control`.
  A blob an instance administrator purged is `410 E_PRUNED` on `GET` and `HEAD` too.
- **Confirming.** `POST /v1/channels/{id}/blobs/{blob_id}/confirm` reads no body and answers `204`; confirming a confirmed reference is `204` again. No reference in this channel is `404 E_NOT_FOUND`; a reference another user's device made is `403 E_NOT_UPLOADER`; bytes an instance administrator purged are `410 E_PRUNED`. A pending reference older than `blobs.pending_ttl` (default 24 h; `0` turns this off) is removed by the instance as retention removes one, and the blob then follows the deletion rule below; references stored before this rule existed count as confirmed. The instance still never learns which message names a blob.
- **Deleting.** `DELETE` removes this channel's **reference**, never the bytes. Only the user
  whose device made the reference may delete it, from any of their devices; anyone else —
  including a holder of `manage_messages` and the community owner — is `403 E_NOT_UPLOADER`,
  because moderator deletion needs a signed moderation event this version does not have. Deleting
  a reference that is already gone is `204`, so a retry is harmless. When the last reference to a
  blob anywhere goes, the instance marks the blob unreferenced, and unlinks the file once
  `blobs.gc_grace` (default 24 h) has passed with no reference created in between; a `PUT` that
  re-uploads the bytes inside that window keeps them.
  A client deletes the references of a message it discards before sending it and of a message it
  deletes for everyone (`04` § Semantics).
- **Retention.** An attachment's retention is its community's archival retention: a reference
  older than the community policy's `retention_days` (§ Communities) is removed, and the blob then
  follows the deletion rule above. A community without `retention_days`, a DM and a group DM keep
  attachments indefinitely. Deleting a channel removes its references. Attachment retention never
  follows `02`'s 30-day delivery window, because an archive restore needs attachments far older.

### GIFs

An optional GIF picker, per server. A server manager enters that server's Giphy API key in its settings; a server
without a key has no GIFs (every search and media route answers `404 E_NOT_FOUND`), and the web client shows no
picker. A DM client may use a shared community's existing routes after explicit consent; there is no DM GIF route,
and the existing community membership checks still apply. Every route is `E`.

| Method and path | Request | Response | Errors |
|---|---|---|---|
| `GET /v1/communities/{id}/gifs` | — | `[gifs(uint 0 or 1), rating(tstr)]`: whether the server has a key, and the rating (`g`, `pg`, `pg-13` or `r`). Never the key | `404` to a non-member |
| `PUT /v1/communities/{id}/gifs/key` | `[key(tstr)]`, 8 to 64 letters and digits | `204` | `403 E_FORBIDDEN` without Manage Server (the owner holds it), `400 E_INVALID_REQUEST` |
| `DELETE /v1/communities/{id}/gifs/key` | — | `204` | `403 E_FORBIDDEN` |
| `PUT /v1/communities/{id}/gifs/rating` | `[rating(tstr)]` | `204` | `403 E_FORBIDDEN`, `400 E_INVALID_REQUEST` |
| `GET /v1/communities/{id}/gifs/search?q=&offset=` | `q` is at most 100 bytes of text and may be empty (trending); `offset` is `0..4999` | `[[[gif_id(tstr), title(tstr), width(uint), height(uint), path(tstr)]], next_offset(uint or null)]`; 24 results a page, `path` is this server's own media path | `400 E_INVALID_REQUEST`, `404`, `429 E_RATE_LIMITED` (per member across all servers, 10 then 1 a second), `503 E_UNAVAILABLE` with `Retry-After` |
| `GET /v1/communities/{id}/gifs/media/{gif_id}?r=preview` | `gif_id` is a Giphy id (letters and digits, at most 64); `r` is `preview` or `full` | `200` the GIF, `Content-Type: image/gif` (a preview is at most 5 MiB, a full GIF at most 25 MiB) | `400 E_INVALID_REQUEST`, `404`, `429 E_RATE_LIMITED` (per member across all servers, 60 then 20 a second), `503 E_UNAVAILABLE` with `Retry-After` |

- **The key is write-only.** It is stored in the instance's settings table, never read back out by any route, and is in
  no reply, header, log line, audit detail or diagnostics line. Setting, replacing and removing it, and changing the
  rating, each write an audit row (`community.gifs.key_set`, `community.gifs.key_remove`, `community.gifs.rating`)
  whose detail is empty. Deleting the server forgets the key.
- **What each party sees.** Search and the picked GIF go through the instance, so Giphy sees the instance and never a
  person. The instance sees the search terms and which GIF a member picked and when, and can probably link a pick to
  the attachment that follows from its timing and size; it cannot read the message or learn who reads it. The GIF
  is sent as an ordinary encrypted attachment (`04` § Envelope), so a reader's client never contacts Giphy.
- **Client privacy in DMs.** Opening a DM performs no GIF status, search or media request. Only an explicit GIF-button
  press may check shared communities' status, most recently active first, to find a key. That status request contains
  no DM id, peer, content or search text. The client then names the selected server in a privacy notice; no search
  (including trending), preview or full media request is permitted before **Use GIFs**. **Not now** stores no consent.
  No available key leaves the picker closed. Consent and the last disclosed proxy are persisted only in the existing
  client settings store, scoped to instance, account and device, never sent as a server setting or synced to another
  device. Settings → Privacy reflects and revokes it; enabling there still requires the named-server notice before
  loading GIFs. A different selected proxy requires a fresh notice. Revocation stops subsequent requests and discards
  pending downloads; a file already added to the attachment tray remains there, and an already dispatched request
  cannot be recalled. The proxy and Giphy observe search text and media
  identifiers outside DM E2EE. Correlating a full fetch with the following encrypted upload's timing and size can let
  the instance infer which GIF was attached to the DM; encryption does not hide that correlation.
- **No URL comes from the caller.** The instance builds the one upstream URL from the checked `gif_id` and a host on a
  fixed allow-list of Giphy's own hosts, over HTTPS; a redirect off the list, a non-GIF content type or magic, a body
  over the cap (counted on the bytes, not the header), a timeout, a connection to a non-public address, or no free
  fetch slot is `503 E_UNAVAILABLE`.
- **Fetch slots.** A media or search request holds one of eight instance-wide slots (at most three per member and four per server) from
  before the upstream request until its reply is written; a request waits briefly for a slot before the `503`. The
  rating filters search; a media route fetches any Giphy id a member asks for.

### Backups

The two header objects of `06-backup-archive.md` ("Header"), for the session's own user: no route
names a user. `{kind}` is `0` (the root object) or `1` (the state object) and `{chunk_seq}` is `0`,
both as plain decimal path segments; any other value of either, kind `2` (archive chunks)
included, is `404 E_NOT_FOUND`: this wire version serves no archive. The instance stores each
object under `SHA-256(object)` in its blob store and cannot open it.

| Method and path | Auth | Request | Response |
|---|---|---|---|
| `PUT /v1/backups/{kind}/{chunk_seq}` | E | `[object(bstr)]` | `201 [blob_id(bstr 32), size(uint)]` when the user had no object of this kind, `200` with the same body when it was replaced or already held these bytes |
| `GET /v1/backups` | E or P | — | `[[kind(uint), chunk_seq(uint), size(uint), created(uint)]]`, kind `0` first; `[]` when none |
| `GET /v1/backups/{kind}/{chunk_seq}` | E or P | — | `[object(bstr), created(uint)]`; `404 E_NOT_FOUND` when none is stored |
| `DELETE /v1/backups/{kind}/{chunk_seq}` | E | — | `501 E_INTERNAL`: not served at wires 1 and 2 |

- **Shape.** `object` is a stored object of `06`, `[1, nonce(bstr 12), ciphertext(bstr)]` in
  deterministic CBOR with at least 16 bytes of ciphertext; anything else is `400 E_INVALID_REQUEST`.
  A root object is exactly 103 bytes (`400 E_INVALID_REQUEST` otherwise). The body is at most 256
  bytes for kind `0` and 1,048,640 bytes for kind `1` (`413 E_TOO_LARGE`).
- **The root object is written once.** The first `PUT` of kind `0` stores it; the same bytes again
  answer `200` and change nothing; different bytes answer `409 E_INVALID_REQUEST` and the stored
  object stays. A stolen enrolled session can upload a junk root before the owner's first upload;
  the owner's first upload then gets `409`, fetches the stored root, compares it, and raises
  `E_ROOT_MISMATCH` with an alert. The server's `409` detail reveals nothing about stored bytes.
- **The state object is replaced.** Each `PUT` of kind `1` replaces it; the replaced bytes, when
  nothing else names them, are deleted at once (their blob row in the replacing transaction, their
  file right after it, after a re-check that no reference or backup object names them again),
  never kept for `blobs.gc_grace`. One user's state replacements are serialised, so concurrent
  `PUT`s from two devices leave the last committed object and no other. An upload of the same bytes
  (a backup or an attachment `PUT`) racing that delete either records before it and keeps the
  bytes, or finds them gone in its own transaction and answers `503 E_UNAVAILABLE` with a short
  `retry_after_ms`, recording nothing; sent again, the bytes are stored anew.
- **Storage.** Backup objects count against `blobs.store_max_bytes` (new bytes that would pass it
  are `507 E_STORAGE_FULL` and nothing is stored) and toward the uploader's
  `blobs.quota_bytes_per_user` together with their attachments, each distinct blob once (§ Blobs);
  an upload that would take the user past it is `507 E_STORAGE_FULL` and the stored object stays.
  A replaced state object leaves the count, and the quota check for a replacement is the delta over
  the object it replaces: a replacement no larger than its predecessor never fails the quota, even
  for a user already at or past it, so re-sealing the state object (which revoking another device
  starts with) is never refused for storage. An object whose bytes an instance administrator removed (§ Admin) is `410 E_PRUNED` and nothing
  is stored, as for a blob: content addressing would otherwise hand the removed name straight back.
- **Pending.** The two reads admit a `pending` session (`02` § Device sessions item 4), so a
  device entering the recovery key reads both objects before it holds a credential; `PUT` and
  `DELETE` need an `enrolled` one (`403 E_FORBIDDEN`).
- Every route spends the device session's `read` or `write` bucket (§ Rate limits). A `PUT` of the
  root (kind `0`) also spends the user's blob upload budget exactly as an attachment upload does
  (§ Blobs, "Upload rate"): one upload of `blobs.uploads_per_minute` and the bytes it reads of
  `blobs.upload_bytes_per_day`, `429 E_RATE_LIMITED` with its `retry_after_ms` over either, before
  the body is read. A `PUT` of the state object (kind `1`) does not touch that shared budget: on
  it, a stolen session of the same user could keep it empty and so refuse the owner the state
  `PUT` that revoking the thief's device starts with. It spends the device session's `write`
  bucket and a per-device budget of 64 MiB a day (67,108,864 bytes, refilling continuously, every
  byte read counted), `429 E_RATE_LIMITED` with its `retry_after_ms` past it, before the body is
  read; another device of the same user keeps its own. With the replaced object deleted at once,
  one device's state `PUT`s hold at most one state object on disk.

### Voice

A call happens in a voice channel, a DM or a group DM, and its media keys come from the channel's
**call group** (`01`, group kind 1), which its clients register as for any group. A call is keyed by
the call group's call id (`mls_groups.call_id`, the channel id for a call group the instance
registered), so every device of the group lands in the same call. Every route below is `E`; a
caller who may not view the channel gets `404 E_NOT_FOUND`, as for an unknown one.

| Method and path | Request | Response | Permission |
|---|---|---|---|
| `POST /v1/channels/{id}/calls` | `[]` or `[vdec(tstr)]` | `201 [call_id(bstr16), group_id(bstr16), livekit_url(tstr), token(tstr), ice_servers([[urls([tstr]), username(tstr), credential(tstr)]]), caps([max_audio_bitrate_bps(uint), max_share_bitrate_bps(uint), vp9(uint)])]` when the call is opened, `200` with the same body when it is already live; a `201` that opens a DM or group-DM call carries a seventh element, `rung([user_id(bstr16)])`, the users this start rang (see **Ringing**); `409 E_CALL_FULL`; `429 E_RATE_LIMITED` with `retry_after_ms` (a few milliseconds, transient) when the device was cut from the relay in the same millisecond or while the request was served; after the relay credential is minted, the refusal its gate would have given (`403 E_FORBIDDEN` barred, `403 E_LEAF_NOT_CURRENT`, `404 E_NOT_FOUND` without `view_channel`, `403 E_FORBIDDEN` without `connect`) when that changed while the request was served; `503 E_UNAVAILABLE` with `retry_after_ms` when the call ended as the request opened it (see **Ending**) | `connect`, and a current leaf of the call group |
| `POST /v1/calls/{call_id}/share` | `[]` | `204` once the device holds a sharing slot and the SFU holds its new permission; `409 E_CALL_SHARERS_FULL`; `404 E_NOT_FOUND` when the call has ended or the device is not in its room; `403 E_FORBIDDEN` while the device's removal or demotion in the call is pending or the device is barred | `connect`, `video` or `screen_share`, and a current leaf of the call's group |
| `DELETE /v1/calls/{call_id}/share` | — | `204`, also when the device held no slot or the call has ended | `view_channel` |
| `POST /v1/calls/{call_id}/rtt` | `[ms(uint)]` or `[]` | `204` once the SFU carries `ms` (0..5000, the device's round-trip time to the SFU in whole milliseconds) as the device's participant attribute `dilla.rtt`, or, with `[]`, no longer carries one (the client is paused or reconnecting; a clear is never rate limited and is a no-op when nothing is set); only the caller's own identity and only that key are written, through the SFU's admin API; `400 E_INVALID_REQUEST` for any other body; `429 E_RATE_LIMITED` with `retry_after_ms` for a second value within 1.5 s; `404 E_NOT_FOUND` when the call has ended or the device is not in its room | `view_channel`, and a current leaf of the call's group |
| `POST /v1/calls/{call_id}/stats` | `[candidate_type(uint), relay_protocol(uint\|null), rtt_ms(uint), fraction_lost_permille(uint), decrypt_failures(uint), frames_encrypted(uint)]` | `204`; `429 E_RATE_LIMITED` above one report per device per 5 s; `404 E_NOT_FOUND` once the call has ended; `403 E_FORBIDDEN` for a barred device; `400 E_INVALID_REQUEST` for `decrypt_failures` or `frames_encrypted` above 1048576 | `view_channel` and `connect`, and a current leaf of the call's group |
| `DELETE /v1/calls/{call_id}` | — | `204`, also when the call has already ended or its call group is closed; `403 E_FORBIDDEN` for a barred device and for a device that is listen-only in the call's room for media that is not dilla's | `view_channel` and `connect`, and a current leaf of the call's group while that group is open |
| `DELETE /v1/calls/{call_id}?if_alone=1` | — | as `DELETE /v1/calls/{call_id}`, but a live call ends only while the caller's device holds a current leaf and no other non-bot member remains (managed-bot leaves are ignored), checked under the group lock that every commit takes; `409 E_CALL_NOT_ALONE` otherwise, and nothing ends; `400 E_INVALID_REQUEST` for any other value of `if_alone` | as `DELETE /v1/calls/{call_id}` |
| `GET /v1/channels/{id}/call` | — | `200 [group_id(bstr16\|null), call_id(bstr16\|null), participants([[user_id(bstr16), device_id(bstr16), flags(uint)]])]`; `400 E_INVALID_REQUEST` for a text channel or a category | `view_channel`, or a participant of the DM or group DM |
| `GET /v1/communities/{id}/calls` | — | `200 [[channel_id(bstr16), call_id(bstr16), participants]]`, the live calls of the voice channels the caller may view, sorted by `channel_id`, unpaginated; `404 E_NOT_FOUND` for a non-member | membership |
| `POST /v1/calls/{call_id}/decline` | `[expires_ts(uint)]` | `204`, also when the user was no longer ringing or `expires_ts` names no ring; `400 E_INVALID_REQUEST` for a call of a community voice channel or a malformed body; `404 E_NOT_FOUND` for an unknown or ended call | a participant of the call's DM or group DM |
| `GET /v1/communities/{id}/afk/settings` | — | `200 [channel_id(bstr16\|null), timeout_seconds(uint)]`: the built-in AFK channel and its timeout (default `300`) | `manage_community` |
| `PUT /v1/communities/{id}/afk/settings` | `[timeout_seconds(uint)]`, or `[channel_id(bstr16), timeout_seconds(uint)]` when `channel_id` is the built-in AFK channel | `204`; timeout is one of `60, 300, 900, 1800, 3600` (there is no "off"); the channel is never chosen here: `400 E_INVALID_REQUEST` `the AFK channel is built in; send [timeout_seconds]` for any other `channel_id`, `null` included, and `400 E_INVALID_REQUEST` for any other timeout | `manage_community` |
| `GET /v1/communities/{id}/afk` | — | `200 [channel_id(bstr16\|null), timeout_seconds(uint), [], pending(uint), away([[user_id(bstr16), since_ts(uint)]])]`: the built-in AFK channel; the third element is always the empty array (web3.8's retired occupant list, kept so a web3.8 parser still reads it); `pending` is the calling device's pending cut, a reason code (the dilla CBOR subset has no booleans): `0` none, `1` the sweep moved the device, `2` the device was cut because its user chose AFK (`PUT …/afk/away`); it is device-scoped, so every community's snapshot reports it to that device, a read does not consume it, and it lasts through gateway disconnects until an admission of that device clears it, at most 24 hours (held in memory; a restart drops it with the calls); a client reads any value it does not know as pending; `away` is the community's away roster (below), sorted by `user_id`, at most 1024 entries, `since_ts` the unix second each member became away. A client that reads four elements is unaffected. A community with no live AFK channel gets one here first; `channel_id` is `null` only if that fails. `away` is empty whenever `channel_id` is `null` for the caller, so the roster goes only to those who may reach the AFK channel, the audience of `afk_away` | community membership |
| `PUT /v1/communities/{id}/afk/away` | `[]` | `204`; the caller's user goes to the AFK channel: the user is first marked away in this community (a member already away keeps the earlier `since_ts`; a user who holds no gateway connection, or one of whose devices was admitted to a call meanwhile, is not marked), then every device of the user is cut from every call room it is in (a community's or a DM's, in any community). Each device it cuts gets pending `2` (the snapshot above) before its removal, so its first read already says why; so does every other device of the user that was admitted to a call in the last 2 minutes and that no room lists now (a call recovering after the media server dropped it), so that its recovery does not rejoin. A cut that fails is logged, the device keeps pending `2`, and the answer is still `204`. `400 E_INVALID_REQUEST` for any other body; `404 E_NOT_FOUND` for a non-member (also when a kick lands during the request) and for a community whose AFK channel the caller cannot reach. There is no `DELETE`: only the instance clears away | community membership |
| `POST /v1/communities/{id}/afk/activity` | `[channel_id(bstr16)]` or `[channel_id(bstr16), exempt(uint)]` | `204`; records this authenticated device's activity in that live community voice call. `exempt` is `0` or `1` (`400 E_INVALID_REQUEST` otherwise, and for a CBOR boolean); with `1` the device is never moved out of that call's room, even when the report precedes its admission | community membership |
| `POST /v1/communities/{id}/afk` | any | `404 E_NOT_FOUND` `retired in web3.9` (web3.8's manual move) | — |
| `DELETE /v1/communities/{id}/afk` | — | `404 E_NOT_FOUND` `retired in web3.9` (web3.8's leave) | — |

- **The built-in AFK channel.** Every community has exactly one AFK channel, written by the
  instance: a voice channel named `AFK`, end-to-end encrypted and private, at the top level, at
  position `2^30` (clients skip it when they compute the next position), with no topic, no slow
  mode and no overwrites. A new community gets it in the transaction that creates the community; an
  existing one (a web3.8 community, or one whose pointer went missing) gets it once when the
  instance starts, and the snapshot creates it lazily if it is still missing. A web3.8 community
  whose administrator had picked an AFK channel gets a new built-in channel with the stored timeout,
  and the old channel becomes an ordinary voice channel (its name, overwrites and live call stay).
  An overwrite left on the built-in channel (by a rollback to an older release) is removed at the
  next start, which also puts back its built-in mode, visibility, topic and slow mode.
- **What may change.** Administrators may rename it and move it to another category (under the
  category rule of § Channels); its position stays `2^30` (§ Channels), and the next start puts it
  back there if it is anywhere else. `DELETE /v1/channels/{id}` on it is `403 E_FORBIDDEN` `the AFK
  channel cannot be deleted`; a `PATCH` that names its topic, mode, visibility or slow mode is
  `403 E_FORBIDDEN` `the AFK channel keeps its kind, mode, visibility, topic and slow mode`; `PUT`
  and `DELETE /v1/channels/{id}/overwrites/{kind}/{target_id}` on it are `403 E_FORBIDDEN` `the AFK
  channel takes no permission overwrites`. A category delete leaves it at the top level, and a
  community transfer leaves it as it is.
- **Permissions and no calls.** Every member holds `view_channel` and `connect` in the AFK
  channel whatever their roles say (the owner and administrators keep every bit), so every member is
  in its `channel_members` and its call group. The AFK channel never hosts a call (R-AW1): `POST
  /v1/channels/{id}/calls` on it is `403 E_FORBIDDEN` `nobody joins a call in the AFK channel`, and
  so is the `/rtc` admission of a token minted for it, so no token for it is minted or admitted. A
  call an earlier release left live there ends when the instance starts, and every user with a
  device in its call group becomes away (below). The listen-only grant stays as a backstop: a grant
  re-push in such a room gives no source to publish, `POST /v1/calls/{call_id}/share` is `403
  E_FORBIDDEN` `nobody shares in the AFK channel`, and when the instance cannot read the AFK pointer,
  the grant is listen-only.
- **Away (AFK means disconnected).** A member who is *away* is listed under the community's AFK
  channel and has no voice connection. The instance stores away per community and user, and away and
  calls never overlap: if a user is away in any community, no device of that user is in any call
  (R-AW2). A user becomes away in a community (R-AW3):
  - by the sweep: its cut of an idle device (below) marks the user away there, `since_ts` the cut,
    unless another device of the user is in a call (listed in any room, or admitted to one within the
    last 30 seconds), an admission of the user came after the sweep decided, or the user holds no
    gateway connection; otherwise the idle device is only cut;
  - by choosing AFK: `PUT /v1/communities/{id}/afk/away`, which the client sends after it has left
    its call; the instance marks the user away and then cuts any device of the user still in a
    room.

  Away is cleared (R-AW4) by any admission of any device of the user to any call room, which clears
  every community; by a message upload (an end-to-end encrypted upload into a text group of one of
  the community's channels, or an accepted `POST /v1/channels/{id}/messages` in a readable one) or a
  `typing_set` on by the user in
  a channel of that community, which clears that community; by the user holding no gateway
  connection for 30 continuous seconds, which clears every community: a second open connection
  keeps it, a connection that comes back within the 30 seconds (a page reload, a short network
  drop) keeps it, and a later close starts the 30 seconds again; nothing is cleared this way while
  the instance shuts down; and by
  leaving the community, a kick or a ban (the row goes with the membership, and the remaining
  audience is sent `afk_away` with `since_ts` `0`). No client route clears it. Every change is announced with
  `afk_away` (`02` § Gateway frames, op 55) to every user who may view the AFK channel; a change that
  changed nothing is not. The roster is stored, so a restart keeps it for users who stay connected;
  60 seconds after the instance starts, the rows of users with no gateway connection are cleared and
  announced.
- **AFK lifecycle.** The instance keeps a server-side activity time for each device in community
  voice. Browser keyboard, pointer and touch input, and microphone speech report activity; listening
  alone does not. A device that cannot be idle-detected (a phone) reports `exempt` `1` once per join
  and is never moved. Every retry tick moves a device that exceeded the community's timeout, even
  when its browser timers are suspended; the AFK channel's own calls are never tracked. The server
  decides a move and the client carries it out, because the server cannot add a device to an
  end-to-end encrypted call group: the instance removes the device from its SFU room, revokes its
  relay, marks a pending move for that device alone (`pending` `1`; it lasts through disconnects and
  reads until an admission of that device, at most 24 hours), marks the user away as above, and
  sends `community_changed` (`02`) to that user's live connections. The client reads `pending` from
  the snapshot only to tell the user why the device left its call (`1` moved by the sweep, `2` chose
  AFK), never to join anything; any admission of that device to a room clears it. Only the sweep
  (`1`) and `PUT …/afk/away` (`2`, for each device it cuts) set `pending`. Settings changes send
  `community_changed` to the members.

- **Discovery and snapshot.** `GET /v1/channels/{id}/call` tells a client which call group to join
  or register and who is in the channel's call. `group_id` is the live call's group; with no live
  call, the channel's newest open `call` group; `null` when there is none, which means "register
  one". `call_id` is the live call, or `null`. `participants` are the devices in the live call's
  room with their current `voice_state` flags (`02` § Gateway frames), sorted by `user_id` then
  `device_id`, at most `livekit.max_voice_participants`. A caller who may not view the channel, or
  is no participant of the DM or group DM, gets `404 E_NOT_FOUND`; a text channel or a category is
  `400 E_INVALID_REQUEST`. `GET /v1/communities/{id}/calls` gives the same `call_id` and
  `participants` for every live call of a voice channel of the community that the caller may view,
  sorted by `channel_id` and unpaginated; a non-member gets `404 E_NOT_FOUND`. Both are on the
  `read` bucket. A client reads the snapshot first and then applies the `voice_state` frames it
  received during the read, in order (`02` § Gateway frames).
- **Ringing.** A start that **opens** a call (`201`) in a DM or group DM rings every other
  participant who has no device in the call: each of that user's online devices receives
  `call_ring` state 1 (`02` § Gateway frames, op 53) with `expires_ts` 30 seconds ahead. A `200`
  start of a live call rings no one, and there is at most one ring per (call, user). A user's ring
  stops — state 0 to every device of that user — at the first of: a device of that user is
  admitted to the call's room, the call ends, 30 seconds pass on the instance clock, or the user
  declines. `POST /v1/calls/{call_id}/decline` carries the `expires_ts` of the ring it answers,
  because a call id is the channel id for every call of that channel: a late decline of an earlier
  ring must not stop the next ring of the same DM or tell its caller "declined". The decline acts
  only on the ring whose `expires_ts` equals the body's; any other value is `204` with no effect.
  It stops that user's ring (state 0 to every device of the user) and sends state 2, naming the
  decliner, to every device of the caller. It is idempotent and on the `write` bucket; only a
  participant of the call's DM or group DM may call it (anyone else, an unknown call and an ended
  call get `404 E_NOT_FOUND`), and a call of a community voice channel is `400 E_INVALID_REQUEST`.
  **The pair bound.** Any user may open a DM with any account on the instance (§ DMs), and any
  group-DM participant may add anyone, so the instance rings a given (caller user, recipient user)
  pair at most once per 15 seconds and at most 20 times per rolling hour, across all channels. A
  start beyond the bound still opens the call (`201`) but sends that recipient no `call_ring`, and
  the instance logs one `INFO` line per suppressed pair per hour. The start's `rung` element names
  exactly the users it rang, so a recipient the bound (or a cap below) held back is absent from it
  and the caller's client never shows that recipient as ringing (added 2026-10-14). The bound is keyed by the pair
  only, never by the recipient alone, so no third party can spend a recipient's budget and silence
  honest callers. The state lives in memory, at most 65 536 pairs: a pair whose hour has passed is
  dropped first, one caller user holds at most 1 024 live pairs (a new recipient beyond that is not
  rung, for that caller only), and only a table full of live pairs evicts the oldest live pair. One
  caller user has at most 8 calls ringing at once; a further start by that caller opens the call
  but rings nobody. Every end of a call stops its rings, and of two starts that race to open the
  same call only the one whose write opened it rings.
  Rings reach only a device with an open gateway session; nothing is pushed to a closed client.
- **The leaf gate.** A token is minted only for a device whose leaf is in the call group's
  **current epoch**: added at or before it and not removed. Any other device — a removed one, one
  whose leaf the instance records only from a later epoch, one with no leaf, or any device while
  the group is epoch-unknown after a restore — is `403 E_LEAF_NOT_CURRENT`, so a device that
  cannot derive the call's media keys cannot join its room either. A channel of another kind is
  `400 E_INVALID_REQUEST`; a voice channel whose call group is not registered yet is
  `404 E_NOT_FOUND`. While a call is live, "the call group" is the one the call was opened on,
  never merely the newest open call group of the channel: a leaf of any other call group is
  `403 E_LEAF_NOT_CURRENT` for that call. A closed call group holds no leaf at all: no commit of it
  can land, so no device is admitted to a call on it. A live call whose group has been closed ends
  at the next start that passes the gate, which opens a fresh call on the channel's current call
  group, or at the room sweep (below), whichever comes first.
- **Capacity.** A start that finds `livekit.max_voice_participants` other devices already in the
  call's room is `409 E_CALL_FULL`. The count leaves out the caller's own device (a rejoin replaces
  its old session), disconnected participants and non-device participants; it is advisory and
  fails open when the SFU cannot answer. The SFU's own room cap is authoritative: a device that
  passes the count in a race with another is refused by the SFU's WebSocket upgrade (HTTP 500), and
  a client shows "could not join the call" for that.
- **The token.** `token` is a LiveKit room-join JWT for the call's room whose identity is the device
  id. It carries the **base grant** only: subscribe, never data, and publish for the microphone when
  the device holds `speak` — never the camera or the screen, even for a device that holds a sharing
  slot. A human device without `speak` is listen-only. Managed Bot devices never receive the
  subscribe grant and cannot receive other participants' media; their current permission updates
  remain limited to microphone publication, and lookup failures never restore subscription.
  The camera and screen sources (`video` the camera,
  `screen_share` the screen and its audio) reach a session only as the permission the instance pushes
  after `POST …/share` (below), so a token can never be replayed for video after the device stopped
  sharing; a client that reconnects with a fresh token while it shares POSTs `…/share` again, which
  is idempotent for a slot it holds. When the request carries `vdec` — the device's video decoders,
  comma-separated from `vp8`, `h264` and `vp9`, each at most once (any other spelling is
  `400 E_INVALID_REQUEST`) — the token carries it as the participant attribute `dilla.vdec`, which
  the other devices read to choose a codec every subscriber can decode. No call token carries LiveKit's
  own-metadata right (`canUpdateOwnMetadata`), human or bot, and the `/rtc` gate refuses one that
  does: LiveKit bounds the size of one attribute update but neither the merged map nor the rate, so
  any participant with the right could flood the room. A device's round-trip time to the SFU
  reaches the others through `POST …/rtt` (below), which the instance writes as that device's
  participant attribute `dilla.rtt`.
  The token is valid for one
  hour, but its lifetime bounds neither a session nor a rejoin: the SFU re-issues a connected
  participant's token every five minutes from its current grants, valid for at least ten more
  minutes, so a client that stays connected always holds a token it could rejoin with. What bounds
  access is the signalling proxy, which checks every join and resume against the device's current
  state (below), and the cuts in "Losing access". `livekit_url` is where the client connects with it: `wss://` and the
  instance's own host (its public IP in `acme_ip`), whose `/rtc` paths the instance proxies to its
  in-process SFU, which listens only on `livekit.bind_address`, a loopback IP literal (the
  instance refuses any other value at start). Every start opens the call's room in
  the SFU first and the SFU never opens one on a join, so a token for a room the instance has closed
  is refused by the SFU (HTTP 404). A call gets a fresh room each time it is opened, so a device of
  the previous call cannot remain in the next one. `caps` is what the client applies to its publish
  options: `livekit.max_audio_bitrate_kbps` and `livekit.max_share_bitrate_kbps` in bits per second,
  and `vp9` 1 when `livekit.vp9` is on, else 0 (the instance refuses `livekit.vp9 = true` at start
  until VP9 has a frame test vector and a measured run through the SFU, so `vp9` is 0 today). An instance that runs no SFU (`livekit.enabled =
  false`) still answers every refusal above, and a start that passes them is `501 E_INTERNAL` with
  no call recorded.
- **Sharing.** A device takes a **sharing slot** before it publishes its camera or its screen: one
  slot per device covers camera, screen and screen audio together, screen audio never needs one of
  its own, and the microphone never does. At most `livekit.max_publishers` devices of a call hold a
  slot; the next `POST …/share` is `409 E_CALL_SHARERS_FULL`, decided by the instance alone, so of
  two devices racing for the last slot exactly one wins. The instance pushes the device's complete
  new permission to the SFU before it answers `204`; the client publishes only after the SFU's
  permission update shows the new source. `DELETE …/share` takes the camera and screen sources away
  first and frees the slot after. The slot is also freed when the device loses both `video` and
  `screen_share`, when it is cut from the call, when a share or a permission push finds it no longer
  in the room, and when the call ends; a slot belongs to the room it was taken in and never carries
  into a later call. The instance also frees the slot when its SFU reports that the device stopped
  its last camera and screen track (the camera and screen sources are taken away first) — unless
  the SFU shows the device publishing a camera or screen track again by the time the report is
  handled, as when it switches from one to the other — or left the room; those reports can be late
  or lost, so a device that stops sharing sends `DELETE …/share` itself, and the room sweep frees
  the slot of a device its room no longer holds, so a device that crashed or whose leave report was
  lost holds its slot no longer than the room sweep's bound (below); one whose removal or demotion is
  pending keeps it until that repair lands. A permission change during a call is pushed to the SFU
  immediately after it (below).
- **Losing access.** A device is **barred** from every call when the device is revoked or
  quarantined (`02` invariant 9) or its user is disabled or deleted: a start answers it
  `403 E_FORBIDDEN` and mints nothing, a share and an end (`DELETE /v1/calls/{call_id}`) answer
  `403 E_FORBIDDEN`, and the signalling proxy refuses it. A device that is barred, or whose user
  loses `view_channel` or `connect` in the channel, is disconnected from the call's room with every
  `"<device>#…"` participant of it, before the call group's `Remove` of its leaf is committed, and
  its sharing slot is freed. No request waits on the SFU for that: a role or overwrite change, a
  kick, a leave, a ban, a group-DM removal, a channel's visibility change, a device revocation
  through `DELETE /v1/devices/{id}` or an account deletion, a user disable through the admin route,
  a fork quarantine and a commit that removes a device from a call group each **queue** the cut once
  their change has landed, and the instance's background loop, woken by the request, makes it
  immediately after on its own; the request answers without waiting for it. "Immediately after" is
  at once when the loop is idle; when the loop is in one of its passes (a retry pass, at most 15
  seconds; a room sweep, at most 20; the closing of ended calls' rooms, at most 15), the queued cut
  is made between two calls or rooms of that pass, after at most one call's work (2 seconds of
  waiting for the call's serialisation and its bounded SFU calls). A cut that has to visit many
  calls (a role change in a large community) runs for at most 15 seconds and leaves every call it
  did not reach to the next retry pass, at most five seconds later. Meanwhile the device is
  refused everywhere, since the start, share and signalling-proxy checks re-read its state, its
  permissions and its leaf on every request. The queue is bounded (1024 entries) and coalesces
  repeats; a request that does not fit makes the loop run the room sweep (below) at once, so its
  cut waits for the room sweep's bound. A change written to the database by another process —
  `dillad admin user disable` or `delete`, an admin device revoke — and a device admitted by the signalling proxy just
  before it lost access are caught by the **room sweep**: on the loop's five-second tick, at most
  every 30 seconds, the instance lists its SFU's rooms and, for each live call's room, applies the
  same rule to every participant. **The room sweep's bound**: one pass visits at most 64 rooms,
  going on from where the last pass stopped, lasts at most 20 seconds (the rooms it does not reach
  wait for the next pass), and skips a call whose serialisation stays busy for 2 seconds (it is
  visited again at the next pass); a pass starts after the tick's queued work and retry pass. So a
  room is reached within about 30 seconds per 64 live call rooms when the SFU answers promptly,
  and one more period for each pass cut short or call skipped. The sweep also closes every room that belongs to no
  live call — a room whose close failed when its call ended, an older room of a call, a name that is
  no call's — and ends a live call whose channel is deleted or whose call group is closed, closing
  its room. When the instance cannot resolve a participant's permissions or state, it takes every
  publish grant away from that participant rather than leave the old one in place. A participant
  whose identity is no device of the instance is removed from a call's room. A removal or demotion
  the SFU does not take stays **pending**: the device keeps counting against
  `livekit.max_publishers`, a start of the call answers it `403 E_FORBIDDEN` and the signalling proxy
  refuses it, and the instance retries every five seconds and at that device's next start, share or
  signalling request until the SFU holds a permission within the device's entitlement or the device
  has left. Every slot transition and every permission the instance pushes for a call is serialised
  per call, so the devices that may publish a camera or screen source are always among the slot
  holders. A start, a share, an unshare or the signalling proxy that cannot take that serialisation
  within two seconds — the SFU is stuck on the call — answers `503 E_UNAVAILABLE` with
  `retry_after_ms`; the client retries after the delay. Once it has it, a request holds it for at most
  six seconds and the instance's own background work for at most ten per call.
- **The signalling proxy.** The `/rtc` paths admit only `GET` (anything else is `405`). The access
  token — a non-empty `Authorization` header, which must then be `Bearer`, else the `access_token`
  query parameter — must be one the instance minted (`403 E_FORBIDDEN` otherwise), for a device, and
  that device must be a current leaf of the live call whose room the token names
  (`403 E_LEAF_NOT_CURRENT` otherwise), so a device the call group has removed cannot rejoin with a
  token the SFU re-issued to it. The device must not be barred, its user must still hold
  `view_channel` and `connect` in the channel, and no removal or demotion of it may be pending
  (`403 E_FORBIDDEN`), and the token may
  confer nothing beyond the device's current base grant (`403 E_FORBIDDEN`): a token minted while
  the device held `speak` is refused once `speak` is revoked, and a token the SFU re-issued while the
  device shared (it carries the camera) is refused for a fresh join — the client starts the call
  again for a base token and POSTs `…/share`. A resume of a session is exempt from the per-source
  comparison only, because the SFU keeps a resumed participant's own permission and never reads the
  token's, and refuses a resume of a participant it does not hold; every other check applies. The
  instance reads a resume exactly as its SFU does: without `join_request`, `reconnect` `1` or
  `true`; with `join_request` (the `/rtc/v1` form), the `reconnect` field of the base64url
  `WrappedJoinRequest`'s `JoinRequest`, uncompressed or gzip. A `join_request` the SFU would refuse —
  not base64url, not protobuf, or larger than 1 MiB raw or once decompressed — is
  `400 E_INVALID_REQUEST`. A join the checks above admit must also name signalling protocol 17 —
  the `protocol` query parameter without `join_request`, the `JoinRequest`'s `client_info.protocol`
  with it — and, with `join_request`, a `client_info.sdk` the SFU's enum names; any other value,
  including none, is `400 E_INVALID_REQUEST`, because the SFU labels its metrics with both. The
  paths are metered per client address on the `unauth` bucket of
  `[limits.rate]` (`429 E_RATE_LIMITED` with `retry_after_ms`). The instance removes a `publish`
  query parameter and the `CF-Connecting-IP` and `X-Real-IP` headers before the request reaches the
  SFU, and `X-Forwarded-For` carries only the client address the instance resolved.
- **Relays.** `ice_servers` is the `RTCIceServer` list for the client's peer connection: one entry
  when the instance runs its TURN relay, with a fresh ephemeral credential — `username` is
  `"<expiry>:<device_id>:<issued>"` — the expiry in unix seconds, `turn.credential_ttl` ahead
  (default one hour), and the time it was minted in unix milliseconds, never ahead of the
  instance's clock — and
  `credential` is `base64(HMAC-SHA1(turn shared secret, username))`, the time-limited REST form
  TURN servers validate — and an empty array when it runs none. A client passes this list to its
  peer connection even when it is empty, so the SFU's own server list never reaches it, and sets
  `bundlePolicy: 'max-bundle'`. The relay is `turns:` on the instance's 443, where the instance
  tells a STUN stream from HTTP by its first bytes after the TLS handshake; in `behind_proxy` it is
  a separate operator-configured TCP port, and without one the list is empty and the client's
  "relay unavailable" dialog applies: the call is direct UDP or nothing. That relay is plain `turn:`
  on the listen port, because the instance terminates no TLS for it there (a recorded deviation
  from the spec, plan dillad-2 **D25**): its credential username and allocation traffic cross the
  network in cleartext unless a TLS front the operator runs carries it, while the media itself stays
  DTLS-SRTP with SFrame. When `turn.public_url` is set, in any mode, it is the one URL in `urls`,
  verbatim — for a proxy that terminates TLS for the relay (`turns:`) or publishes it on another
  port. The credential's expiry is checked on `Allocate` only: an allocation made before it keeps
  refreshing and permitting after it, until `turn.max_allocation_age` (default two hours, never
  shorter than `turn.credential_ttl`) has passed since the credential was issued (the issue time it
  carries), when every request made with that credential is refused and the client's next ICE
  restart allocates anew. The bound is per credential: a refresh made with a newer credential of the
  same device keeps the allocation, and channel data, which is never authenticated, flows until its
  permission or channel binding lapses (at most five and ten minutes). A device that is cut — revoked,
  quarantined, logged out, of a user who is disabled, or removed from a call's room for any reason
  (barred, no longer a leaf of the call group, without `view_channel` or `connect`, evicted, or
  disconnected for media that is not dilla's) — loses the relay at once: every credential of it
  issued at or before the cut's millisecond (never later than the instance's clock) is refused on
  every request, and its allocations end. Its `Allocate`s whose relay pion was still creating as the
  cut landed are refused with STUN error 508 — and so may be every `Allocate` of that device for up
  to 5 seconds after the last of them, a fresh credential's included, so a client retries a refused
  `Allocate` after more than 5 seconds. A device that may still take part in a call gets a credential
  issued after the cut from its next `POST /v1/channels/{id}/calls`; a client whose relay refuses it
  (a failed relayed pair, an allocation the relay dropped, an `Allocate` refused 400, 401 or 508)
  re-POSTs that route and ICE-restarts with the new list. A start whose device was cut in the same
  millisecond, or while the start was served, is answered `429 E_RATE_LIMITED` with a
  `retry_after_ms` of a few milliseconds and no credential; it changes no cut, and the retry's own
  gates answer whether the device may still call. The instance's wall clock stepping backwards
  stretches that wait: until the clock passes a device's cut again, its starts are answered
  `429 E_RATE_LIMITED` with a `retry_after_ms` that names the remaining time (a credential issued at
  or before the cut would be refused by the relay anyway), and a step back past the process start
  refuses every device's start the same way until the clock passes the start. A start whose gates changed after the credential
  was minted (barred, no longer a leaf, without `view_channel` or `connect`) is refused with that
  gate's code, and the device is cut as of then, which covers the credential just minted. The
  relay refuses every credential issued before its process started: the cuts are held in memory
  only, and a restart ends every call anyway (the SFU runs in the same process), so every client
  re-POSTs the calls route for a fresh credential. Should the instance ever hold more live cuts than
  it can store (65 536), the cut that does not fit refuses every device's credentials issued up to
  it, so every relayed client re-fetches its servers rather than any revocation being forgotten. A
  device another process bars (`dillad admin`) is found by the relay itself on its next
  authenticated request — which can be up to 30 seconds late, because a lookup that found the device
  not barred is trusted for 30 seconds — or, while it holds an allocation and sends only channel data, by a re-check
  every 30 seconds that trusts a lookup for 30 seconds: within 62 seconds of the revocation at worst
  (two periods and a 2-second lookup); while the database cannot answer, such a device stays bounded
  by `turn.max_allocation_age` only. A user disabled in the instance's own process loses every
  device's relay when the call cut queue processes the user, at once while the SFU runs and the
  queue (1024 pending cuts) has room; otherwise, as for a revocation by another process, through the
  relay's own lookup, and the room sweep removes such a device from every call room within its
  period. A client keeps fresh servers by repeating `POST /v1/channels/{id}/calls` before
  `turn.credential_ttl` runs out and handing the new list to its peer connection for that restart.
  The relay checks the username strictly: exactly three fields, both numbers unsigned decimal
  digits without a leading zero, the device a device id, the issue time no more than 2 seconds
  ahead of its clock, and a lifetime no longer than `turn.credential_ttl` and those 2 seconds
  (`turn.credential_ttl` is at least one minute). An `Allocate` is accepted while the relay's clock,
  in milliseconds, is at most the expiry times 1000 — that millisecond included, the next one
  refused — so a credential never works for an `Allocate` past its
  issue time plus `turn.credential_ttl`. `dillad doctor` mints its probe credential in its own
  process: run from another host, it needs that host's clock within 2 seconds of the instance's, or
  the relay refuses the probe. At most
  `turn.allocations_per_device` relay allocations are live per device (default 4); another is
  refused with STUN error 486 until one ends, and the instance counts the refusals. An allocation
  the relay cannot create (STUN error 508 — a relay of an address family the relay address is not,
  say) holds no slot. Four is two
  networks through one ICE-restart overlap: a browser holds about `T × N × U` allocations — T = 1
  gathering transport under `max-bundle` (seven during the first offer under the default
  `balanced`), N = the networks it gathers on, U = 1 relay URL. Whatever the devices, at most 8192
  relay allocations are live on the instance; another is refused with STUN error 486 until one
  ends. The relay reaches only the instance's own SFU, at addresses the SFU both offers and
  receives media on: `livekit.node_ip` when it is an address the SFU listens on in the instance's
  host, or when `livekit.advertise_internal_ip` is false (it is then the SFU's only candidate, and
  relayed media leaves the host for it and comes back), and with `livekit.advertise_internal_ip`
  the host's interface addresses the SFU listens on — none inside `livekit.ips_excludes`. A
  `CreatePermission` or `ChannelBind` for any other peer is refused with STUN error 403, and with
  LiveKit off every one is. On an admitted address the relay exchanges datagrams only with the
  SFU's media port (`livekit.udp_port`, or the SFU's 50000-60000 range when it is 0) and never
  with another relay allocation: a datagram a client sends to any other port, or one that arrives
  from any other port, is dropped without an error. With `livekit.udp_port` 0 that range is a range
  of ports, not the SFU's sockets alone: any UDP service bound in 50000-60000 on an admitted address
  is reachable through the relay too. The relay offers UDP relays only: an `Allocate`
  with `REQUESTED-TRANSPORT` TCP (RFC 6062) is refused with STUN error 508, so no `Connect` can open
  a TCP connection through it; an `Allocate` with `EVEN-PORT` or `RESERVATION-TOKEN` is refused with
  STUN error 508 too. A connection to the relay that has created no allocation within 30 seconds of
  connecting is closed, and so is one that has gone 61 minutes since its allocation was created or
  since its last authenticated request while it held one; other bytes, an `Allocate` refused after
  authentication, and requests that fail authentication (a revoked device, an expired credential)
  keep no connection open.
- **Call stats.** A device in a call reports its connection about every 30 seconds: the selected
  candidate's type (0 host, 1 srflx, 2 prflx, 3 relay), the relay's transport when it is a relay
  (0 udp, 1 tcp, 2 tls; null for any other type), the round-trip time in milliseconds (at most
  60 000), the fraction of packets lost in thousandths (at most 1000), and the frames it failed to
  decrypt and the frames it encrypted since its previous report (each at most 1 048 576). Anything
  else is `400 E_INVALID_REQUEST`. The leaf gate and `connect` apply as for a share
  (`403 E_FORBIDDEN` without `connect`), and a barred device is `403 E_FORBIDDEN`. The instance
  keeps the reports in memory only, for the admin's diagnostics (§ Admin) and counters that name no
  device, and accepts one per device per 5 seconds (`429 E_RATE_LIMITED` with `retry_after_ms`); a
  report sent sooner is refused before anything else is checked.
- **Media that is not dilla's.** Every track a device publishes must be `dilla-sframe/1` (`05`) and
  of its source's kind (audio for the microphone and screen audio, video for the camera and the
  screen). The first track the SFU reports otherwise — flagged unencrypted, or of the wrong kind,
  in the SFU's report of the publication or in its participant list as the room sweep reads it
  (one publication counts once, whichever saw it first) — makes the device listen-only for the rest of the call's room: every publish permission is taken
  away (which unpublishes its tracks), its sharing slot is freed, a start mints it a listen-only
  token, the signalling proxy admits it listen-only, and a share answers `403 E_FORBIDDEN`. A second
  such track disconnects it from the room.
- **Ending.** `DELETE` ends the call for everyone and is kept to its participants. Managed bots do
  not keep a call alive after its last non-bot member leaves: when two consecutive reconciliations
  of the whole room, at least one room-sweep interval apart, find a managed-bot leaf but no non-bot
  leaf present in the room or admitted within the 30-second join window, the instance ends
  the call with reason `bots_only` and deletes the room instead of proposing Removes that only the
  bots could commit. Any whole-room grant sync can make these observations, including a room sweep,
  room resync or channel-wide permission change. A human present or inside the join window, or an
  uncertain account, group or participant listing, resets the observations. A member whose account
  kind cannot be read is logged and skipped for absent-leaf tracking; other absent leaves still
  progress, while the call stays live. Managed bots are not eligible committers. A call also ends
  when the SFU closes its room — 20 seconds after the last participant leaves, or 300 seconds after
  it was opened when nobody joined; a report of the close of a room the SFU has since re-opened
  under the same name, for a start that came after it, ends nothing, and neither does one the
  instance cannot check against its SFU (the call then stays recorded live, with no room, until its
  next start re-opens the room) — when a start finds its call
  group closed, and when its channel, or the community of its channel, is deleted. Ending a call
  **closes its call group** first, then ends its voice session, closes its room (disconnecting
  everyone still in it), frees every sharing slot and sends `voice_state` with `flags` 0 (`02` §
  Gateway frames) for every device still in it; the next call registers a fresh call group (`01`),
  and a start before one is registered is `404 E_NOT_FOUND`. A start that read the call group open
  just before an end closed it, and so would reopen the call on a closed group, ends that call again
  at once and answers `503 E_UNAVAILABLE` with `retry_after_ms`; its retry opens the next call or is
  `404 E_NOT_FOUND`. A deleted channel's call is ended in the record within the delete request, so
  every start, share and signalling-proxy request refuses it from then on; its room is closed
  immediately after by the background loop. When the instance cannot read the channel's live calls
  in the delete request, the room sweep ends them within its bound (the signalling proxy refuses
  every join of a deleted channel's call meanwhile). A restore ends every live call (`02` invariant 11).
  **An idle call group is closed.** Every 60 seconds on the instance clock, the instance closes,
  through the delivery service as an ending call does, each open `call` group that no live voice
  session names and that was created at least 600 seconds ago: at most 1 024 groups per pass,
  oldest first, the rest at the next pass. A live call's group is never closed this way; ending a
  live call stays `DELETE /v1/calls/{call_id}` by a current leaf. A client that leaves a call it
  believes it is alone in ends it with `?if_alone=1`; managed-bot leaves are ignored, while another
  non-bot device whose external commit has just landed keeps the call (`409 E_CALL_NOT_ALONE`);
  the unconditional form is unchanged. A closed group answers as above
  (`403 E_LEAF_NOT_CURRENT` at a start), `GET /v1/channels/{id}/call` no longer names it, and the
  next joiner registers a fresh call group. A start that read the group open and writes its call
  after the close is ended at once and answers `503 E_UNAVAILABLE` with `retry_after_ms`; a start whose
  call was written just before the close can instead answer `201` and see the call end at once. Either
  way the client retries with a fresh `GET /v1/channels/{id}/call`. So a call group that a member poisoned with a
  device no list names, and that no call keeps live, refuses honest joiners for at most 10 minutes.
- **Leaving.** A device leaves a call by posting its self-Remove proposal to the call group (`01`
  § Joining) and only then disconnecting from the SFU. A client whose page went away in a call
  without leaving it (a reload, a crash, a closed tab) leaves at its next ready, before it serves a
  join (a join waits for this at most 5 seconds): for each call group it still holds and is not
  joining, it reads the channel's call; when that names another group or none, or the channel
  answers `404`, it only forgets the group, and any other refusal leaves the group as it is; when
  no other device is listed, it ends the call with `DELETE /v1/calls/{call_id}?if_alone=1` and only
  forgets the group when that ended it or answered `404`; whenever the call did not end, the
  conditional end refused included (`409 E_CALL_NOT_ALONE` or any other answer), and whenever other
  devices are listed, it posts its self-Remove, then forgets the group. Offline, it sweeps at its next connection instead, and a group a join takes up
  meanwhile is left alone. The instance learns of the leave from the
  SFU — under a millisecond after a graceful disconnect on loopback, and 20–22 seconds after a crash
  or a lost network (the SFU's ICE timeout plus its cleanup; `docs/spikes/2026-10-livekit-webhooks.md`
  measured 20.4–21.9 seconds with a killed client) — and the SFU's reports can arrive up to about 45
  seconds late after the instance's receiver was briefly unavailable, or not at all. On that report
  the instance frees the device's sharing slot and proposes its own `Remove` of the device's leaf,
  whether or not the device posted one (whichever of the two is committed removes it, `02`
  invariant 6); the `Remove` is of the leaf the leaving session was admitted with by the signalling
  proxy — that leaf, taken in that epoch — and is not proposed when the device has since left the
  call group or rejoined it (at another leaf, or the same leaf added again in a later epoch), nor
  when the instance does not know the session's admission. When that membership is gone but the
  same device holds another leaf of the call group — it re-joined the group from inside the call,
  as a resync does — the `Remove` is of the leaf it holds now, provided the SFU shows the device out
  of the room and it was not admitted in the last 30 seconds; otherwise the sweep below decides.
  The admission is recorded only for a join the signalling proxy lets through to the SFU: a request
  it refuses records none. A leave is bound to the admission recorded at its session's join, else,
  when that join was not seen, to the latest admission at or before the join time the leave names;
  a leave that names no join time binds only a member recorded at its session's join, never the
  device's latest admission. When the device was **admitted again** by a fresh join after the admission a leave is
  bound to (a reload and rejoin, or a full reconnect that keeps its leaf; a signal resume of the same
  session is not a new admission), the leave is of an earlier session and never cuts the newer one:
  it proposes no `Remove`, neither of the earlier leaf nor of the one the device holds now, and
  announces nothing, and self flags the newer session has set are kept; that session's own events
  and the room sweep decide its membership. Should the newer session never join, the held-back leave
  completes once that admission is older than the 30-second join window and the SFU lists the device
  out of the room: the instance announces the device out (`voice_state` flags 0) and drops its self
  flags, and leaves its leaf to the room sweep's two-sighting rule (below). The background loop
  looks at no more than 8 held-back leaves a pass, within 4 seconds, after its repairs and never on
  their budget, so a revocation is never held up by them. A known limitation: a leave that names no
  join time still announces the device out (flags 0) when the device is already recorded under a
  later session and the SFU lists it out of the room. A
  leave of a session the device has
  already replaced by rejoining, and a leave the SFU reports after the call has ended, change
  nothing. A device that stays out of the call's room while it holds a leaf of the call group — its
  leave report lost, or disconnected by the instance and the report of that lost — loses the leaf
  too: when a room sweep (or any reconcile of the whole room: a room resync, a channel- or
  community-wide grant change) finds a current leaf whose device is not in the room, and one at
  least one sweep period (30 seconds) later finds the same leaf, taken in the same epoch, still not in it,
  the instance proposes its `Remove`, bound to that membership. A device the signalling proxy
  admitted, or a start gave a token, in the last 30 seconds is left alone while it connects. So such
  a leaf is removed within about two sweep periods, longer under the room sweep's bound (above),
  and never once the device has rejoined the group. The `voice_state` frames
  these reports cause are coalesced per device: a device's latest state is sent at most every
  250 milliseconds. A device the call group removes is
  disconnected from the room as soon as the commit is accepted (the background loop removes it, the
  committer's request does not wait for it), a removal the SFU does not take is
  retried like any other pending removal, the room sweep disconnects any participant that is no
  current leaf of the call group, and the signalling proxy refuses its rejoin.

### Reports

A report reveals one message to the instance's admins so they can verify what was sent (`04` §
Franking). Every route below is `E`: filing is open to any enrolled session; the queue and its
status are instance-admin only (§ Flags), `403 E_FORBIDDEN` for anyone else.

| Method and path | Request | Response |
|---|---|---|
| `POST /v1/reports` | `[group_id(bstr 16), seq(uint), envelope(bstr), k_f(bstr 32)]` | `201 [report_id(bstr 16), verification_result(tstr)]` |
| `GET /v1/reports?limit=` | — | `[[report_id(bstr 16), reporter(bstr 16), group_id(bstr 16), seq(uint), revealed_envelope(bstr), verification_result(tstr), status(uint), created(uint)]]` |
| `PATCH /v1/reports/{id}` | `[status(uint), result(tstr)]` | `204` |

- **The target.** A `group_id` that names a registered MLS group is a ciphertext message of that
  group; any other is read as a server-readable channel's `channel_id`, whose messages carry the
  same franking tuple with the channel id in the `group_id` slot and epoch `0` (§ Readable
  channels). A message the instance never stored is `404 E_NOT_FOUND`; a deleted one is still
  reportable, because the tuple outlives the content. A malformed body, or a `k_f` that is not 32
  bytes, is `400 E_INVALID_REQUEST`. The body cap is 96 KiB, as on the envelope routes, so a
  maximal legal envelope can be reported.
- **Verification.** The instance recomputes `04`'s `C` from the submitted `envelope` and `k_f` and
  compares it with the `C` it stored at upload, then recomputes `T` from its own stored `(group_id,
  epoch, seq, uploader_device, recv_ts)` and that `C` under the franking key the message records
  (`03` § Instance keys: retired franking keys are kept for this), and compares it with the stored
  tag. On a server-readable message edited since, the stored tuple is the edit's: the editing
  device and the edit's time. A report that does not verify is still filed: it is evidence about
  the reporter. `verification_result` is exactly one of `verified` (both equations hold),
  `envelope_malformed` (not a `04` envelope, so no `C`), `no_commitment_stored` (the instance holds
  no `C` for the message), `commitment_mismatch` (the envelope or `k_f` is not what was sent),
  `franking_key_unavailable` (the key the message names is no longer held), `franking_key_unknown`
  (a message stored before its key was recorded, and no retained key made its tag) and
  `tag_mismatch` (`C` matches but the stored tag does not).
- **What is kept and shown.** The report stores exactly the submitted `envelope` bytes, `k_f`, the
  id of the key the message was franked with, the result and the reporter. The queue shows the
  eight elements above and nothing else about the group: no other message and no member list.
  Authorship rests on the instance's session-to-device record, an operator attestation that is
  deniable to third parties.
- **The queue.** Newest first (ties by `report_id`), at most `limit` (default 100, at most 1000).
- **Status.** `0` open (the state a report is filed in), `1` resolved, `2` dismissed; any other
  value is `400 E_INVALID_REQUEST`, and so is a `result` over 1 024 bytes or with a NUL. An unknown
  report is `404`. `PATCH` never changes `verification_result`, which is the instance's finding and
  not a moderator's; `result` becomes the detail of an audit row whose action is `report.reopen`,
  `report.resolve` or `report.dismiss` and whose target is the report id in hex.

### Admin

The instance-admin routes. Every one is `E` and needs a user whose `users.flags` has bit 0 set
(§ Flags); anyone else is `403 E_FORBIDDEN`. Every action is written to the audit log.

| Method and path | Request | Response |
|---|---|---|
| `DELETE /v1/admin/blobs/{blob_id}` | `[reason(tstr)]` | `204` |
| `GET /v1/admin/audit?since=&limit=` | — | `[[actor(bstr 16)\|null, action(tstr), target(tstr), detail(tstr), at(uint)]]` |
| `POST /v1/admin/users/{id}/disable` | `[disabled(uint)]` | `204` |
| `GET /v1/admin/diagnostics` | — | `[[name(tstr), status(uint), detail(tstr), fix(tstr)]]` (below) |

- **Purge.** `DELETE /v1/admin/blobs/{blob_id}` removes every reference to the blob in every
  channel, deletes it and unlinks the file at once, and records a tombstone, so a later `PUT`,
  `GET` or `HEAD` of those bytes is `410 E_PRUNED`. Without the tombstone, content addressing would
  hand the purged name straight back to anyone still holding the ciphertext. Purging bytes the
  instance does not hold still records the tombstone, so the table is also the operator's
  blocklist; a second purge of the same bytes is `204`. For bytes a stored backup object names
  (§ Backups) the purge also removes every channel reference, records the tombstone and writes its
  audit row, and answers `204`, but keeps the blob and does not unlink the file: the root object
  is written once, so removing its bytes would leave the account with no recovery, and the backup
  reads do not consult tombstones. The tombstone refuses every new attachment or backup of those
  bytes; when the state object that names them is replaced, the replacement unlinks them and the
  tombstone stays (`dillad admin blob purge` does the same and says the file was kept). Backup
  objects and attachments never share bytes: a backup `PUT` of bytes a channel references, and an
  attachment `PUT` of bytes a backup object names, are `409 E_INVALID_REQUEST`, decided in the
  transaction that would record them after it takes the per-blob lock both routes take (the blobs
  row `FOR UPDATE` on Postgres; on SQLite the one writer serialises them), so of two concurrent
  uploads of the same bytes, one of each kind, exactly one is recorded; the purge case arises only
  from data stored before the rule. `reason` is 1..1024 bytes with no NUL and
  becomes the audit row's `detail` under the action `blob.purge`, whose `target` is the blob id in
  hex. A purge removes **bytes, not content**: every attachment is encrypted under its own random
  key, so the same file sent again by anyone has a different `blob_id`.
- **Audit.** The rows whose `at` is at or after `since` (unix seconds, default 0), newest first, at
  most `limit` (default 100, at most 1000). An empty log is an empty array.
- **Disable.** `[1]` sets the user's `disabled_at` and, in the same transaction, deletes every
  session of every device of that user; `[0]` clears `disabled_at`. Any other value is
  `400 E_INVALID_REQUEST`, an unknown user `404 E_NOT_FOUND`. The audit action is `user.disable`
  or `user.enable` with the user id in hex as `target`.
- **Diagnostics.** `GET /v1/admin/diagnostics` answers `200 [[name(tstr), status(uint),
  detail(tstr), fix(tstr)]]`: first the `dillad doctor` legs the running instance can answer
  itself, under doctor's names and in doctor's order — `database` (the schema version), `data_dir`
  (its mode), `wasi` (the core the delivery service validates in), `udp`, `webauthn` (the passkey
  relying party and the origins it accepts), `blobs` (every referenced
  file present, no stray files) — then two legs of the instance's own, which doctor does not have:
  `calls` (the call stats of the last 15 minutes: how many of the calls that reported are still
  live, the reports, how many had a relay selected, the median and 95th-percentile round-trip time,
  and the decrypt failures against the frames encrypted; WARN while any decrypt failure was
  reported) and `turn_relay` (the relay's live allocations and the quota refusals since start;
  WARN after any refusal). `status` is 0 OK, 1 WARN, 2 FAIL; `fix` is the operator's next step or
  `""`. The legs that probe the network or need a process of their own — the config parse, the
  SQLite pragmas, clock skew, the certificate and doctor's `turn` leg, a TURN allocation — are only
  `dillad doctor`'s, so an admin request never makes the instance dial out. The report runs only
  after the admin check.

- **Reset factors.** `dillad admin user reset-factors` removes a person's second factors and backup codes and marks the account owing one (`§ Second factors`); sessions stay, and every device sets up a factor before it is used again.

Informative: the operator's own surface is `dillad admin <noun> <verb>`, run at a shell on the
instance's host and acting as the operator, who is not a `users` row. `user list|show|disable|
enable|delete|reset-factors`, `invite create|list|revoke`, `community list|show`, `device revoke`, `blob purge`
and `audit` read and write the same tables as the routes above. Every verb that changes state
writes an audit row with a null `actor` (and the same `action` names: `user.disable`, `user.enable`,
`user.delete`, `invite.create`, `invite.revoke`, `device.revoke`, `blob.purge`); a read-only verb
writes none. `invite create` prints the link once and stores only its hash; the audit row and the
log carry the 8-hex reference. The command is a separate process from `dillad serve` and cannot
close a gateway connection; the running instance ends the connections of a disabled user or a
revoked device within one heartbeat interval (`02` § Device sessions, item 6). `user delete` is the
tombstone of `DELETE /v1/accounts/me`: the handle stays reserved. The command refuses a database
whose schema is not the one the binary embeds (exit 78).

## Permissions

A permission set is a 64-bit unsigned integer; `roles.allow`, `roles.deny` and a channel
overwrite's `allow` and `deny` all carry it. The values are fixed and never renumbered, because a
stored row is a number, not a name. Bit 62 and above are never used, because `allow` and `deny`
are stored in a signed 64-bit integer on Postgres. A stored bit outside this table is ignored.

| bit | name | scope |
|---|---|---|
| 0 | `view_channel` | channel |
| 1 | `send_messages` | channel |
| 2 | `manage_messages` | channel |
| 3 | `pin_messages` | channel |
| 4 | `attach_files` | channel |
| 5 | `add_reactions` | channel |
| 6 | `read_history` | channel |
| 7 | `connect` | channel |
| 8 | `speak` | channel |
| 9 | `video` | channel |
| 10 | `screen_share` | channel |
| 11 | `create_invite` | channel |
| 12 | `kick_members` | community |
| 13 | `ban_members` | community |
| 14 | `manage_channels` | channel |
| 15 | `manage_roles` | community |
| 16 | `manage_community` | community |
| 17 | `view_audit_log` | community |
| 18 | `mention_everyone` | channel |
| 19 | `bypass_slowmode` | channel |
| 20 | `administrator` | community |
| 21 | `mute_members` | channel |
| 22 | `move_members` | channel |
| 23 | `manage_nicknames` | community |
| 24 | `moderate_members` | community |
| 25 | `control_music` | channel |

`control_music` lets a member control the managed Bot's music in a voice channel: play,
queue, skip, stop, seek and volume. The Bot resolves it for the call's voice channel with
that channel's overwrites (the actor must also hold `view_channel` there and be in the
call), so an overwrite can grant or deny DJ in one channel. Owners and administrators hold
it as they hold every bit. A Bot account never holds it. New communities do not give it to
`@everyone`.

`moderate_members` allows warning a member and applying or removing a text timeout. It is a
community permission, so channel overwrites cannot grant or remove it. In a community that
requires moderator second factors, a role grant carrying this bit requires the target to hold a
confirmed TOTP secret or a passkey.

A **community** bit is about the community, not one channel: a channel overwrite can neither grant
nor remove it. `@everyone` is the role at position 0 and applies to every member; no other role may
be created at or moved to position 0. A user's permissions in a channel resolve in this order:

1. The community owner holds every bit.
2. Starting from none, `@everyone` and then each role the user holds, in ascending `position`
   (ties broken by role id, bytewise), apply their `deny` and then their `allow`.
3. `administrator` in the result means every bit; without `view_channel`, no bit at all.
4. The channel's role overwrites for the roles applied in step 2, in the same order, then the
   user's own overwrite, apply their `deny` and then their `allow`; community bits keep their
   value from step 2.
5. `administrator` means every bit; without `view_channel`, no bit at all.

A DM or group DM (§ DMs) has no community, no roles and no overwrites: each participant holds
exactly `view_channel`, `send_messages`, `pin_messages`, `attach_files`, `add_reactions`,
`read_history`, `connect`, `speak`, `video` and `screen_share` in it, and anyone else holds nothing.

## Rate limits

Every bucket is in-process, keyed by `(class, subject)` where subject is the device session, the
user, or the client address (IPv6 keyed by `/64`, not `/128`). A refusal never reserves a token it
then gives back: an `Allow`-style check mutates nothing when it refuses. `retry_after_ms` in the
error array is the bucket's actual deficit, rounded up to the next millisecond; the `Retry-After`
header carries the same value in whole seconds. The commit and proposal paths are metered
separately and loosely, so the delivery service's own throttle never becomes the reason its
committer-election watchdog removes a device.

Body caps: `max_ciphertext_bytes + 4096` on `POST /v1/groups/{id}/message`; 96 KiB on
`POST /v1/channels/{id}/messages`, `PATCH /v1/channels/{id}/messages/{seq}` and `POST /v1/reports`
(`protocol/04`'s own worst-case legal envelope is ≈ 74 KiB, so a smaller cap would refuse a maximal
but valid envelope before the validator ever saw it); `PUT /v1/backups/0/0` 256 bytes,
`PUT /v1/backups/1/0` 1,048,640 bytes (1 MiB + 64); 64 KiB on every other CBOR route;
`blobs.max_blob_bytes` on a blob `PUT`.

## Instance

| Method and path | Auth | Request | Response | Errors |
|---|---|---|---|---|
| `GET /v1/instance` | — or E, F (`instance.discovery`) | — | the discovery document below | `401` |
| `GET /v1/instance/limits` | — | — | the limits array below | — |

The discovery document is an eleven-element array, in this order:

```
[
  wire_versions,           ; [uint]
  e2ee_versions,           ; [uint]
  media_versions,          ; [uint]
  instance_id,             ; bstr 16
  generation,              ; uint
  name,                    ; tstr, the instance domain
  registration_mode,       ; uint
  auth_methods,            ; [uint], ascending, no duplicates
  policy_version,          ; uint
  external_sender_key_id,  ; bstr 16
  external_sender_pub      ; bstr 32
]
```

`wire_versions` is the list the gateway's `hello` offers, from the same value: `[3,2]` in this
version (`02` § Gateway frames, op 0; `10` § Session capability and compatibility). `e2ee_versions` and `media_versions` are `[1]`.

`external_sender_key_id` and `external_sender_pub` are the key id and the Ed25519 public key of the
instance's current external-sender signing key (`03-identity.md` § Instance keys): the entry of
`key_history` the instance row names. A client creating a `text` or `call` group puts
`external_sender_pub` in the group's `external_senders` extension, which is how the instance can
propose into it. Only the public half is served; `key_history` itself never is. After a rotation
the document carries the new key, and the rotation proposal of `03` is how existing groups move.

`registration_mode` is **0 invite-only, 1 open, 2 closed**. Invite-only is 0 because it is the
default an instance starts in; `closed` accepts no new account by any route. `auth_methods` values
are **0 password, 1 totp, 2 passkey, 3 oidc**. Every other value of either field is reserved and a
client MUST ignore an `auth_methods` entry it does not know rather than refusing the document.

The limits array is eleven unsigned integers, in this order:

```
[
  max_ciphertext_bytes, max_blob_bytes, max_attachments, max_previews,
  max_keypackages_per_device, keypackage_refill_threshold, blob_quota_bytes,
  heartbeat_ms, max_frame_bytes, handshake_retention_days, ciphertext_retention_days
]
```

`max_frame_bytes` is `max_ciphertext_bytes + 512` and is the same number the gateway advertises in
its `hello` frame; an instance that changes `limits.max_ciphertext_bytes` moves both together.

Every HTTP response under `/v1` carries `X-Dilla-Generation: <uint>`, so an HTTP-only client notices
a restore without holding a gateway connection.

## Operational endpoints

These are not under `/v1` and carry no CBOR.

| Method and path | Auth | Response |
|---|---|---|
| `GET /healthz` | — | `200 ok` while every background worker makes progress; `500` naming the stalled ones otherwise |
| `GET /readyz` | — | `200 {"status":"ready"}`, or `503` with `"unready"` and only the failed gates named, or `"draining"` during shutdown |
| `GET <metrics.path>` (default `/metrics`) | `Bearer` token when `metrics.require_admin` | the Prometheus text exposition; `401` without the token |

The metrics token is the environment variable `DILLA_METRICS_TOKEN`, never a `dilla.toml` key: the
configuration file is diffed and copied around, and the token is a credential (systemd's
`EnvironmentFile=` is where it belongs). With `metrics.require_admin = true` and the variable unset,
the instance guards the endpoint with a random token nobody holds and logs that every scrape will be
refused, so an unset token never leaves the endpoint open.

## Web client and content manifest

The instance serves the browser client from its own origin. The client therefore needs no CORS
(the instance sends none) and passes the gateway's same-host `Origin` rule (`02` § Gateway frames,
Connecting). The served tree is embedded in the binary when it is built; a binary built without the
client serves a one-page placeholder that names itself with `<meta name="dilla-web"
content="placeholder">`.

**Which requests the client answers.** A request for which the instance has a route — every route
of this document and of `02`, `GET /gateway`, `/rtc`, `GET /i/{code}` and the operational
endpoints — is answered by that route when its method is routed. A path equal to or below one of the
reserved prefixes `/v1/`, `/gateway`, `/rtc`, `/i/`, `/healthz`, `/readyz`, `/metrics` and `/debug/`
(each also without its trailing `/`) is never answered by the client: an unknown one is
`404` with the body `404 page not found`, a known one asked with the wrong method `405` with
`Allow`. Every other request is the client's, in this order:

1. A method other than `GET` or `HEAD` is `405` with `Allow: GET, HEAD`.
2. A path containing `..` or a NUL byte is `404`. `/` is `index.html`.
3. A path naming a file of the manifest is that file.
4. A path whose last segment contains no `.` is `index.html` with `200`: the client's own routes
   (`/welcome`, `/c/<community>/<channel>`) are resolved in the browser.
5. Anything else is `404`.

**Headers.** Every file is served with an explicit `Content-Type` chosen by extension and never
sniffed: `.html` `text/html; charset=utf-8`, `.js` `text/javascript; charset=utf-8`, `.css`
`text/css; charset=utf-8`, `.wasm` `application/wasm`, `.json` `application/json`, `.woff2`
`font/woff2`, `.woff` `font/woff`, `.svg` `image/svg+xml`, `.png` `image/png`, `.ico` `image/x-icon`,
`.txt` `text/plain; charset=utf-8`; a tree holding a file of any other extension is refused at
start. `ETag` is the file's quoted SHA-256 from the manifest, and a request whose `If-None-Match`
names it is `304` (which carries no `Content-Type`). Files under `/assets/` (content-hashed by the
build) are `Cache-Control: public, max-age=31536000, immutable`; every other file, `index.html`
included, is `Cache-Control: no-cache`.

Every `200` and every `304` of every file, whatever its extension, carries the same set of headers:

    X-Content-Type-Options: nosniff
    Referrer-Policy: no-referrer
    X-Frame-Options: DENY
    Permissions-Policy: camera=(self), microphone=(self), display-capture=(self), geolocation=()
    Content-Security-Policy: default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' blob:; font-src 'self'; connect-src 'self' ws://HOST wss://HOST; worker-src 'self'; media-src blob:; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'

The policy is on every file, not only on HTML, because a dedicated worker loaded from a URL takes
its policy from its own script response, not from the document that started it, and the client's
worker holds its keys and does its network calls. The client renders the images it decrypted from
`blob:` URLs it mints in the page, which is what `img-src blob:` admits. The camera, the
microphone and screen capture are granted to the instance's own origin only, for calls (§ Voice),
and to no frame of another origin. `media-src blob:` admits the inline audio and video players,
which play decrypted attachments from `blob:` URLs the page mints; call audio uses `srcObject`
and WebAudio, which `media-src` does not govern, and the client's sounds are synthesised, so no
other media source is admitted. The media worker and the noise-suppression worklet are same-origin
files, which `worker-src 'self'` and `script-src 'self' 'wasm-unsafe-eval'` already admit. Later
changes widen these headers with their own row in `07`. `HOST` is the request's `Host` header when it is a host name or IPv4 address
of at most 253 characters, or a bracketed IPv6 literal, either with an optional port; for any other
`Host` the two `ws` sources are left out. The client's own `404` and `405` answers carry only
`X-Content-Type-Options: nosniff` from Go's `http.Error`; they carry no CSP, ETag, Cache-Control or
policy headers. No `Strict-Transport-Security`, `Cross-Origin-Opener-Policy`,
`Cross-Origin-Embedder-Policy` or `Access-Control-*` header is sent: TLS termination and HSTS belong
to the operator's proxy, and the client needs no cross-origin isolation. Behind a proxy that
rewrites `Host`, the CSP names the rewritten host and the browser refuses the gateway connection;
such a proxy must forward the original `Host`.

**The content manifest.** The root of the tree holds `dilla-manifest.json`, one JSON object on one
line followed by a newline:

    {"v":1,"files":[{"path":"assets/index-AbC123.js","sha256":"<64 lowercase hex>","size":12345},{"path":"index.html","sha256":"…","size":678}]}

`files` lists every regular file of the tree except the manifest itself, by its `/`-separated path
relative to the root, in strictly ascending byte order of `path`, with the SHA-256 of its bytes as
64 lowercase hex digits and its length in bytes. The instance verifies the tree against it when it
starts and refuses to start when `v` is not `1`, when an entry is malformed or out of order, when
`index.html` is not listed, when a listed file is missing, differs in size or digest, or is not a
regular file, or when a file is present that is not listed; it then serves only the bytes it
verified. The manifest is the per-file integrity record a later client-side or third-party check
can be built on; it is not itself served.

**The invite landing page.** The invite landing page (`GET /i/{code}`, `text/html`) links to
`/welcome?invite=<code>` on its own origin; the client prefills its invite field from it.

## Flags

`users.flags` is a bitfield. Two bits are assigned at `wire_version = 1`:

| bit | meaning |
|---|---|
| 0 | instance admin |
| 1 | bot operator |

Every other bit is reserved, MUST be written as 0 and MUST be ignored on read.


### Wire-5 ephemeral transport compatibility

Human clients offer wires `[5,4,3,2]` to session negotiation and the gateway. Managed Bots continue
to negotiate exactly 3. Wire 5 retains the wire-4 HTTP response baseline, including account-kind
attestation, and adds the ephemeral ops of `02`. `/app-version.json` remains the authoritative
manifest check following a build notice or startup/foreground/online/reconnect; no endpoint is removed. The current web app has no periodic version polling or HTTP telemetry fallback.

The HTTP presence activity API remains available to existing clients; the current web app sends presence input only through WSS and drops it when unavailable. Its gateway device
allowance is the same one charged by op 57, so switching transports cannot refresh activity more
than once per five seconds. Activity inbox reads and voice AFK policy updates are separate routes.

Call statistics on op 58 and `POST /v1/calls/{call_id}/stats` use the same typed acceptance service
and atomic per-device five-second reservation. Rejected/cancelled reports release the reservation;
accepted reports update only the existing coarse in-memory counters. HTTP stays available on older
instances or unavailable gateways. No HTTP retry follows an ambiguously delivered WSS sample.
