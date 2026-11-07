# 10 — Community bot

## Scope

Internal first-party bot contracts for wire 3. The `/v1` prefix is unchanged;
E2EE and media remain version 1. One independently signed native bot identity per
community, operated by a separate service. No public SDK or third-party bot
installation interface. This contract defines formats; implementation status is
tracked in the bot plan rather than inferred from this document.

## Encoding

Use the deterministic subset in `04`: uint64, bstr, tstr, arrays and null only,
minimal heads, definite lengths, no trailing bytes, at most eight nesting levels.
All identifiers in this document are nonzero bstr16, lowercase hex in URL paths.
Length limits are encoded bytes, not characters. Raw byte fields containing CBOR
are decoded separately under the same strict rules. Error responses expose stable
codes, never submitted messages, provider credentials or signing material.

## Application requests

A type-0 envelope body is `dilla.bot/1:` plus UNPADDED canonical base64url of
strict deterministic CBOR. Total UTF-8 body <=4000 bytes. Reject whitespace,
noncanonical encodings/pad bits, padding, bad prefix or binary. CBOR array:
`[1, request_id, community_id, channel_id|null, operation, revision|null, expires|null, args]`.
Request_id is also the stable mutation operation id. Context validation requires
an expected community id from the authenticated interaction binding, never the
payload itself. Actor comes ONLY from authenticated MLS sender; no actor field.

Operation, arguments, channel requirement, mutation:
- 0 overview.get: [], no channel, read
- 1 settings.get: [section 0..5], no channel, read
- 2 settings.put: [section 0..5, document bstr(1..2400)], no channel, mutation
- 10 music.state: [], channel required, read
- 11 music.search: [query tstr(1..200 UTF8 bytes)], channel required, read
- 12 music.enqueue: [input tstr(1..2048 UTF8 bytes)], channel required, mutation
- 13 music.pause: [], channel required, mutation
- 14 music.resume: [], channel required, mutation
- 15 music.skip: [] or [expected_current_item:tstr], channel required, mutation
- 16 music.stop: [], channel required, mutation
- 17 music.seek: [position_ms uint], channel required, mutation
- 18 music.volume: [percent uint 0..100], channel required, mutation
- 19 music.shuffle: [], channel required, mutation
- 20 music.repeat: [mode uint 0..2], channel required, mutation
- 21 music.remove: [queue_item_id], channel required, mutation
- 22 music.move: [queue_item_id, before_item_id|null], channel required, mutation
- 23 music.clear: [], channel required, mutation
- 24 music.providers.status: [], no channel, read; returns a fresh bounded provider snapshot
- 25 music.announce: [text_channel_id], channel required (the voice channel), mutation, revision must be 0
- 30 welcome.preview: [template tstr(1..2000 UTF8 bytes)], no channel, read; requires current `manage_community` and returns data `[1, rendered_text]` using the sample references `member`, `server`, and `channel` without posting
- 31 roles.panel.publish: [destination_channel_id, mappings_bstr], no interaction channel, mutation; requires current `manage_roles`, ready Roles-capable Bot, and a readable destination; sends a normal Bot-authored encrypted text message through the send coordinator and returns `[destination_channel_id, accepted_message_seq]` only after its accepted send receipt
- 40 roles.list: [cursor uint], no channel, read (selections use human HTTP)
- 50 announcements.list: [cursor uint], no channel, read; response revision is the scheduler document revision from the same encrypted-state read as the page
- 51 announcements.put: [schedule_id|null, document bstr(1..2200)], no channel, mutation; request revision is the expected scheduler document revision, while the document carries its own per-schedule revision
- 52 announcements.delete: [schedule_id, expected_schedule_revision], no channel, mutation; request revision is the expected scheduler document revision and the argument's positive revision must match the stored schedule
- 60 moderation.cases: [cursor bstr(1..256)|null], no channel, read; null requests the first page
- 61 moderation.evidence: legacy removal `[action_request_id, reason tstr(1..1024 UTF8 bytes), excerpt tstr(0..512 UTF8 bytes)]`, or manual warning `[action_request_id, "warn", reason tstr(1..1024 UTF8 bytes), ""]`; no channel, mutation. The warning form is accepted only after authoritative lookup of the matching human warning receipt.
- 62 automod.config.get: [], no channel, read
- 63 automod.config.put: [grant_token tstr(1..512 UTF8 bytes), config_document bstr(1..2400)], no channel, mutation
- 64 moderation.evidence.list: [cursor bstr(1..256)|null], no channel, read; null requests the first page
- 3 capabilities: [], no channel, read
- 70 poll.create: [payload bstr(1..2200)], no channel, mutation with revision 0. Payload is canonical CBOR `[1, channel_id, thread_id|null, question, answers, multi, duration_seconds]`; channel_id is required bstr16, question is 1..300 Unicode scalar values, answers are 2..10 unique nonblank strings of 1..100 Unicode scalar values, multi is 0 or 1, and duration is one of 3600, 14400, 28800, 86400, 259200, 604800 seconds. The encoded payload is capped at 1800 bytes so a result fits the response cap.
- 71 poll.get: [poll_id], no channel, read
- 72 poll.list: [channel_id, thread_id|null, cursor bstr(1..256)|null], no channel, read
- 73 poll.vote: [poll_id, selected bstr], no channel, mutation. selected is canonical CBOR array of up to 10 unique option indexes 0..9; empty withdraws. The handler checks indexes against actual choices and enforces single choice.
- 74 poll.close: [poll_id], no channel, mutation
- 75 poll.voters: [poll_id, option 0..9, cursor bstr(1..256)|null], no channel, read
- 80 reminder.create: [payload bstr(1..2200)], no channel, mutation with revision 0. Payload is canonical CBOR `[1, when_kind, when, text, timezone, source_channel_id|null, source_thread_id|null]`; kind 0 is relative seconds 60..31536000, kind 1 is Unix seconds 1..253402300799; text is 1..1500 UTF8 bytes, timezone 1..100 UTF8 bytes, and a thread requires its source channel. Encoded payload is capped at 2200 bytes.
- 81 reminder.list: [cursor bstr(1..256)|null], no channel, read
- 82 reminder.cancel: [reminder_id], no channel, mutation
- 83 reminder.dismiss: [reminder_id], no channel, mutation
- 84 reminder.due: [cursor bstr(1..256)|null], no channel, read

Before requesting operation 24, clients negotiate through operation 0. Its legacy
response is `[1, actor_id, effective_permission_bits, owner, administrator]`.
Runtimes that register the provider-status handler return
`[2, actor_id, effective_permission_bits, owner, administrator, [24]]` instead.
Both responses are bounded to 128 bytes; actor IDs are nonzero bstr16, permissions
are uint64, flags are uint0..1, and the capability list contains at most 32 unique
uint8 operation codes. A legacy, malformed, or unavailable response does not
authorize sending operation 24. Older runtimes therefore remain usable without
receiving an unknown operation that could block their interaction delivery.

Operation 24 returns `[version=1, installation_id, observed_at, reserved_3,
reserved_4, reserved_5, reserved_6,
youtube_playback, youtube_playback_reason]`. `installation_id` is bstr16 and
`observed_at` is a Unix timestamp in seconds. Slots 3 through 6 are reserved:
new runtimes emit zero, and clients ignore unsigned integer values retained in
older encrypted receipts. Setup values are `0 unknown`, `1 configured`, and
`2 missing_requirements`. Reason values are `0 none`, `8 dependencies_missing`,
`9 unsupported_bot`, and `10 unavailable`; values 1 through 7 are reserved.
The remaining fields keep their existing indexes and enum values. The response
contains no paths, provider errors, or credentials. The handler checks the actor's current
`manage_community` authority and recomputes the response on an exact replay.

The native client keeps unconsumed responses in its encrypted journal, with a
separate 4096-entry encrypted archive for consumed receipts. A selected ACK copies
the exact response and its source provenance into that archive and consumes the
journal row in one storage transaction. If that transaction fails, the journal row
stays pending. The archive evicts only its oldest consumed receipts when full; it
never evicts pending journal data. A read names the exact request IDs and operation
codes it expects; generic pagination never includes archived history. Exact-request
recovery rechecks the current owner/device, community membership, active Bot
installation and identity, original source message, and current cryptographic trust
before returning archived bytes. Revoked or replaced contexts receive no old result.

Pending journal reads return an opaque, read-bound ACK lease for the selected
delivery IDs. The client acknowledges only after processing the exact response,
and the lease cannot be used as a delivery cursor or to acknowledge an unselected
prefix. Leases expire when their read context is forgotten. A stale or mismatched
lease is rejected. Reads keep their normal live authorization checks; a cached
response is not new evidence of current permissions or provider state.

Operation 61 uses revision `0` and the standard mutation expiry (at most 30 seconds). Its legacy removal form resolves `action_request_id` against an accepted, signed human message-removal receipt, checks its community and authenticated actor/device, and stores the reason and excerpt in the Bot's encrypted state. The warning form resolves the request against the persisted human warning receipt and checks its community, target user, and authenticated actor/device before saving a manual warning case. A missing or conflicting receipt refuses the write. Both forms use the immutable action request ID as the evidence key; the revision is checked by this handler rather than against a shared settings revision. Evidence is encrypted in Bot state and retained for 90 days.

Operation 64 returns `[version=1, rows, cursor|null]`, where each row is
`[evidence_id, action, reason, excerpt, accepted_at_unix]`. The server checks
the requesting actor's current `view_audit_log` permission before every page,
scans no more than 100 encrypted state entries per page, and returns at most
one manual case so the deterministic CBOR response remains below 2400 bytes.
The opaque cursor is bound to the installation and community. Expired and
tombstoned entries are skipped while advancing the cursor; a page with no
visible rows can still carry a continuation cursor.

For no-channel operations channel MUST be null. For reads revision/expires MUST
be null. For mutations both MUST be uint; expiry must satisfy now < expires <=
now+30 (seconds), rejecting overflow. Feature handlers compare stale revisions
against current state at their write boundary; announcement Put/Delete compare
the scheduler document revision in the same encrypted transaction that stores
the outcome receipt. The receipt binds the request ID to the complete canonical
request and replays the same outcome for exact retries within its live expiry.
Do not enforce deadline during a structural encode/decode:
provide explicit validate_context(expected_community, now) that checks it. Decode
must still enforce presence/types and args. Context mismatch/expiry refuse.
Document bstr payloads must themselves be deterministic CBOR arrays (not arbitrary
bytes); sections' actual fields are validated by feature dispatcher. They never
contain generic SQL or executable code. Reject empty/whitespace-only query/input;
control characters U+0000..001F and U+007F disallowed in those two inputs only.

Role-panel operation 31 requires revision zero (reserved; no client policy state is
being changed). Its `mappings_bstr` is deterministic CBOR `[[emoji, role_id, role_name]...]`;
it contains 1..100 rows, each with a UTF-8 emoji (1..32 bytes), nonzero 16-byte
role ID, and plain role name (1..100 UTF-8 bytes, no controls). Emoji and role IDs
must be unique. The Bot renders these values into an ordinary encrypted channel
message and sends it via MLS; it does not forge or upload a message. A human client
may register the returned exact channel/sequence through the HTTP role-panel route.
That route rechecks the current manager session, ready Bot and Roles capability,
Bot user/device authorship of the live message, and current approved-role policy.
The registration call occurs only after the Bot's accepted send receipt is read.

`settings.get` and `settings.put` section 5 address welcome configuration. The
get response data is the encrypted configuration document
`[1, channel_id, template, server_name]`, or the empty array when no configuration
exists; its response revision is the native state revision read with that value.
Both operations require current `manage_community`. The put document has the same four fields.
The request revision is compared with welcome state area 8 and the state change
uses that revision as its atomic expected revision. Success returns the new state
revision and empty-array data; a stale write returns status 4 with the current
revision. The template is validated by the same welcome renderer used by preview.

Response CBOR (same body-prefix and encoded byte limit):
`[1, request_id, community_id, status uint0..9, revision|null, data bstr(0..2400)]`.
For `announcements.list`, data is `[1, offset, total, rows]` and response
revision is the scheduler document revision. A successful or conflicting
announcement mutation returns the resulting document revision; a stale request
returns status 4. Production registers list, Put, and Delete. The bot checks
the authenticated actor's current `manage_community` authority before each
mutation; that permission decision is the action's acceptance boundary.
For `moderation.cases`, data is `[1, rows, next_cursor|null]`; each row is
`[source_community, group_id, channel_id, actor_id, message_id, source_revision,
reason, action, excerpt, received_at_unix]`. The cursor is opaque deterministic
CBOR bound to the installation community. Clients MUST pass it unchanged to the
next request and MUST stop when it is null. Each page rechecks current
`view_audit_log` permission before scanning encrypted case state. A human client may receive a
delayed encrypted page after that check; before rendering its reason or excerpt, the UI/server MUST
recheck the current human moderator's `view_audit_log` authority through the authoritative human
API. MLS sender authentication and request correlation do not establish the receiving human's
current permission.

`automod.config.get` returns the encrypted-state revision and the canonical
configuration document `[1, enabled 0|1, rate_action, duplicate_action,
mention_action, timeout_ms, exempt_user_ids]`. Actions are `log`, `remove`,
`warn`, or `timeout`; timeout is `1..2419200000` ms, and there are at most 256
unique nonempty exempt user IDs of at most 128 UTF-8 bytes each. An absent row
reads as disabled, all rules log-only, a 600000 ms timeout, and no exemptions.
Both configuration operations bind the encrypted state to the bot installation,
community, and instance as the existing area-8 store does.

Each read and write resolves the authenticated MLS sender's current server
membership and permissions. The actor must currently be an owner, administrator,
or hold `manage_community`; writing a config that names `remove` additionally
requires current `manage_messages`, and `warn` or `timeout` additionally requires
current `moderate_members`. Owners and administrators satisfy those action
requirements. A denied actor or cross-community request is refused before the
encrypted row is read or changed. This check uses the authenticated bot session's
current actor-authority endpoint, never actor data in `args`.

`automod.config.put` compares the top-level request revision atomically with the
encrypted-state revision. A stale or racing update returns status 4 and the
current revision, without changing the row. Its `grant_token` is an opaque,
one-use grant issued after the configuring human's current step-up; it carries no
configuration plaintext. Redemption is bound to the exact authenticated actor
and interaction context plus community, request ID, operation 63, expected
revision, request expiry, and SHA-256 of the exact canonical configuration
document. The bot sends only that digest to the redemption endpoint; plaintext
configuration remains inside the MLS-decrypting bot process. It redeems before
attempting the state transaction. The state transaction ID is the request ID, so exact retries of a
committed write recover the original revision receipt. Grant redemption must
return the same receipt for an exact retry, including after restart: if a grant
was redeemed but the state transaction then conflicted or failed, the row is
unchanged and the same request can retry with that receipt. Reusing a grant for a
different request tuple is refused.

A successful settings write activates the evaluator, fresh action dependencies,
revision and replay boundary before its acknowledgement. Later reconciliation
of that revision preserves the activated evaluator and rolling observations;
an exact successful retry also preserves them. Dependency or replay-source
readiness failures refuse the write. Receipt timestamps have second resolution,
so events from the activation's own receipt second remain conservatively
replay-only; the first strictly later receipt second is eligible for live
evaluation. Pre-activation history arriving through delayed source sync never
becomes a new punitive action.

The config service does not treat stored bot capability bits as actor authority.
At event enforcement time, the moderation action endpoint rechecks the active
installation's approved warning/timeout or removal capability and the actor's
current moderation permission. If either has been revoked, the action fails
closed and no punitive action is accepted. The human step-up grant implementation
is required for op 63; deployments without a configured redeemer return status 5.

The human arms a config write with `POST /v1/groups/{group}/automod-grants/arm`
using `[request_id, community_id, expected_revision, request_expires,
config_hash32, step_up_token]`; the response is `[grant_token,
grant_expires_unix]`. This requires the current human group context, community
membership, bot installation, and a valid session-bound step-up. Revision zero
is valid for the first write. The server stores the digest and exact interaction
tuple, and stores only a hash of the opaque grant token.

The bot redeems through
`POST /v1/bot-runtime/installations/{installation}/automod-grants/redeem` with
`[grant_token, community_id, group_id, request_id, expected_revision,
request_expires, actor_user, actor_device, config_hash32]`; success returns
`[true, receipt_id]`. The request must come from the matching ready installation
and current bot session. The server validates the current interaction group,
roster, participant device, actor membership, exact config digest, and every
stored tuple field. An exact duplicate redemption returns the original stable
receipt; changed or expired tuples fail. Config plaintext is never sent to these
HTTP endpoints.
Nonempty data is strict deterministic CBOR array. Empty data allowed. status:
0 ok,1 invalid,2 forbidden,3 expired,4 conflict,5 unavailable,6 busy,7 not_found,
8 upgrade_required,9 internal. Validate expected community on response separately;
request correlation is caller's job. Pagination is by encoded body size.

For `music.search`, successful data is `[1, tracks]`, with at most five YouTube
track tuples. Each tuple is `[queue_item_id, provider, canonical_track_id, title,
artist, album, duration_ms, requester_id]`, matching the track tuple in
`music.state`; search tuples use the resolver's stable `yt:<video-id>` ID and an
empty requester ID. A flat search suggestion may report duration 0 when unknown;
enqueue resolves complete metadata and still requires a valid duration.
YouTube `canonical_track_id` is the raw 11-character video
ID stored in the queue; the playback adapter converts it to the validated watch
URL used by the provider. The entire data array is at most 2400 encoded bytes; if the
provider results do not fit, the server returns the largest prefix that fits.
`music.enqueue` accepts one search term, video URL, or playlist URL. It resolves
at most 500 YouTube tracks and commits all descriptors in one revision-checked
queue operation, or commits none. Queue item IDs are freshly generated per
enqueue and requester IDs come from the authenticated actor. An import that
would exceed the 500-item queue limit fails without a partial import. The
YouTube resolver accepts only its bounded search/video/playlist grammar and
runs extraction in its public-YouTube-only network sandbox; arbitrary extractor
syntax and other hosts are rejected.

`music.state` data is `[1, revision, current_track|null, queue, position_ms,
paused, volume, shuffle, repeat, can_control, position_at_ms, now_ms]`. The last
two slots: the Bot's Unix milliseconds at which `position_ms` was the playhead
(0 unknown) and the Bot's Unix milliseconds when it answered. While a track
plays, a client shows `position_ms + (now_ms - position_at_ms)` plus its own
time since receipt, clamped to the track; it never compares its clock with the
Bot's. The Bot settles the reached playhead at every mutation and restarts it at
0 when a provider starts a track (providers start from the beginning; `seek` is
validated and acknowledged but moves nothing until a provider applies seeks).
Readers accept arrays of at least 10 slots (and track tuples of at least 8),
read the stamp only when slots 10 and 11 are present, and ignore later slots.
Reading the state requires the actor's current View channel permission on the
voice channel, resolved with its overrides.

Before starting playback, clients may use the existing `music.announce` request to
register the command's text channel. While the queue is idle or paused, the Bot
loads the queue and checks its live revision, DJ authority, and the named text
channel's view, send and history permissions and MLS binding. Any authorized DJ
may register a destination for a paused track, including another member's track;
registration does not spend or require a started-announcement limiter slot. It acknowledges registration with CBOR `[]` (`0x80`)
without posting a started card. There is no new request field. A refused
registration does not block Resume or change an existing destination. Active
playback retains the normal requester, announcement-window and rate checks;
registration changes only after an accepted announcement send.

Classified track-specific start failures remove an item durably and try the
next item, ignoring repeat for failed items. Provider-wide, deployment, network, call, room, cancellation and
deadline failures pause without removing the item. YouTube soft refusals such as
"This content isn't available, try again later" pause.
A start run tries each item at most once and is bounded by skip count, elapsed
time and a consecutive-provider-failure circuit breaker; an exhausted queue is
empty and paused. Stop and shutdown can interrupt a run between attempts.

Skipped items from one start run produce one generic public notice through a
bounded installation worker after the supervisor lock has been released. A
single-item notice is `Couldn't play a track. Skipping.`; a batch is
`Couldn't play {n} tracks. Skipping.` with the batch count in place of `{n}`. No title, requester, URL, provider detail or
queue item identifier appears in the public message. The worker posts at most
one notice per voice channel per ten seconds, drops and safely logs the oldest
unprepared work on overflow, and posts nothing without a registered destination.
Community, voice and item-derived operation/attempt identities and per-item
tracking deduplicate repeated or overlapping batches. The in-memory dedupe cache
is bounded; durable per-item reservations preserve deduplication after eviction.
The existing encrypted
native state journals prepared notices' exact body, timestamp, destination group
and generation before sending, so timeout and restart recovery resume the same
immutable attempt. `SendAuthorized` checks current call/DJ and text-channel
authority immediately before preparation and upload. Call end, registrant leave
and installation disable clear registration and revoke its grant. A transient
voice transport disconnect, connection lifetime expiry or epoch refresh retains
the registration for a reconnect; preflight still rechecks the current call and
clears the grant when that call is definitively inactive. Registrations are bound
to the active call's MLS group, so a fresh call in the same voice channel cannot
inherit the previous destination. Server `ROOM_DELETED` and `ROOM_CLOSED`
disconnect reasons clear the registration immediately; server shutdown and media
or transport failure retain it for reconnect. An unresolved
attempt first recovers an existing receipt or uses a resolve-only request to
close the exact prepared ciphertext without uploading. Once receipt lookup has
returned not-found, a definitive resolve refusal (missing/closed group, forbidden
or leaf no longer current) drops the notice from the queue and journal with a
bounded diagnostic. Every prepared notice also has at most six send/recovery
attempts, durably reserved before each send so restarting cannot reset the bound.
Exhausted notices are dropped and logged. Ambiguous native journal
transactions retain their original transaction ID and body until their outcome
is known, before a newer notification snapshot is written.

After playback starts, `music.announce` posts the one public "started music" message: body
`Music started.\n[dilla-music:v1:<voice_channel>:<requester>:<queue_item>]`,
sent through the Bot's MLS group with a durable receipt, into the named text
channel's group. The Bot accepts it only from the requester of the current,
playing track that started at most 30 seconds ago, who may DJ in that call and
whose effective permissions in the named text channel (overrides included, no
owner/administrator shortcut) allow view, send and read history; at most one new
track per requester per minute. The operation id derives from (community, voice
channel, queue item), so a repeat resumes the first message instead of posting
again. A conflict between live and stored state answers 4. Clients draw the live
card only for an unedited message from the community's managed Bot account
(member kind 1) and only while that queue item plays. Text other members author
for the Bot to post (announcements, welcome messages, role panels) has
`[dilla-music:` neutralized.

Upgrade order for this change: update `dillad` and its web client first, reload
open tabs, then update `dillabot`. A web client from before it rejects a snapshot
longer than 10 slots; a Bot from before it refuses operation 25 (no announcement
appears).

## Signed moderation removal

Unsigned proof: `[1, request_id, actor_user, actor_device, community_id, group_id,
 target_seq uint>0, expires uint>0]`. Signature preimage is ASCII
`dilla moderation removal v1` (NO NUL) concatenated with canonical encoded proof.
Detached Ed25519 signature64; signer is registered device key. Core signs; server
checks current session user/device, matching community/group, hierarchy/permission,
and now < expires <= now+30 on new acceptance. The native core signs this
preimage with its device key; the server verifies it against the authenticated
session and current authority.
Accepted stored proofs remain evidence after expiry; decoding never applies time.

## Automatic warning and timeout acceptance

`POST /v1/bot-runtime/installations/{installation}/moderation/actions` accepts
`[request_id, group_id, source_seq, action, rules, timeout_ms, expires]` and
returns `[action_id, community_revision, expires|null]`. The request is accepted
only for an enrolled session bound to that exact Ready installation and when
both the current installation and session capabilities include warnings/timeouts.
The server rechecks the active installation, source text group and recent
undeleted message, uploader membership, current `moderate_members` permission,
and role hierarchy in the same transaction that records the event and receipt.
The source sequence and group determine a stable operation ID; an exact replay
returns the original receipt, including after the request acceptance window.
Timeouts update the member timeout row, which applies to every upload class.

Both this route and `POST /v1/groups/{group}/moderation/removals` accept
`?receipt_only=1` for reconciliation. They validate the same authenticated
installation/device and exact operation payload (including the signed removal
proof). An existing matching receipt returns its original result. An absent
receipt returns `404 E_NOT_FOUND` before any new effect, regardless of whether
the source would otherwise still be eligible. Unknown transport results remain
retryable; a lookup is never promoted into a new acceptance request.

The Bot checkpoints rolling observations, source deduplication, the processed
frontier and any pending decision in one protected native state transaction
before acknowledging a delivery. There are 256 fixed actor shards (each bounded
by the native 256 KiB value limit) and 1,024 reusable pending slots per
installation. Full or unpersistable state defers intake before acknowledgement;
logs expose only the refusal stage and counter. Activation barriers are persisted
before a configuration update is acknowledged and reused after restart. A legacy
installation without a checkpoint seeds one conservative barrier. Pending
old-policy decisions can reconcile existing receipts but cannot authorize new
effects after disablement, policy change or source expiry. Accepted case receipts
are saved before evidence attachment, allowing local evidence recovery after
revocation. Protected case format v3 adds an explicit enforcement-refused flag;
v2 records remain readable. Checkpoint-only native transactions in this new
area-8 namespace retain their latest 64 transaction receipts; configuration and
command receipts keep their existing retention. An older checkpoint retry is
still guarded by CAS revisions and recovered through row/frontier reconciliation.
Rolling/dedup snapshots omit observations outside their evaluation windows.
None of these state formats changes MLS or wire versions.

The server can verify the source message and authorization, but it cannot inspect
the encrypted message or independently attest that a listed rule matched. Rule
IDs and the runtime-generated action are inputs from the authenticated bot
session. This endpoint does not claim cryptographic evaluator attestation.

## Durable structural event

`[1, event_id, sequence uint>0, community_id, timestamp uint, kind uint1..8, data]`.
Data schemas (all arrays) by kind:
1 member_join [user_id, episode_id]
2 member_leave [user_id, episode_id]
3 installation [installation_id, state uint0..5, revision uint]
4 permission [target_kind uint0..3, target_id, revision uint]
5 channel [channel_id, change uint0..2, revision uint]
6 moderation [action_id, request_id, actor_user, actor_device, target_user|null,
 group_id|null, target_seq uint>0|null, action uint0..6, expires uint|null]
7 role_panel [panel_id, revision uint, active uint0..1]
8 device_list [user_id, signed_list_version uint>0]
State:0pending,1enrolling,2joining,3ready,4disabled,5error. Permission target:
0community,1channel,2role,3member. Channel change:0created,1updated,2deleted.
Moderation:0warn,1timeout,2kick,3ban,4remove,5clear_timeout,6unban. For remove,
group+target_seq required and target_user optional; other actions require target_user,
group+target_seq null. Timeout requires expires>timestamp; other actions expires
null except ban may have expires>timestamp. Identifiers and metadata only: no
message text, reasons, excerpts, names, URLs. Max encoded event4096 bytes.
Validate community separately against route/session binding. Stream retention30d;
ack semantics and pagination handled by API. Event id survives retries. A human signed-device-list
publication appends kind 8 in the same transaction as the new list, once for each of the user's
current community memberships; the event contains only the account ID
and accepted list version, never device keys or list contents. The Bot resolves the current member
episode from the community before using the event to enqueue device registration.

## Send receipt

`[1, operation_id, attempt_id, predecessor_attempt_id|null, community_id,
 group_id, expected_generation, ciphertext_sha256 bstr32, class uint0..1,
 outcome uint0..2, epoch, send_response_body|null]`.

Outcome 0 closes an attempt as not accepted, 1 records acceptance, and 2 records
an unknown outcome after restore. Accepted alone requires a nonnull bstr holding
the exact original deterministic `[sequence,franking_tag32,receive_timestamp]`
response. Other outcomes require null. Sequence is positive; generation, epoch,
sequence and timestamp are uint at most signed-int64 max. The attempted epoch is
present for every outcome. All IDs, including a nonnull predecessor, are nonzero
bstr16. Max receipt 512 bytes. Decode the embedded response strictly; integer
arrays cannot substitute for byte strings, and noncanonical embedded CBOR fails.
Context validation requires expected community and group. The authenticated
caller supplies installation/device authority; no plaintext is stored here.

The logical operation ID identifies one welcome, occurrence or application
action. Each exact ciphertext has its own attempt ID. HTTP retries preserve both
IDs and all bytes. A successor may name only the immediately previous attempt
durably closed with outcome 0. Acceptance seals the entire logical operation;
outcome 2 suppresses automatic retry. A missing response or receipt is not proof
of nonacceptance. This replaces the unreleased eight-field pending/accepted
receipt without changing wire 3, E2EE or media versions.

## Helper transport envelope

Private inherited Unix socket; 4-byte BIG-ENDIAN unsigned length (excluding prefix),
1..1048576 bytes. No network listener. Request `[1, request_id, operation uint1..18,
 payload bstr]`; response `[1, request_id, status uint0..9, payload bstr]`.
Payload is encoded strict deterministic CBOR array (possibly empty array), maximum
1048480 bytes and total encoded body<=1048576. No empty raw bstr. Request operations:
1open,2bootstrap,3enroll_complete,4session,5key_packages,6group_apply,7prepare_send,
8send_receipt,9call_epoch,10sign_removal,11state_get,12state_transaction,
13cursor_ack,14key_packages_uploaded,15shutdown,16group_transfer,
17ingest_removals,18refresh_removal_actor_history. No generic SQL or identity-key
export. The open key arrives only in this private channel or a protected
descriptor. Truncated, mismatched and oversized lengths close the connection
without allocating the announced body. Request and response decoders are distinct.

### Helper payloads

One helper opens one installation and serializes every core mutation. Its state
directory is fixed at process launch; no operation accepts a database path. Before
any schema access it applies the 32-byte database key and verifies decryption by
reading `sqlite_schema`. New directory mode is 0700, files 0600, process umask077;
symlinks and insecure existing state paths are refused. Database, WAL, temporary
files and crash diagnostics are included in this boundary. Open binds the stored
installation, instance and community; any later mismatch refuses. The key is
zeroized after configuring the connection and is never returned or logged.

In the table, `body` is a bounded byte string containing the existing core's
deterministic CBOR input/output for the named operation, validated again by that
operation; it is never SQL. A void result is `[]`. Scalar results are wrapped in
an array. Returned core bodies are `[body]` to keep the helper payload an array.
Unknown operations/suboperations fail with status1, unsupported state with5,
revision conflicts with4, and internal failures with9 plus `[]`. Error responses
never contain database, provider or decrypted-message error text.

| Op | Request payload | Successful response |
| --- | --- | --- |
| 1 open | `[instance_id, community_id, installation_id, database_key32]` | `[store_version, identity_info_body]` |
| 2 bootstrap | `[instance_id, community_id, installation_id, external_sender_key32]` | `[umk_pub32, ssk_pub32, sig_umk_ssk64, device]` |
| 3 enroll_complete | `[bot_user, username, now]` | `[final_credential, signed_device_list_body]` |
| 4 session | `[suboperation, ...arguments]`, below | core body or void |
| 5 key_packages | `[operation_id, count, last_resort, expected_generation]`, count1..32, last_resort0..1 | `[upload_body, package_refs, greatest_expiry]` |
| 6 group_apply | `[suboperation, ...arguments]`, below | core body or void |
| 7 prepare_send | `[operation_id, attempt_id, group_id, send_request_body, original_now, expected_generation]` | `[message_id, upload_body]` |
| 8 send_receipt | `[operation_id, receipt_body]` | `[]` |
| 9 call_epoch | `[suboperation, group_id]`, below | core body or void |
| 10 sign_removal | `[request_id, group_id, target_sequence, expires, now]` | `[proof, signature64]` |
| 11 state_get | `[suboperation, ...arguments]`, below | state records |
| 12 state_transaction | `[transaction_id, changes]`, below | `[revision, ...]`, one per change |
| 13 cursor_ack | `[group_id, through_sequence, through_epoch]` | `[]` |
| 14 key_packages_uploaded | `[operation_id, package_refs, outcome, now, current_generation]`, outcome0 accepted /1 obsolete | `[]` |
| 15 shutdown | `[]` | `[]`, then close |
| 16 group_transfer | `[suboperation, ...arguments]`, below | transfer result |
| 17 ingest_removals | `[group_id, entries]`, entries are 0..256 ordered `[revision, proof_bstr, signature64, accepted_at]` rows | `[persisted_cursor]` |
| 18 refresh_removal_actor_history | `[group_id, [proof_bstr, signature64, accepted_at], history_bstr\|null, head_list_bstr\|null]` | `[trusted_list_version]` |

Bootstrap is repeatable across restarts and returns the same pending identity.
It fixes Bot/Native/Native together and cannot adopt a human identity or overwrite
another installation. Enrollment completion is repeatable for the same account;
conflicting account completion fails. A bot bootstrap cannot be completed through
human signup. The helper verifies the final credential and signed device list
before any KeyPackage or group operation. Signing removal derives actor/account,
device and community from this identity and the bound group, never caller fields.
It signs the exact removal preimage above after validating the 30-second window.

Op17 accepts only a text group bound to the installation's community. Empty
entries query its durable cursor. Nonempty entries must have contiguous revisions
after that cursor; an exact replay is allowed, while a conflicting replay or gap
fails without a partial write. The core checks each proof's group and community,
server acceptance window, signed device-list chain anchored to the pinned user
key, device validity at acceptance and detached signature. An actor whose
authenticated signing history is unavailable fails closed. Each successful page
atomically stores tombstones and its cursor, and suppresses late message content.

Op18 requires an active text group in the installation's community and an
existing pinned, signed device-list cache for the proof actor. With both history
and head null, it returns the cached version as a fetch hint without accepting
the proof. With both byte strings present, it validates the complete forward
history chain from that anchor under the pinned signing key, then checks the
proof signature and device validity at server acceptance time before updating
the cache. Mixed null/bytes arguments fail. Combined history and head bytes are
bounded below the 1 MiB helper frame. An unknown actor cannot be bootstrapped by
this operation and must fail closed.

Session suboperations are: 0 identity `[]`; 1 sign `[nonce32,purpose0..1]`;
2 store `[token,expires,idle_expires]`; 3 read `[]`; 4 clear `[]`;
5 device_list `[]`; 6 device_list_published `[]`. Token text is bounded to256
bytes and exists only within protected IPC and the encrypted store. Session
responses never include an identity signing key or recovery key.

### KeyPackage publication intents

Op5 requires a fully enrolled native Bot and a nonzero bstr16 operation id.
Generations and timestamps are uint values at most signed-int64 max; generation 0
is valid. `package_refs` is the ordered array of distinct bstr32 MLS references
computed by the native core for the complete batch, including its fallback if
present. There are count plus last_resort entries, at most 33. `greatest_expiry`
is the greatest expiry of the generated packages. Go never computes MLS refs.
The canonical upload body is at most 1,047,296 bytes (1 MiB minus 1,280), reserving
space for all 33 refs, the expiry and CBOR wrappers within the existing helper
payload/frame bounds. Its ordinary package array has 1..32 nonempty byte strings;
the optional fallback is null or a nonempty byte string. The successful op5
result has exactly as many refs as packages including that fallback. Crypto
validation remains in the native core and server WASI, not the shape codecs.

A dedicated identity-scoped encrypted journal permits one unresolved batch per
installation/device. Generating MLS private material and retaining the exact
public upload body, ordered refs, expiry and canonical `[count,last_resort,
expected_generation]` hash share one native transaction. Equal pending retries
return identical output. Conflicting inputs and resolved crypto retries refuse.
This journal is separate from group upload intents below.

Op14 requires the complete ordered refs. Outcome 0 means the parent verified a
matching authenticated publication receipt, including body hash and count.
Outcome 1 records an obsolete batch whose historical acceptance remains unknown:
its stored generation is less than authenticated current_generation, or all its
packages have expired at authenticated server time now. The core checks that
condition. Capacity refusal keeps the batch pending; invalid credentials or
malformed uploads surface a service fault instead of generating replacement keys.

Resolution atomically clears public output and retains compact retry metadata;
it never deletes private MLS KeyPackage material needed for late Welcomes. Its
semantic hash covers outcome and ordered refs. now/current_generation supply
first-resolution evidence, so an identical resolution can repeat after restart
with newer evidence. Opposite outcomes or changed refs conflict. Session renewal
does not change an intent's original expected_generation.

Group suboperations are: 0 list `[]`; 1 inspect `[group_id]`;
2 create `[operation_id,group_id,target_id,kind,policy,external_sender_pub32|null]`;
3 registered `[operation_id,group_id,next_sequence]`;
4 discard `[operation_id,group_id]`;
5 external_join `[operation_id,group_id,channel_id,kind,policy,group_info,tree]`;
6 joined `[operation_id,group_id,sequence]`;
7 welcomes `[welcomes_body,expected_bindings_body]`;
8 apply `[group_id,handshakes_body,messages_body,through_sequence]`;
9 build_commit `[operation_id,group_id,proposals_body]`;
10 confirm_commit `[operation_id,group_id]`;
11 abort_commit `[operation_id,group_id]`; 12 cursor `[group_id]`;
13 revalidate_sender `[group_id,leaf_index,limit]`;
14 trust_sender `[group_id,leaf_index,requested_user,history_body,head_list,now]`;
15 proposed_adds `[group_id,proposals_body]`;
16 trust_add `[group_id,proposals_body,index,requested_user,history_body,head_list,now]`;
17 pending_leaves `[]`; 18 events_read `[after_delivery_id,limit]`;
19 events_ack `[through_delivery_id]`; 20 intent_result `[operation_id]`.
Here argument arrays expand after the suboperation number rather than adding a
nested array. Kind is the existing text, call or interaction group kind. Text and
The Bot helper currently admits only text (0) and call (1) group creation,
external join and Welcome expectations. Pairing (2) and interaction (3) are
refused before MLS or durable-intent mutation. Text and call groups require
this installation's community. An interaction binding, when decoded by the
shared protocol library, preserves `01`'s null legacy community slot and carries
the Bot account, installation community, one participant user and one
participant device in its typed v2 extension. That shape is structural data;
it does not authenticate the participant or authorize admission. Do not create
an interaction group by trusting those fields from the helper request.

### Kind-3 Bot admission contract proposal

Kind-3 Bot admission remains unavailable until the server and native core can
bind an authenticated admission decision to the exact MLS roster. Existing
evidence does not supply that decision: a Bot session authenticates the acting
installation, community, Bot account and Bot device; the current group
registration receipt binds that session to the submitted group body and
generation; and a signed device list authenticates a device's account
credential. None of those proves that a selected human is currently eligible
for this Bot interaction or binds that selection to the resulting group.
The current registration check also authenticates only the registering Bot
leaf. A participant tuple copied from an event, URL, helper request or MLS
binding remains caller-declared data.

The issuance-only route is
`POST /v1/bot-runtime/installations/{installation}/interaction-admissions`,
under the current wire-3 Bot session for that installation, in joining or ready
state. Its request is `[request_id,group_id,participant_user,participant_device]`.
The server requires the participant to be an active human account and current,
non-banned member of the
installation community and the selected device to be active in that user's
latest valid signed device list. The proof records that eligibility decision;
it does not currently admit the interaction group. Once admission is wired,
the Bot dispatcher must still check each operation's permissions at use time.

Issuance is fenced by the participant user row and a durable per-community,
per-user eligibility revision. A repeated request ID with the same group and
participant tuple returns the same 30-second proof while it remains current;
conflicting reuse fails. Expired proofs can be refreshed for the same tuple.
After 30 days, the request-hash tombstone may be compacted and the ID reused.
An admission receipt is not a group-registration receipt.

The response is `[payload_bstr,signature_bstr]`. The payload is strict
deterministic CBOR `[1,admission_id,instance_id,installation_id,community_id,
bot_user_id,bot_device_id,participant_user_id,participant_device_id,group_id,
eligibility_revision,not_after]`. All IDs are nonzero bstr16; revision and
`not_after` are server uint values at most signed-int64 max. The instance signs
`ASCII("dilla bot interaction admission v1") || 0x00 || payload` with the
Ed25519 private key corresponding to its published instance external-sender
public key. The Go client obtains that key from authenticated `GET /v1/instance`
discovery and passes it to native bootstrap over private helper IPC. The client
requires HTTPS except for explicitly enabled loopback HTTP, disables redirects,
and checks the configured instance id. The native core stores the key in its
encrypted Bot store alongside the installation binding. An existing ready Bot
store without a pin requires operator recovery; bootstrap never silently assigns
it a new key. A caller-supplied key or expected tuple is never a trust anchor.
Every later identity reconciliation checks discovery against the stored key.
Changing the pinned key requires an explicit trusted instance-key transition,
not an admission retry.

The read-only context endpoint returns `[snapshot_payload_bstr,
snapshot_signature_bstr]`. Its canonical CBOR payload is array v1
`[1,request_id,admission_id,admission_payload,admission_signature,server_now,
eligibility_revision,policy_version,e2ee_version,media_version,instance_id,
external_sender_key_id,instance_signer_pub,expected_tuple,bot_list,
participant_list]`. `expected_tuple` is the eight IDs in the admission tuple order.
Each list is `[user_id,umk_pub32,ssk_pub32,sig_umk_ssk64,version,blob,
ssk_signature64,prev_hash32,sha256_blob32]`. The instance signs
`ASCII("dilla bot interaction context snapshot v1") || 0x00 || payload` with
the same pinned external-sender key. The included public key must equal the
client's stored pin. The key ID is included in the signed payload, but this client
version does not persist a separate key-ID pin and therefore treats that ID as
descriptive. Each list blob is at most 64 KiB and the snapshot payload is at most
160 KiB. This snapshot proves which
server context was read together; it is not authorization to register a group.
The registrar still checks current state and consumes the admission receipt
atomically with registration.

For the human participant, the Bot accepts either an already verified UMK pin or
an unchanged first-seen pin. First-seen acceptance is limited to this operation:
the pin must exist, its UMK must equal the snapshot UMK, and it must have no
pending changed key; the snapshot's participant list must also be byte-for-byte
equal to the list already ingested and cached by the native core, including its
version and UMK/SSK. The snapshot signature and the device-list signatures and
cross-signatures are still verified. A missing pin, changed key, or any mismatch
between the signed snapshot and cached list refuses interaction. This lets a Bot
serve a newly joined member under the existing first-seen trust model without
marking that member verified. The Bot's own identity always requires its own
verified pin.

The core rejects an invalid signature, any tuple mismatch, `not_after` at or before
than authenticated server time, an admission replayed for another operation or
group, or an `eligibility_revision` older than the latest admission known for
that installation and participant. The server increments that revision when
membership, ban state, account disable/deletion or the participant's active device set changes, and
rechecks it under the same transaction fence as group registration. Registration
and recovery receipts bind the admission id and payload hash in addition to
the existing installation, operation, body and generation fields. This receipt
does not replace authenticated group state or permit a stale retry to regenerate
an MLS mutation.

After verifying that result, the core must inspect the authenticated MLS
GroupInfo and tree and require a v2 interaction binding equal to the result in
all immutable fields. The initial authenticated roster contains exactly two
leaves: the Bot's registered device and the selected human device. Each leaf
credential must match its account's current signature-verified device list,
including the complete Bot credential match required by managed Bot
registration. Binding equality alone is insufficient. The server must
independently enforce the same admission tuple and exact roster when it accepts
registration, later Adds or Removes, Welcome acknowledgement or resync; a
valid initial receipt must not authorize a changed roster. A participant device
replacement requires a fresh admission and a committed removal of the old leaf
before the new device becomes actionable.

The core must persist the verified admission tuple and receipt atomically with
the group state and its registration or Welcome outcome. Recovery and duplicate
handling must compare the exact receipt and group body, and any participant
eligibility change or removal must stop new interaction sends and invalidate
the admission for subsequent membership changes. Event attribution must come
from the MLS-authenticated sender and pass the same current device-list checks;
the binding's participant fields never override the authenticated sender.
Until all these checks and recovery rules exist end to end, kind 3 creation,
join, Welcome admission, synchronization and event delivery stay unavailable.

For the currently supported text and call paths, sender validation uses the
existing credential/device-list chain; unvalidated pending history must never
produce actionable bot messages.
Revalidation limit1..256; leaf index at most uint32 max. Network pages are bounded
by encoded size; indivisible large group objects use group_transfer below.
Actionable decrypted messages are delivered through the durable event journal,
retaining the core-authenticated sender and server receipt time. A user field in
application content cannot replace attribution.

For suboperation7, `expected_bindings_body` is a deterministic CBOR array of
`[group_id,community_id,target_id,kind,policy_version]` tuples. IDs are nonzero
bstr16, kind is0 text or1 call, and each community is the helper's installation
community. There is exactly one expectation for each distinct group in the
Welcome batch. Validate all expectations before applying any Welcome; reject
duplicates, missing or unrelated expectations and unsupported kinds. Mixed text
and call batches preserve the native per-Welcome outcome order. Both the actual
cryptographic binding and an existing active group's binding must match the
expectation before a Welcome can be treated as accepted or already processed.
Interaction expectations require the later typed participant contract and remain
unavailable until then. The existing human native Welcome API keeps its original
four-field expected tuples and behavior.

For a Bot Welcome that successfully activates a group, the helper stores one
versioned, fixed-size receipt in encrypted `app_meta`, keyed by group id. Its
digest covers the exact Welcome bytes, ratchet tree, group/epoch/commit/tree-hash
labels, installation, and immutable MLS binding (instance, community, target,
kind and protocol versions); it excludes delivery id and policy-version snapshot.
An already-active Bot group is acknowledged as a duplicate only when its current
validated MLS binding and the submitted content match that receipt. Missing or
changed content is refused without mutation. Group creation, discard, and
external-join replacement clear the receipt in the same storage transaction as
their state change, so stale Welcome evidence cannot survive another admission.

### Group upload intents and recovery

Operation ids are nonzero bstr16. Create, external_join and build_commit persist
their exact public upload body and canonical input hash in the same core storage
unit as the MLS mutation. The intent binds operation id, installation, group and
suboperation. A matching pending retry returns those bytes; conflicting input
fails. This applies to both direct requests and group_transfer. The native core
must not generate another pending commit after a lost helper or HTTP response.

intent_result returns `[kind,group_id,state,result|null]`, or `[null]` if absent.
kind is2 create,5 external_join or9 build_commit; state0 pending,1 accepted or2
refused. Pending result is the exact original core upload body as bstr. Registered,
joined, confirm_commit and their matching discard/abort operations resolve only
the matching intent in the same unit as the core transition. They retain the
resolution argument hash: equal retries are no-ops, conflicting outcomes or
sequence arguments fail. Resolved intents retain input/binding/state metadata and
clear the large result. Retrying a crypto-producing operation for a resolved id
returns conflict; intent_result provides its outcome without generating crypto.

The Go client persists its operation id before requesting an intent and queries
it after restart. It resolves an intent only after determining the authoritative
server outcome from a matching binding, handshake and epoch or a conclusive
refusal. A missing response is not refusal. Group application and Welcome retries
instead recover through persisted source progress and group membership; successful
late sender revalidation recovers through the event journal.

For first registration, the Go service also obtains the current server generation
and durably commits `(operation_id, expected_generation)` before it asks the native
core to create the group. A retry or restart reuses both values exactly; it never
substitutes a newly sampled generation or host clock. The native create intent's
pending result is the exact deterministic-CBOR kind-3 registration body
`[group_id,binding,group_info,ratchet_tree,initial_welcome]` as bstr. The Go service sends those
same bytes to the scoped HTTP route and retains them until a receipt resolves the
intent. The generation fence therefore covers the native MLS mutation that
produced the body, even though the generation is carried outside that body.

### Authenticated event journal

Only a validated native Bot identity can populate or read the journal. Accepted
non-own text-group applications enter it in the same core unit as their verified
message insertion, before folding changes their body. Successful pending-sender
revalidation does the same, even for an older source sequence. No event is created
from an undecryptable, pending, mismatched, deleted or pruned row, a call-group
application, or an own upload echo. Group scope comes from the verified stored
binding. The initial feed admits only text groups in the installation community;
interaction admission and its event context require the later typed installation
binding and are refused until that contract is implemented.

Each event has an independent, monotonically increasing delivery id. The unique
source key is `(group_id,source_sequence)`, which remains after acknowledgement.
A newly authenticated old message receives a new delivery id. The event shape is:

`[delivery_id,group_id,community_id,channel_id,source_sequence,epoch,recv_ts,
 sender_user,sender_device,sender_kind,sender_tier,sender_leaf,message_id,
 envelope_type,original_body,reply_to|null,thread_id|null]`.

The original body is the authenticated envelope body, bounded by the existing
envelope limit. Sender fields derive from MLS credential, uploader agreement and
device-list validation. Internal provenance retains the signature-key information
needed for current trust checks; no private key, franking key or raw envelope is
exported to Go.

events_read has limit1..100 and returns `[events,next_delivery_id,high_water]`.
Its exclusive cursor is the greater of after_delivery_id and the durable ack.
Pages obey the full encoded response limit. next_delivery_id is the last examined
id, including retracted rows; a live row excluded for size stays unexamined. An
oversized first live row fails, never silently skips. high_water is the maximum
created delivery id. events_ack persists its id and clears acknowledged plaintext;
an equal ack is an idempotent retry, while lower or future ids fail. Compact
source/delivery/disposition metadata remains until a proven non-replay floor
permits removal. This acknowledgement is distinct from server cursor_ack op13.

Tombstones, author deletions and trust retraction suppress queued plaintext in
the same unit that withdraws their source. This includes late tombstones,
message_deleted, fold-driven target deletion and trust quarantine. An authenticated
author-delete event may itself be delivered while its target is suppressed.
Drain rechecks source status and current group binding/state/trust; a journal row
alone is not authority. Go persists feature progress before acknowledging, uses
stable effect ids on redelivery, and checks current permission before acting.
There is no atomic transaction spanning an external effect and the local journal.

### Bounded group transfer

Some server-valid group objects exceed one helper frame. Op16 retains the1MiB
frame cap while carrying a typed op6 logical request/result in bounded chunks.
Only target operation6 is allowed: no nested transfer, path, SQL, identity export
or arbitrary byte store. Input and result are each at most32MiB; existing core
and server operation limits still apply, including the256-Welcome commit limit.

| Suboperation payload | Response |
| --- | --- |
| `[0,6,input_length,input_sha256]` | `[transfer_id]` |
| `[1,transfer_id,offset,chunk_bstr]` | `[next_offset]` |
| `[2,transfer_id]` | `[target_status,result_length,result_sha256]` |
| `[3,transfer_id,offset,requested_length]` | `[chunk_bstr]` |
| `[4,transfer_id]` | `[]` |

transfer_id is a generated opaque bstr16 bound to the initiating request and
inherited socket connection. One transfer exists per helper; chunks are at most
768KiB. Validate declared lengths before allocation, exact increasing upload
offsets, full length and SHA256 before executing. Reads require execution and an
in-range slice of1..768KiB; repeated reads are allowed. Repeated execute returns
the same saved descriptor without executing again on that connection. A target
failure returns its normal sanitized status with deterministic `[]` as result.

Transfers use no disk staging. Clear and zeroize buffers on failure, release,
shutdown or disconnect. Upload has a30-second deadline from begin; completed
results expire after30 idle seconds. A silent peer must not prevent expiry.
A second connection cannot resume/read another transfer.
Unknown ids, invalid length/hash/offset/target or reads before execution fail with
status1; contention uses6. Core mutation stays serialized; unrelated mutation
requests receive6 until release rather than blocking the socket. Shutdown remains
available. Transport-owned input, result and encoding/chunk scratch stay
within96MiB in aggregate; do not multiply whole-payload copies. Core MLS working
state separately follows its existing bounds.

The Go parent enforces execution deadlines and terminates an unresponsive helper.
A transfer descriptor is not durable proof of execution: after interruption,
the client uses the typed intent or source-progress recovery rules above. It never
blindly repeats a crypto-producing operation or discards uncertain network work.

Call suboperations are: 0 media_epoch; 1 join_check; 2 leave_proposal; 3 rotate;
4 forget. Each calls the corresponding existing call-group core operation. Media
epoch material is returned only for an active, authorized call group, with its
epoch and roster. The private Go media pump owns separate key rings per call and
clears these secrets when that call stops.

Prepare-send durably associates a logical operation with its immutable input,
original timestamp, message ID and envelope. Crypto state, attempt and exact
upload result commit together before returning. Exact attempt retries return
identical bytes after reopen. Upload body is the seven-field Bot upload below;
its predecessor is derived from the native journal, never trusted from a caller.
Keep one unresolved attempt per operation and the normal one-flight-per-group
rule. A helper timeout never permits a new attempt ID or replacement envelope.
The franking commitment remains inside the existing MLS construction; neither
upload-class enforcement nor franking is bypassed.

Receipt confirmation checks operation, attempt, predecessor, community, group,
generation, epoch, class, ciphertext hash and the exact original response before
changing the outbox or timeline. Acceptance atomically confirms the outgoing
message and seals the operation, with idempotent confirmation after outbox
deletion. Outcome 0 retires only that attempt and permits the same logical
envelope/message ID to produce a successor. Outcome 2 makes the operation
visibly uncertain and terminal for automatic execution. Conflicting or opposite
outcomes fail; an old retry cannot change a newer successor. Retiring unused
ciphertext never rewinds MLS ratchets. No generic send-requeue bypass is exposed.

State areas are 0 settings,1 queues,2 schedules,3 occurrences,4 moderation,
5 processed operations,6 structural cursors,7 role-grant sources,8 send operations.
Keys are nonempty bstr of at most128 bytes; values are deterministic CBOR arrays
of at most262144 bytes. State-get suboperation0 is `[area,key]` and returns
`[revision,value|null]`, missing revision0. Suboperation1 scans
`[area,after_key|null,limit]`, limit1..100, and returns `[rows,next_key|null]`;
rows are `[key,revision,value|null]`, sorted lexicographically and also limited by
encoded response size. Tombstones retain their revision. State never exposes
identity, session or MLS internal tables.

Changes in a state transaction are 1..64 unique `[area,key,expected_revision,
value|null]` entries, still under the whole frame limit. Every revision check and
write commits atomically; each revision advances by one and overflow refuses.
The encrypted transaction receipt binds the transaction id to the canonical
change bytes and original result. Repeating matching bytes returns the same
revisions; conflicting reuse fails. No partial writes survive a conflict or
storage failure. Structural cursor updates can share this transaction with the
resulting feature-state change; network acknowledgement occurs afterwards.

## Session capability and compatibility

Wire 3 adds the bot contract; wire 2 remains supported outside bot-enabled
functionality. Human signup, E2EE envelopes and media formats are unchanged.
`POST /v1/sessions/negotiate` requires an enrolled device session and accepts
`[[wire_versions], [e2ee_versions], [media_versions]]`, each list of 1..8 unique
positive uints. It chooses the highest common version from server support and
persists the selection against the authenticated bearer session. Response:
`[wire, e2ee, media]`. No common version is `400 E_VERSION`; no row changes on
failure. An authenticated gateway identify records its accepted versions against
the same bearer session before publishing ready. An accepted downgrade replaces
previous capability; device identity alone cannot lend capability to another
session. Revocation removes the binding. Resume verifies the current bearer and
reestablishes the original connection's versions before replay.

All bot routes and bot-enabled community mutation/sync operations check the
resolved session's negotiated wire >=3. Missing/older capability yields
`400 E_VERSION` with `supported=[3]`, allowing the UI to show an upgrade action.
A request header containing a version is not evidence of negotiation. Existing
wire-2 sessions can continue using unaffected communities, DMs and account routes.
The shipping human client advertises [5,4,3,2], selects the highest supported version, and
retains an explicit bot-unavailable/upgrade state when selected wire is 2.
Managed Bots continue to negotiate exactly wire 3. Wires 4 and 5 preserve the Bot
operations, including music controls and announcement registration; wire 5 adds
the human client's ephemeral transport described in `02` and `09`.
Runtime provisioning credentials are a separate scope, never a device session or
instance-admin bearer. Bot runtime claims and enrollment require the wire-3 body
contract directly; enrolled bot actions require the normal negotiated session.

For this gate, a community is bot-enabled while its current installation has no
removal timestamp and is not disabled. Pending, enrolling, joining, ready and
error installations all count. Ordinary community, channel, member, role,
permission, invite and ban metadata snapshots remain readable under their normal
authorization, so an older client can discover the community and explain its
upgrade requirement. The gate covers community structural mutations, group
registration and synchronization, message history and readable-channel message
operations (including reactions, pins, search and read state), channel attachment
operations, and community call admission/control/synchronization. DM, pairing,
account and instance operations with no community binding remain available.
Publishing one's account KeyPackages and fetching a device's public KeyPackage
remain unscoped directory operations: those requests carry no group binding.
The subsequent group registration, Add or resync admission and each Welcome are
checked against the actual group's scope. Bot interaction groups
retain their existing null-community binding but require wire 3 for registration,
synchronization and delivery independently, as do bot application endpoints.

Resolve and authorize the actual resource before reporting an upgrade refusal.
Unknown or hidden resources keep their existing refusal, including reader-ACL
group access and the signed body binding on group registration. A request checks
the current committed installation state at its authorization boundary before
performing the protected operation; an operation already admitted may finish if
enablement commits concurrently. This is a compatibility gate and does not
replace the operation's current permission, membership or transaction checks.
Store failures refuse the affected operation with an availability error, rather
than treating the community as disabled.

Gateway compatibility is per resource, not per user. A wire-2 connection may
identify and receive unaffected groups, DMs and account traffic. Its ready digest,
subscriptions, group notifications, channel content and community call traffic
exclude enabled communities. Group committer eligibility applies the same gate.
Ordinary community metadata-change notifications may still prompt a metadata
refresh. Each scoped delivery checks committed state before entering the writer
queue; a frame already queued before enablement may finish. Every gateway uses
the shared authoritative state, without a stale process-local enablement cache.
Replay entries retain internal resource scope, including all groups in a ready
digest. If an entry to be replayed is now disallowed, resume is refused as
nonresumable and the client identifies again for a filtered digest; entries must
not be silently skipped within the sequence. A live connection retains its
identify-time version until it closes; HTTP uses its bearer's latest negotiated
selection. Resume revalidates the bearer and current resource eligibility before
restoring the original connection versions. Managed bot connections additionally
retain the requirement that both the bearer and original connection selected
wire 3.

## Installation and enrollment API

Auth labels E and A here mean enrolled human and instance-admin sessions. B means
a live managed bot session bound to this community's installation/account/device.
B uses the existing enrolled device-session scope (numeric 0), with additional
installation-state and route restrictions; it is not a fourth session scope.
The installation state enrolling is independent of that session scope. Such a
session may negotiate capabilities while enrolling, joining or ready, before
calling routes that require wire 3. Capability negotiation itself does not
require a previously negotiated version and cannot relax installation limits.
R means a hashed, operator-registered runtime credential with provisioning scope.
All E/A changes require existing step-up when the corresponding human operation
requires it. Installation configuration additionally checks each granted
capability's corresponding permission and role ceiling. A bit alone never grants
new authority to the runtime or bot account.

Capabilities are a uint bitmask: bit0 music,1 welcomes,2 roles,3 announcements,
4 warnings/timeouts,5 message removal,6 kick,7 ban, 8 polls, 9 reminders. Unknown
bits fail. Enabled capabilities are approved constraints in addition to normal
current permissions. Default automation actions are log-only; automatic bans are
unavailable. Polls and reminders (bits 8 and 9) need no community permission and
gate only creation (`poll.create`, op 70, and `reminder.create`, op 80): with the
bit off, op 3 omits the create operation, so `/poll` or `/remind` leaves the
picker, and the Bot refuses a create with status 5 after reading the
installation's current mask. Every other poll and reminder operation stays:
existing polls can still be voted on and closed, and members still list, cancel,
dismiss and receive due reminders. Installations from before the bits existed
have both on (migration 00050), which is how they behaved.

`installation = [installation_id, community_id, state, revision, bot_user|null,
 bot_device|null, capabilities, updated_at]`; state uses the structural event enum.
The manager's read (`GET /v1/communities/{community}/bot`) and a runtime that asks
for `GET /v1/bot-runtime/installations?v=2` get the widened installation: those
eight fields followed by `[restart_requested, restart_handled,
runtime_version|null, music_volume, music_idle_minutes]`. A runtime that does not
ask for `v=2`, and every Bot-session credential or ready answer, gets the eight
fields with only the original capability byte (bits 0..7), which every runtime
parses. A runtime that asked for `v=2` and still gets eight fields is talking to
a server that predates bits 8 and 9; it treats Polls and Reminders as on, as that
server has no switch for them.

**Restart.** `POST /v1/communities/{community}/bot/restart` (manage_community)
counts a restart request on the installation and is audited (`bot.restart`). A
request while an earlier one is still unhandled joins it, so a double click or
a repeat restarts once. The runtime sees `restart_requested` above what it last
honoured on its next poll, tears down everything it holds for the installation
(native helper, music queues and voice rooms, automatic moderation state,
gateway connection) and builds them again from durable state. Pending
interaction results recover through the usual durable outbox. A runtime that
starts counts any pending request as honoured: starting is the restart. It then
reports `PUT /v1/bot-runtime/installations/{installation}/status` (R, owning
runtime) with `[version|null, restart_handled]`: `version` is its build string
(1..32 of `[0-9A-Za-z.+_-]`), and `restart_handled` is stored as at most
`restart_requested` and never goes backwards. A runtime that never reports
leaves `runtime_version` null, and clients show no version.

**Music defaults.** `PUT /v1/communities/{community}/bot/music-settings`
(manage_community, audited `bot.music_settings`) stores the default volume (25,
50, 75 or 100) and the idle-leave time (5, 15 or 30 minutes). Installations from
before them have 100 and 30, which is today's behaviour. The Bot applies them
and enforces the same options itself: any other value, or a server that does
not send them, is 100 and 30. A new session (a track enqueued while nothing
plays or waits) starts at the default volume. Once nothing has played for the
idle time (paused, stopped or the queue finished), the Bot leaves the voice
room. Playing again cancels the timer, and the next explicit play reconnects.
The room's absolute 30-minute connection lifetime still applies.
A single non-removed installation exists per community; repeat enable with the
same request id and payload returns the original result. Removed installations
are never reactivated; a new enable creates a new identity and installation id.
Soft disable retains the installation ID. Who may DJ is not installation
state: it is the `control_music` role permission (09 § Permissions), so it
survives disable, re-enable and replacing the installation.

Music playback controls require `view_channel` and `control_music` in the
target call's voice channel, resolved now by the server with that channel's
overwrites (`GET /v1/communities/{community}/bot/actors/{actor}/channels/{channel}/authority`;
owners and administrators resolve to every bit), plus membership in the
target's current voice call. The
Bot must retain connect and speak permissions and an active, ready Music-enabled
installation. These checks run again before a queue mutation is committed,
including after provider lookup. Owners and administrators may additionally
issue an emergency stop without being in the call. Instance-admin status alone
does not grant community music control. Structured music commands target the
caller's joined voice channel, including an emergency stop.

Managed Bot voice access is publication-only: tokens and current SFU permission
updates prohibit media subscription and permit only microphone-source audio
publication when Speak is granted. Camera, screen, data and receiving other
participants' tracks are unavailable. Admission checks reject older tokens
that grant subscription; grant reconciliation and fallback demotion cannot
restore that right. The music publisher also disables automatic subscriptions.

| Method and path | Auth | Request | Response |
| --- | --- | --- | --- |
| `GET /v1/communities/{community}/bot` | E, manage_community | — | installation or CBOR null when absent |
| `PATCH /v1/communities/{community}/bot/profile` | E, manage_community | `[display_name]` | `[normalized_display_name]`; only the managed bot account's display name changes |
| `PUT /v1/communities/{community}/bot` | E, manage_community | `[request_id, expected_revision|null, capabilities, step_up]` | installation, 201 new / 200 existing. With `v=2` the mask is the full ten bits and is stored as sent. Without it the mask may name only bits 0–7 (400 otherwise); bits 8 (Polls) and 9 (Reminders) keep their current value, and a new installation starts with both on. The request hash covers `v=2`. |
| `POST /v1/communities/{community}/bot/disable` | E, manage_community | `[request_id, expected_revision]` | installation |
| `DELETE /v1/communities/{community}/bot` | E, manage_community | `[request_id, expected_revision]` | 204 |
| `POST /v1/communities/{community}/bot/restart` | E, manage_community | `[installation_id]` | `[restart_requested, restart_handled]`; 409 for another, disabled or runtime-less installation |
| `PUT /v1/communities/{community}/bot/music-settings` | E, manage_community | `[installation_id, volume, idle_minutes]` | widened installation; 400 off the options, 409 for another installation |
| `GET /v1/bot-runtime/installations` | R | — | array of installation tuples, at most 100; `after` is last returned installation id; `v=2` asks for the widened tuple |
| `PUT /v1/bot-runtime/installations/{installation}/status` | R, owning runtime | `[version|null, restart_handled]` | 204; 404 for an installation this runtime does not own |
| `POST /v1/bot-runtime/installations/{installation}/claim` | R | `[request_id]` | `[installation_id, claim_id, claim_expires]` |
| `POST /v1/bot-runtime/installations/{installation}/enroll` | R, matching claim | `[request_id, claim_id, umk_pub32, ssk_pub32, sig_umk_ssk64, device]` | `[bot_user, bot_device, username]` |
| `PUT /v1/bot-runtime/installations/{installation}/credential` | B, enrolling; completed retries while joining/ready | `[request_id, expected_revision, credential_bstr]` | installation |
| `POST /v1/bot-runtime/installations/{installation}/ready` | B, joining; completed retry while ready | `[request_id, expected_revision]` | installation |
| `POST /v1/bot-runtime/installations/{installation}/keypackages` | B, joining/ready | `[operation_id, expected_generation, upload_body]` | `201 [count]` |
| `GET /v1/bot-runtime/installations/{installation}/keypackages` | B, joining/ready | — | `[generation, server_time, ordinary_active_count, has_current_fallback]` |
| `GET /v1/bot-runtime/installations/{installation}/keypackages/{operation}` | B, joining/ready | — | `[expected_generation, upload_sha256, count]` or 404 |
| `POST /v1/bot-runtime/installations/{installation}/groups` | B, joining/ready | `[operation_id,expected_generation,registration_body]` or kind-3 `[operation_id,expected_generation,registration_body,admission_request_id]` | `201` new / `200` exact replay: registration receipt |
| `GET /v1/bot-runtime/installations/{installation}/groups/{operation}` | B, joining/ready | — | registration receipt or 404 |
| `POST /v1/bot-runtime/installations/{installation}/interaction-admissions` | B, joining/ready, wire 3 | `[request_id, group_id, participant_user, participant_device]` | `[payload_bstr, signature_bstr]`; issues the short-lived proof consumed by kind-3 registration |
| `GET /v1/bot-runtime/installations/{installation}/interaction-admissions/{request}/group-absence` | B, ready, wire 3 | — | CBOR boolean; true only while the matching admission is live and its exact group ID is absent |
| `POST /v1/bot-runtime/installations/{installation}/interaction-admissions/context` | B, joining/ready, wire 3 | `[request_id, group_id, participant_user, participant_device]` | `[snapshot_payload_bstr, snapshot_signature_bstr]`; read-only v1 snapshot signed with the instance context-snapshot domain, max 160 KiB |
| `GET /v1/groups/{id}/interaction-admission-context` | enrolled human participant device | no body | `[current_payload_bstr, signature_bstr]`; read-only v1 current-group proof signed with the dedicated current-interaction-context domain, max 160 KiB |

`device` is `09`'s native registration tuple, with device tier Native and signer
tier Native. Credential kind MUST be Bot, and all key ids, key signatures, device
keys and tiers must agree. The placeholder user id is zero only in the enrollment
credential because the server creates the account id; the final credential and
signed device list MUST bind the assigned account before any group admission.
Human `/v1/accounts` cannot enroll Bot credentials. Claims last ten minutes, are
bound to the runtime credential and installation, and repeat requests return the
same unexpired claim. Enrollment stores its operation result transactionally;
repeat matching requests return the same account/device, not a second identity.

The human `GET /v1/groups/{id}/interaction-admission-context` is a read-only
Welcome-processing input. The path id is only a lookup hint: the server resolves
the immutable accepted registration receipt, imports the currently persisted MLS
public state, and refuses unless its canonical v2 binding and exact two-leaf
authenticated roster still agree with that receipt. The enrolled, non-Bot session
must be the participant user and device named by that binding. At issuance the
server also requires that participant and Bot accounts/devices remain active,
both remain community members and are not currently banned, both devices remain
in their users' verified signed device lists, the Bot installation remains
joining or ready, and the current policy still matches the binding. This route
does not mutate group, admission, receipt, or Welcome state.

The payload is deterministic CBOR fixed-position array v1:
`[version,instance_id,instance_key_id,instance_signer_pub,issued_at,not_after,
registration,binding,current_epoch,leaf_count,tree_hash,roster,
eligibility_revision,policy_version,e2ee_version,media_version,bot_device_list,
participant_device_list]`. `registration` is
`[installation_id,operation_id,bot_user_id,bot_device_id,community_id,group_id,
body_sha256,generation,accepted_epoch,next_seq,accepted_at]`; `binding` is
`[instance_id,installation_id,community_id,bot_user_id,participant_user_id,
participant_device_id,policy_version,e2ee_version,media_version]`; each roster
entry is `[leaf_index,user_id,device_id,signature_key]`. Each device-list record
uses the existing snapshot tuple `[user_id,umk_pub,ssk_pub,ssk_binding_signature,
version,blob,signature,prev_hash,hash]`. The response signature is Ed25519 over
`ASCII("dilla bot current interaction context v1") || 0x00 || payload`; clients MUST treat
the proof as a short-lived snapshot (30 seconds), verify the signature against
the persisted instance signing-key pin, and check its current-state fields before offering
or processing a Welcome. `issued_at` and `accepted_at` are instance-clock values while the
client checks them against its own clock `now`. The human client allows 5 seconds before
them: it accepts `accepted_at <= issued_at <= now + 5`. It allows nothing past `not_after`,
which must be strictly after `now`; `issued_at < not_after` and `not_after - issued_at <= 30`
hold exactly. The Bot side's pre-registration snapshot check differs: it allows the same 5
seconds for its `now` ahead of the Bot's clock and also 5 seconds past its `not_after`. A
client clock more than 5 seconds behind the instance cannot process interaction Welcomes
until its clock is corrected.
A human client establishes its first pin from `/v1/instance` over the
bearer-authenticated session created by account enrollment, and stores that key atomically with
local enrollment. Neither unauthenticated discovery nor a key included in a proof may establish a
pin. A later discovery key mismatch fails closed; this version defines no silent key rotation or
repair. Existing ready identities created before this pin existed have no authenticated pin to
migrate and must refuse this proof until an explicit trusted migration is defined. An interrupted
pending enrollment may resume its pin after it establishes a session and reads authenticated
discovery. This is distinct from the Bot-only pre-registration context snapshot above.
For an already committed enrollment, a live runtime credential may replay the
same operation id and exact payload for that installation after claim expiry.
The stored result is bound to the original runtime credential id; authorization
and that binding are checked before replay, and an expired claim cannot authorize
a new enrollment. A revoked runtime cannot retrieve results. Operator credential
rotation replaces the token hash on the same runtime id, invalidates outstanding
claims and keeps completed operation receipts; the old token immediately fails.
Rotation therefore preserves recovery without issuing a second bot identity.

The claim response names the installation id (bstr16), not an installation
snapshot. Claiming does not change the installation state or revision. Runtime
discovery supplies the current installation tuple after lifecycle changes.

Runtime discovery filters to that runtime's assigned installations and unassigned
pending installations before applying the page limit. Removed installations are
excluded. The response is the direct array of installation tuples, with no extra
outer tuple; an empty page means no further visible installation after the cursor.
Bot enrollment refuses a device signing key already registered to any device.
Subsequent device registration also refuses reuse of an existing bot's signing
key, including a revoked bot device. These checks serialize competing inserts;
they do not impose a new uniqueness constraint on legacy human-only keys.
Enrollment creates no bearer token. The helper completes its local identity with
the assigned account, then obtains a session using the normal device-key challenge
and proof. This also recovers a lost enrollment response without retaining raw
bearer tokens. The credential route verifies the final Bot/Native/Native credential
against the already registered account, UMK, SSK and device key, and atomically
stores it before moving enrolling to joining. The original operation result is
retained for matching retries; conflicting reuse fails. Claim expiry or runtime
revocation never exposes existing private identity state.

A lost finalization response is retried with the same request id and payload
after the installation has entered joining or ready. In those phases the
credential route may return only a matching completed receipt; it cannot accept
a new credential operation. The ready route likewise permits a matching
completed retry after reaching ready. Current session and installation authority
is checked before receipt lookup, so disablement or revocation still refuses a
retry. These recovery paths never reopen enrollment or replace identity keys.

All bot sessions are bound to their installation, account and registered device.
An enrolling installation permits only capability negotiation, its credential
finalization and session termination. Joining permits normal signed device-list,
KeyPackage and authorized group synchronization operations. Ready additionally
permits enabled feature actions. Disabled or removed installations cannot mint or
use sessions, including through ordinary device-key renewal. Existing non-bot
session scopes and human factor requirements remain unchanged.

Ready is accepted only after the final credential, signed native device list,
KeyPackages and required group membership are verified. Revocation/disable stops
future actions, invalidates installation sessions/claims, issues ordinary MLS
removals and disconnects associated media publishers. A runtime credential has
no generic admin or moderation access. The managed moderator-factor exception
requires a validated live installation and explicit capability approval; a bare
Bot-kind account cannot bypass human factor requirements.

`step_up` is the existing short-lived human step-up token, bound to that bearer
session. A new enable or capability change consumes it after checking current
permissions. Its secret bytes are excluded from the durable operation digest and
receipt. A matching completed retry checks current authority and the original
actor binding, then returns the stored result without consuming another token or
applying another change. Disable and removal remain available through ordinary
current human authorization so emergency shutdown requires no bot availability.

### Managed Bot KeyPackage publication

Managed Bots publish only through the installation-scoped route above; their
generic `POST /v1/keypackages` is refused. The existing human publication API is
unchanged. All three routes require wire 3, a current credential-verified Bot
session bound to this exact installation/account/device, and joining or ready
state. Runtime credentials cannot use them. `upload_body` is a bstr containing
the existing strict deterministic `[packages,last_resort]` upload. Operation ids
are nonzero bstr16, generations are uint at most signed-int64 max (including 0),
and upload_sha256 is bstr32. Count is the number of distinct accepted refs.

An accepted publication receipt binds installation, user, device, operation,
expected generation, SHA256 of the exact canonical upload bytes, count and
acceptance time. Compact receipts remain until installation removal. Reuse of an
operation with a different body or generation conflicts. Current authority is
checked before receipt lookup. Equal accepted retries return the historical
result before expiry, capacity or fallback validation and never write packages.
This remains true after a generation change, consumption or expiry. Retrying
accepted fallback A after B cannot replace B. A receipt proves historical
acceptance only; current inventory and readiness use active package counts.

The collection GET returns a read-only inventory snapshot under the same
generation, runtime, community, installation, user and device fences. Sample one
server Unix timestamp in seconds after acquiring the locks. Count ordinary
unconsumed packages whose expiry is strictly later than that timestamp, and set
the fallback flag to 1 only when the current last-resort package is unexpired.
Never use a consuming directory operation to obtain this snapshot. Generation,
timestamp and ordinary count are uint at most signed-int64 max; the flag is uint
0 or 1. This authenticated response body, rather than the caller's wall clock or
an intermediary's HTTP Date header, can establish expiry for native obsolete
resolution. The snapshot does not reserve packages: subsequent consumption may
change availability. Responses must not be cached.

First acceptance serializes publication for the installation/device using
database locks. A locked preflight checks authority, receipts and generation;
WASI then validates every package against the registered credential outside the
database transaction. The final transaction reacquires all fences and repeats
authority, receipt, generation, credential and expiry checks. It deduplicates
refs and enforces capacity counting only newly active ordinary rows.
All package writes, fallback replacement and the receipt commit atomically.
Human publication shares device serialization and atomic capacity enforcement,
without Bot receipts. Consumed ordinary refs are never revived by a replay.

For managed Bots, matching user/device ids and the leaf signing key alone is
insufficient: the complete MLS leaf credential must equal the installation's
signature-verified final registered credential. Apply this check at publication,
group registration, Add acceptance, external join/resync and heal/reseed. A human
credential under Bot account metadata is refused. The internal WASI ABI8
`validate_key_package` success tuple is
`[0,device_id,user_id,last_resort,not_after,kp_ref,signature_key,credential_identity]`;
the final field is the validated leaf's BasicCredential identity byte string,
bounded to 8192 bytes by the host. It contains no private key.

### Managed Bot group registration and recovery

Managed Bots use only `POST /v1/bot-runtime/installations/{installation}/groups`
and `GET /v1/bot-runtime/installations/{installation}/groups/{operation}` for
first registration. A B session must be enrolled, wire 3, in joining or ready
state, and bound to the path installation, its Bot account and device, and that
installation's community. Generic human `POST /v1/groups` refuses B sessions
with `403 E_FORBIDDEN`; its human authorization, four-field request and response
remain unchanged.

The POST tuple is `[operation_id,expected_generation,registration_body]` for
existing non-kind-3 registration. Initial kind-3 interaction registration adds
the nonzero `admission_request_id` as a fourth element.
`operation_id` is a nonzero bstr16, `expected_generation` is uint 0 through
signed-int64 max (nonzero for kind-3), and `registration_body` is a bstr of at most 65,505 bytes
(the whole CBOR request is capped at 64 KiB) containing
the exact canonical deterministic-CBOR four-element registration array
`[group_id(bstr16),binding(bstr),group_info(bstr),ratchet_tree(bstr)]` for existing
non-kind-3 groups. Kind-3 initial registration appends `initial_welcome(bstr)`
as a fifth element. That Welcome must address exactly one KeyPackageRef
published by the admitted participant device. The body must
decode strictly, have no trailing bytes and be byte-for-byte canonical; malformed
or noncanonical input is `400 E_INVALID_REQUEST`, a wrong media type is `415`,
and an oversized request is `413 E_TOO_LARGE`. The handler hashes the supplied
bytes directly as
`SHA-256(ASCII("dilla bot group registration v1") || 0x00 || registration_body)`;
operation id and generation are not in this hash.

The first acceptance compares `expected_generation` with the instance generation
under the same transaction/serialization fence as group creation. A stale value
is `409 E_INVALID_REQUEST` and writes nothing. A fresh acceptance applies the
current registration ACL and every existing DS group validation, then commits
the group row, serialized public MLS state, initial member rows, the kind-3
participant Welcome outbox row and immutable receipt in one transaction. The exact receipt tuple is
`[operation_id,group_id,registration_sha256,accepted_generation,initial_epoch,
next_seq]`; new acceptance returns `201`, with `next_seq = 1`, the exact value
from the original registration response. No gateway, state-cache or hook failure
after commit changes the receipt or its meaning.

Receipt identity is `(installation_id,operation_id)` and the immutable row binds
installation, operation, user, device, community, group, body hash, accepted
generation, initial epoch, next sequence and acceptance time. Operation ids are
scoped to their installation: another installation may independently use the
same 16-byte operation id, but can never read or affect this receipt. A session
whose binding does not match the path installation gets `404 E_NOT_FOUND`, with
no information about another scope. For the same installation, the exact same
operation, expected generation, group and body
returns `200` and the original receipt before checking current epoch, generation
or mutable ACL; any changed input for that operation is `409 E_INVALID_REQUEST`.
A different operation colliding with an existing group is `409 E_GROUP_EXISTS`
without group metadata. Uniqueness races roll back before the winner is read in a
new transaction; identical requests return the winner's receipt, conflicts reveal
no group metadata.

For a kind-3 request, the server first imports GroupInfo and the ratchet tree
through OpenMLS, then reads the canonical v2 binding from the authenticated
GroupContext. Its participant IDs are routing hints for loading the matching
admission preflight. Fresh acceptance requires that trusted preflight and
repeats installation, account, device-list, membership, ban, eligibility,
policy, generation and receipt checks in the final transaction that stores the
group, its two initial members and the consumed admission. A kind-3 group is
already populated by those two leaves, so the channel `AfterRegister` hook does
not run.

Before loading fresh context, a kind-3 retry checks the session-fenced durable
operation and consumed-admission receipts. An exact operation, admission,
generation and body retry returns the original receipt even after the admission
proof expires or its signed payload is compacted. Reusing either the operation
or admission request with changed data is `409 E_INVALID_REQUEST`.

The GET requires the same current installation-bound B authority. It returns the
exact six-field receipt or `404 E_NOT_FOUND`; an installation cannot distinguish
another installation's receipt from absence. Receipts survive disablement and
normal backup/restore. A restored receipt proves original acceptance even after
the group advances. A restored backup without the receipt and with a changed
generation is an unknown outcome: pause the group and retain the native intent.
Receipt absence proves nonacceptance only when the intent's original generation
is still current, the group id has no registered row, and every bot registration
uses this scoped route. The coordinator may call `bot_group_registered` only
after authenticating and matching the exact body hash, operation, group and
generation, using the receipt's original `next_seq`. It may call
`bot_group_discard` only after a conclusive server refusal or an explicit recovery
procedure that proves the original registration cannot be accepted. Never
discard, regenerate or replay the MLS mutation solely because receipt lookup is
absent.

In particular, if generation changed after the intent was created, retrying its
old expected generation may now receive `409 E_INVALID_REQUEST`; that stale
generation refusal does not resolve the earlier ambiguous request. Without a
matching receipt, preserve the intent as unknown until an explicit recovery
procedure proves the original registration cannot be accepted.

A missing receipt after restore cannot admit an old-generation publication.
Requests waiting for a device lock recheck generation and expiry when admitted;
a delayed first acceptance cannot overwrite a later batch after releasing that
lock. A missing response or receipt is not proof of refusal. The caller retains
and retries the same pending native operation until it resolves acceptance or
the explicit obsolete condition above. Restore generation and package purge
remain one exclusive server operation.

## Durable events and receipts API

| Method and path | Auth | Request | Response |
| --- | --- | --- | --- |
| `GET /v1/communities/{community}/bot/events?after=N&limit=L` | B | — | `[events, next_seq, high_water, floor]` |
| `PUT /v1/communities/{community}/bot/events/ack` | B | `[through]` | 204 |
| `GET /v1/communities/{community}/bot/receipts/{operation}?attempt=ID` | B | — | send receipt or 404 |
| `POST /v1/communities/{community}/bot/receipts/{operation}/resolve` | B | `[attempt_id,predecessor_attempt_id\|null,group_id,expected_generation,epoch,class,ciphertext_sha256]` | send receipt |
| `POST /v1/groups/{group}/bot/messages` | B | `[operation_id,attempt_id,predecessor_attempt_id\|null,expected_generation,epoch,private_message,class]` | send receipt |

L defaults to 100 and is at most 250; each page also stays below 1 MiB. Sequence
values admitted to SQL are at most signed-int64 max; overflow refuses, never wraps.
Events and their community sequence increments commit with the structural change.
Gateway opcode54 remains a wakeup; recovery always polls this stream. `after` is
exclusive. A cursor below the retained floor returns `410 E_PRUNED` with the floor
and high-water. Reconcile current membership/configuration, report the gap and
avoid inventing historical welcomes. Ack is monotone and cannot exceed the
published high-water; consumer commits progress locally before acknowledging.

### Send attempts and resolution

All three send endpoints return the direct twelve-field receipt above, without a
wrapper or separate response parameter. Every generation and epoch is bounded
to signed-int64 before any row is written, including resolve without ciphertext.
Attempt IDs and logical operation IDs are independent nonzero identifiers. First
attempt has a null predecessor; a successor names the immediately preceding
closed-not-accepted attempt. A logical operation is immutably bound to its
installation, account, device, community and group. Reusing an attempt with
different immutable data returns 409 and causes no write or notification.

The immutable fingerprint is SHA256 of deterministic
`[1,installation,user,device,operation,attempt,predecessor|null,community,group,
expected_generation,epoch,class,SHA256(private_message)]`. Resolve supplies that
same ciphertext hash without sending content. Under the same database locks as
upload, it returns a matching existing receipt or closes the unused attempt.
A later original upload must consult that closure and cannot accept it.

When the expected generation is current, resolve can durably record outcome 0.
When it is older and no receipt survived restore, resolve records outcome 2;
it must never claim the old message was not displayed. Future generations refuse.
Retained exact historical receipts take precedence over generation/epoch changes.
Unknown outcomes require visible recovery; no automatic re-encryption, delivered
status or skipped status may replace uncertainty. A deliberate human retry is a
new operation with the possible duplicate made clear. A trusted local accepted
record is not downgraded because a restored server lacks its receipt.

Current session, runtime, installation, account, device and group access are
checked before receipt lookup. Current members who lost send permission can
recover their accepted receipt or close an unused attempt; new acceptance still
requires current send policy. Revoked or disabled identities, removed membership
and lost group access refuse. Receipt recovery does not reapply mutable epoch,
slowmode or send permission to a historical result.

### Atomic acceptance

Both databases persist a logical operation row keyed by installation/operation,
with immutable scope, current attempt, optional accepted attempt and state
active/accepted/uncertain. Attempt rows are keyed by installation/operation/attempt
with immutable predecessor, generation, epoch, class, ciphertext hash, fingerprint,
outcome and canonical receipt. Enforce one null-predecessor first attempt per
operation and one successor per nonnull predecessor with database uniqueness.
Acceptance and successor creation use compare-and-swap on the logical current
attempt and require no accepted attempt. A concurrent second branch cannot win.

Use the lock order generation → runtime → community → installation → user →
device → logical operation → MLS group. The runtime validity lock applies even
when authentication used a Bot device session. Re-read authority under these
locks. Community authorization writers share the actual community row fence;
epoch, sequence, membership and freeze writers must share the actual MLS group
row fence. An in-process mutex alone cannot serialize different server processes.
Policy and permission resolvers must read the transaction's repository, including
channel/member/role/overwrite/slowmode state. Sample one server receipt time after
locking for policy, franking, stored message and original response.

Message, sequence allocation, slowmode update, canonical receipt and logical
acceptance seal commit atomically. Notify only after a new acceptance, never on
replay. Resolve uses the same operation/attempt chain and group serialization.
If restore lost an entire operation, an outcome-2 row may retain an orphan
predecessor identifier without inventing its missing history. A conflicting
existing branch refuses and stays unresolved; unknown is never a valid successor
predecessor. Structural events retain 30 days. Compact send receipts for all
outcomes retain until installation removal, independently of message retention,
so delayed closed attempts cannot become acceptable. Removal revokes the identity
before deleting receipts. The ordinary human upload API remains unchanged.

## Role delegation and human moderation API

`moderate_members` is permission bit24; include it in known masks, role editors,
hierarchy checks and moderator-factor checks. It is community-scoped, not
channel-overwritable. Existing kick/ban/manage_messages permissions retain their
meanings. Owners and administrators are exempt from automatic moderation.

| Method and path | Auth | Request | Response |
| --- | --- | --- | --- |
| `PUT /v1/communities/{community}/bot/role-policy` | E, manage_roles | `[request_id, expected_revision, join_role|null, allowed_roles]` | `[revision]` |
| `PUT /v1/communities/{community}/bot/role-panels/{panel}` | E, manage_roles | `[request_id, expected_revision, channel_id, message_id, entries]` | `[revision]` |
| `DELETE /v1/communities/{community}/bot/role-panels/{panel}` | E, manage_roles | `[request_id, expected_revision]` | 204 |
| `PUT /v1/communities/{community}/bot/role-panels/{panel}/selection` | E, own member | `[request_id, expected_revision, selected_roles]` | `[revision, own_roles]` |
| `POST /v1/communities/{community}/bot/role-grants` | B | `[operation_id, member_id, source_kind, source_id, role_id, selected]` | 204 |
| `PUT /v1/communities/{community}/members/{member}/timeout` | E or capability-approved B | `[request_id, expires]` | `[request_id, expires]` |
| `DELETE /v1/communities/{community}/members/{member}/timeout` | E, moderate_members | `[request_id]` | 204 |
| `GET /v1/communities/{community}/members/{member}/timeout` | E, own member or moderate_members | — | `[expires|null]` |
| `POST /v1/communities/{community}/members/{member}/warning` | E, moderate_members and actor outranks target | `[request_id]` | `[request_id, action_id, community_revision, accepted_at]` |
| `POST /v1/communities/{community}/moderation/actions` | E or capability-approved B | `[request_id, action, target_user|null, expires|null]` | `[action_id]` |
| `POST /v1/groups/{group}/moderation/removals` | E or capability-approved B | `[proof_bstr, signature64]` | `[action_id, removal_revision]` |
| `GET /v1/bot-runtime/installations/{installation}/moderation/removals/{request}` | B, ready installation with removal capability | — | `[action_id,community_id,group_id,actor_user_id,actor_device_id,target_seq,removal_revision,accepted_at]`; authoritative accepted-action lookup for encrypted case attachment |
| `GET /v1/communities/{community}/moderation/actions?after=N&limit=L` | E, view_audit_log or B | — | `[structural_actions, next_seq]` |
| `GET /v1/groups/{group}/moderation/removals?after=N&limit=L` | E or B, group access | — | `[removal_rows, next_revision]` |

Removal request IDs are idempotency keys. An exact accepted proof/signature replay
returns its original receipt before checking proof expiry or mutable moderator
authority. If no receipt exists and the proof is outside its acceptance window,
the server returns `400 E_REMOVAL_PROOF_EXPIRED`; the signer may create a fresh
proof with the same request ID. Clients must preserve the exact proof after any
unknown network outcome and must not infer expiry from transport errors.

Role lists have at most 100 unique ids and entries are `[role_id, emoji|null]`,
emoji at most32 UTF8 bytes. Configuring human must satisfy normal rank and
permission ceilings; privileged roles and roles above the ceiling are refused.
Panels must name an accepted bot-authored send receipt in that channel; spoofed
panels, deleted roles and inactive panels are inert. Human selection acts only
on the session user. In E2EE channels, the client submits an explicit
human-authenticated panel action only after the encrypted reaction upload is
accepted. The server binds its event sequence to the session device's accepted
application message, but cannot inspect the encrypted emoji plaintext; the API
action is constrained to the exact registered mapping and current self-role
policy. Reaction grants use the authenticated session user, never the panel
author. Tracking independent sources preserves a manual grant when a panel
reaction is removed, including after the self-role approval is withdrawn. A
manual revocation clears relevant selection sources; new selection reevaluates
current policy. The bot never receives unrestricted manage_roles.

Action uses the structural-event moderation enum. Normal current permissions,
hierarchy and step-up apply; manual UI requests use the initiating human session.
The human warning endpoint above accepts no reason and stores none in plaintext;
the encrypted manual warning-case submission flow is not yet defined.
Timeout defaults ten minutes for automation, maximum28 days per request. Expiry
is evaluated at request time without a cleanup dependency. Timeout blocks text,
reactions, edits and pins before encrypted class-1 shortcuts and on readable
routes; reading, DMs, voice, MLS/sync and self-deletion remain available. Private
music interactions are allowed when DJ authorization holds.

Evidence is an encrypted bot outbox item keyed by action request id. Attach details
only after matching a successful authoritative action record; forged/failed
requests cannot create completed cases. Default encrypted evidence retention90d,
excerpt<=512 UTF8 bytes. Structural rows never carry reason/excerpt. The bot checks
fresh view_audit_log authority before serving details. Destructive actions remain
available while the bot is offline.

Removal acceptance binds the proof to the authenticated actor/device and current
authority, then atomically writes proof/signature/action/revision/tombstone before
emitting the existing deletion wakeup. `removal_row = [revision, proof_bstr,
 signature64, accepted_at]`. The recovery feed retains markers independently of
evidence retention and includes targets a client has not downloaded. Clients sync
markers before publishing recovered timelines and exclude removed content from
late originals/edits, search, pins, replies and attachment previews. Expiry gates
new acceptance only, not interpretation of already accepted proofs.

## Command catalog, command key and Bot channels

Bot v2 keeps, per community, the catalog of commands the Bot offers, the Bot's signed
command-key publication and the list of Bot channels. None of it is command content.

A catalog is the CBOR array `[[name, description, operation, args_shape, permission_bit,
needs_bot_channel, handler_kind]…]`: `name` 1–64 bytes of text and unique in the catalog,
`description` at most 512 bytes of text, `operation` a uint, `args_shape` at most 16 arguments
`[name(text 1–32 bytes, unique), type_code(uint), required(uint 0|1)]` with type codes 0 text, 1
uint, 2 user, 3 channel, 4 duration (uint seconds), 5 bool (uint 0|1) — a user or channel argument
is a 16-byte id the member picks, never one the catalog carries; anything else is refused, `permission_bit` a mask of the permission bits of `09` § Permissions (0
means every member), `needs_bot_channel` uint 0 or 1 (the subset has no boolean), and
`handler_kind` a uint: 0 is a built-in feature, the other values are reserved for outgoing
integrations and kept as sent. At most 256 commands.

A member is shown only the commands they may use. A command is shown when `permission_bit` is 0,
or when the member's community-wide permissions hold every bit of it and, for a bit a channel
overwrite can change, at least one live text or voice channel of the community still grants it
to the member after that channel's overwrites. A Bot account is shown nothing. "Needs a Bot
channel" is a command's `needs_bot_channel` with an empty Bot channel list, not a Bot state.

The command-key publication is `dilla_core::bot::command`'s `[1, community_id, key_id,
public_key, not_after, bot_device_id, sig]`, signed by the Bot device's DSK over `"dilla bot
command key v1" || CBOR([1, community_id, key_id, public_key, not_after, bot_device_id])`. The
instance stores it only when `bot_device_id` is a listed, unrevoked device of the community's
Bot user in the newest verified device list it serves, the signature verifies under that
device's key, `community_id` is the route's community, and `now <= not_after <= now + 691200`
(8 days), and `bot_device_id` is the installation's own device. Clients verify it again against
the device list before sealing to it. A publication with a new `key_id` gives the catalog the next
revision and is pushed with it (gateway op 61 carries the publication), so a rotated key reaches
connected clients at once and reconnecting ones through the revision sync.

A Bot channel is unmarked in the same transaction when its channel stops being an end-to-end text
channel (a channel PATCH to readable), when the channel is deleted, and when the community is
deleted (which also drops the community's catalog, command key and webhooks).

| Method and path | Auth | Request | Response |
|---|---|---|---|
| `GET /v1/communities/{community}/bot/catalog` | E (community member), socket | — | `[revision, commands, publication\|null, status]`; `commands` filtered for the caller; `status` is `[state, since\|null]`, state 0 ready, 1 degraded, 2 offline, 3 disabled, 4 absent; 404 for a non-member |
| `GET /v1/bot/revisions` | E, socket | — | `[user_revision, [[scope_kind, scope_id, revision]…]]`: kinds 1 channel and 2 call (scope_id the voice channel) for channels the caller can view, kind 3 each of the caller's communities' catalog revision |
| `PUT /v1/bot-runtime/communities/{community}/catalog` | B (ready, path community) | the catalog | `[revision]`; a changed catalog takes the next revision and is pushed as gateway op 61 to every connected member, filtered for that member, after the answer; an unchanged one keeps its revision |
| `PUT /v1/bot-runtime/communities/{community}/command-key` | B (ready, path community) | the publication | `204`; `403` when the signer is not the installation's listed, unrevoked device or the signature fails, `400` for another community or a `not_after` in the past or more than 8 days ahead; a new `key_id` bumps the catalog revision and pushes op 61 |
| `PUT /v1/communities/{community}/channels/{channel}/bot-channel` | E (manage_community), socket | — | `204`; `400` unless the channel is a live end-to-end text channel that is not the AFK channel; `404` for a channel outside the community |
| `DELETE /v1/communities/{community}/channels/{channel}/bot-channel` | E (manage_community), socket | — | `204`, also when the channel was not marked |
| `GET /v1/communities/{community}/bot/channels` | E (any human member), socket | — | `[[channel_id, marked_at, marked_by]…]` of the Bot channels the caller can view (`view_channel`), oldest mark first; 404 for a non-member |

## Sealed command relay, replies and state hints

A member's device sends a sealed command (`dilla_core::bot::command`, `[1, key_id(bstr 16),
request_id(bstr 16), enc(bstr 32), ciphertext(bstr)]`, at most 16384 bytes) over its gateway
socket as `POST /v1/communities/{community}/bot/commands`, body `[request_id(bstr 16), sealed]`.
The instance decodes only that outer array, strictly: deterministic CBOR, version 1, exact lengths,
non-zero ids, and the sealed `request_id` equal to the body's (else `400`). It never opens the seal
and does not check `key_id` (it stores only the current publication, so it cannot tell a
previous key still inside its 24-hour overlap from an unknown one; the Bot refuses an unknown key).
It pushes gateway op 64 `bot_command` `[request_id, community_id, user_id, device_id, sealed]` to
the Bot runtime's connection, where `user_id` and `device_id` are the sending connection's
authenticated session, never anything from the body, and answers `202 [0]`.

The relay's only state is an in-memory ledger of `request_id → (device_id, community_id)`, kept
five minutes after the latest relay of that id and bounded at 65536 entries (the oldest is evicted
first). A command whose `request_id` another device (or another community) sent within the five
minutes is `409 E_BOT_DUPLICATE`; the same device sending it again is accepted and pushed again
(the Bot de-duplicates). The socket's `idem` replay record (`02` § `request`) answers a retry of the
same request frame from its record without pushing again; a re-send with a new `idem` reaches the
handler and is pushed again. Nothing about a command is stored: no row, no log line carries the
sealed bytes. The route is metered per device in its own `bot_command` bucket at the `write` rate
(`limits.rate.write_per_second` / `write_burst`, 2/s and burst 20 by default).

The Bot answers through `POST /v1/bot-runtime/replies`, body `[device_id(bstr 16), request_id(bstr
16), seq(uint ≤ 2^32−1), sealed(bstr 1..65536), final(uint 0|1)]` (the subset has no boolean).
The instance delivers gateway op 62 `bot_reply` `[request_id, seq, sealed, final]` to that device
only, and answers `204`; a `request_id` the ledger does not hold for that device in the replying
Bot's own community is `404`. A device whose socket dropped holds the reply in its suspended
connection's replay ring for at most two minutes (`02` § Ephemeral). `POST
/v1/bot-runtime/state-hints`, body `[scope_kind(uint 0 user, 1 channel, 2 call), scope_id(bstr
16)]`, raises the scope's stored revision and answers `200 [revision]`. The scope must be the Bot's
own community's: a member (user), a live text channel (channel) or a live voice channel (call; the
scope id is the voice channel, as `GET /v1/bot/revisions` reads it); any other scope is `404`.
Gateway op 63 `bot_state_changed` `[scope_kind, scope_id, revision]` goes to the user's live
connections, to the connections subscribed to the channel's text group, or to the online devices
of the call group of the channel's live call. Both runtime routes are metered per Bot device in a
`bot_runtime` bucket at the `read` rate (10/s, burst 60 by default).

| Method and path | Auth | Request | Response |
|---|---|---|---|
| `POST /v1/communities/{community}/bot/commands` | E (community member), socket | `[request_id, sealed]` | `202 [0]`; `400` for a malformed body or sealed form; `404` for a non-member; `403` when the installation is disabled; `409 E_BOT_DUPLICATE`; `503 E_BOT_OFFLINE` with no installation, an installation that is not ready, or no live Bot runtime connection |
| `POST /v1/bot-runtime/replies` | B (ready, own community) | `[device_id, request_id, seq, sealed, final]` | `204`; `404` for a request not relayed from that device into the Bot's community in the last five minutes |
| `POST /v1/bot-runtime/state-hints` | B (ready, own community) | `[scope_kind, scope_id]` | `200 [revision]`; `404` for a scope outside the Bot's community; `400` for another scope kind |

## Vectors

`vectors/bot.json` is generated by `packages/protocol-vectors` and consumed by
Go `internal/bot/protocol`, Rust `dilla_core::bot`, and TypeScript client-core bot
codecs. Cases roundtrip canonical bytes; rejection cases include malformed types,
wrong context, expired mutations and noncanonical encodings. Pure validation is
not runtime authorization: enrollment, current permissions, account binding,
provider authentication and store atomicity have independent integration tests.

## Native helper call-join operations

The local encrypted helper request envelope accepts operation codes 1 through 20.
Codes 19 and 20 are private IPC operations, not Bot interaction operation codes:

- 19 `call_join_check`: payload `[call_channel_id]`; checks a staged external call-group
  join against the native media-key and trust gate before the commit is posted.
- 20 `call_group_forget`: payload `[call_channel_id]`; forgets a gone local call group
  so the same channel can be joined in a later call.

Installation reconciliation may retain local groups from calls that have ended.
A call-source `403 E_FORBIDDEN` or member-only `404 E_NOT_FOUND` leaves that
source unapplied and unacknowledged and permits unrelated installation
reconciliation to continue. It is not evidence of current call authority; a later
pass retries, and new playback still requires fresh call admission. Text-source
403 errors and transport failures remain terminal for that reconciliation pass.

Call-group membership still follows the normal external-join sequence. A helper check
does not authorize a server request or mint media credentials; the server's current-leaf
gate remains in force.

### Immediate music controls

Pause (op 13) and skip (op 15) act on the current authorized queue under the
playback supervisor's state lock. Their required revision field is advisory;
clients may send 0 without first reading state. Playback advancement by the Bot
cannot cause a revision conflict for these controls. The Bot rechecks the live
call and DJ authority before applying them. Other queue mutations retain their
revision preconditions. Skip accepts an optional current item ID (32 lowercase
hex characters, nonzero), or an empty string for a visibly empty player. Under
the same state lock, a mismatched ID succeeds without advancing or writing the
queue and returns the current snapshot. Clients bind a skip to the item already
visible locally; concurrent presses or DJs targeting that item advance once.
Legacy requests with no argument remain unconditional. Immediate limiter
priority begins only after the initial CanUse and CanDJ checks pass.

A successful pause or skip may return a snapshot v1 in its response data instead
of the empty mutation receipt. The response revision must match the snapshot.
Clients use that authenticated snapshot directly; an empty receipt retains the
state-read fallback for Bots that do not return a snapshot with a control.
The production handler delegates durable reconciliation to the supervisor
without an additional handler or runtime state read. If a reload's resend
conflicts with the immutable durable request body, the client reconciles its
existing authenticated receipt; missing or failed receipt reads stay uncertain.
Provider startup runs outside the interaction drain and the supervisor state
lock, allowing controls to interrupt an in-progress start.

Snapshots that cannot fit all upcoming items within the 2400-byte response data
limit contain the largest queue prefix that fits, plus an optional slot 12 with
the total number of upcoming tracks. The stored queue is unchanged. Clients
expose that total and identify a partial queue listing; within one installation,
late snapshots cannot replace a newer authenticated revision.
