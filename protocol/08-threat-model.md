# 08 — Threat model

## Adversaries

In scope, in the order the design defends against them:

1. **The instance operator** and anyone with the operator's access: disk, database dumps, backups,
   process memory of `dillad`, TLS keys.
2. **The hosting provider and the network**: hypervisor snapshots, traffic capture, relays (TURN,
   the SFU), the push relay.
3. **A malicious or compromised member** of a group: reads what the group shares, may try to
   forge attribution, may report messages.
4. **A compromised member device**: a device whose keys were extracted; the goal is forward
   secrecy for earlier messages and post-compromise security after the device updates or is
   revoked.
5. **An operator serving malicious client code** to `browser`-tier sessions. This is a documented
   exclusion for that tier: the web client is a lower-trust tier and the UI marks members on it.
   Native clients bundle their code; Android and Linux desktop builds are reproducible.

## Guarantees

For `text`, `call`, `pairing` and `interaction` groups, against adversaries 1–3:

- Confidentiality of envelopes, attachments and media frames: only current members' devices hold
  the epoch secrets (RFC 9420 §16).
- Forward secrecy: a device deletes past epoch secrets after the windows in `01-groups.md`; an
  attacker who later obtains the device cannot decrypt earlier epochs beyond those windows.
- Post-compromise security: after an `Update` from the compromised device, or its removal, later
  epochs are secret again (`01-groups.md`, cadence).
- Membership integrity: adds and removes are proposals bound to the permission system through the
  instance's external-sender key, committed by members, and visible to every member as tree
  changes; the instance cannot silently add a leaf that members do not see.
- Sender authenticity for envelopes: MLS `PrivateMessage` signatures under the sender's DSK, with
  the credential chain to a pinned UMK. The own account's UMK is never trusted on first use: each
  device pins it, verified, from its own credential, so a leaf the instance joins under the own
  user id with another UMK fails validation and blocks sending in its group
  (`03-identity.md`, rule 2). A real change of the own UMK on another device is therefore not
  accepted by a device still holding the old credential; that device must enrol again.
- Verifiable reports: franking (`04-envelope-and-franking.md`).
- Cryptographic device revocation: signed device lists (`03-identity.md`).
- Call media keys follow the group's validation: a device derives no `base_key` for a `call`
  group while any other leaf of it holds a failed, changed or pending verdict, or, for a DM's or
  group DM's call, while that DM's `text` group is send-blocked (`01-groups.md`, "Calls"). Nor
  does it go on sending under the previous epoch's key once a newer epoch is refused: from the
  moment it applies the new epoch's commit it holds the call with no media under the previous
  key until the current epoch's key is derived.
- The browser key path: in a browser call, each epoch's `base_key` leaves the core worker only as
  a transferred `ArrayBuffer` posted to the page that owns the call, which hands it, transferred
  again, to that call's media worker and zero-fills its own copy after the hand-off. It is never in
  a state slice, a command result, a log, storage, a URL or a `BroadcastChannel`.
- Attachment players: an audio or video attachment of a listed type plays only on an explicit
  play, from a `blob:` URL the page mints from the decrypted bytes and revokes when the player
  goes away. Its bytes are sender-supplied and are decoded by the browser's media pipeline in the
  page: the same exposure as a decrypted image, which the page's image decoder parses. No player
  autoplays, and no decrypted attachment is kept in any store.
- Local stores are never downgraded: a client refuses to open a local store written by a newer
  client (an app schema above the one it knows, or a newer OpenMLS storage version) before any
  migration or write, so an older client never rewrites or drops what a newer one stored, the
  group state and keys included. It says so with both numbers and offers no reset, which would
  delete those keys.

Against adversary 4: forward secrecy and post-compromise security as above; recovery keys are never
on devices except during signup or recovery.

## Residual trust

Stated plainly in the product's documentation and onboarding:

- The instance may add any **legitimate** user's devices to groups it controls (adversary 1). This
  is visible: members see "X joined" and the leaf in the tree, and a pinned-UMK mismatch is a loud
  alert. It is not silent, but it is possible.
- A call's previous epoch key still reaches media until each client applies the commit that
  ended it: a removed leaf can read what members send in that window (the commit's delivery and
  each client's hand-over to its media path), and an instance that delays or withholds the commit
  from a member stretches it for that member. The removed leaf is told nothing new by the commit
  itself.
- A call start's `rung` (`09` § Voice) is empty also when the instance's table of ringing calls is
  full (4 096 calls, which takes at least 512 callers): any caller can then tell the instance is
  under that load, though not who causes it or whom they call.
- The instance serves the role view that clients use to count pins and `@everyone`/`@here` (`04`
  § Semantics). It is not authenticated by the group. The instance can hide every pin and
  broadcast mention, give pin or broadcast rights to any member, or serve different views to
  different members or devices so their pins differ. It cannot read or forge a message, and the
  view never decides which messages are shown.
- Anyone who knows a username can lock that account's failed-login ledger with wrong passwords
  (at the per-handle rate). On an instance whose `auth.webauthn.user_verification` is not
  `required`, that lockout also refuses the owner's presence-only passkey login, because it would
  still owe a code the lockout forbids guessing. A passkey login with user verification (the
  default) is the whole login and still works; the lockout ages out over the observation window.
- The instance may withhold newer device-list history on first sight. Trust on first use cannot
  prove that the served signed head is the newest, so first sight is not called verified.
- A remote member can repeatedly rotate a changed-UMK leaf and keep an honest sender blocked in
  groups still containing it. Exact-key verification or a validated Remove ends that group's
  block; removing it from one group does not approve the key or release another group.
- A member who commits an Add, or the instance, can put a leaf that fails validation into a group
  and so block sending there (fail closed: no member encrypts to a leaf it could not validate).
  A validated Remove of that leaf releases that group only; the committer stays usable elsewhere
  and the audit names it.
- In a call, each participant's round-trip time to the SFU (`dilla.rtt`, `09` § Voice, reported
  through the instance) is visible to every other participant of that call and to the instance,
  which measures it anyway. A participant can report a false value; it is display only and nothing
  else reads it.
- Media authenticity is group-level: any member can derive any sender's frame keys
  (`05-media-frames.md`); a colluding operator and member can inject media attributed to another
  participant. A member a Commit removed still holds the superseded epoch's key while receivers
  retain that epoch (10 s after each processed the Commit): receivers refuse its own frames under
  that epoch, but with the SFU's help it can still forge frames attributed to a participant who
  remains in the call, under the superseded epoch, until they drop it.
- In a browser call, the call group's per-epoch 16-byte `base_key` lives in the call tab's media
  worker for the epoch plus 10 s of retention, so script injected into that page can read it. It
  never reaches long-lived MLS or store secrets, and every member derives every sender's frame keys
  from it anyway. The worker zeroes the transferred key and each copy it hands the wasm cipher, and
  the cipher zeroes the buffer each copy arrives in; the receiver's key install borrows the key and
  copies it once into a zeroized-on-drop heap buffer it keeps for the retention period. The worker
  also keeps one JavaScript copy of each held epoch's `base_key`, only to recognise an identical
  duplicate install; it is zeroed when that epoch retires or is evicted, when its install fails, and
  on `clearKeys`. The JavaScript engine may move that small buffer during garbage collection, so
  earlier unzeroed images of it can remain in freed heap until overwritten. Not zeroed:
  the transient copies that key derivation leaves, because every derivation hashes `base_key` as the
  HKDF input keying material and the hasher's working buffers hold it until they are overwritten;
  they stay in the wasm stack region until later calls overwrite them; and any copy the browser
  makes internally while transferring the key to the worker. A media worker that fails during a
  call is told to clear its keys and then terminated, so no key in it is used after the failure; its
  memory is released by the browser, not zeroed.
- A follower tab receives no group material, timeline/search slices, plaintext, or `base_key`.
  Its “use this tab” notice is only `{t:'takeover',instance:<hex>,nonce:<16-byte random hex>}`.
  The leader closes command admission, drains accepted work, stops gateway and sync, pauses its
  core, releases the Web Lock and awaits the lock callback's exit before sending the matching
  `{t:'released',instance,nonce}` notice. The next leader acquires that lock and reloads its own
  store. Neither notice carries a key, token, method arguments or content; a missing release
  leaves the follower in its other-tab state.
  On a separate channel the leader also says whether it is in a call, `{t:'voice',instance,on}` with
  `on` a boolean, on every change and when a follower asks `{t:'voice?',instance}`, so the follower
  warns before a takeover ends that call (2026-10-14). It names no channel, person or key.
- Metadata is visible to the instance: membership, presence, typing, who is in voice, message
  timestamps and sizes, attachment sizes, franking tags, and in a call each participant's audio
  level and speaking activity (the RTP audio-level header extension is outside SFrame, and a silent
  microphone's zero-byte DTX frames mark its silent intervals) and every frame's size and timing.
  From web3.7 (2026-10-16) the instance also learns, for every end-to-end encrypted upload, whether it is a post
  or a fold (an edit, a delete, a reaction or a pin): its upload class (`02` § Upload class). That
  is the accepted cost of host-enforced send rights and slow mode; otherwise ciphertexts are padded
  to a multiple of 256 bytes, so a short post and a reaction look alike.
- History before a member joined is not shared with them at this version.
- A user who loses every device and the recovery key loses their history.
- A `browser`-tier session trusts the code the instance serves.
- Whether an account is a Bot is the instance's statement. From wire 4 clients judge every leaf's
  credential kind against the instance-signed account-kind attestation (`03` § Account kind
  attestation), so a member cannot make its messages appear as a Bot's; the instance, which holds
  the signing key and defines account kinds, can still attest any kind, and before a client first
  stores an attestation from its instance (only ever wire 2 or 3) its core does not check kind.
  The attestation states the account's kind, not a key's (it binds the instance and the account
  only), so it holds for every key the account presents; trust in each key stays with the pins and
  the person's verification.

## Out of scope

Traffic analysis beyond the metadata above; denial of service by the operator; compromise of the
platform CSPRNG or OS keystore; side channels on the device; legal compulsion of the user; the
security of channels in `readable` mode, which is TLS to the instance and nothing more.
