# Slash command catalog coverage

This matrix is checked against `packages/client-core/src/commands/registry.ts`.
“Handler evidence” means the production handler is reached with a real in-memory
client/adapter and its result or mutation is asserted. It is not browser evidence.
The shipping-browser evidence is limited to the rows named in
`e2e/web/slash-commands.spec.ts`; other commands remain unit/integration covered.
Passing focused evidence recorded below is Chromium only; the final Firefox and
WebKit matrix is reported separately in the acceptance run report.

The catalog guards in `builtin.test.ts` and `bot-handlers.test.tsx` assert that
every built-in and bot catalog entry has a handler. `registry.test.ts` covers
catalog-driven argument and permission validation. Any new `COMMANDS` entry must
be added to this matrix and receive an outcome assertion before it is considered
covered.

| Catalog command | Handler | Permission/context and result evidence | Public/private outcome | Browser evidence |
|---|---|---|---|---|
| `help` | `builtinHandlers.help` | `builtin.test.ts`: private registry help; rejects unknown command | Private text result | Composer private/literal flow |
| `me` | `builtinHandlers.me` | formatting-handler table in `builtin.test.ts`; exact plain type-0 send | Public message | Not claimed |
| `shrug` | `builtinHandlers.shrug` | formatting-handler table in `builtin.test.ts`; exact plain type-0 send | Public message | Slash E2E: publishes `/shrug` with explicit text and checks the rendered message |
| `tableflip` | `builtinHandlers.tableflip` | formatting-handler table in `builtin.test.ts`; exact plain type-0 send | Public message | Not claimed |
| `unflip` | `builtinHandlers.unflip` | formatting-handler table in `builtin.test.ts`; exact plain type-0 send | Public message | Not claimed |
| `code` | `builtinHandlers.code` | formatting-handler table in `builtin.test.ts`; exact plain type-0 send | Public message | Not claimed |
| `giphy` | `builtinHandlers.giphy` | registered-handler guard; `CommandHost.test.tsx` verifies the callback route; feature requires GIFs enabled | Private picker, then user-selected public message | Not claimed |
| `dm` | `builtinHandlers.dm` | `builtin.test.ts`: navigation through explicit callback, no stale navigation | Private navigation | Not claimed |
| `invite` | `builtinHandlers.invite` | `builtin.test.ts`: focused invite callback; current-community permission comes from registry | Private dialog/navigation | Not claimed |
| `search` | `builtinHandlers.search` | `builtin.test.ts`: explicit search callback and arguments | Private navigation | Not claimed |
| `topic` | `builtinHandlers.topic` | `builtin.test.ts`: updates only topic field; Manage Channels permission checked by handler | Private result; topic mutation | Not claimed |
| `thread` | `builtinHandlers.thread` | `builtin.test.ts`: opens the loaded thread root by stable message ID | Private navigation | Thread UI acceptance is in `threads.spec.ts`; command route not claimed |
| `reply` | `builtinHandlers.reply` | `builtin.test.ts`: exact message reply in bound channel/thread | Public message | Not claimed |
| `edit` | `builtinHandlers.edit` | `builtin.test.ts`: owner-only check and exact loaded message; mention encoding/UTF-8 tests | Public edit | Not claimed |
| `delete` | `builtinHandlers.delete` | registered-handler guard plus common confirmation/cancel and current-permission regression tests in `builtin.test.ts` | Public deletion after confirmation | Not claimed |
| `pin` | `builtinHandlers.pin` | `builtin.test.ts`: Pin Messages accepted without Manage Messages | Public pin event | Not claimed |
| `unpin` | `builtinHandlers.unpin` | `builtin.test.ts`: unpins exact loaded message with Pin Messages | Public unpin event | Not claimed |
| `join` | `builtinHandlers.join` | `builtin.test.ts`: waits for a real voice session and reports setup failure | Private voice control | Not claimed |
| `leave` | `builtinHandlers.leave` | `builtin.test.ts`: leaves the live voice call and reports outcome | Private voice control | Not claimed |
| `mute` | `builtinHandlers.mute` | registered-handler guard; voice control actions use the live call context | Private voice control | Not claimed |
| `unmute` | `builtinHandlers.unmute` | registered-handler guard; voice control actions use the live call context | Private voice control | Not claimed |
| `deafen` | `builtinHandlers.deafen` | `builtin.test.ts`: stale-view deafen does not toggle mute back off | Private voice control | Not claimed |
| `undeafen` | `builtinHandlers.undeafen` | registered-handler guard; voice control actions use the live call context | Private voice control | Not claimed |
| `play` | bot music handler | `bot-handlers.test.tsx`: first real worker search result; URL handling; current-call, DJ, revision and uncertainty regressions | Private result; changes voice queue | Not claimed; slash E2E does not invoke `/play` |
| `pause` | bot music handler | `bot-handlers.test.tsx`: exact operation and state change; no-call and non-DJ refusals | Private result; changes voice player | Not claimed |
| `resume` | bot music handler | `bot-handlers.test.tsx`: exact operation and state change | Private result; changes voice player | Not claimed |
| `skip` | bot music handler | parameterized operation test asserts current/next queue state | Private result; changes voice queue | Not claimed |
| `stop` | bot music handler | parameterized operation test asserts queue and player cleared | Private result; changes voice player | Not claimed |
| `queue` | bot music handler | `bot-handlers.test.tsx`: authenticated empty/filled snapshot result; requires active voice context | Private snapshot | Slash E2E: invokes `/queue` in the currently joined voice context and checks the private empty snapshot |
| `nowplaying` | bot music handler | same authenticated snapshot path; handler tests assert private output | Private snapshot | Not claimed |
| `seek` | bot music handler | parameterized operation test asserts requested position and snapshot refresh | Private result; changes voice player | Not claimed |
| `volume` | bot music handler | parameterized operation test asserts requested volume | Private result; changes voice player | Not claimed |
| `shuffle` | bot music handler | parameterized operation test asserts shuffle state | Private result; changes voice queue | Not claimed |
| `repeat` | bot music handler | parameterized operation test asserts repeat mode | Private result; changes voice queue | Not claimed |
| `queue remove` | bot music handler | parameterized operation test asserts stable selected queue-item removal | Private result; changes voice queue | Not claimed |
| `queue move` | bot music handler | parameterized operation test asserts stable item reorder; immutable retry body is covered | Private result; changes voice queue | Not claimed |
| `queue clear` | bot music handler | parameterized operation test asserts queue empty | Private result; changes voice queue | Slash E2E: invokes `/queue clear` in the currently joined voice context and checks the private result |
| `music` | bot music handler | `bot-handlers.test.tsx`: help text comes from music catalog; current call required | Private help text | Not claimed |
| `music help` | bot music handler | same help handler and permission/context rules as `music` | Private help text | Not claimed |
| `kick` | `builtinHandlers.kick` | `builtin.test.ts`: real two-party accepted removal after confirmation; cancellation and permission recheck | Private confirmation; public membership removal | Slash E2E: live member autocomplete, cancel, confirm, and removal of a distinct joined account (Chromium focused pass) |
| `ban` | `builtinHandlers.ban` | `builtin.test.ts`: catalog permission, safe reason validation and stable target; registered-handler guard | Private confirmation; public membership restriction | Not claimed |
| `unban` | `builtinHandlers.unban` | `builtin.test.ts`: loads current bans before confirmed stable-ID mutation | Private confirmation; public membership restoration | Not claimed |
| `timeout` | `builtinHandlers.timeout` | handler guard plus `builtin.test.ts` confirmation and rank/permission checks; warnings capability required | Private confirmation; public member restriction | Not claimed |
| `untimeout` | `builtinHandlers.untimeout` | `builtin.test.ts`: clears current timeout with confirmed stable user ID | Private confirmation; public member update | Not claimed |
| `warn` | `builtinHandlers.warn` | moderation evidence tests cover accepted removal/warning outcome and current permission; handler guard | Private confirmation; public moderation record | Not claimed |
| `modlog` | `builtinHandlers.modlog` | `builtin.test.ts`: opens moderation history through explicit host callback; permission gated | Private navigation | Not claimed |
| `role add` | `builtinHandlers.role add` | `builtin.test.ts`: rank-safe role grant and observed member slice update | Private confirmation/result; public role mutation | Not claimed |
| `role remove` | `builtinHandlers.role remove` | `builtin.test.ts`: exact target IDs after confirmation; cancel sends no mutation | Private confirmation/result; public role mutation | Not claimed |
| `selfrole list` | `builtinHandlers.selfrole list` | `builtin.test.ts`: only service-returned assignable roles are listed | Private result | Not claimed |
| `selfrole add` | `builtinHandlers.selfrole add` | `builtin.test.ts`: list and grant only service-returned role by stable ID | Private result; public role mutation | Not claimed |
| `selfrole remove` | `builtinHandlers.selfrole remove` | `builtin.test.ts`: removes only a current self-role with stable ID and attempt ID | Private result; public role mutation | Not claimed |
| `rolepanel create` | bot role-panel handler | `bot-handlers.test.tsx`: focused editor; publication and registration use distinct IDs and recover without duplicate post | Public panel plus private result; Manage Roles and Bot capability required | Not claimed |
| `announcement list` | bot announcement handler | `bot-handlers.test.tsx`: authenticated response is shown privately | Private snapshot | Existing announcement scheduler E2E covers shipping scheduling UI, not slash handler |
| `announcement create` | bot announcement handler | `bot-handlers.test.tsx`: valid dialog submission only; immutable body, current permission and uncertain receipt regressions | Private result; scheduled action | Not claimed |
| `announcement edit` | bot announcement handler | `bot-handlers.test.tsx`: stable-ID suggestion, current revision and recurrence preservation | Private result; scheduled action | Not claimed |
| `announcement delete` | bot announcement handler | `bot-handlers.test.tsx`: explicit confirmation and revision-bound delete | Private confirmation/result; scheduled action | Not claimed |
| `poll create` | utility provider | `UtilityCommandProvider.test.tsx`: dialog adapter publication; edited payload survives uncertainty; send permission rechecked | One authenticated public card; private command status | Slash E2E: channel and thread creation dialogs, authenticated published cards, and same-identity closed-card recovery after disable/re-enable (Chromium focused pass) |
| `poll vote` | utility provider | `UtilityCommandProvider.test.tsx`: answer replacement, withdrawal, real nonempty selection/retry, voters and close behavior | Private command status; authenticated card mutation | Not claimed for this slash handler; Chromium focused card UI passes real selection, accepted vote, replacement and withdrawal |
| `poll results` | utility provider | provider tests cover authenticated snapshot/result and bad marker/refused binding | Private result | Not claimed for this slash handler; Chromium focused card UI passes authenticated results and named Bea voter display |
| `poll close` | utility provider | provider tests cover creator/moderator authorization, close outcome, conflict refresh and uncertain recovery | Private status; closes authenticated card | Not claimed for this slash handler; Chromium focused card UI passes creator close and closed-state recovery after re-enable |
| `remind` | utility provider | `UtilityCommandProvider.test.tsx`: create dialog, IANA timezone/DST fold, private account ownership and uncertainty | Private result; account-owned durable reminder | Slash E2E: creates a real reminder through its dialog; two-device due/dismiss lifecycle and post-ACK same-device archive recovery pass in Chromium |
| `reminders list` | utility provider | provider tests cover due list, multi-page limit and owner-change redaction | Private list | Not claimed for this slash handler; shipping inbox list/dismiss UI passes in Chromium |
| `reminders cancel` | utility provider | provider tests cover scheduled/due cancel and source-access revocation | Private result; cancels personal record | Not claimed |
| `reminders dismiss` | utility provider | provider tests cover account-level dismissal and uncertain recovery | Private result; dismisses due record | Not claimed for this slash handler; shipping inbox list/dismiss UI passes in Chromium |

## Deliberate browser limits

Slash E2E does not claim that every catalog handler ran in a browser. Music queue
search/playback depends on an external media source and is covered with the
deterministic adapter/unit fixture; browser coverage proves the current joined
voice context, authenticated state read, and private result. Announcement slash
handlers and most moderation/role handlers likewise remain covered by focused
handler tests, while the browser runs a real two-user moderation confirmation
and shipping composer/poll/reminder paths. External provider playback acceptance
is reported separately; this matrix does not claim a full external playback pass.

The current WebKit runner lacks the native encrypted-media transform API and
reaches the shipping “Voice does not work in this browser” gate. Its music
scenario asserts that refusal, no joined call or accepted music transport,
private slash-command prerequisites, and no implicit publication. Accepted SFU
`/queue` and `/queue clear` evidence belongs to Chromium and Firefox; adding the
missing WebKit media capability is outside the slash-command plan. The scenario
checks actual native capabilities, so a future capable WebKit runs the SFU path.

## Shared composer interactions

- `CommandComposer.test.tsx`: keyboard picker navigation and Tab selection,
  Enter selection versus execution, two-stage Escape, IME Enter handling,
  multiline/free-text values, named argument validation, stable entity IDs,
  and uncertain-result draft retention.
- `CommandHost.test.tsx`: built-in production dispatch, command availability
  after context/permission changes, unknown slash remaining private until the
  explicit literal action, and stable request IDs across intentional retries.
- `slash-commands.spec.ts`: the shipping app mounts the real picker/options and
  handlers; it selects a command through the picker, runs private and public
  outcomes, sends unknown and `//` text explicitly, and keeps main/thread drafts
  separate. These browser claims do not extend to every handler in the catalog.
