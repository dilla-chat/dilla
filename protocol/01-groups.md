# 01 — Groups

## Group kinds

Every end-to-end encrypted conversation or session is one MLS group. There are three kinds in use:

| kind | value | one per | members (leaves) | external sender |
|---|---|---|---|---|
| `text` | 0 | e2ee text channel, DM or group DM | every device of every member | the instance |
| `call` | 1 | voice session in a voice channel, DM call or group-DM call | every device present in the call | the instance |
| `pairing` | 2 | device enrolment | exactly two leaves: the authorising device and the new device | none |
| `interaction` | 3 | retired (Bot v2): refused, never created | — | — |

A `readable` text channel has no group. A call in any voice channel, including a voice channel of
a community whose text channels are `readable`, always has a `call` group: calls are always
end-to-end encrypted.

The instance keeps a `call` group to the devices present in the call. A device that stays out of
the call's room while it holds a leaf — its leave report lost, or disconnected by the instance and
that report lost — loses the leaf: when two room sweeps at least one sweep period apart find the
same leaf, taken in the same epoch, with its device out of the room, and the device was not admitted
to the room in the last 30 seconds, the instance proposes a DS `Remove` of that leaf bound to that
membership, so a device that has rejoined the group since is never removed (`09` § Voice, Leaving).

Groups use MLS ciphersuite `0x0001`. Handshake messages (Proposal, Commit) are sent as
`PublicMessage`; application data as `PrivateMessage`. Application data MUST be padded so that the
`PrivateMessage` ciphertext length is a multiple of 256 bytes (RFC 9420 §6.3.1 padding field).

Group configuration:

- `use_ratchet_tree_extension`: off. The DS serves the ratchet tree (`02-delivery-service.md`).
- `required_capabilities`: extension types `[0xF001]`; proposal types `[]`; credential types
  `[basic]`. `external_senders` is not listed in `extension_types` because RFC 9420 §7.2 forbids
  listing default extension types in `required_capabilities`.
- Past epoch secrets: a client keeps the message secrets of past epochs for 300 seconds in `text`
  groups, 10 seconds in `call` groups, and 0 seconds in `pairing` groups, then deletes them.

## dilla_binding

Every group carries a GroupContext extension of type `0xF001` (RFC 9420 private-use range) named
`dilla_binding`. Its `extension_data` is the deterministic CBOR encoding of:

```
[
  v,               ; uint, = 1
  instance_id,     ; bstr, 16 bytes
  community_id,    ; bstr, 16 bytes, or null for DMs, group DMs and pairing
  target_id,       ; bstr, 16 bytes: channel_id for text and call groups in a community;
                   ;   dm_id for DMs and their calls; the new device_id for pairing
  kind,            ; uint, 0..2 as in the table above (3 is retired)
  policy_version,  ; uint, the instance's policy version at group creation (see 02, invariant 1)
  e2ee_version,    ; uint, = 1
  media_version    ; uint, = 1 for call groups, 0 for the other kinds
]
```

The interaction kind (3) and its typed v2 binding are retired (Bot v2 Task 23, `10` § Retired v1
surfaces): a client, the Bot and the instance refuse a kind-3 binding, and each deletes any kind-3
group it still holds.

Rules:

1. A client MUST reject a Welcome, GroupInfo, Proposal or Commit whose group's `dilla_binding`
   does not match the instance it is connected to (`instance_id`), the object it expects
   (`community_id`, `target_id`, `kind`), or a version it supports. Error code `E_BINDING`.
2. `dilla_binding` MUST be listed in `required_capabilities.extension_types`, so a leaf that does
   not understand it cannot be added.
3. The extension is immutable for the life of the group. A change requires a new group.
4. The group id inside a Welcome's or a GroupInfo's GroupContext MUST equal the id of the group the
   client is joining: the `group_id` the DS serves the Welcome under, or the group whose GroupInfo
   the client fetched for an external join. A client rejects any other before it stores anything
   (a Welcome so rejected is reported as `E_BINDING`). The id and the binding are public, so
   neither alone ties a Welcome or a GroupInfo to the group the DS sequences.
5. A Welcome's GroupContext `epoch` and `tree_hash` MUST equal the `epoch` and `tree_hash` the DS
   serves with it (`02-delivery-service.md`, `GET /v1/welcomes` and `mls.welcome`: the group's
   epoch and tree hash right after the commit the Welcome was uploaded with, from the instance's
   own public view of the group). A client rejects a Welcome that does not match before it stores
   anything, and the KeyPackage it was addressed to stays usable. A member can otherwise address,
   to a device the group adds, the Welcome of a private group with the same id and binding.
6. A client keeps, per group id, the highest epoch it has held: every epoch it reached by a
   Welcome, by an external commit the DS accepted, or by a commit it merged. Rejoining over a
   group it has held, it MUST reject a Welcome whose epoch is at or below that epoch (a replay: a
   real re-admission is committed after it) and a GroupInfo whose epoch is below it (an external
   commit lands one epoch above the GroupInfo). A first join has no such floor. A heal after a
   restore (`02-delivery-service.md`, invariant 11) adopts a member's GroupInfo at or above the
   instance's restored epoch; a device whose floor is above the healed epoch can rejoin only once
   the group has passed it.

## External senders

`text` and `call` groups carry the instance signing key in the `external_senders` GroupContext
extension (RFC 9420 §12.1.8.1), with a `basic` credential whose identity is the CBOR array
`[v = 1, "instance", instance_id]`. `pairing` and `interaction` groups MUST NOT carry any external
sender; a client MUST reject a `pairing` or `interaction` GroupInfo or Welcome that has one
(`E_EXTERNAL_SENDER_FORBIDDEN`).

Client policy for proposals from the external sender:

| proposal | text / call | pairing / interaction |
|---|---|---|
| Add | accept if the credential passes `03-identity.md` checks and the user is eligible per the client's role snapshot | reject |
| Remove | accept | reject |
| GroupContextExtensions | accept only if the sole change is to `external_senders` and the new instance key is signed by the old one (`03-identity.md`, "Instance key rotation") | reject |
| ReInit, PreSharedKey, Update, any other | reject | reject |

A client that holds no role snapshot (the browser client of this version) accepts the instance's
`Add` on the `03-identity.md` checks alone; the instance's own eligibility check (`02` invariant 4)
is then the only gate.

Client policy for proposals from members:

- `Update`: accept only if its leaf node carries a credential byte-identical to the one the
  proposer's leaf holds before the commit, and the same `signature_key`; reject otherwise
  (`E_CREDENTIAL`). The same rule binds a member commit's UpdatePath: its leaf node keeps the
  committer's credential and signature key. It binds a resync too: an external commit that removes
  a leaf of the joiner's own device (the `device_id` its new leaf names) carries, in its UpdatePath
  leaf node, exactly the credential the removed leaf held. Once a leaf is in a group its credential
  — which binds the leaf to a `user_id` and a `device_id` (`03-identity.md`, "Credential") — and its
  signature key (the device's `dsk_pub`, which never changes) are immutable, across a resync
  included. An external commit whose new leaf names a device that already holds a leaf in the
  tree before the commit must remove that leaf (OpenMLS allows one `Remove` in an external commit,
  so a device found at two leaves cannot resync at all); otherwise `E_CREDENTIAL`. The device is
  read from the credentials of the tree before the commit. A client refuses such a proposal before
  queueing it and such a commit before merging it; the DS refuses both
  (`02-delivery-service.md`, invariants 4 and 5). This client rule covers external commits only:
  it does not judge an `Add`, so a client by itself does not refuse an `Add` that gives a device
  that already holds a leaf a second one on another key. The DS does: it binds every added leaf's
  `signature_key` to the device's registered key (invariant 4's `Add` clause), so a second leaf of
  a device would carry the key its first leaf already carries, and OpenMLS refuses an `Add` whose
  signature key a leaf of the tree already holds. Through a conforming DS a device therefore
  holds at most one leaf, and a new credential enters a group only with the leaf of a device that
  holds none (a first join, or an `Add`).
- `Remove`: accept only if the target leaf belongs to the proposer's own user — the committer's for
  a `Remove` carried in the commit (device revocation), the proposing member's for a `Remove`
  proposal the commit references (a member leaving, below); reject otherwise
  (`E_MEMBER_REMOVE_FORBIDDEN`). Removing other users is the instance's job, bound to roles. A
  member's `Remove` never cancels or stands in for the instance's `Remove` of the same leaf while it
  is only proposed: a committer holding both commits the instance's. RFC 9420 allows one `Remove`
  per leaf in a commit, and OpenMLS commits the later of two queued for one leaf, so a client queues
  a member's `Remove` in front of an instance `Remove` of the same leaf it already holds, whatever
  order it received them in, and keeps both queued. A commit that applies the member's instead is
  still accepted: it removes the same device, and the DS counts the instance's `Remove` as
  satisfied (`02-delivery-service.md`, invariant 4).
- `Add`: accept only in `pairing` (first join of the second leaf) and `interaction` groups (the
  user's device adding the bot device or a new own device); reject in `text` and `call` groups.
- `GroupContextExtensions`, `ReInit`, `PreSharedKey`: reject. dilla has no feature that changes a
  group's context extensions after creation, so a `GroupContextExtensions` proposal is refused
  standalone (before queueing) and in a commit, by value or by reference, whoever sent it, and the
  group state is left unchanged; the DS refuses the same (`02-delivery-service.md`, invariant 4),
  so a member cannot replace `external_senders` with a key the instance does not hold.

External commits (RFC 9420 §12.4.3.2) are accepted in `text` and `call` groups for: joining a
community's channels, adding a new device of an existing member, joining a call, and resyncing.
The `Remove` proposal inside an external commit MUST target only a leaf with the joiner's own
`device_id`; otherwise the client rejects the commit (`E_EXTERNAL_COMMIT_REMOVE`).

## Joining

At a Welcome join or external join, the client validates every current authenticated MLS leaf
against its signed device-list history before displaying group content. It repeats that check for
each proposed `Add` before creating its own commit, after merging a remote commit, and for the
actual authenticated MLS sender of every application message. The credential identity and DSK
come from that leaf, never from a worker-supplied claim. An invalid `Add` this device would commit
is refused before commit creation. If a remote commit has already merged, its invalid leaf stays
in the MLS tree but is quarantined: its messages are withheld and previously displayed messages
from that device are retracted (`03-identity.md`, "Device list"). Unknown history leaves the leaf
pending, without rendering its message body. A validated `Remove` of a changed-UMK leaf clears
the send block for that group alone; it does not verify the changed key or release other groups.

- An online device joins by **external commit** using the GroupInfo and ratchet tree served by
  the DS. The DS refuses external commits while a DS proposal is outstanding, except when no member
  device is online (`02-delivery-service.md`, invariant 5).
- An offline device is added by a **DS Add proposal** carrying one of the device's KeyPackages,
  committed by an online member. Each device keeps 32 ordinary KeyPackages plus 1 **last-resort**
  KeyPackage on the DS; a join that consumed the last-resort package MUST be followed by an
  `Update` from that device in its next commit opportunity.
- Creating a private channel, or granting a role that opens a channel to many members, is done by
  the DS issuing Add proposals in batches: the creator's device commits at most 256 Adds per
  commit, each producing one Welcome, until all eligible devices are members.
- A member leaves a channel or a call by a `Remove` **proposal** of its own leaf, which another
  member commits — a device cannot commit its own removal (RFC 9420 forbids a committer removing
  itself) — or is removed by the DS. A client leaving a call posts that proposal before it
  disconnects from the SFU. A member proposal never cancels, voids, blocks or replaces a DS
  proposal: when the DS is already removing the leaf (a kick, a ban, an eviction) it refuses the
  member's own `Remove` with `409 E_REMOVE_PENDING`, which the client reads as "I am being removed"
  and answers by withdrawing its refused proposal, without retrying (a client that re-uploads its
  own proposal, for instance after a lost answer, is answered as the first time — never with this
  code — and keeps its proposal);
  and the DS issues its own `Remove` of a leaf regardless of the member's own `Remove` of it
  (`02-delivery-service.md`, invariant 6).
- **Calls.** A client neither joins a `call` group nor derives its media keys (`05` § Key
  schedule) while any leaf of it other than its own holds a failed or changed verdict, or a verdict
  still pending a signed device list; it refreshes those lists once, against the staged join, and
  posts its external commit only after the whole tree passes. For the call group of a DM or group
  DM it also refuses while that DM's `text` group is send-blocked. During a call, a leaf whose
  verdict becomes failed makes the client derive no further key and leave the call (its own
  `Remove` proposal, as above); a leaf whose verdict becomes changed (a member's identity changed
  and is not yet verified) makes the client pause instead: it derives, sends and installs no key
  for that call and stays a member, until the change is verified or that leaf leaves the group.
  When a new epoch's key cannot be derived for any other reason (a leaf still pending its list
  after the refresh, or an error), the client holds the call the same way and also stops sending
  and playing media under the previous epoch's key, which a leaf that epoch's commit removed still
  holds: no media under the previous key from the moment the client applies the new epoch's
  commit, not only no new key, until the current epoch's key is derived. Frames sent before that
  moment (the commit's delivery and the client's own hand-over to its media path) are under the
  previous key; `08` § Residual trust states the window.
  The call goes on for the other members either way.

## Cadence

- **Update cadence (post-compromise security):** a device in a `text` group sends an `Update`
  proposal at most every `24h × max(1, ceil(leaves / 64))` and only if it has sent at least one
  application message since its last `Update`. In `call` groups a device rotates its leaf at least
  every 60 minutes, by an empty commit with an UpdatePath or an `Update` proposal another member
  commits; this is a client obligation the DS does not check. The web client rotates its call leaf
  every 60 minutes after it joined, and again 60 minutes after each of its own commits. The DS never commits: it elects one
  committer per round for its own proposals (02, invariant 7).
- **Inactivity:** a device that has not connected for **90 days** is removed from every group by a
  DS `Remove` proposal (founder decision 2026-09-23). It rejoins by external commit.
- **Quarantined and revoked devices:** the leaf of a device that is quarantined (`02` invariant 9)
  or revoked is removed from every `text` and `call` group (DMs included) by a DS `Remove`, which
  the instance's periodic reconcile proposes again for as long as the device holds a leaf there
  with no DS `Remove` of it outstanding (`02` invariants 9 and 11). Each such `Remove` freezes its
  group until a member commits it, so a revocation is followed by a membership commit in every
  group the device was in. `pairing` and `interaction` groups carry no external sender: there a
  member removes a revoked device of its own user itself (§ External senders, member `Remove`s).
- **Epoch rotation on membership change:** every Add or Remove is a new epoch; senders MUST NOT
  send application data in an epoch whose commit they have not processed.

## Error codes

`E_BINDING`, `E_EXTERNAL_SENDER_FORBIDDEN`, `E_MEMBER_REMOVE_FORBIDDEN`, `E_EXTERNAL_COMMIT_REMOVE`,
`E_UNSUPPORTED_VERSION`, `E_UNSUPPORTED_SUITE`. Codes are stable strings; clients MAY show them in
diagnostics.
