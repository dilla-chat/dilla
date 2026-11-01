# Structured slash commands

Approved scope: one PR against main; Discord-style structured composer, named arguments,
autocomplete and pre-send validation. Built-in and managed bot only. Private status/results
except explicit publication. Complex actions can open prefilled dialogs. Replace ordinary
chat-text music execution, including replay/edit triggers. No third-party API, account
settings, server/channel/role creation, bot installation, credentials or bot configuration.

## Catalog
- help [command]; me <text>; shrug/tableflip/unflip [text]; code <language> <text>
- giphy [query]; dm <user>; invite; search <query> [scope]; topic <text>; thread <message>
- reply <message> <text>; edit <message> <text>; delete <message> [reason]; pin/unpin <message>
- join <voice-channel>; leave; mute/unmute; deafen/undeafen
- play <query-or-url>; pause/resume/skip/stop; queue; nowplaying; seek <position>;
  volume <0..100>; shuffle <on|off>; repeat <off|one|all>
- queue remove <item>; queue move <item> <position>; queue clear; music (help)
- kick <user>; ban <user> [duration] [reason]; unban <user>; timeout <user> <duration>;
  untimeout <user>; warn <user> <reason>; modlog
- role add/remove <user> <role>; selfrole list/add/remove; rolepanel create
- announcement list/create/edit/delete
- poll create/vote/results/close
- remind <when> <text>; reminders list/cancel/dismiss

## Composer
Slash at start opens command picker; selecting yields command chip, named fields, required
and optional arguments and entity suggestions with stable IDs. Complete typed commands
also work. Arrow navigation, Enter/Tab selection, Enter execution only after validation,
Escape list then mode cancellation preserving draft. IME/multiline/touch/accessibility
and normal mention composition preserved. Unknown slash input stays private; explicit
Send as text and // escape supported. Text and command drafts isolated per conversation
and thread. Commands cannot consume unrelated attachment/reply state. Distinct pending,
success/failure/uncertain results; uploaded is not completed. Inaccessible entities never
suggested. Explain temporary prerequisites. Music targets caller's joined community voice
channel, respects DJ/owner/admin checks, plays selected first search result or URL playlist.

## Polls
Bot-enabled community channels and threads. 2..10 answers, question <=300 Unicode chars,
answer <=100, single choice default and optional multi-choice; named voters, vote replacement
and withdrawal, deadline, creator/Manage Messages closure. Duration presets 1h/4h/8h/24h/3d/7d,
default24h. Maximum128 open/community. Authoritative encrypted bot records with per-user
votes, bounded pages; authenticated bot card bound to installation/community/channel/thread.
One durable publication, readable fallback, receipt recovery. No raw marker trust. Current
permission and deadline checks atomic with mutations; correct after downtime. Closed polls
stay readable. Protocol body limits apply as aggregate UTF-8 budgets as well as char limits.

## Reminders
One-time bot-backed personal reminders, account-owned across devices via device-bound
private interaction requests. 1 minute..365 days, duration or explicit local date/time;
preview exact time/zone. IANA submitting-device timezone, stored UTC; DST gaps rejected and
ambiguous times explicitly selected. Max100 outstanding/user/community; due stays until
dismissed. Durable before success, catch up after bot downtime/browser closure. Private
inbox with badge/upcoming/due/cancel/dismiss; account-level dismissal, per-device notification
dedup, reconnect/unlock and bounded staggered poll; existing DND/notification permissions.
No promise of OS notifications while browser closed. No delivery/access after membership
revocation or disabled bot. No auto-deletion of due reminders.

## Architecture and correctness
Shared typed registry/parser/validator/help. Separate structured React composer while normal
textarea remains. Existing worker/action/voice/music services and dialogs; no React privileged
HTTP bypass. New op3 safe capability read (op0 manager-only unchanged). New ops70..75 polls,
80..84 reminders with strict Go/Rust/TS parity/shared vectors and Rust response-binding.
Worker owns durable consumer inbox: validated response persisted before journal ACK,
correlated by installation/community/request/op/target. Migrate existing response consumers.
Encrypted client migrations preserve pending data. No crypto membership weakening. Native
bot state area8 with disjoint versioned prefixes, bounded keys/values/transactions/scans.
Durable request fingerprint/outcome and mutation in same transaction, restart-safe dedup.
Utilities need no new privileged cap; current membership/channel permissions enforced.

## Acceptance
Catalog-driven coverage for every command: validation, permission, real handler outcome.
Test concurrent consumers, duplicates, stale context, restart/uncertain upload, expiry,
permission changes, spoofing, cross-user/device isolation, poll concurrency/publication,
reminder offline/cross-device/DST/quota. Browser Chromium/Firefox/WebKit for real composer,
music, moderation second user, channel/thread poll, two-device reminders, restart and
private/public separation. Setup through shipping flows where possible. Affected Go/Rust,
shared vectors, TS/UI/web, typechecks/lint/build/docs and final-head CI. One PR, no deploy
or auto-merge. Offer focused Obsidian save after verification.
