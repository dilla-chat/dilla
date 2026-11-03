# dilla — Design Specification

**Status:** approved by Jonas Thim on 2026-09-23 (plan mode), pending spec review.
**Scope:** the whole product for v1 (daily-use cutover at week 10, remaining v1 by week 13) and the
phase-2 outline. Sub-project specs and implementation plans derive from this document.
**Licence of this document:** AGPL-3.0-or-later (project licence).

## How to read this spec

1. "Founder decisions" and "UX, visual design system and accessibility" are the top of the precedence
   order: where Appendix A (the system design produced by the judged design panel) puts trust or
   encryption markers in the chrome, the design section wins.
2. "Hard truths" are verified constraints; do not design against them.
3. "Design addenda" settle questions the panel left open.
4. Appendix A is normative for architecture, protocol, media, deployment, data model, error handling,
   testing and the roadmap.

## Context

Jonas and friends want to leave Discord over its 2025-2026 privacy changes and build an open-source
replacement that is end-to-end encrypted for chat, voice and video, self-hostable on Proxmox, and
"1:1 fully comparable" to Discord.

A 1:1 Discord clone is several subsystems (realtime chat, servers/roles/permissions, media/SFU,
E2EE key management, web/desktop/mobile clients, bots/API, moderation). Under the brainstorming
skill this is architectural and must be decomposed into sub-projects, each with its own spec and plan.
Before decomposition, the questions below have to be answered because they change what gets built.

## Homelab facts that constrain the design (from the vault, verified 2026-08/09)

- Public ingress is only Pangolin on the GleSYS VPS (`46.246.48.100`). Traefik handles HTTP; `gerbil`
  already does raw UDP/TCP forwards for `steam` (.114) and `teamspeak` (.112) on isolated VLAN 70.
  Voice needs UDP, so a media server either lives on the VPS or sits on VLAN 70 behind a gerbil forward
  (double hop, VPS bandwidth x2). WAN type (public IP vs CGNAT) is unconfirmed in the vault.
- A self-hosted TeamSpeak already exists: the group already runs voice themselves.
- Authentik (CT 100) is the OIDC IdP; Pangolin SSO forward-auth breaks native apps (Home Assistant iOS
  precedent), so a native client app cannot sit behind forward-auth.
- Monitoring estate: Prometheus/Thanos/Loki/Vector/Grafana/Tempo/ntfy, Garage S3. New services are
  expected to expose Prometheus metrics and structured logs.
- Deployment convention: one community-script-style LXC per app on VLAN 62 with IP = VMID, HA +
  `*/5` replication (5-minute RPO), idempotent installer (Styr, valheim-server-ui precedent).
- Jonas's stacks: Go servers (Styr, valheim-server-ui), TypeScript/Vite front ends, MIT licences so far.

## Framing constraints (initial)

1. Discord itself only end-to-end encrypts voice/video (DAVE, MLS-based, 2024). Text is server-readable.
   An E2EE-text clone is already "more than Discord", and several Discord features depend on the server
   reading content: full history for new members, server-wide search, link embeds, bots reading messages,
   AutoMod, server-side thumbnails/transcodes.
2. Discord is engineered for millions of users per guild. A self-hosted instance for one community is a
   different system. The scale target must be chosen explicitly.
3. Full parity is a multi-year, multi-person effort. v1 must be "what we use daily", parity a roadmap.
4. A web client served by the same server that the E2EE protects against is a weaker trust anchor.

## Hard truths that survived adversarial verification (corrected wording)

1. Discord does not E2EE text and excludes Stage channels from DAVE; DAVE-only voice bots were cut over
   2026-03-01. "1:1" and "E2EE chat" conflict wherever the server reads content → two-tier design.
2. E2EE on channels anyone can join by link protects against the operator and network, not the public.
   UK OSA, EU DSA, US 2258A and the Take It Down Act have no size floor but do have scope gates: OSA
   needs UK links, the DSA binds services normally provided for remuneration (a friends' hobby host is
   mostly out of scope), 2258A and Take It Down bind providers of user content in the US.
3. No email-reset recovery under E2EE: user-held recovery key + client-encrypted key backup; lose the
   key and every device = history gone. No HSM path exists for hobby hosts.
4. Web client = served JavaScript = operator can backdoor it; Code Verify is Meta-only, WAICT is a
   Firefox Nightly prototype (2026-05). Native builds must bundle code and be reproducible.
5. iOS push needs the publisher's APNs key → a project-run content-blind push gateway (or per-host app
   builds); Android can use UnifiedPush. iOS home-screen PWAs get Web Push since 16.4 but no CallKit.
6. Media sets the bill: SFU egresses one copy per viewer (LiveKit 16-core: 150 video participants at
   85% CPU, 1 publisher → 3,000 viewers at 0.53 GB/s); home uplinks cap Go Live; hosts need a public IP
   with UDP or a relay VPS; TCP/TURN-443 works but degraded.
7. Store binaries carry a legal identity and bills: Apple $99/yr + notarisation (D-U-N-S only for
   organisations), Google $25 + tester gate, publicly trusted Windows signing keys in FIPS hardware
   since 2023-06 (Windows itself does not require signing; SmartScreen warns). A legal entity should
   own trademark, keys and store accounts. Trademark: clearance for "dilla" (J Dilla music marks exist);
   DISCORD is registered in classes 9 and 38 (Reg. 4930980); Discord's brand guidelines are private
   policy but trade-dress law binds; feature parity and generic UI patterns are lawful.
8. Bots in E2EE channels are cryptographic members; media bots must do MLS + frame encryption → an
   official bot SDK is core scope.
9. History for newcomers under E2EE has a shipped precedent now: Matrix MSC4268 in spec v1.19
   (2026-07-08), in Element; trade-offs are trusting the inviter and giving up forward secrecy for the
   shared history. MLS "two to thousands" is a design target, not a cap.
10. Funding: Sovereign Tech Fund excludes messaging apps; NGI0 Commons closed 2026-06-01; NLnet first
    grants ≤ €50k (Restack deadline 2026-11-03 incl. in-kind audits).
11. Sweden's data-retention/lawful-access proposal is stalled (2024-11 draft, no proposition tabled as of
    2026-09-22); the UK has technology-notice powers; the EU interim CSA regulation (Reg. 2026/1881,
    in force 2026-07-31 until 2028-04-03) while the permanent one is negotiated → design so no party can
    be compelled to hand over keys; no project-run flagship instance.
13. OpenMLS: one research agent reported a 2025 SRLabs audit, the design panel found no statement on
    the OpenMLS site or repository. Treated as UNVERIFIED and a gate item: dilla's own audit scope
    includes the OpenMLS integration surface.
12. Echo cancellation: Discord's desktop and mobile voice run on libwebrtc's audio processing (AEC3, NS,
    AGC) plus Krisp for noise. Electron and libwebrtc-based mobile SDKs give the same AEC3 for free; a
    system WebView (Tauri on WebKitGTK) does not. Krisp is proprietary; RNNoise (BSD-3, maintained,
    jitsi/rnnoise-wasm) at v1, DeepFilterNet (MIT/Apache; third-party WASM builds on npm since 2026)
    as a "high" mode on native.

## Founder decisions (2026-09-22, structured prompts)

- Instance model: one install hosts many communities, one account per instance, official client
  multi-homes several instances into one sidebar, no federation, identity designed for later portability.
- E2EE scope: per-channel mode; DMs, group DMs, voice/video, private channels E2EE by default; channels
  joinable by public invite/discovery are server-readable and labelled.
- Threat model: defeat operator, hosting provider, network, malicious member; web client is a labelled
  lower-trust tier; native clients bundle code with reproducible builds.
- Central services: only a content-blind push relay for iOS (+ UnifiedPush on Android). No flagship,
  no public TURN.
- Team: mostly Jonas + Claude agents (Styr-style parallel cards), evenings/weekends.
- Timeline: ~3 months to daily use by the group: text + voice, web + Electron desktop. Mobile is phase 2.
- Platforms in use: Windows desktop, Linux desktop, Android, iPhone (all four eventually).
- Alternatives tried and rejected: Fluxer, Matrix/Element (+ Element Call).
- v1 must-haves beyond basics: screen share with application audio, game detection + Rich Presence,
  music bot in voice (→ bot SDK with MLS + frame encryption is v1 scope). Discord import NOT in v1.
- Own deployment: all-in-one container on Proxmox VLAN 70 behind a gerbil UDP forward (TeamSpeak pattern).
- Legal entity: deferred until mobile starts (so mobile cannot start before it is settled).
- Name: "dilla" is final → trademark clearance is a gate before the first public release.
- All "my pick" recommendations in the question list accepted by silence.
- Round 4 (design open decisions): calls are always E2EE as a separate call group even in public voice
  channels (text readable, calls E2EE, distinct label); split v1 accepted (group moves in at W10 with
  text, voice, screen share; music bot, SAS pairing, archive, game detection, per-app capture in
  W11-W13); browser signup allowed, badged until a native device redeems the recovery key, history in
  browser sessions opt-in default off; public repository from the start with a "not audited, not for
  production" banner and an NLnet Restack application before 2026-11-03.
- Assumed unless Jonas objects: wazero-hosted core on the server with cgo fallback; 90-day inactivity
  Remove; separate 8 GB Proxmox VM for load tests; "dilla" in module paths and ids from day one with
  clearance filed in W1; no TCP/TURN fallback for voice at daily use (Pangolin's Traefik owns 443).
- Round 5 (design, 2026-09-23): design = dilla-chat's Mesh UI with encryption markers removed;
  greenfield codebase as planned, reimplementing the Mesh design system (not evolving dilla-chat's
  Rust/Tauri/Signal code); the server-readable kanal shows a small muted glyph next to its name.
  Design process preference: design in code from a written brief, WCAG 2.2 AA + EN 301 549, dark /
  light / high-contrast themes; UX proposals must be at Mesh's fidelity and keep its terminal, mono,
  keyboard-first character.
- **ADR-0001 (assumed ranking of unacceptable mistakes, from the discovery critic):** (1) a false or
  weak E2EE claim or key compromise, (2) permanent user data loss, (3) legal exposure for hosts or the
  publisher, (4) never shipping, (5) visibly worse than Discord. Security-first with a public honesty
  statement; recorded so later contributors cannot invert it.

## Founder clarifications (2026-09-20)

- "This should be runnable by anyone who can host it somewhere." → general self-hostable product; no
  dependency on Jonas's homelab (Authentik, Pangolin become optional integrations); third-party hosts
  inherit legal duties; install UX and generic packaging are first-class.
- "We need really great echo cancellation etc." → audio-processing quality (AEC, noise suppression,
  AGC) is a stated requirement. Consequence: browsers/WebViews only expose the engine's built-in AEC
  (Chromium AEC3 in Chrome/Electron; WebKit's in Safari/Tauri-on-macOS; WebKitGTK on Linux is weak),
  so "great" AEC pushes toward a native audio pipeline in desktop/mobile clients (libwebrtc AEC3 or an
  ML model) and makes Tauri-on-Linux a risk. Must be asked explicitly.

## Recommended approach (summary; Appendix A is normative)

- **One Go binary `dillad`** (cgo-free, amd64+arm64): HTTPS API + CBOR WebSocket gateway on 443, the
  MLS Delivery Service and external sender, LiveKit v1.13.7 constructed in-process on loopback (child
  process mode kept green in CI), dilla's own pion/turn behind a STUN/HTTP byte demux on 443 with relay
  sockets on the local interface, certmagic ACME (TLS-ALPN-01 default, DNS-01, IP certs, behind_proxy),
  SQLite (modernc) or Postgres (pgx) behind one sqlc repository, blobs on disk, web client embedded.
  Public ports: 443/tcp + one UDP mux port (7882). Nothing else, including TURN.
- **One Rust crate `dilla-core`** (Apache-2.0) linked by every client, bot and the server: OpenMLS 0.9.0
  (suite 0x0001) with a custom StorageProvider (rusqlite native, sqlite-wasm-rs on wasm32), identity
  (UMK/SSK/DSK, hash-chained signed device list, tiers), envelope + franking, RFC 9605 SFrame with
  DAVE-style codec prefixes ("dilla-sframe/1"), backup archive, encrypted local store + FTS5, sync.
  Built as wasm-bindgen (web/Electron/TS SDK), wasm32-wasip1 under wazero (server DS, Go SDK, testkit),
  UniFFI later (mobile). The server tracks a `PublicGroup` per group via wazero and never holds a
  group secret; no hand-written Go MLS code.
- **MLS topology**: separate groups of kind text | call | pairing | interaction, each bound to
  `{instance, community, target, kind, policy_version, e2ee_version, media_version}`; the instance is
  an external sender only in text and call groups. DS invariants (one commit per epoch, freeze while a
  DS proposal is outstanding with an external-commit clause, void rule with TTLs and KeyPackage
  validation, fork report + resync + quarantine, restore via instance generation + handshake tails,
  application messages only from current leaves) each have a chaos test.
- **Custody by tier**: UMK_priv only in the recovery-key-encrypted header; SSK_priv only on native
  devices; browser devices are signed by a native device or a one-shot RK entry and badged.
- **Media**: one LiveKit room per call, JWT minted only for leaves in the call group's current epoch,
  Opus 48k/20ms DTX+FEC, RFC 6464 audio levels for top-6 forwarding, VP8/H.264 simulcast, VP9 SVC
  flagged. Hardened E2EEManager in a dedicated worker; frames that fail to decrypt are dropped, never
  passed through. Audio: Chromium AEC3/NS/AGC (Discord's stack) → RNNoise AudioWorklet → VAD/PTT.
  Screen share audio: Windows system loopback via Electron at daily use, Windows 11 per-process
  loopback add-on later; Linux `pactl` null-sink helper at daily use, PipeWire add-on later.
- **Clients**: `dilla-client-core` (TS: multi-instance sessions, sync, attachments, sender-side previews,
  voice controller) + `dilla-web` (React) served as the lower-trust tier + `dilla-desktop`
  (Electron, bundles the same build, safeStorage KEK, pinned updater, capture, PTT, game detection via
  `detectables` CC0 data, Rich Presence local IPC). **Supersession (2026-09-23):** wherever Appendix A
  puts encryption markers in the chrome (per-channel mode badge, E2EE labels in headers or status
  bars, call-code in the room frame), the design section below wins: no encryption markers in the
  chrome; the readable-channel glyph is the only exception; encryption details move to Settings →
  Privacy, the ceremonies and overlays. The `web` tag on browser members and the bot "can hear" tag
  in voice channels stay visible. The underlying states, keys and protocol rules in Appendix A are
  unchanged.
- **Bots**: users of kind `bot` as ordinary MLS leaves with tree-derived badges; `dilla-sdk-go` over
  wazero with media through server-sdk-go's FrameEncryptor; interaction groups (user's devices + bot)
  for slash commands; `dilla-music` (yt-dlp/ffmpeg → Opus → SFrame) as reference bot and CI test.
- **Founder deployment**: dillad in an LXC on VLAN 70, Pangolin HTTP resource (behind_proxy) + raw UDP
  7882 via gerbil; optional raw TCP 5349 for TURN/TLS later; `node_ip` = VPS IP; MTU 1420.
- **Testing**: `dilla-testkit` headless multi-client harness from W1 with a scenario DSL; chaos scenario
  per DS invariant; cross-target vectors (native, wasm32, wazero); cargo-fuzz; Playwright 3-context
  E2EE calls; 1,500-leaf DS load test and 25/10 media test on a separate Proxmox VM; second read-only
  reviewer agent on every crypto/DS/media/key-storage card; formats freeze one week after their
  end-to-end scenario passes.

## Design addenda (settled 2026-09-23)

- **Authentication**: password + TOTP or passkeys (WebAuthn) on the host, optional OIDC (Authentik for
  Jonas's instance); E2EE keys are device-generated and never derived from or mixed with login
  secrets; a per-community "moderators must have 2FA" flag; join gates by account age, invite and
  membership screening, never phone numbers.
- **Handles**: `handle@host` plus display name; reject mixed-script confusables (UTS #39 skeleton);
  render the handle next to the display name whenever a message crosses a trust boundary.
- **First launch**: "paste an invite link or server URL" plus a "host your own" path; no default host,
  no project-curated directory in the client.
- **Portability and export**: the client exports readable history (v1 acceptance test, since a host can
  only return ciphertext to a GDPR portability request); a versioned community export/move bundle
  (structure, roles, members, ciphertext + members' archives) and a host shutdown protocol before the
  first public release.
- **Retention and deletion**: host keeps ciphertext and metadata per owner-set community policy
  (default indefinite); account deletion = credential purge + Remove from every group + tombstoned
  metadata; other members' devices keep their copies, stated in the UI.
- **Remote content in E2EE channels**: no remote loads at all; only sender-attached previews and
  re-hosted encrypted attachments. Server-readable channels may enable a host proxy per community.
- **On-device filters vs scanning (charter rule)**: a filter that runs on the user's device, is
  controlled by that user, and reports to no one (explicit-media blur, suspicious-link warning) is
  allowed; anything that reports, blocks or matches on behalf of a third party is client-side scanning
  and requires a public governance decision, never a config flag.
- **Threads**: public threads are messages carrying a `thread_id` inside the parent channel's group;
  private threads and forum posts with restricted membership are their own MLS groups. Audit log =
  server-recorded ciphertext operations + client-signed moderation events.
- **Inbound webhooks and channel following**: server-readable channels only at v1; later an
  owner-run "integration device" that is a visible MLS member; never a silent host-run relay.
- **Minimum-version enforcement**: releases carry a signed minimum server and protocol version;
  official clients refuse instances below it and show the advisory; delivered through the existing
  release channel, no new central service.
- **Project-dependency register**: a published page listing every project-run thing an instance or
  client depends on (push relay, update channel, signing keys, domains, store accounts) with the
  handover plan if the project stops; Android/F-Droid on UnifiedPush so one platform is fully
  independent; push relay URL configurable per client build and per instance.
- **Abuse at shared services**: instances authenticate to the push relay with an instance identity
  key and get quotas, so a rogue instance can be denied without content access.
- **Accessibility and i18n scaffolding from the first commit**: semantic UI primitives, externalised
  strings, RTL and IME as test cases; the E2EE ceremonies (recovery key, SAS pairing, safety numbers)
  usable with a screen reader and without fine motor control; WCAG 2.2 AA before public release.
- **Project home**: the project's own community, host support and security contact run on dilla
  itself from cutover (dogfooding), with a forum as fallback; hosts get docs and a forum, not a helpdesk.
- **Census gaps to place in the must/later/never list during the spec**: threads and forums, audit
  log (45-day Discord retention), scheduled events, slowmode (server-enforceable on ciphertext),
  age-restricted channel flag, verification levels, mods-need-2FA, membership screening and welcome
  screen, vanity invites, boost-tier caps as host config, streamer mode, camera/video effects, voice
  channel status, profile surface (banner, bio, pronouns, connections), ignore/notes/friend nicknames,
  server templates as the structure-only import path, /tts, accessibility settings page.

## UX, visual design system and accessibility

**Design direction (final):** the visual design system is **dilla-chat's "Mesh"** (Jonas's existing
app, `github.com/dilla-chat/dilla-chat`, AGPLv3, same authors), reimplemented in the greenfield
codebase, **with every encryption marker removed**. Three rounds of new directions (generic trio,
Mesh reskins, three full concepts from a judged panel) were rejected; the founder's instruction was
"go back to dilla and remove the encryption markers". Encryption is the silent default.

- **Source of truth for tokens and layout**: `design_handoff_dilla_mesh/README.md` (§Design Tokens),
  `client/src/themes/themes.ts` (`meshTheme`), `client/src/styles/base-tokens.css` (density modes),
  the handoff CSS (`chat.css`, `mesh-chrome.css`, `settings.css`, `extras.css`, `onboarding.css`) and
  the live prototype `Dilla Mesh.html` (a clone sits in the session scratchpad; re-clone from GitHub).
  Palette: bg #070809, bg-2 #0C0D0F, surface-2 #14171A, surface-hi #1A1E22, hairline #1F2226 /
  #363B41, fg #E8ECE8 / #A0A6A0 / #5E635E, accent #7CFF8E (ink #06150A), danger #FF6E6E,
  warn/mention #FFD16A. Type: JetBrains Mono everywhere (display 700, body 420, uppercase mono
  labels). Radii 0–3px, matte (no glass/blur), shadows as 1px accent rings. Density compact/regular/
  cozy. Accent variants green (default), amber, cyan, magenta.
- **Layout (Mesh, verbatim)**: 60px server rail (40×40 team tiles, 2px radius, accent fill + white
  left bar when active, drag-reorder, `+ Add team` dashed); resizable sidebar (default 240) with team
  name in serif italic + node line, KANALS / PMS tabs, an **Active voice** section on top with
  participants and speaking pulse, then Kanals (unread pill accent, mention pill amber, muted dim),
  then Voice, a voice dock when connected, and the user panel; main pane (48px header `[ # NAME ]` +
  topic + actions: threads · saved · pinned · members · scoped search; feed with day dividers,
  grouped messages, hover toolbar, reactions, thread previews; pill composer with upload tray,
  mention and slash popups, typing indicator); resizable member list (Admin / Online / Offline) or a
  380px thread panel; optional 32px top bar (mono D-tile + DILLA + blinking caret, team · node ·
  status, live clock, keybind hints ⌘K / `/` / `?`) and 26px bottom bar (node, peers, lamport,
  latency, voice codec + 12-bar meter, version). Command palette ⌘K (sectioned NAVIGATE / VOICE /
  FEDERATION→INSTANCES / ACCOUNT), search palette `/`, first-run splash with boot log, settings
  modal 960×640 with 220px nav, onboarding wizard (connect → identity → keys → safety → done).
- **Removed from the chrome (the founder's instruction)**: the `[E2E]` shield badge in kanal headers,
  the `SRTP` badge in voice headers, the "messages are end-to-end encrypted with Signal Protocol"
  composer footer, the `e2e SIGNAL · X3DH · AES-256-GCM` and `db SQLCIPHER` bottom-bar chunks, the
  encryption pills on empty kanals and DMs, the 🔒 "sent an encrypted message" notification teaser,
  the drop-overlay encryption copy. A verified rendering without them is the reference (screenshots
  `mesh-after.jpeg` / `mesh-before.jpeg` in the session scratchpad).
- **The one exception**: a kanal anyone can join by invite (server-readable) shows a **small muted
  glyph next to its name**, the same weight as the existing lock on private voice kanals; nothing
  else in the chrome. Mode is set and explained in channel settings.
- **Where trust states live instead**: Settings → Privacy (safety number, verify contacts, devices
  with tier and verification state, encryption details), the onboarding "Keys" and "Safety" steps,
  the recovery-key ceremony as a modal (shown once), the SafetyCompare overlay on demand, the
  IncomingCall overlay, and connection banners for degraded links. Two trust facts about people
  stay visible in the chrome (decided at spec review): a small muted `web` tag on members signed in
  from a browser, and the bot tag plus "can hear" on a bot present in a voice channel. They are
  facts about who is present, not encryption markers.
- **Design brief (W1 deliverable, `docs/design/brief.md`)**: the Mesh token table and layout spec
  transcribed from the handoff, the exclusion list above, the glyph for readable kanals, the mono
  brand mark rules (top bar uses the mono D-tile, never the SVG logo; branding SVGs for favicon,
  splash, README), sound rules (short mono-era clicks, one ping; no Discord sounds), motion rules
  (Mesh's toast/splash/speaking-pulse timings, all stopped under reduced motion), and the flow list.
- **Tokens (`packages/design-tokens`)**: the Mesh palette as semantic CSS variables plus the light
  and high-contrast derivations (same structure, inverted with intent; light theme must not be a
  colour inversion), role-colour contrast guard, type scale in rem, density tokens, focus ring,
  motion durations honouring `prefers-reduced-motion`, sound tokens. Exported as CSS variables and a
  TS module. Fonts self-hosted (JetBrains Mono; DM Serif Display for the serif-italic team name).
- **Licensing note**: reimplementing dilla-chat's design (tokens, CSS, layout) in the new AGPL-3.0
  codebase is licence-compatible (dilla-chat is AGPLv3 by the same authors); keep attribution in the
  design brief.
- **Component library (`packages/ui`, React)**: primitives (button, input, menu, dialog, toast,
  tooltip, tabs, list, virtualised log), chat composer with markdown preview, message row, reaction
  bar, member list, voice tile, speaking indicator (shape + colour, never colour alone), device and
  trust badges, ceremony components (recovery key display with copy/print and forced confirmation,
  SAS comparison, safety number). Storybook with the a11y addon; every story passes axe in CI.
- **Flows wireframed (markdown + ASCII/annotated screens) one week before their build week**:
  first launch and invite join (W2), signup with recovery key ceremony (W2 for W3), channel list and
  chat with mode labels (W2), DMs and group DMs (W2), voice join/leave, mute/deafen/PTT and speaking
  state (W3 for W4), screen-share picker and audio options (W6 for W7), community settings, roles and
  overwrites (W4 for W5), new-device pairing by SAS and RK entry (W7 for W8), report dialog and
  "what is revealed" copy (W11 for W12), bot install and badges (W10 for W11), Electron chrome, tray
  and notifications (W5 for W6), diagnostics page (W8 for W9).
- **Accessibility model**: full keyboard operation with a documented keybind map (Discord parity
  where sensible: channel/server navigation, mark read, mute/deafen, PTT), visible focus everywhere,
  chat log as a `log` region with a polite live region for new messages and a virtualised list that
  keeps semantics, voice state announced by text not only colour, font scaling and zoom to 200 %
  without loss, no motion-only cues, high-contrast theme, colour-blind safe defaults, error messages
  linked to fields, dialogs with focus trap and return. Ceremonies: recovery key readable and
  copyable as text, SAS as words as well as emoji, safety numbers in chunked digits with copy.
- **Testing**: axe-core on every Storybook story and Playwright page from W1; keyboard-only Playwright
  traversal of each flow; manual passes in W12 with NVDA (Windows), Orca (Linux) and VoiceOver
  (macOS) including every ceremony; conformance statement (WCAG 2.2 AA, EN 301 549) before the first
  public release; accessibility of the ceremonies included in the audit scope.
- **Roadmap placement**: W1 brief + tokens + primitives + Storybook/axe (3 cards); W2-W3 chat
  surfaces; W4 voice UI; W5 community/permissions UI; W6 Electron chrome; W8 recovery/trust UX; W10
  polish from the group's first week; W12 accessibility pass. Sound set and logo by W9.

### Items confirmed at spec review (2026-09-23)

1. The `web` tag on members signed in from a browser stays visible in the member list and message
   header, as originally decided; encryption details stay out of the chrome.
2. The bot tag plus "can hear" stays visible on a bot present in a voice channel.
3. Brand and sound as proposed (see below).
4. 2026-09-23 final review: the backup manifest is SSK-signed and the header is split into an
   immutable root and a mutable state object, because UMK_priv is never on a device after signup;
   Appendix A's "UMK-signed manifest" is superseded by protocol/06-backup-archive.md.

### Decisions taken at spec review (2026-09-23)

- **Repository and module path:** `github.com/dilla-chat/dilla`, public from day one. Go module
  `github.com/dilla-chat/dilla`; Rust crate `dilla-core`; npm packages under `@dilla/*`.
- **UI vocabulary:** *server*, *channel*, *DMs*, *voice* (Discord's words; the Mesh handoff's
  team/kanal/PMs labels are replaced in copy). Code, API and protocol keep *community* / *channel*.
- **Brand and sound:** the Mesh mono D-tile with blinking caret in the top bar; dilla-chat's branding
  SVGs for favicon, splash and README; an original short click/ping sound set. Confirmed.
- **Working mode:** parallel agent cards in git worktrees, one pull request per wave, a second
  read-only reviewer on every crypto, delivery-service, media and key-storage card, Jonas merges;
  nothing lands on `main` without his go.

## Sub-project decomposition (spec order)

1. `dilla-protocol` (normative spec + test vectors) — FIRST spec.
2. `dilla-core` (Rust) and `dilla-testkit` — depend on 1.
3. `dillad` (API, gateway, DS, permissions, storage) — depends on 1, 2.
4. `dilla-media` (LiveKit adapter, TURN, media worker, Go FrameEncryptor) — depends on 2, 3.
4b. `dilla-design` (brief, tokens, `packages/ui` + Storybook, flows, sound set, brand, accessibility
   model and tests) — depends on nothing; starts W1 alongside 1 and 2; consumed by 5.
5. `dilla-web` + `dilla-client-core`, then `dilla-desktop` — depend on 2, 3, 4, 4b.
6. `dilla-sdk-go` + `dilla-music`; `dilla-sdk-ts` — depend on 2, 3, 4.
7. `dilla-deploy` (container, Compose, Proxmox helper, Pangolin recipes, doctor, runbooks).
8. `dilla-security-and-compliance` (threat model, disclosure, audit scoping, NLnet, trademark gate).
9. `dilla-mobile-and-push` (phase 2).

## Verification (end to end)

- Testkit scenarios green in CI for every DS invariant; vectors identical across the three core builds.
- Three Chromium contexts complete an E2EE call with zero pass-through frames and correct epoch rekey
  on join/leave (Playwright); Go bot audio decrypts in the browser and vice versa.
- Founder path: `dillad doctor` passes in the LXC behind Pangolin; a friend outside the LAN joins voice
  over UDP 7882; diagnostics page shows relay share 0 % for UDP-capable peers; measured 25-voice/10-
  sharer numbers recorded through the VPS hop.
- Negative security tests (cross-group Welcome replay, foreign-device resync, removed-leaf upload,
  franking mismatch, NONE-flagged track, third-party STUN absent) all fail closed.
- Daily-use acceptance: the founder's group uses text, voice and screen share for one week with the
  known-gap list published; no transport-only call ever occurs.
- Design and accessibility: every Storybook story and Playwright page passes axe with zero serious or
  critical violations; each flow completes keyboard-only in Playwright; all three themes render every
  component with AA contrast (automated check on tokens); the recovery-key, SAS and safety-number
  ceremonies complete with NVDA, Orca and VoiceOver in the W12 manual pass; the client ships no
  Discord colours, icons, sounds or typefaces (checked against the design brief's exclusion list).

## Appendix A: System design (normative except where superseded in "UX, visual design system and accessibility")

### Basis

Protocol-correctness architect ("one core, one binary, MLS-everywhere"), kept for its MLS topology, DS invariants, dilla_binding, RFC 9605 key schedule, Messenger-style franking and archive-not-keys backup; every fatal flaw and factual error the judges found is fixed below (freeze-rule deadlock, external-commit bypass, fork recovery, browser persistence, identity keys on the browser tier, pairing-group injection, franking receiver check, unauthenticated bot keys, media authenticity claim, TURN/443 port story, 1,000-member handshake cost, restore semantics, MLS-Exporter definition, generateKeyFrame, LiveKit replace directives, Windows loopback floor, OpenMLS audit claim), with the Go MLS parser replaced by the Rust core running under wazero (from Graft), dilla's own pion/turn behind a 443 demux (from Operator-first), and interaction groups instead of HPKE (from Ship-in-90).

### Overview and scope of the daily-use version

dilla is one Go binary (`dillad`) and one Rust core (`dilla-core`) that every client, bot and the server itself link. The server is an RFC 9420 Delivery Service and external sender that sequences and policy-binds ciphertext; it never holds a group secret. Text channels, calls, device pairing and bot interactions are separate MLS groups; media is encrypted per frame with an RFC 9605 schedule keyed from the MLS exporter and forwarded opaquely by an in-process LiveKit SFU.

**Daily-use build (target cutover W10, 2026-11-24; hard fallback W12):**
- E2EE DMs, group DMs and private channels; server-readable public channels with a persistent label; roles, per-channel overwrites, invites (invite-only registration), kicks/bans mirrored into MLS.
- E2EE voice (Opus, RNNoise, AEC3) and screen share video on web and Electron (Windows, Linux; macOS builds). Screen-share audio: Windows system loopback (Electron built-in), Linux per-application audio via a `pactl` helper.
- Mandatory recovery key at signup, RK-based new-device enrolment, TOFU pinning with key-change alerts, franking commitment verified on receipt (report UI later).
- Founder deployment in a Proxmox LXC behind Pangolin (HTTP resource + raw UDP mux), measured 25-voice / 10-sharer numbers on the real path.

**Inside the 90 days but after cutover (still v1):** Go bot SDK and the music bot (W11), device pairing by SAS and the encrypted history archive (W10-W11), game detection and Rich Presence (W11), Linux PipeWire native capture and Windows 11 per-process loopback (W12), security pass and threat model (W12).

**Before first public release, not in the 90 days:** FTS5 local index (daily use has substring search), TURN/TLS through 443 on hosts that own 443 (the founder's topology uses a separate raw TCP port), TypeScript SDK media, DeepFilterNet, VP9 SVC default, macOS application audio, compliance kit, open registration, Postgres benchmarks, reproducible builds, SBOM, cosign, independent audit, trademark clearance.

**Rules that shape the plan:** a format freezes one week after it has been exercised end to end (credential chain end of W3, envelope+franking end of W4, dilla-sframe/1 and DS API end of W5, backup archive end of W11), never on paper. The multi-client headless harness exists from W1 so DS cards are agent-verifiable. Every crypto or DS card gets the founder's review plus a second read-only reviewer agent. No public "E2EE" claim before an audit; the record's "OpenMLS audited 2025" is unverified (no statement on github.com/openmls or openmls.tech as of 2026-09-22) and is treated as a gate item, not a fact.

### System architecture

**One process per instance: `dillad` (Go 1.27, `CGO_ENABLED=0`, linux/amd64+arm64).** It embeds:
1. HTTPS API (JSON) and the realtime gateway (WebSocket, CBOR frames with fixed field order) on 443.
2. The MLS Delivery Service (`dilla-ds`): KeyPackage directory, per-group sequencer, GroupInfo store, external-sender signer, freeze/void/re-issue logic.
3. `dilla-core-wasi`: the same Rust core compiled to `wasm32-wasip1` and executed by wazero (Apache-2.0, pure Go) for exactly three jobs: maintain an OpenMLS `PublicGroup` per group from PublicMessage handshakes, structurally validate commits/proposals/GroupInfo, and sign external proposals. No Go MLS code, no cgo, no decryption capability. Fallback if the W1 spike fails: cbindgen + cgo (drops the static-binary claim, keeps one binary).
4. LiveKit server v1.13.7 constructed in-process (`config.NewConfig` -> `routing.NewLocalNode` -> `service.InitializeServer`), bound to `127.0.0.1:7880`, `/rtc*` reverse-proxied by dillad over loopback, one UDP mux port, no Redis, `turn.enabled: false`. dillad's `go.mod` carries LiveKit's three pion-fork `replace` directives (webrtc-pion v4.2.18-warp.1, dtls v3.1.5-warp.1, ice v4.4.0-warp.2). A supervised child-process mode of the same LiveKit build is kept green in CI.
5. dilla's own pion/turn v5 behind a post-TLS byte demux on 443 (STUN magic cookie -> TURN, else HTTP), relay sockets bound to the host's local interface so relayed media reaches the co-located SFU without any published port range.
6. TLS via certmagic (TLS-ALPN-01 default, DNS-01, IP certificates, or `behind_proxy` mode).
7. Storage: SQLite (modernc, cgo-free, single writer, WAL) or Postgres (pgx) behind one sqlc repository; blobs on local disk; goose migrations.
8. The web client from `embed.FS`.

**`dilla-core` (Rust, Apache-2.0)** is built three ways from one crate: `wasm32-unknown-unknown` via wasm-bindgen for web, Electron, the TypeScript SDK and the media worker; `wasm32-wasip1` for wazero inside dillad, the test harness and the Go SDK; UniFFI (Swift/Kotlin) in phase 2.

**Clients:** `dilla-web` (TypeScript/React) served by dillad as the labelled lower-trust tier, bundled unchanged into `dilla-desktop` (Electron 44, Chromium 152). One core instance per device runs in a dedicated Web Worker elected by Web Locks.

**Bots** are users of kind `bot` with their own credential chain; the Go SDK links the core through wazero and publishes media through LiveKit's Go SDK `FrameEncryptor` hook.

**Trust boundaries.** B1 core+OS keystore vs UI: only the core holds keys. B2 client vs dillad: ciphertext, trees, timestamps, sizes, presence, franking HMAC key. B3 dillad vs LiveKit: room membership and QoS; JWTs minted only for leaves present in the call group's current epoch. B4 instance vs instance: none. Server-readable channels bypass B2 by design and are never allowed an MLS text group.

**Public ports:** 443/TCP and one UDP port (7882). Nothing else, on every path including TURN.

### Rust core (crypto, sync, storage, search)

`dilla-core` owns every secret and every plaintext. Modules:

- **mls**: OpenMLS 0.9.0 pinned (MIT), suite 0x0001 (MTI) only at v1, `openmls_rust_crypto`. A **custom `StorageProvider`** implemented on dilla-core's own SQLite connection (rusqlite 0.40 native; `sqlite-wasm-rs` >= 0.5.5 with the `sqlite3mc` feature on wasm32) using `openmls_sqlite_storage` 0.3.0 only as a reference, because that crate pins rusqlite 0.37 without wasm support. Every commit merge and every message-secret consumption is one SQLite transaction; state is never persisted half-way. Group config: PublicMessage handshakes, PrivateMessage application data, padding to 256-byte buckets, `use_ratchet_tree_extension` off (the DS serves the tree), external senders and `dilla_binding` in `required_capabilities`. Past-epoch secrets: `KeepAll` plus a core-owned timer that calls `delete_past_epoch_secrets` after 5 min (text), 10 s (call), 0 (pairing, interaction), independent of whether the OpenMLS policy enum offers a time variant.
- **identity**: UMK/SSK/DSK, tiers, the SSK-signed device list (monotonic version, hash-chained), TOFU pin table, safety numbers, SAS derivation from `epoch_authenticator`.
- **envelope**: deterministic encoding as fixed-position CBOR arrays (ciborium does not canonicalise map keys); franking `K_f`/`C`; receiver verification.
- **sframe**: RFC 9605 literal (see Media), codec prefix parsers for Opus/VP8/VP9/H.264, RBSP escaping, CTR partition with refuse-on-wrap.
- **backup**: `K_header`/`K_backup` = HKDF-SHA256(RK, "dilla header v1" / "dilla archive v1"); archive chunks, SSK-signed manifest, idempotent merge by `msg_id`.
- **store**: SQLite page-encrypted under a 256-bit device KEK (sqlite3mc / bundled-sqlcipher), tables for messages, attachments cache, membership cache; FTS5 index before public release, substring scan at daily use.
- **sync**: per-group cursors; catch-up interleaves handshakes and application messages per epoch so a device offline longer than the past-epoch window still decrypts everything it can, and labels the rest "undecryptable (too old)" with a reason code.
- **public_group** (wasi export only): `PublicGroup::from_external`, `process_message`, `merge_commit`, `export_ratchet_tree`, `ExternalProposal::new_remove/new_add` (all verified in 0.9.0).

**Browser hosting (fixes the SharedWorker flaw).** `sqlite-wasm-rs` persists only through the OPFS `sahpool` VFS, which needs synchronous access handles available solely in dedicated workers. Each tab spawns a dedicated worker; `navigator.locks.request("dilla-core:<instance>")` elects one leader that opens the OPFS file; other tabs forward calls over `BroadcastChannel` and render from the leader's events. When the leader tab closes the lock passes and the next worker reopens the file. If OPFS is unavailable (private window) the core runs in memory, shows a "no persistence in this browser" banner and rejoins groups by external commit on reload.

**MLS-Exporter**, corrected: `MLS-Exporter(Label, Context, Length) = ExpandWithLabel(DeriveSecret(exporter_secret, Label), "exported", Hash(Context), Length)`; dilla always calls OpenMLS `export_secret`, and the spec text and vectors use this definition.

**Bindings**: wasm-bindgen typings; wasi ABI of about twelve C-shaped exports (protobuf-in/out) shared by dillad, testkit and the Go SDK; UniFFI later. Published test vectors for credential chain, device list, envelope, franking, dilla-sframe/1, backup archive.

### Server (Go): API, realtime gateway, delivery service, permissions on ciphertext

**API and gateway.** Accounts, devices, sessions, communities, channels (mode `e2ee|readable`, visibility `private|invite|discoverable`, host policy), roles, overwrites, invites, bans, blobs, backups, reports, bots. Gateway frames: presence, typing, voice state, `mls.handshake`, `mls.commit_needed`, `mls.epoch_changed`, `message.ct`, `message.plain`, `interaction`; resumable sequence numbers; wire and E2EE versions negotiated at connect with an N-2 window; an instance `generation` counter in READY.

**Delivery Service invariants (each is a chaos test):**
1. Groups are registered with `dilla_binding`; the DS refuses text groups for public/discoverable channels; call groups exist for every voice session regardless of channel mode.
2. A `PublicGroup` per group (via wazero) tracks tree, epoch and leaf credentials; committers upload a signed GroupInfo **without** the ratchet tree; the DS serves the tree from its `PublicGroup` and joiners verify `tree_hash`.
3. Exactly one commit per epoch; the loser gets 409 with the winning commit and current proposals.
4. Commit validity: signed by a current leaf or `new_member_commit`; references every outstanding **non-void** DS proposal; no Update from the committer; member-originated Removes may target only the committer's own leaf or another leaf of the same user (device revocation); Adds carry credentials whose user is ACL-eligible and whose DSK is in the latest signed device list; structural validation by `PublicGroup`; GroupInfo epoch consistent.
5. **Freeze rule:** while a DS proposal is outstanding, application messages get 425 `commit_required`, and external commits get 425 too, **unless** no member device is online, in which case the external commit is accepted, the proposals are re-issued for the new epoch and `commit_needed` goes to the joiner. After any commit that omitted DS proposals, non-void ones are re-issued and the freeze stays.
6. **Void rule:** the DS validates a KeyPackage (lifetime, capabilities, unconsumed) and a Remove target (leaf still present) before proposing; a proposal older than its TTL (30 s calls, 24 h text) is marked void and commits may omit it.
7. `commit_needed` goes to the lowest-index online device, bot devices first; others back off 300 ms plus jitter; 2 s watchdog nudges the next candidate; three lost rounds remove the failing device.
8. Application messages are accepted only from a device session whose leaf is in the current `PublicGroup` (closes the post-removal sending window).
9. Fork handling: a member that cannot process an accepted commit reports it and resyncs to the DS head by external commit; three distinct reports on one commit quarantine the committer (external Remove, device flagged).
10. Handshake retention 30 days; ciphertext until every member cursor passes it or 30 days; per-device cursors.
11. Restore: `dillad restore` bumps `generation`; every group becomes epoch-unknown; the first member-signed GroupInfo with epoch >= stored plus that member's handshake tail (clients keep the last 64) is replayed through `PublicGroup` and adopted; otherwise the group is closed and re-created; all non-last-resort KeyPackages are purged; live calls end.

**Permissions on ciphertext.** PrivateMessage hides the sender, so send permission, slowmode, mute and rate limits bind to the uploading device session; clients independently drop messages whose MLS-authenticated sender lacks send permission in their role snapshot. Size caps on blobs; edit/delete/pin are signed envelopes; delete-for-everyone also deletes the server copy (best-effort tombstone elsewhere). Franking tag = HMAC-SHA256(K_frank, group||epoch||seq||uploader_device||C||recv_ts).

**Server-readable channels:** same envelope as plaintext, FTS search (modernc FTS5 verified by a `CREATE VIRTUAL TABLE` test in W1, LIKE fallback), metadata AutoMod (mention spam, join raids, rate limits).

### E2EE protocol layer (MLS groups, devices, recovery, franking, bots)

**Suite and credentials.** MLS 1.0 suite 0x0001; suite per group so P-256 (hardware keys) or PQ-hybrid can arrive under N-2. Per user: UMK (root, Ed25519) and SSK (signed by UMK). Per device: DSK = MLS leaf signature key. BasicCredential identity = fixed-order CBOR `{v, umk_pub, user_id, device_id, kind user|bot, tier native|browser, signer_tier, ssk_pub, sig_umk(ssk_pub), sig_ssk(device_id||dsk_pub||tier)}`. The user's **SSK-signed device list** (version, entries, revocations, hash chain) is published by the server; clients validate every leaf against the newest list they have seen for that pinned UMK and reject unlisted or revoked DSKs, so revocation is cryptographic, not operator-dependent. Key custody by tier: `UMK_priv` lives only in the RK-encrypted header; `SSK_priv` only on native devices; a browser device is signed by a native device (SAS) or by a one-shot RK entry in the browser that signs with `signer_tier=browser` and zeroises. Peers see: a badge on browser-signed leaves, a soft "new device for X" notice derived from the tree, and a loud un-dismissable alert on UMK change; safety numbers = SHA-256 of both UMKs as 60 digits.

**Groups.** Kinds `text|call|pairing|interaction`; `dilla_binding {instance_id, community_id, target_id, kind, policy_version, e2ee_version, media_version}` in `required_capabilities`; the instance key in `external_senders` for text and call groups only. Client policy on external proposals: Add/Remove accepted in text/call groups; GroupContextExtensions only to rotate `external_senders` with the new key signed by the old; ReInit/PSK rejected; any external proposal rejected in pairing and interaction groups; external-commit Removes accepted only for the joiner's own `device_id`.

**Joins.** Online device: external commit (community join, new device, call join, resync). Offline recipient: DS Add proposal with a validated KeyPackage (32 + 1 last-resort per device; a last-resort join sends an Update immediately). New private channel or grant in a large community: the DS batches Adds and the creator commits <= 256 per commit with one Welcome each, so a 1,000-member community never serialises 1,500 external commits.

**Cadence.** Text Update interval per device = 24 h x max(1, ceil(leaves/64)), skipped for devices that have not sent since their last refresh; the DS batches into one commit per max(60 s, leaves x 1 s) (about 58 commits/day at 1,500 leaves). Call groups: Update every 60 min, one commit/min. Inactivity 30 days -> Remove.

**Pairing.** Two-leaf `pairing` group without external senders. The new device shows a QR/fingerprint of its DSK; the old device refuses unless the tree has exactly two leaves and the peer DSK matches; SAS from `epoch_authenticator`; then the old device sends, to a native device, `SSK_priv`, `K_backup` and the pin table; to a browser device only its signed credential (`K_backup` only if the user enables "history in browser sessions").

**Recovery.** 256-bit RK at signup (52 Crockford base32 characters or 24 BIP-39 words, forced acknowledgement). Root `{UMK_priv, SSK_priv}` under `K_header` (written only at signup and recovery); state `{device list, pins}` under `K_backup` (rewritten by any native device on every change); archive of decrypted messages under `K_backup` (per-device immutable chunks, SSK-signed manifest, merge by `msg_id`). Lost RK with no signed-in device = lost history, stated in onboarding.

**Franking.** `K_f` (32 random bytes) inside the envelope; `C = HMAC-SHA256(K_f, envelope without K_f)` in `authenticated_data`; **recipients recompute C on decrypt and hard-reject mismatches**; each edit has its own `K_f`. A report reveals `(envelope, K_f)`; authorship is bound through the server's session-to-device record (operator attestation, deniable to third parties), stated as such.

**Bots.** Users of kind `bot` with their own chain; ordinary leaves with tree-derived "can read / can hear" badges. Slash commands in a channel travel through the channel group; ephemeral responses and commands to non-member bots use an **interaction group** per (user, bot): all of the user's devices plus the bot device, created lazily by the user's device via Add+Welcome. No HPKE at v1.

**Residual trust, stated:** the server may add any legitimate user's devices to groups it controls (visible via tree-derived member lists and "X joined" events); voice/video authenticity is group-level.

### Media (SFU, frame encryption, audio pipeline incl. echo cancellation, screen share with app audio)

**Transport.** One LiveKit room per call; participant identity = `device_id`; dillad mints the JWT only after the leaf is in the call group's current epoch, with grants mirroring speak/video/stream and caps (25 voice, 10 publishers) enforced at token time. Opus 48 kHz, 20 ms, DTX and in-band FEC on, 64 kbps default; RFC 6464 audio level in the clear for top-6 active-speaker forwarding. Video: VP8 and H.264 Constrained Baseline with simulcast; VP9 SVC behind a flag (livekit-client disables backup codecs under E2EE, so VP9 is enabled only when every subscriber advertises VP9 decode); AV1 later. ICE order: UDP mux -> TURN/TLS on 443 (degraded, labelled); ICE-TCP is off by default (optional 7881).

**Worker.** `Room({ e2ee: { e2eeManager: DillaE2EEManager, worker } })`. Our manager never posts `enable:false` or `setSifTrailer`; the worker speaks livekit-client's `init/encode/decode/updateCodec` protocol, ignores `setKey/ratchetRequest/enable/setSifTrailer`, and treats every incoming frame as ciphertext: a frame that fails to parse or decrypt after the buffer window is dropped and counted, never passed through. A room participant absent from the MLS tree is shown as "unverified stream" and not played.

**Key schedule (RFC 9605 §5.2, literal).** `base_key = MLS-Exporter("SFrame 1.0 Base Key", "", 16)` per epoch. `KID = (0 << 24) + (leaf_index << 8) + (epoch mod 256)` with S=16, E=8 (3 bytes). `sframe_secret = HKDF-Extract("", base_key)`; key/salt via HKDF-Expand with labels `"SFrame 1.0 Secret key "` / `"SFrame 1.0 Secret salt "` || KID (8 bytes BE) || 0x0004. Suite AES_128_GCM_SHA256_128 (0x0004). Nonce = salt XOR CTR; CTR partitioned in one media worker per device as slot (8 bits: mic, camera, screen video, screen audio) || layer (4) || 52-bit sequence, refuse-on-wrap. Old epoch keys kept 10 s; unknown-KID frames buffered 2 s.

**Frame `dilla-sframe/1`:** `[clear codec prefix][SFrame header][ciphertext][16-byte tag]`, AAD = header || prefix; one ciphertext per simulcast/SVC layer. Prefix: Opus 0; VP8 1 (inter) / 10 (key); VP9 0; H.264 non-VCL NALs clear, VCL NAL header clear, RBSP encrypted with emulation-prevention re-escaping. **Key frames:** no `generateKeyFrame()` (not in Chromium); the SFU's PLI on new subscription plus receiver `sendKeyFrameRequest()` cover epoch changes; a sender-side `replaceTrack` workaround only if W7 measurement shows a gap.

**Audio pipeline.** `getUserMedia` with echoCancellation/noiseSuppression/autoGainControl on (Chromium AEC3, the stack Discord uses) -> RNNoise AudioWorklet (jitsi/rnnoise-wasm sync build) -> VAD or PTT gate -> LiveKit track. DeepFilterNet "high" mode on Electron later. Playback via WebAudio with per-user volume; the screen-audio track is published with AEC/NS/AGC off and mixed locally by viewers.

**Screen share with application audio.** Windows daily use: `session.setDisplayMediaRequestHandler` with `audio: 'loopbackWithMute'` (system audio, no native code; Electron-verified Windows-only). Windows v1: WASAPI `AUDIOCLIENT_PROCESS_LOOPBACK_PARAMS` add-on for per-process audio, gated on build >= 20348 (Windows 11), system loopback fallback on Windows 10. Linux daily use: helper running `pactl load-module module-null-sink` + `move-sink-input` + `module-loopback` (works on pipewire-pulse), captured as a device with processing off; v1: PipeWire native add-on capturing the application's node next to the portal ScreenCast. macOS: video only until ScreenCaptureKit audio lands after the entity exists.

**Authenticity caveat (threat model and public text):** any member can derive any sender's key (RFC 9605 §7.2); attribution rests on the SFU's SSRC binding, so a colluding operator and member can inject media attributed to another participant. Same as DAVE; stated, not buried.

**Capacity:** publish only measured numbers as Mbps per (sharers x viewers x layer); a single 4 Mbps share to 24 viewers is ~96 Mbps of SFU egress, so host caps on publishers, viewers and bitrate ship on day one.

### Clients: web and Electron desktop

**Shared TypeScript layer (`dilla-client-core`).** Multi-instance session manager (one gateway, one core store and one device identity per instance; sidebar merges communities under instance headers; DMs stay per instance); sync engine; attachment upload/download with client-generated thumbnails (canvas) and per-blob random AES-256-GCM keys; sender-side link previews inside the ciphertext; on-device mention/mute/unread semantics; voice controller (join -> external commit -> keys installed -> publish). Search is a substring scan over the decrypted local store at daily use; the FTS5 index follows before public release.

**Web client.** React/Vite, served by dillad. The core runs in a dedicated worker with OPFS persistence and Web Locks leadership (see Rust core). Credential `tier=browser`; a permanent "browser session: code served by this operator, lower trust" badge; no `SSK_priv`, `K_backup` only by opt-in. Voice requires `RTCRtpScriptTransform` (Chrome 141+, Firefox 117+, Safari 15.4+); older browsers get a dialog, never a transport-only call. Assets ship a content manifest (per-file SHA-256) so a WAICT-style integrity layer can be added later.

**Electron desktop (44.x, Chromium 152, Node 24).** Bundles the identical web build; never loads application code from the server. Device KEK wrapped by `safeStorage` (Keychain, DPAPI, libsecret/kwallet); on Linux `getSelectedStorageBackend() === 'basic_text'` downgrades the displayed tier and warns. Auto-update via electron-updater with a pinned update key; Windows and Linux installers unsigned at daily use (SmartScreen note in docs) until the legal entity buys certificates; macOS builds in CI. Native features: custom picker over `desktopCapturer`; Windows system loopback and the Linux `pactl` helper at daily use, PipeWire and WASAPI add-ons (Rust, napi-rs) at v1; push-to-talk as toggle via `globalShortcut` at daily use (press-only API), hold-to-talk through a native key hook (Windows/X11) and the GlobalShortcuts portal (Wayland) as a v1 card; tray, autostart, desktop notifications; game detection and Rich Presence (own section).

**Trust and recovery UX.** Recovery key shown once with a forced confirmation; new-device flows (SAS from a native device, or RK entry); per-channel mode badge (E2EE vs readable by this server; calls always E2EE); bot badges; "cannot decrypt" and "undecryptable (too old)" states with reason codes; "delete for everyone is best effort" copy; report dialog that states exactly what is revealed and to whom.

**Platform floors:** Windows 10 (system audio) / Windows 11 (per-process audio); Linux with PipeWire or PulseAudio; macOS 13+ builds.

**Mobile** is phase 2 on the same core via UniFFI.

### Bot SDK and the music bot

**Shape.** Bots are ordinary clients: a user of kind `bot` with its own UMK/SSK/DSK generated by the SDK and held by the bot operator, one device per running instance, KeyPackages published like any device. `dilla-sdk-go` (Apache-2.0) embeds `dilla-core-wasi` under wazero, so bots need no cgo and cross-compile to any architecture; `dilla-core-ffi` (cbindgen) is the fallback. `dilla-sdk-ts` (Apache-2.0) embeds the wasm-bindgen build under Node.

**Go SDK surface.**
```go
bot := dilla.Login(ctx, instanceURL, botToken)      // registers or resumes the device
bot.OnMessage(func(ctx, m dilla.Message) {...})     // decrypted envelopes
bot.OnInteraction(func(ctx, i dilla.Interaction) dilla.Response {...})
bot.Send(ctx, channelID, dilla.Text("..."))
vc, _ := bot.JoinVoice(ctx, channelID)             // external commit into the call group + LiveKit join
vc.WritePCM(samples []int16)                        // 48 kHz stereo, 20 ms
vc.Leave(ctx)
```
Under the hood: channel membership by DS Add proposal on install or external commit; interaction groups for slash commands (`RegisterCommand(name, schema)`); envelope + franking through the core; media through `server-sdk-go`'s `NewPCMLocalTrack` with a `FrameEncryptor` that applies dilla-sframe/1 via the core, keyed per epoch from the call group. The SDK re-derives keys on every commit; key bytes never cross the SDK boundary. Bots are preferred committers (always online, lowest cost), which makes voice removals fast whenever a bot is present.

**Music bot (`dilla-music`, Go).** `/play`, `/skip`, `/queue`, `/volume`, `/stop`. Sources: local library and host-allowed URLs via `yt-dlp` (Unlicense) and `ffmpeg` subprocesses -> PCM -> Opus 48 kHz stereo 96 kbps, DTX off -> SFrame -> LiveKit. It subscribes to nothing, appears in the voice member list with a tree-derived "bot: can hear this call" badge, replies ephemerally through the interaction group and posts "now playing" only where it is a channel member. Ships as its own container or systemd unit next to dillad with a Compose snippet; it is also the SDK's integration test in CI.

**TypeScript SDK** ships text, moderation and interaction bots at v1; media publishing arrives after daily use through a Go media sidecar bundled with the SDK (no frame-transform hook exists in `@livekit/rtc-node` 1.1.0, and LiveKit's native SDKs expose only their own FrameCryptor).

**Not provided:** a hosted bot runtime (it would make the operator a decrypting member); Discord-compatible bot APIs (Discord's own bot transition ended 2026-03-01). Webhooks and channel following exist only into server-readable channels.

### Game detection and Rich Presence

**Game detection ("Playing X").** Electron main polls the process list every 15 s (`sysinfo` via a small Rust napi add-on, `ps-list` as fallback) and matches executable names and paths against a bundled copy of the `fluxerapp/detectables` dataset (mapping data CC0-1.0, verified by the panel) plus user-added executables. Opt-in per user, with a per-community "show my activity" toggle. The result is published as presence metadata `{activity: {app_id, name, since}}` over the gateway; the record treats presence as Discord-parity metadata visible to the server, and the UI says so in the setting. Nothing about window titles, arguments or content is read. On Linux under Flatpak the process list is sandboxed, so the docs recommend the AppImage/deb build for detection; on Wayland nothing else changes. No overlay is ever drawn and no DLL or library is injected.

**Rich Presence.** A local IPC endpoint (`\\.\pipe\dilla-ipc-0` on Windows, `$XDG_RUNTIME_DIR/dilla-ipc-0` on Linux/macOS) accepts newline-delimited JSON frames `{v:1, client_id, activity: {details, state, timestamps{start,end}, assets{large_image,large_text,small_image,small_text}, party{size,max}}}` and `clear`. The first connection from a new `client_id` prompts the user once (allow/deny, remembered). A tiny reference client library (Go and TypeScript, Apache-2.0) and a shell script example ship with the desktop app. Presence is merged per instance and shown in the member list and profile card.

**Scope limits.** Invites/joins via Rich Presence, spectate, and any game-side SDK beyond the local socket are not planned. The dataset is refreshed with releases, never fetched at runtime (no outbound calls from the desktop client except to the user's instances and the update server).

### Deployment: one binary, one container, Proxmox behind a UDP forward, upgrades and backups

**Unit.** `dillad` as a static multi-arch binary or a distroless image; `dilla.toml` written by `dillad init` (domain, public IP, UDP port, TLS mode, DB, blob dir, registration mode, admin invite). Packaging: Compose with **bridged networking publishing only 443/tcp and 7882/udp** (host networking optional), a systemd unit, a Proxmox helper that creates a Debian LXC and installs the binary (no Docker, no nesting), Helm later.

**TLS.** certmagic with TLS-ALPN-01 by default (no port 80 needed through a raw TCP pipe), HTTP-01 and DNS-01 optional, Let's Encrypt IP certificates for domainless hosts, `behind_proxy` mode (plain HTTP on 127.0.0.1:8080, trusted proxy CIDRs) that still terminates TURN/TLS itself.

**Ports and TURN, made true.** dillad terminates TLS on 443 and demuxes STUN-cookie bytes to its own pion/turn and everything else to HTTP (API, gateway, `/rtc` proxy). Relay sockets use `RelayAddressGeneratorStatic` on the host's local interface address; LiveKit runs with `rtc.udp_port: 7882`, `rtc.tcp_port: 0`, `rtc.node_ip: <public IP>`, `use_external_ip: false`, `advertise_internal_ip: true` (so the SFU also offers its local host candidate and relay pairing stays on-host; verified by the panel that without it LiveKit replaces host candidates), `rtc.stun_servers` pointed at the instance itself so LiveKit never appends Google/Twilio STUN, `turn.enabled: false`. TURN credentials are ephemeral HMAC (`expiry:device_id`, 1 h, two allocations per device) delivered by the join API as `rtcConfig.iceServers`, which livekit-client honours over server-provided lists. Result: no relay range is published, bridged Docker works, and the TCP fallback is TURN/TLS on 443 only.

**Founder path.** dillad in an LXC on the isolated VLAN. Pangolin's Traefik owns 443 on the VPS, so: HTTPS/WSS via a Pangolin HTTP resource with dillad in `behind_proxy` mode; UDP 7882 as a raw UDP resource (`allow_raw_resources: true`, gerbil port mapping; source IPs appear as newt's address, fine for ICE); optional raw TCP 5349 for TURN/TLS with a DNS-01 certificate. `node_ip` = VPS IP; WireGuard MTU 1420 clears Pion's 1200-byte packets. Documented as the CGNAT "relay node on a small VPS" pattern.

**Upgrades.** Forward-only goose migrations at start after an automatic pre-migration backup (SQLite `VACUUM INTO`; Postgres via a Go-native logical dump over pgx `COPY TO`, so no `pg_dump` binary is required); refuse to start on a newer schema; semver; wire/E2EE/media versions independent with N-2. `dillad backup` = one tarball (DB snapshot, blobs, config, instance keys); `dillad restore` bumps the instance generation and triggers the group-heal protocol (Server section); the docs state that restores drop live calls and that backups hold no end-to-end-encrypted plaintext: they contain ciphertext, server-readable channel content, revealed report envelopes, TLS material and the instance keys.

**Observability and doctor.** `/metrics` (dilla + LiveKit on one registry, incl. `dilla_mls_pending_removal_age_seconds`), `/healthz`, `/readyz`, JSON logs, an admin call-diagnostics page (candidate types, relay share, loss, decrypt failures, aggregate SFU egress). `dillad doctor` checks config, DB, ACME, clock skew, a TURN allocation on 443, and UDP reachability by asking the operator to run `dilla-desktop --probe <host>` from another network (no project-run reflector: the record allows only the push relay as a central service).

**Registration:** invite-only; email never required; open registration with email + CAPTCHA later.

### Data model

> **Deviation (2026-09-24):** this section is superseded on six points by
> `scratchpad/planning/dillad/interfaces.md` §0.1, each with its ruling.
> **D2 (R24)** identifiers are 16 CSPRNG bytes stored as `BLOB(16)`/`BYTEA`, not ULIDs, and every
> timestamp is an integer unix second, not a string.
> **D3 (R37)** the migration table is goose's default `goose_db_version`, not `schema_migrations`.
> **D4 (gap-13)** `mls_welcomes` becomes three tables — `mls_welcome_payloads`, `mls_epoch_trees`,
> `mls_welcomes`.
> **D5 (R5)** `mls_handshakes.kind ∈ {0 proposal, 1 commit, 2 external_commit}`; a Welcome is not a
> handshake.
> **D13 (`protocol/01`)** the inactivity Remove is 90 days, not 30.
> **D15 (R38)** "backups hold ciphertext only" becomes "backups hold no end-to-end-encrypted
> plaintext: they contain ciphertext, server-readable channel content, revealed report envelopes,
> TLS material and the instance keys".

**Server (one schema, SQLite or Postgres; ULIDs; UTC timestamps).**
- Identity: `instances(instance_id, external_sender_key_id, key_history, franking_key_id, generation)`; `users(id, username, display, kind, umk_pub, ssk_pub, sig_umk_ssk, flags, age_bracket, created, disabled_at)`; `devices(id, user_id, dsk_pub, tier, signer_tier, credential_blob, verified_at, revoked_at, last_seen)`; `device_lists(user_id, version, blob, ssk_signature, prev_hash)`; `key_packages(device_id, kp_ref, blob, last_resort, expires, consumed_at)`; `sessions(token_hash, device_id, expires)`.
- Structure: `communities(id, owner, name, icon_blob, policy_json)`; `channels(id, community_id nullable, kind text|voice|category|dm|group_dm, mode e2ee|readable, visibility, parent_id, position, settings_json, host_policy_version)`; `roles`, `member_roles`, `channel_overwrites(target role|user, allow, deny)`, `members`, `channel_members` (DMs, group DMs, materialised private-channel eligibility), `bans`, `invites`.
- MLS: `mls_groups(group_id, binding_json, kind, ciphersuite, epoch, seq, group_info_blob, public_group_state blob, external_sender_key_id, e2ee_version, media_version, created, closed_at)`; `mls_handshakes(group_id, seq, epoch, kind proposal|commit|welcome, sender_device|external, blob, created)`; `mls_pending_proposals(group_id, epoch, ref, kind, target_leaf, origin, issued_at, ttl, void_at)`; `mls_welcomes(device_id, group_id, blob, delivered_at)`; `mls_members(group_id, leaf_index, user_id, device_id, added_epoch, removed_epoch)`; `device_cursors(device_id, group_id, last_seq, last_epoch)`.
- Messages: `mls_app_messages(group_id, epoch, seq, uploader_device, blob, commitment_c, franking_tag, size, created, expires)`; `readable_messages(channel_id, seq, sender, envelope_json, franking_tag, edited, deleted)` + FTS; `read_state(user_id, channel_id, last_read_seq)`; `attachments(blob_id = SHA-256(ciphertext), channel_id, uploader_device, size, mime, storage_ref, created, expires)`.
- Backups and ops: `backups(user_id, kind root|state|chunk, device_id, chunk_seq, blob_id, manifest_sig)`; `voice_sessions(call_id, channel_id, group_id, livekit_room, started, ended)`; `reports(id, reporter, message_ref, revealed_envelope, k_f, verification_result, status)`; `audit_log`; `push_registrations` (phase 2); `instance_settings`; `schema_migrations`.
- In memory: presence, typing, voice states, gateway sessions, pending `commit_needed` timers.

**Client (dilla-core encrypted SQLite, one file per instance).** OpenMLS provider tables (groups, epochs, key packages, signature and encryption keys, past epoch secrets); `identity(dsk_priv, ssk_priv nullable, k_backup nullable, tier)`; `device_list_cache`; `pinned_users(user_id, umk_pub, first_seen, verified, change_alerts)`; `messages(msg_id, group_id, channel_id, epoch, seq, sender_device, envelope, franking_tag, received_at, edited_by, deleted_by)` + FTS5; `handshake_tail(group_id, ring of 64)`; `attachments_cache`; `membership_cache`; `backup_state`; `settings`.

**Envelope (fixed-position CBOR array):** `[v, msg_id(16), type, thread_id?, reply_to?, body, attachments[[blob_id, key32, nonce12, size, mime, w, h, thumb]], previews[], k_f(32)]`, padded to 256-byte buckets. **Archive chunk:** `[v, device_id, chunk_seq, nonce, AES-256-GCM ciphertext]`; manifest `[header_version, chunks[], ssk_signature]`.

**Versioning.** Every stored blob and wire message carries a version byte; wire, E2EE and media versions negotiate independently (N-2); `dilla_binding.e2ee_version/media_version` record the group's floor; incompatible groups are re-created, never migrated in place.

### Error handling and failure modes

Every failure below has a defined recovery, a reason code shown in the UI, and a harness scenario.

**Sequencing.**
- Commit race: 409 returns the winning commit and proposals; the loser clears its pending commit (OpenMLS "discarding commits"), processes the log, retries with jitter.
- Freeze: 425 `commit_required`; the client commits pending proposals before resending; UI shows "syncing membership" for up to 2 s.
- Void proposal: a DS proposal past its TTL or found invalid is voided; commits omitting it are accepted; the underlying action (kick, add) is re-attempted with a fresh KeyPackage or skipped if the leaf is gone.
- External commit during freeze: refused unless no member is online; then accepted, proposals re-issued, `commit_needed` to the joiner.
- Fork (accepted by DS, rejected by members): reporter resyncs to head by external commit; three reports quarantine the committer.
- Lost commit rounds: watchdog removes the device after three misses; the device resyncs on return.
- Stale device (behind retention or past-epoch window): resync by own-leaf external commit; missed messages are labelled "undecryptable (too old)" rather than silently dropped.
- Post-removal upload: rejected (leaf not current); the client explains "you are no longer a member".
- KeyPackage exhaustion: the last-resort package is used; the joining device sends an Update immediately; READY reports the refill threshold (< 8).

**State and storage.**
- Server restore: generation bump, group heal from member handshake tails, else close-and-recreate with a visible system message; KeyPackages purged; live calls end.
- Browser without OPFS or a killed leader tab: in-memory core with banner; the lock passes to another tab; on reload the device resyncs by external commit and loses only unsynced drafts.
- Corrupt local store: checksummed pages, a recovery scan, then archive restore via `K_backup` or RK.
- Archive gaps: manifest verification reports missing chunks in device settings.

**Identity.**
- UMK change: loud, persistent alert; sending stays possible but flagged.
- Unlisted or revoked DSK in a tree: leaf rejected, message not rendered, sender shown as "unverified device".
- `safeStorage` backend `basic_text`: tier downgraded in UI, warning with fix instructions.

**Media.**
- Unknown KID: buffer 2 s, then drop and count.
- Decrypt failure or NONE-flagged track: frame dropped; participant tile shows "cannot decrypt"; never transport-only.
- Room participant absent from the tree: "unverified stream", not played.
- Late leave detection: LiveKit `participant_left` (~15 s on crash) -> Remove proposal; the SFU session is cut immediately on kick.
- UDP blocked: TURN/TLS with a "degraded connection" indicator; TURN unavailable in `behind_proxy` without the extra port: explicit dialog.
- LiveKit in-process failure: adapter restarts it; if the API contract breaks, the child-process mode takes over; calls are re-created (new call group).
- Uplink saturation: adaptive lower-layer subscription, host caps, and the diagnostics page showing aggregate egress.

**Operational.** Migration failure restores the pre-migration backup and exits non-zero with the heal protocol pending; ACME failure keeps the last certificate and alerts; clock skew beyond 60 s fails `doctor`.

### Testing and verification strategy

**Harness first (W1).** `dilla-testkit` runs N headless clients (native `dilla-core`, no browser) plus a Go SDK bot against an in-process `dillad` with SQLite in a temp dir, driven by a scenario DSL (`join`, `send`, `kick`, `go_offline`, `restore_snapshot`, `expect_decrypts`, `expect_425`). DS cards from W2 onward are merged only with a scenario.

**Chaos scenarios (one per DS invariant):** concurrent commits from 5 clients; kick while all members are offline then a join; external commit during a freeze with and without online members; expired KeyPackage in a DS Add (void path); Remove of a leaf already gone; committer that uploads a malformed path (fork report, quarantine, resync); device behind retention (resync); watchdog after three lost rounds; DS restore from a stale snapshot with group heal and with forced re-creation; 30-day inactivity Remove; join storm (256-per-commit batching) in a 1,000-member community.

**Conformance and cross-target vectors.** JSON vectors for credential chain, device list, envelope, franking, dilla-sframe/1 (built on RFC 9605's published vectors for suite 0x0004), backup archive, wire frames; each vector is checked on native, wasm32-unknown-unknown and wasm32-wasip1-under-wazero in CI so the three builds cannot diverge. OpenMLS's own test suite runs on wasm32 in dilla's CI (upstream builds but does not test that target).

**Fuzzing and negative security tests.** cargo-fuzz on envelope, SFrame frame, credential, GroupInfo and device-list parsers; negative tests: cross-channel GroupInfo/Welcome replay (binding mismatch), resync with a foreign `device_id`, external Add in a DM or interaction group, GroupContextExtensions changing `external_senders` without the old key's signature, message upload from a removed leaf, C mismatch (recipient rejects), NONE-flagged track and SIF-trailer frames (dropped), KID/CTR wrap guard, browser-signed device badge, unlisted DSK rejection, provisional credential outside a pairing group, third-party STUN absent from every ICE server list.

**Browser and media.** Playwright: three Chromium contexts in one E2EE call with decrypt counters, epoch change on join/leave, keyframe latency after a new leaf; Firefox smoke. Cross-SDK: the Go `FrameEncryptor` output decrypted by the JS worker and vice versa.

**Load and measurement.** DS test at 1,500 leaves (native headless clients on a separate Proxmox VM with 8 GB, not the LXC) measuring commits/day, fan-out bytes and per-device download; `lk load-test` plus our encryptor for 25 voice / 10 sharers, first on LAN (W7) then through the VPS hop (W9); numbers are published only from these runs.

**Review policy.** Every card touching `dilla-core`, `dilla-ds`, the media worker, capture add-ons or key storage is reviewed by the founder and by a second read-only reviewer agent (the pattern that found five failure-path bugs on the previous project); formats freeze one week after their end-to-end scenario passes; dependency pins, SBOM (syft) and cosign in CI from W12; release jobs refuse a public tag without the audit and trademark checklist.

### 90-day roadmap with parallel agent cards

Cards are self-contained tasks in parallel git worktrees; counts are targets, not promises. Daily use starts W10; W12-W13 absorb slip.

- **W1 (Sep 22-28), 10 cards: foundations and spikes.** Monorepo + CI (cargo, go, vite, wasm32 x2); `dilla-protocol` v0; core skeleton on OpenMLS 0.9.0 with the custom StorageProvider (native + wasm); **wazero spike** (PublicGroup + external Remove signing) go/no-go with cgo fallback; **LiveKit in-process spike** (loopback bind, replace directives, `/rtc` proxy); dillad skeleton (auth, devices, schema, gateway); **testkit v0** with a 2-client scenario; dedicated-worker + OPFS + Web Locks spike; modernc FTS5 check; trademark clearance request filed.
- **W2 (Sep 29-Oct 5), 12 cards: DS and credentials.** Group registry + binding; PublicGroup tracking; sequencer with 409/`commit_needed`; KeyPackage directory with validation; external Add/Remove with TTL/void; freeze rule incl. external-commit clause and re-issue; GroupInfo-without-tree + DS-served tree; core credential chain, signed device list, KeyPackage publishing; wasm-bindgen worker API; chaos scenarios for races, freeze, void.
- **W3 (Oct 6-12), 12 cards: text E2EE end to end.** DMs and group DMs (Add+Welcome); private channels via batched DS Adds; server-readable channels; envelope + franking with recipient verification; fork report + resync-to-head; encrypted store v0; channel/DM/message UI; 3-client scenarios. Freeze: credential chain + device list.
- **W4 (Oct 13-19), 10 cards: voice spike.** LiveKit adapter, JWT gated on leaf; call-group lifecycle; hardened E2EEManager + `dilla-media-worker` with Opus over dilla-sframe/1; own pion/turn behind the 443 demux with static relay + ephemeral creds; three browsers talk E2EE. Freeze: envelope + franking.
- **W5 (Oct 20-26), 12 cards: communities and permissions.** Communities, categories, channels, roles, overwrites, invites, bans; kick/ban -> Removes with freeze; channel mode + host policy + labels; tree-derived member list with badges and "X joined"; offline-removal chaos tests. Freeze: dilla-sframe/1, DS API v1.
- **W6 (Oct 27-Nov 2), 10 cards: voice hardening and Electron.** Electron shell, safeStorage KEK, pinned updater; RNNoise, PTT/VAD, device pickers; top-6 forwarding, DTX/FEC; call Update cadence and past-epoch windows; reconnect/resync; 10-client soak via Go encryptor.
- **W7 (Nov 3-9), 10 cards: video and screen share.** VP8/H.264 prefix rules, RBSP, simulcast, per-layer CTR; PLI/`sendKeyFrameRequest` path; Windows system loopback; Linux `pactl` helper; caps at token time; LAN bandwidth measurement; Windows/Linux installers.
- **W8 (Nov 10-16), 11 cards: recovery and packaging.** RK at signup + header; RK-based enrolment; signed device list + revocation + Removes everywhere; TOFU/safety numbers/alerts; container, Compose, Proxmox helper, ACME modes, migrations + pre-migration backup, `restore` generation protocol, `doctor`; attachments + thumbnails; sender link previews.
- **W9 (Nov 17-23), 10 cards: founder deployment.** LXC behind Pangolin (HTTP resource, raw UDP, optional raw TCP TURN), `node_ip`/`advertise_internal_ip`; measured 25/10 numbers through the VPS hop; metadata AutoMod + rate limits; admin basics; edit/delete/reaction/pin envelopes; presence/typing polish.
- **W10 (Nov 24-30), 8 cards: DAILY USE.** Founder's group moves in (text, voice, screen share; Windows/Linux Electron + web); pairing by SAS; bug wave; known-gap list.
- **W11 (Dec 1-7), 10 cards: bots and archive.** Go SDK via wazero; interaction groups; music bot; slash-command UI; bots as preferred committers; TS SDK text; backup archive upload/restore. Freeze: archive.
- **W12 (Dec 8-14), 9 cards: security pass and native capture.** Spec self-review, cargo-fuzz, negative tests, threat model, security.txt, disclosure policy, SBOM/pins; 1,500-leaf DS test; game detection + Rich Presence; PipeWire add-on; Windows 11 per-process loopback; report UI.
- **W13 (Dec 15-21), 5 cards: buffer.** Slipped cards, retro, phase-2 backlog, "not yet audited" README, DeepFilterNet if time.

If the W4 voice spike slips a week, cutover moves to W12 and the bot SDK crosses day 90; nothing else reorders.

### Phase 2 and later (mobile, push relay, import, compliance kit, audit)

**Before first public release (after the 90 days, still v1).** FTS5 local index on every platform; TURN/TLS through 443 for hosts that own 443; TypeScript SDK media via the Go sidecar; DeepFilterNet "high" mode; VP9 SVC as default where all subscribers support it; macOS ScreenCaptureKit application audio and notarised builds; soundboard (client-side playback of a shared encrypted asset); webhooks and channel following into server-readable channels; open registration with email verification + CAPTCHA; hash-matching host toggle for unencrypted public uploads; Postgres benchmarks and S3 blob backend; host compliance kit (report/removal flows, statement-of-reasons templates, GDPR export/erasure, DSA/DMCA contact fields, host guide, age-bracket interface); reproducible Linux desktop build, cosign, SBOM publication; hold-to-talk on Wayland; private threads as own groups; Helm chart.

**Gates.** Independent audit of `dilla-core`, `dilla-ds`, the media worker and Go encryptor, key storage adapters, pairing/recovery flows and the update pipeline, with the OpenMLS integration surface explicitly in scope because its own audit is unverified; funding route: NLnet Restack application (EUR 5k-50k, in-kind security audits, deadline 2026-11-03 12:00 CET) plus donations. Trademark clearance for "dilla". The release job refuses a public tag without both.

**Mobile (phase 2).** Android and iOS on the same core via UniFFI. Known blocker: LiveKit's native SDKs expose only their own FrameCryptor, so dilla-sframe/1 needs a generic encoded-frame transformer hook in the livekit/webrtc fork or the LiveKit Rust SDK path; decide in W8 and prototype on Android before creating the legal entity. UnifiedPush on Android from the instance; the project-run content-blind APNs relay (opaque wake tokens only) and the entity for store accounts; reproducible Android builds; CallKit/PushKit later.

**Protocol evolution.** MSC4268-style inviter history sharing with a visible "history provided by X" label (signed group state is kept so the message format does not change); owner/admin-signed membership grants verified by members (narrows the accepted server residual power); key transparency or client gossip over the hash-chained device lists; P-256 suite with hardware-non-extractable signing keys; PQ-hybrid suite when the IETF draft becomes an RFC; AV1; top-N forwarding and signaling for 99 voice / 50 sharers; identity portability across instances via `umk_pub`; social/threshold recovery; P2P mode for 1:1 calls behind an IP-exposure warning.

**Product.** Discord import tooling (explicitly not v1), threads/forums beyond envelope-level threading, custom emoji/stickers, scheduled events, multi-node SFU with a Redis/NATS bus, Web Push for browsers. Stage-scale audiences remain out of scope.

### Risks

1. **Timeline compression.** Even with the reduced daily-use scope, W2-W5 carry protocol code that only the founder can review. Mitigation: harness from W1, second reviewer agent, freeze-after-exercise, and the explicit rule that cutover slides to W12 before any format is frozen unexercised.
2. **wazero path fails or is slow** (wasip1 build, HPKE cost in a 1,500-leaf `PublicGroup`). Mitigation: W1 go/no-go with numbers; cgo fallback keeps one binary; the DS performs a handful of operations per membership change, not per message.
3. **LiveKit coupling** (unexported internals, forked pion replace directives, `/rtc` proxying). Mitigation: adapter pinned to v1.13.7 with a contract test, child-process mode green in CI, monthly upgrade card.
4. **TURN redesign untested at scale.** Own pion/turn behind the demux, static relay on the local interface and `advertise_internal_ip` have not been run together by anyone yet. Mitigation: W4 basic path, W9 measurement on the founder's topology, `doctor` allocation probe, ICE-TCP kept as an optional third port.
5. **Browser persistence.** Leader election and OPFS sahpool are novel glue; a bug forks a device out of its groups. Mitigation: atomic per-commit writes, resync on any inconsistency, Playwright tests that kill the leader tab mid-commit.
6. **Large-group handshake cost.** The cadence formula is untested until W12. Mitigation: parameters live in `policy_version`, the DS enforces the commit budget, numbers are published only after the 1,500-leaf run.
7. **Media authenticity and ghost-member residual trust** are accepted limits that could be read as "not really E2EE". Mitigation: stated verbatim in the threat model, onboarding and channel labels; owner-signed grants and per-sender signatures listed as later work.
8. **Screen-share audio quality** (system loopback re-shares remote voices on Windows 10; `pactl` helper brittleness). Mitigation: `loopbackWithMute`, per-process loopback on Windows 11 in W12, PipeWire add-on, capability probes with an honest "audio unavailable" state.
9. **Founder's VPS/WireGuard hop** adds latency and caps uplink. Mitigation: diagnostics page per leg, measured numbers, relay-node pattern documented as the escape.
10. **Recovery UX.** Forced recovery keys and no newcomer history will surprise the group. Mitigation: onboarding copy from W3, archive restore before second devices appear (W11), history sharing planned with a label.
11. **Key storage degradation** on Linux (`basic_text`) and browser exfiltration by a malicious operator. Mitigation: tiered custody (no `SSK_priv` or `K_backup` in browsers by default), visible tier badges, P-256 hardware keys later.
12. **Regulatory** (Sweden retention/interception bill, EU CSA, UK OSA). Mitigation: no compellable keys by construction, minimal metadata defaults, compliance kit before public release, entity decision with this in scope.
13. **Unverified upstream audit and solo maintenance.** Mitigation: NLnet Restack application, thin adapters, standards-only crypto lint (`deny.toml`), security.txt and disclosure policy from the first public commit.
14. **Trademark failure after identifiers carry the name.** Mitigation: clearance filed W1; module paths and bundle ids use a neutral codename until W13 (founder decision).

### Sub-project scopes (detail)

- **dilla-protocol** (first spec: True; depends on: none): Normative spec and test vectors: group topology and kinds, dilla_binding, DS API and invariants (sequencing, freeze, void, re-issue, fork, restore), credential chain and signed device list, tier custody rules, pairing and recovery flows, envelope and franking, dilla-sframe/1 and key schedule, backup archive, wire/E2EE/media versioning with N-2, threat model skeleton.
- **dilla-testkit** (first spec: False; depends on: dilla-protocol): Headless multi-client harness with scenario DSL, chaos scenarios per DS invariant, cross-target vector runner (native, wasm32, wazero), fuzz targets, Playwright media tests, load rigs (1,500-leaf DS, 25/10 media), ICE-server assertions.
- **dilla-core** (first spec: False; depends on: dilla-protocol): Rust crate: OpenMLS integration with custom StorageProvider (native and sqlite-wasm-rs), identity and device list, envelope/franking, SFrame, backup archive, encrypted store and FTS5, sync engine, public_group wasi exports; wasm-bindgen and wasip1 builds; browser worker/leader-election contract.
- **dillad (API, gateway, DS, permissions, storage)** (first spec: False; depends on: dilla-protocol, dilla-core): Go server: HTTP API, CBOR gateway, dilla-ds on wazero-hosted core, permission engine and ciphertext enforcement, franking tags, server-readable channels and AutoMod, SQLite/Postgres repository, migrations, backup/restore with generation protocol, admin CLI.
- **dilla-media** (first spec: False; depends on: dillad (API, gateway, DS, permissions, storage), dilla-core): LiveKit adapter (in-process, child-process fallback, config bridge, JWT gating, webhooks), own pion/turn behind the 443 demux with static relay and ephemeral credentials, dilla-media-worker and hardened E2EEManager, Go FrameEncryptor, codec prefix rules, key schedule wiring, capacity measurement.
- **dilla-web and dilla-client-core** (first spec: False; depends on: dilla-core, dillad (API, gateway, DS, permissions, storage), dilla-media): TypeScript client library (multi-instance, sync, attachments, previews, notifications, voice controller, worker bridge) and the React UI (Discord-shaped surfaces, trust/recovery/pairing UX, labels and badges, diagnostics).
- **dilla-desktop** (first spec: False; depends on: dilla-web and dilla-client-core): Electron shell: bundling, safeStorage KEK, pinned updater, capture (Windows loopback, pactl helper, PipeWire and WASAPI add-ons), PTT, tray, game detection with detectables data, Rich Presence IPC, installers, reproducible Linux build.
- **dilla-sdk-go and dilla-music** (first spec: False; depends on: dilla-core, dillad (API, gateway, DS, permissions, storage), dilla-media): Go bot SDK over wazero-hosted core (identity, groups, events, interaction groups, media via server-sdk-go FrameEncryptor) and the reference music bot with yt-dlp/ffmpeg pipeline and packaging.
- **dilla-sdk-ts** (first spec: False; depends on: dilla-core, dillad (API, gateway, DS, permissions, storage)): TypeScript bot SDK for text, moderation and interactions on the wasm core; Go media sidecar design for later media publishing.
- **dilla-deploy** (first spec: False; depends on: dillad (API, gateway, DS, permissions, storage), dilla-media): Container and Compose (bridged, 443 + one UDP), Proxmox LXC helper, systemd unit, Pangolin recipes (HTTP resource + raw UDP + optional raw TCP TURN), ACME modes, doctor, observability, backup/restore runbook, host guide with measured numbers.
- **dilla-security-and-compliance** (first spec: False; depends on: dilla-protocol): Threat model (incl. media authenticity and residual server power), security.txt and disclosure policy, audit scoping and NLnet Restack application, trademark gate, SBOM/cosign/reproducibility pipeline, compliance kit skeleton for first public release.
- **dilla-mobile-and-push (phase 2)** (first spec: False; depends on: dilla-core, dilla-media, dilla-security-and-compliance): UniFFI bindings, Android/iOS UIs, frame-transformer hook decision for LiveKit native SDKs, UnifiedPush, content-blind APNs relay, legal entity and store accounts.

### Open decisions raised by the panel, with the founder's answers (2026-09-22/23)

- Confirm that voice/video is always E2EE as an independent call group even in public or discoverable channels (text readable, calls E2EE, distinct label). The record's 'public channels are server-readable' is read as text-only because 'no downgrade to transport-only' forbids a second media path; this needs an explicit yes.
- Accept the daily-use cut: cutover at W10 with text, voice and screen share (Windows system audio, Linux pactl helper), while the music bot, SAS pairing, history archive, game detection/Rich Presence and per-process/PipeWire capture land in W11-W12 (inside v1, after the group is already using it). Alternative: hold cutover until all v1 must-haves exist, which realistically pushes it past day 90.
- Founder VPS port shape: does Pangolin's Traefik own 443 on the VPS? If yes (expected), HTTPS runs through a Pangolin HTTP resource with dillad in behind_proxy mode and TURN/TLS needs a separate raw TCP port (5349 proposed) with a DNS-01 certificate, or a second VPS IP; decide whether the TCP fallback is wanted at daily use at all.
- Browser-tier custody defaults: (a) allow signup in a browser (browser-rooted identity, badged until a native device redeems the recovery key) or require the first device to be native; (b) 'history in browser sessions' (K_backup handed to browser devices) as a per-user opt-in, default off.
- Server-side core hosting: wazero (static binary, no cgo, multi-arch) as default with cbindgen/cgo as the fallback if the W1 spike shows wasip1 build or performance problems; confirm the fallback is acceptable (loses CGO_ENABLED=0, keeps one binary).
- Inactivity Remove threshold: 30 days as proposed means a friend away longer misses messages sent after day 30 (they rejoin by resync); choose 30, 60 or 90 days for a casual group.
- Test rig: approve a separate Proxmox VM (about 8 GB RAM) for the 1,500-leaf DS measurement and the 25/10 media load test rather than running them on the production LXC.
- Apply to NLnet Restack (deadline 2026-11-03 12:00 CET) for in-kind audit funding; requires a public repository and FOSS outputs by then, which the plan otherwise defers.
- Neutral codename for Go module paths, bundle ids and domains until trademark clearance returns (rename cost is small before W13, larger after), or use 'dilla' everywhere from day one.
- Update cadence trade-off for large groups: accept that PCS refresh scales with group size (about 24 days per device at 1,500 leaves) in exchange for a bounded commit budget, or set a tighter floor and accept higher fan-out on home uplinks.

**Answers:** calls are always E2EE as a separate call group, even in server-readable kanals; the split v1 (cutover W10, remaining v1 in W11–W13) is accepted; Pangolin's Traefik owns 443 on the VPS, so dillad runs in behind_proxy mode with a raw UDP forward and no TCP/TURN fallback at daily use; browser signup is allowed, badged in Settings → Devices until a native device redeems the recovery key, history in browser sessions opt-in and default off; wazero with the cgo fallback is accepted; inactivity Remove threshold 90 days; a separate ~8 GB Proxmox VM for the load tests; NLnet Restack application with a public repository from the start; "dilla" in module paths and identifiers from day one with clearance filed in W1; the group-size-aware update cadence is accepted.

**Calendar note:** week numbers are relative to the project start; the calendar dates in the roadmap assume a start on 2026-09-22 and shift with the real start date.

### Design rationale: ideas grafted from the other proposals

- Run the Rust core under wazero inside dillad (wasm32-wasip1) for PublicGroup tracking, structural commit validation and external-proposal signing, so no Go MLS parser exists and the binary stays cgo-free (source: Fork-or-contribute architect / Graft; endorsed by the Ship-in-90 and Protocol-correctness judges).
- Use the same wazero-hosted core in the Go bot SDK and the test harness, so bots cross-compile without cgo (source: Graft).
- dilla's own pion/turn behind a post-TLS STUN/HTTP byte demux on 443 with RelayAddressGeneratorStatic on the local interface, ephemeral HMAC credentials delivered as rtcConfig.iceServers, and LiveKit's TURN disabled (source: Operator-first architect).
- rtc.advertise_internal_ip: true and rtc.stun_servers pointed at the instance so relay pairing stays on-host and LiveKit never appends Google/Twilio STUN (source: Operator-first judges' findings).
- TLS-ALPN-01 as the default ACME mode, DNS-01 and Let's Encrypt IP certificates, behind_proxy mode that still terminates TURN/TLS (source: Operator-first architect).
- Hardened E2EEManager passed via Room's e2ee.e2eeManager option so enable:false and setSifTrailer are never posted, plus treating NONE-flagged tracks as undecryptable (source: Operator-first and Ship-in-90 judges).
- Custom OpenMLS StorageProvider on dilla-core's own rusqlite/sqlite-wasm-rs connection instead of openmls_sqlite_storage (rusqlite 0.37 pin, no wasm) (source: Operator-first judges).
- MLS rollback protocol after a server restore: clients keep a handshake ring buffer and re-upload the tail, instance generation counter, group re-creation fallback, covered by a stale-snapshot test (source: Operator-first architect, merged with the Protocol-correctness judge's restore fix).
- Backup tarball that includes instance keys taken consistently, VACUUM INTO for SQLite, refuse-to-start on a newer schema (source: Operator-first architect).
- Interaction groups: a lazily created MLS group of the user's devices plus the bot device for slash-command payloads, replacing HPKE (source: Ship-in-90 architect).
- Bots as preferred committers because they are always online, and a dilla_mls_pending_removal_age_seconds metric with an admin warning (source: Ship-in-90 architect).
- Linux per-application share audio via a pactl null-sink / move-sink-input / module-loopback helper as the zero-native-code daily-use path before the PipeWire add-on (source: Ship-in-90 architect).
- Append-only hash-chained, user-signed device list with a monotonic version so revocation is cryptographic and a key-transparency layer can be added later (source: Ship-in-90 architect and Operator-first judge).
- Voice verification code derived from epoch_authenticator shown in the call panel (source: Ship-in-90 architect).
- Publish only measured bandwidth numbers as Mbps per (sharers x viewers x layer) and show aggregate SFU egress on the diagnostics page (source: Ship-in-90 hosting judge).
- Eager, batched joins for private channels (DS Add proposals committed 256 at a time) instead of lazy per-member external commits (source: Ship-in-90 hosting judge's finding).
- Fork-or-contribute verdict on Fluxer (LLM usage policy, BDFL governance, 21-service stack) and Matrix ('Not Yet' MLS), with reuse limited to the CC0 detectables dataset for game detection, native capture modules as reference and packaging config as a template (source: Graft).
- NLnet Restack application as the audit funding route and a neutral codename until trademark clearance (source: Graft).
- Two-recipe founder deployment for a Pangolin VPS whose 443 is owned by Traefik: HTTP resource + behind_proxy mode + raw UDP + alternate raw TCP port for TURN/TLS (source: Operator-first and Graft hosting judges).
- Explicit 'unverified stream' state for SFU participants absent from the MLS tree and 'removal pending' honesty in the UI (source: Graft, matching Protocol-correctness).

### Rejected alternatives

- LiveKit's built-in frame cipher as media_version 0 (Operator-first, Graft, Ship-in-90): the record accepts an RFC 9605-shaped format and requires a concrete flaw to change it; the proposals showed cost, not a flaw; the stock worker also carries operator downgrade paths (trackInfo NONE, SIF trailer), needs an HKDF/PBKDF2 interop fix between SDKs, spends ~30 bytes per Opus frame, and an N-2 window would keep the temporary format alive for years.
- hk_E per-epoch history keys with a parallel history_ct ciphertext (Operator-first): unauthenticated and group-keyed, so the operator can re-attribute restored messages and forward secrecy of stored text collapses; the archive-of-plaintext backup keeps authenticity and FS properties.
- Backing up OpenMLS group state in the nightly export (Ship-in-90): restoring it clones a leaf (sender-ratchet reuse, AES-GCM nonce reuse) and hands current epoch secrets to anyone with the recovery key; only decrypted history and identity are archived.
- HPKE base-mode slash-command payloads to server-published keys (Protocol-correctness, Graft): unauthenticated and substitutable by the operator; replaced by interaction groups.
- Hand-written Go TLS-syntax MLS parser/validator (Protocol-correctness, Operator-first, Ship-in-90): a third of an MLS library written by agents on the critical path with silent-failure bugs; replaced by the wazero-hosted core.
- SharedWorker hosting of the core with the OpenMLS SQLite provider (Protocol-correctness): sqlite-wasm-rs persists only via OPFS sync access handles, which exist only in dedicated workers; replaced by dedicated worker plus Web Locks.
- Committer uploads GroupInfo with the full ratchet tree on every commit (Protocol-correctness): ~1 MB per commit at 1,500 leaves; the DS now serves the tree from its PublicGroup and joiners verify tree_hash.
- Fixed 24 h Update cadence per device (Protocol-correctness): produces ~1,440 commits/day per 1,000-member channel; replaced by the group-size-aware cadence and DS commit budget.
- Rule 'every commit references every outstanding DS proposal' applied to external commits (all four proposals): RFC 9420 §12.4.3.2 forbids proposals by reference in external commits; replaced by the refuse-unless-nobody-online plus re-issue rule.
- LiveKit's embedded TURN and 'TURN on the mux port' (Protocol-correctness): TURN listens on its own ports and advertises relay allocations from a 10,000-port range at node_ip, which black-holes in bridge networking and in the founder's topology.
- ICE-TCP through the 443 demux (Graft): LiveKit's TCP mux binds all interfaces and advertises its own port; TCP fallback is TURN/TLS on 443 only, ICE-TCP optional on 7881.
- Project-run UDP reflector for dilla doctor (Operator-first): a central service the record does not allow; replaced by an operator-run probe from another network.
- Windows per-process WASAPI loopback add-on at daily use (Protocol-correctness): requires build 20348 (Windows 11); Electron's system loopback ships first, per-process arrives as a Windows 11-gated v1 card.
- generateKeyFrame() on epoch change (Protocol-correctness): not implemented in Chromium; SFU PLI on subscription plus receiver sendKeyFrameRequest() instead.
- Protocol frozen on paper in W4 / proto frozen in W1 (Protocol-correctness, Ship-in-90): formats now freeze one week after their end-to-end scenario passes.
- Lazy per-member external-commit joins to private channels (Ship-in-90): loses pre-open messages for existing members and serialises join storms; replaced by eager batched Adds.
- Rust botd daemon with gRPC SDKs as the only bot process (Ship-in-90): the LiveKit Rust SDK exposes only its own FrameCryptor, so it cannot carry dilla-sframe/1; the Go SDK over server-sdk-go's FrameEncryptor hook is the v1 media path.
- max_past_epochs=3 for text and MaxEpochs(2048)/MaxEpochs(8) policies (Graft, Operator-first): weaken forward secrecy against a total-order DS; short time windows kept.
- Forking Fluxer or building on Matrix/Stoat (Graft's analysis): LLM contribution ban and BDFL governance, 21-service stack, no client crypto model; Matrix has no MLS and cannot flip rooms out of encryption; the founder rejected both products.
- Identity private keys (UMK/SSK) on every device including browsers, and SSK transfer to any paired device (Protocol-correctness): lets an operator-served browser session leak the signing capability; tiered custody instead.
- Pairing group with external senders and unbounded leaf count (Protocol-correctness): the instance could add a third leaf and receive SSK_priv; pairing groups now carry no external senders and require exactly two leaves with a fingerprint check.
- Threads-lite, soundboard, webhooks, S3, Helm, open registration, compliance kit and DeepFilterNet in the daily-use build (various): all kept in v1 or phase 2 to protect the protocol and voice work.
