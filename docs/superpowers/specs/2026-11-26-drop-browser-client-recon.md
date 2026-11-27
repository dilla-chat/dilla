# Drop the browser client (@dilla/web SPA) from dillad — recon map

Worktree: /home/thim/Repositories/dilla/.claude/worktrees/signal-design (branch fix/desktop-test-flakes), read-only recon, 2026-11-26.
Decision (Jonas 2026-11-18 / 2026-11-26): dillad keeps API, gateway, /download, /i/<code> (desktop-only), /desktop-auth, webhooks, metrics, health. The SPA goes. TS packages stay in the repo.

## TL;DR — the four things that are not obvious

1. **The test hosts still need a served tree.** `dilla-testhost -web-root` (dilladtest.HostOptions.WebRoot -> dillad.Options.Web -> web.New + Fallback) is used by:
   - e2e core-integration (`e2e/playwright.core.config.ts:9` serves `packages/client-core/harness-dist`, NOT the SPA, verified by the same manifest check);
   - the GPUI Rust tests in clients/desktop (the other builder's area): `dilla-client/src/testing/host.rs:182` (parity world: `-web-root <packages/web/dist-desktop-strings> -parity-fixture`), `testing/host/music.rs:46-57` and `tests/support/call_world.rs:196-205` write a one-page tree + manifest only to trigger the testhost's **"-web-root rule"** (`cmd/dilla-testhost/main.go:173-176`: SFU + web root => `LiveKitURL = ws://<own listener>`).
   => Production must stop serving the SPA, but `Options.Web` / `web.New` / the manifest check must survive as a **test-only seam** (or the LiveKit rule must be decoupled from -web-root first, which touches clients/desktop — defer).
2. **No dilla.toml key is tied to the web client.** `internal/config/config.go` has no web/dist/root key (strict decoder `validate.go:40`, exit 78). Nothing to remove there; no exit-78 risk. `http.trusted_origins` (config.go:115) and `auth.webauthn.rp_origins` (config.go:307) are still needed by the desktop-auth passkey page (same origin) — KEEP.
3. **The real breakage risk is the CLI flag, not config.** CT 144 runs `dillad serve --config=... --desktop-downloads --desktop-downloads-channel=gpui-alpha` (docs/deploy/README.md:483, systemd drop-in). Deleting `--desktop-downloads` (cmd/dillad/serve.go:136) makes `serve` fail with a usage error (exit 2, not in RestartPreventExitStatus => restart loop). Keep the flag accepted as a deprecated no-op (log a warning), downloads always on.
4. **Side-effects that need a Jonas call** (see "Decisions" at the end): the browser OIDC return (`internal/api/auth.go:1306` `oidcReturnURL` -> `https://<domain>/#assertion=...`, the SPA's landing) becomes dead; the invite/download page font (JetBrains Mono woff2, 40 404 B) today comes out of the SPA tree; `/browser-migration` (the one-hour SPA access for browser users moving to desktop) goes with it.

---

## 1. Go

### internal/web (package `web`)
| File | Lines | What | Fate |
|---|---|---|---|
| internal/web/web.go | 1-3 | package doc "serves the browser client (C9)" | rewrite |
| | 23-28 | ManifestName, size caps, `permissionsPolicy` (camera/mic/display=self), `cspHead`/`cspTail` (script-src 'self' 'wasm-unsafe-eval', connect-src ws/wss, worker-src, media-src blob:) — SPA CSP | SPA CSP goes; downloads page gets its own tight CSP |
| | 30-31 | `//go:embed all:dist` `var dist embed.FS` | remove |
| | 48 | `reservedPrefixes` (/v1/, /gateway, /rtc, /i/, /hooks/, /healthz, /readyz, /metrics, /debug/) | keep (fallback must still hand these to the mux) |
| | 50-52 | `belowRootData` regexp -> `E_NOT_FOUND "the instance address must not have a path"` (fallback 300-303) | keep: an API answer to clients with a path in their saved address |
| | 57-64 | `Embedded()` (placeholder or CI-copied real build) | remove |
| | 90-204 | `New(fsys)`: manifest-verified tree | keep only as test seam (dilladtest/testhost), or move under dilladtest |
| | 223-254 | `ServeHTTP`: `/app-version.json` (SPA updater, 232-242), files, **SPA history fallback** (any extensionless path -> index.html, 250-252) | production: gone |
| | 256-268 | `securityHeaders` (nosniff, no-referrer, DENY, Permissions-Policy, SPA CSP with ws:// + wss:// Host) | replace for downloads page |
| | 270-294 | `serve`: ETag, `immutable` for assets/ (static asset caching), no-cache, no-store, 304 | keep the small part the downloads page needs |
| | 296-317 | `Fallback`/`fallback`: mux pattern or reserved prefix -> mux; `/desktop-auth` special-case (306) ; belowRoot -> error; else navigation | keep the routing core (downloads navigation) |
| internal/web/downloads.go | 18-27 | embeds downloads/index.html, style.css, brand.png | keep |
| | 29, 41-53 | font: picks `assets/jetbrains-mono-latin-wght-normal-*.woff2` **from the SPA tree** | breaks silently (system mono) unless the font is embedded here |
| | 31-32, 73-103 | `/browser-migration`, `/browser-migration/exit`, cookie `__Host-dilla-browser-migration` (1 h SPA access) | remove |
| | 115-120 | non-page paths fall through to `d.client.ServeHTTP` (SPA assets, `/app-version.json`) | remove; non-page paths -> 404 |
| | 122-129 | cookie => SPA index.html instead of the download page | remove |
| internal/web/downloads/index.html | 26-35 | "Moving from this browser?" section, links to /browser-migration | remove section |
| internal/web/dist/ | index.html (288 B placeholder), dilla-manifest.json | `.gitignore:36-38` keeps them tracked | delete dir + .gitignore lines |
| internal/web/placeholder_test.go | `//go:build !webdist`, TestTheEmbeddedPlaceholderIsAVerifiedTree | delete |
| internal/web/webdist_test.go | `//go:build webdist`, DILLA_WEB_DIST, TestBuiltClientIsServed | delete |
| internal/web/web_test.go | 104-537: New/manifest (104,132,422), routing five rules (224), headers (298), CSP ws origin (348), 304 (380), Fallback (437, 485), Permissions-Policy (512), CSP media-src (524) | keep manifest tests if New stays as seam; drop SPA routing/CSP tests; keep Fallback/belowRoot tests re-targeted at the downloads handler |
| internal/web/downloads_test.go | 18 navigation, 58 assets+mux routes, 90 & 141 migration, 197 brand | drop 90/141, rewrite 58 (no SPA assets), keep 18/197 |

### internal/dillad
- server.go:38 import `internal/web`.
- server.go:132-139 `tree := o.Web; if nil { web.Embedded() }; client, err := web.New(tree)` ("dillad: web client: ..." error).
- server.go:209-211 `api.Deps{ LandingFont: api.LandingFont(tree), DesktopDownloads: o.DesktopDownloads }`.
- server.go:724-740 mounting: `GET /download/{platform}` = `desktopdownload.NotFound()` unless `o.DesktopDownloads` (729-735); `h := client.Fallback(mux)`; `if o.DesktopDownloads { h = client.DownloadsFallback(mux) }`.
- options.go:86-97 `Web fs.FS`, `DesktopDownloads bool` ("False retains the original browser client/test host"), `DesktopDownloadRoutes`, `DesktopDownloadChannel`.
- Target: production always = downloads fallback + real download routes; `Web` non-nil only from dilladtest (serves that tree as today for the harness/parity). `DesktopDownloads` option can go (or stay true-by-default) — note tests that pass `DesktopDownloadRoutes` fakes.

### cmd/dillad
- serve.go:136 `--desktop-downloads` (bool, default false), :137 `--desktop-downloads-channel` (beta|gpui-alpha), :144 usage error, :339 passes both to Options.
- serve_downloads_test.go:11/21/33 document both flags + unknown-channel refusal.
- doctor.go: no web check (grep clean). init.go:201 prints `bootstrap invite ... https://<domain>/i/<code>` — fine (landing page stays, now desktop-only).
- media.go:24 comment mentions "web client" — cosmetic.

### internal/api (invite landing — keep, make desktop-only)
- invites.go:31-38 landingPage doc ("one same-origin link into the web client", "font is the web client's JetBrains Mono when dillad serves one").
- invites.go:75-81 template: `{{if .DesktopDownloads}}` Open Dilla desktop (`dilla://invite/<host>/<code>`) + `/download` link `{{else}}` **`<a class="open" href="/welcome?invite={{.Code}}">Open dilla in this browser</a>`** + "Or paste the code into a dilla client" `{{end}}`. Drop the else branch and the `DesktopDownloads` field (landingData:94, invites.go:258).
- invites.go:149-163 `landingFontGlob` + `LandingFont(webFS)`; invites.go:176 CSP `default-src 'none'; style-src 'unsafe-inline'; font-src 'self'`.
- api.go:51-56 `Deps.LandingFont`, `Deps.DesktopDownloads`.
- auth.go:1306-1309 `oidcReturnURL` -> `https://<domain>/#assertion=` (SPA OIDC landing). Desktop OIDC uses `desktop_request` -> `../../../desktop-auth/<id>/?done=1` (auth.go:1485) — unaffected.
- desktopauth.go:29-30 mounts `GET|POST /desktop-auth/{path...}` on the mux — unaffected. internal/desktopauth/http.go embeds browser.js + app.css (no SPA font, no SPA links) — unaffected.

### Routes/headers that exist only for the SPA
- SPA history fallback (web.go:250-252), `/app-version.json` (web.go:232; also protocol/02:455, docs/deploy/README.md:135-149), SPA CSP + Permissions-Policy (web.go:23-28, 256-268), immutable asset caching for `assets/` (web.go:197), `/browser-migration*` + cookie (downloads.go), `/welcome?invite=` link (invites.go:79).
- No service worker / web manifest routes exist in Go (grep: none).
- `/i/<code>`: an API route (InviteLanding), stays. `/welcome`, `/c/...`: only SPA navigation; after the change they get the download page (or 404 — pick one; download page is today's DownloadsFallback behaviour).

## 2. Tests

### Go tests that request SPA paths or expect the placeholder/fallback
| Test | File:line | Action |
|---|---|---|
| TestTheEmbeddedWebClientIsServedAtTheRoot (`/`, `/welcome`, `/c/<hex>` -> placeholder) | internal/dillad/server_test.go:813 | rewrite: those paths -> download page |
| TestTheAPIAnswersAsBeforeBesideTheWebClient | server_test.go:833 | keep, rename (API beside downloads) |
| TestTheWebClientAndTheSignallingProxyServeSideBySide (expects `content="placeholder"`) | server_test.go:874 | rewrite to download page |
| TestOptionsWebReplacesTheEmbeddedClient | server_test.go:899 | keep if Options.Web stays as seam |
| TestNewRefusesAWebClientItsManifestDoesNotDescribe | server_test.go:915 | keep if seam stays |
| TestEveryResponseCarriesTheGenerationHeader | server_test.go:518 | check it doesn't rely on placeholder |
| TestDesktopDownloadsAreAnExplicitServerOption (false => `/welcome?invite=` link) | internal/dillad/downloads_test.go:13,52 | rewrite: always desktop |
| TestDesktopDownloadRoutesFollowTheServerOption, TestDesktopDownloadChannelReachesTheDefaultRoutes | internal/dillad/downloads_routes_test.go:16,47 (use `webTree(...)` with SPA font) | rewrite (no option toggle) |
| TestTheLandingPageLinksIntoTheWebClient | internal/api/invites_test.go:605,614 | rewrite -> desktop actions |
| TestLandingHasNoScriptNoFormOneLink (requires `/welcome?invite=`) | invites_test.go:727,742 | rewrite |
| TestLandingOpenLinkIs44OnPhone, TestLandingFont | invites_test.go:713,763 | adapt (font source) |
| TestDesktopInviteActionsUseTheConfiguredHostAndExistingGrammar (`DesktopDownloads: true`, asserts no `/welcome?invite=`) | internal/api/invites_desktop_test.go:15,29,38 | drop the field |
| dilladtest web tests: TestTheHostServesItsWebRootFromItsOwnOrigin, ...RefusesAWebRoot..., TestWithoutAWebRootTheHostServesThePlaceholder | internal/dillad/dilladtest/web_test.go:84,101,112 | keep first two (seam); third -> "without a web root the host serves the download page" |
| cmd/dilla-testhost/main_test.go:12,47,112,131 (-web-root flag, LiveKitURL rule) | keep (seam) |
| cmd/dillad/serve_downloads_test.go:11,21,33 | adapt: flag deprecated no-op, channel still validated |
| internal/web/*_test.go | see section 1 |

Matches for "welcome" in internal/ds, internal/bot, api/directory_test etc. are the MLS **Welcome** queue (`/v1/welcomes`) — unrelated, leave alone.

### e2e
- `e2e/playwright.web.config.ts` (serves `packages/web/dist` via testhost 8463/8464; testIgnore support/core-worker/spike-joinall): only `e2e/web/native-interop.spec.ts` remains (web client vs GPUI peer) — **meaningless**; not run in CI (no `test:e2e:web:*` in any workflow). Delete config + spec + scripts `test:e2e:web:*`, `demo:web` (`playwright.demo.config.ts`, `e2e/web/demo.ts`). The GPUI side of interop is covered by the desktop's own real-dillad tests.
- `e2e/playwright.music.config.ts:16` (webRoot `packages/web/dist`, `e2e/music/youtube-playback.spec.ts`) — SPA-driven, not in CI: **meaningless**, delete (GPUI has its own music tests via `start_music_host`).
- `e2e/ux/*` (world/shots/sheets: drive the SPA for screenshots), `e2e/scripts/shoot-docs.mjs` (`docs:shots`) — SPA screenshot tooling; becomes meaningless for user docs (docs shots now from the desktop). Delete or leave dormant (Jonas: TS stays "for now"; these are tooling, not reference).
- KEEP (do not use the SPA):
  - `playwright.core.config.ts` (core-integration; harness-dist via testhost -web-root) — still meaningful while client-core is the behaviour reference; needs the test-only seam.
  - `playwright.config.ts` (browser-spike: core-wasm-spike Vite dev server, OPFS/mls-store).
  - `playwright.media.config.ts` (browser-media: packages/media harness on Vite 5179 + testhost with SFU, no web root).
  - `playwright.probe.config.ts` (media probe page via Vite).
  - `e2e/unit/*` node tests (peer-seed, totp, wall-clock, axe-contrast, conversation-layout import `e2e/web/support` / packages/web sources) — keep with the support files they import; conversation-layout/axe-contrast are SPA-source tests (optional later cleanup).
- `e2e/web/support/host.ts:35,71,396-397` mentions packages/web/dist in comments/errors — adjust message.
- `scripts/e2e-test-tools.mjs:91` (GPUI interop peer for native-interop) — drop with native-interop.
- `docs/design/signal/parity/shoot.mjs` + `clients/desktop/dilla-desktop/examples/parity_world/main.rs:97` use `packages/web/dist-desktop-strings` (`npm run build:desktop-strings -w @dilla/web`) via testhost -web-root — the GPUI parity **references**; KEEP (local, not in CI).

## 3. CI / release

### .github/workflows/server.yml
- `web-build` job :222-305 (needs changes; if go||web): wasm-pack core-wasm, wasm size, typecheck core-wasm/client-core/web/e2e, `npm run build -w @dilla/web`, `test:noise-release`, `test:dist`, `build:harness`, `go test -tags webdist ./internal/web/...`, uploads `dilla-web-dist` + `dilla-core-harness`. In server.yml its only consumer is go-release (the harness artifact is unused here). **Remove the job.**
- `go-release` :654-701 needs `[changes, web-build]`; steps `rm -rf internal/web/dist` (:665), download dilla-web-dist (:666-670), placeholder guard (:671-674), webdist test w/ DILLA_WEB_DIST (:676-681). **needs: [changes]; drop those 4 steps; gate `go` only** (ci-areas).
- `server-work-complete` :703-704 and `server-ok` :712-713 `needs:` lists include `web-build` — remove it from both (job name `server-ok` unchanged, so the ruleset's required check is unaffected).

### .github/workflows/desktop-ci.yml (workflow `desktop`, required check `desktop-ok`)
- `web-build` :530-612 (if e2e): same steps as server's; uploads dilla-web-dist (:602) + dilla-core-harness (:607). Consumer: `core-integration` only.
- `core-integration` :614-688 needs `[changes, rust-native, rust-wasi, web-build]`; downloads dilla-web-dist into packages/web/dist (:645-649, **unused by the core suite**) and dilla-core-harness (:650-655); runs `test:e2e:core:chromium`.
- Proposal: rename `web-build` -> `core-harness` (wasm-pack core-wasm, wasm size, npm ci, typecheck core-wasm/client-core/e2e, `build:harness`, upload dilla-core-harness only; drop `build -w @dilla/web`, `test:dist`, `test:noise-release`, webdist go test, dilla-web-dist upload). core-integration: needs core-harness, drop the dilla-web-dist download. Or keep the name `web-build` to minimise churn (checker, ci-local, docs all name it) — a rename touches ~6 files + tests.
- `desktop-work-complete` :690-691 / `desktop-ok` :699-700 needs lists name web-build — update with the rename.
- `ui` :104-182 (design-tokens + packages/ui storybook, axe, web-2b/2c/3 screenshots) — does NOT need the SPA build. It tests packages/ui (kept as reference). Keep; optional later trim (screenshot artifacts are web-era).
- `browser-spike` :338-415, `browser-media` :417-528, `rust-wasm-node` :184-250 — no SPA build; keep.
- `node` :63-102 runs `npm run check:desktop-auth` (:81) and `npm test` (incl. packages/web vitest) — keep (desktop-auth needs packages/web/src).

### .github/workflows/release.yml
- header :2-3 "the real web client embedded".
- `web-build` :86-127 (needs tag): builds SPA, webdist test, uploads dilla-web-dist. **Remove.**
- `go-release` :196-254 needs `[tag, web-build]` (:198); `rm -rf internal/web/dist`, download, placeholder guard, webdist test (:207-220). **needs: [tag]; drop steps.**
- SHA256SUMS (:279-294) lists only binaries, wasi core, installers — no web assets; unaffected.

### desktop-gpui.yml / desktop-gpui-release.yml / desktop-gpui-sign.yml / gpui-proofs.yml
- No `@dilla/web` build. desktop-gpui.yml:19,45 path filters on `packages/web/src/strings/**` (strings generator freshness, :226) — keep. Playwright there (:462-470) is for media tests + parity shooter. Testhost built at :244 — its `-web-root` seam must keep working. Nothing to change.

### scripts/check-ci-workflow.mjs (+ check-ci-workflow.test.mjs, 64 matching lines)
- :25-53 server job list includes `'go-release'`, `'web-build'` (comment :52 "go-release embeds the web client").
- :55-70 desktop job list includes `'web-build'` (:68).
- :115-118 `BROWSER_WEB_JOBS = { 'core-integration': ... }`; :149-163 `browserWebSteps` requires `name: dilla-web-dist`, `path: packages/web/dist` in every browser web job.
- :301-319 required `web-build` steps (build -w @dilla/web, test:dist, webdist test, dilla-web-dist upload, harness).
- :401-402 required `go-release` steps (dilla-web-dist, internal/web/dist, `content="placeholder"`, webdist test, DILLA_WEB_DIST).
- :1062-1086 needs rules (`core-integration` needs web-build; `go-release` needs web-build) and the "fetch + refuse placeholder before go build" order check.
- :1174 `RELEASE_NEEDS = { 'web-build': ['tag'], ..., 'go-release': ['tag','web-build'] }`.
- :254-256 node job must run `npm run check:desktop-auth` — keep.
- :582 comment ("web-build's dist is embedded by go-release").
- The aggregate rule (ok/work-complete needs every job) is derived from these lists, so removing jobs from the lists + the yml keeps `server-ok`/`desktop-ok` coherent.

### scripts/ci-areas.mjs (+ ci-areas.test.mjs:238 maps `internal/web/webdist_test.go` -> web)
- :51 comment (downloads_test.go brand asset) — keep.
- :57 `{ areas: ['go'], patterns: ['packages/web/dist', ...] }` — remove.
- :95 comment, :104 server `'web-build': ['go','web']`, :117 `'go-release': ['go','web']` -> `['go']`.
- :127 desktop `'web-build': ['e2e']` (rename if renamed), :128 core-integration.

### scripts/ci-local-plan.mjs, ci-local.mjs, ci-local-fs.mjs (+ ci-local.test.mjs, 10 lines)
- ci-local-plan.mjs:10 comment, :31-36 `SHARED_JOBS['web-build']`, :48 server web-build, :61-62 `'go-release': { exclusive: true, snapshot: ['internal/web/dist'] }` (it swaps the placeholder), :70 comment, :78 desktop web-build.
- ci-local.mjs:897 restores internal/web/dist after a dead go-release; snapshot machinery ci-local.mjs:434-439/630, ci-local-fs.mjs:155-210 — becomes unused for go-release (keep the generic machinery, drop the snapshot entry; go-release no longer needs `exclusive`).
- docs/dev/ci-local.md:21 (shared web-build), :115-116 (go-release runs alone because it replaces internal/web/dist).

### Required checks
- Ruleset requires the aggregates `server-ok` and `desktop-ok` (and the gpui aggregate); none of the removed/renamed jobs is required by name. Keep the aggregate job names; update their `needs:` in yml and the checker lists in the same commit, or `npm run check:ci` fails.

## 4. Must keep working
- **desktop-auth browser.js**: scripts/build-desktop-auth.mjs:3-7 bundles `packages/web/src/desktop-auth-browser.ts` (esbuild) into internal/desktopauth/browser.js; `npm run check:desktop-auth` (package.json:28) in desktop-ci `node` job (:81), enforced by check-ci-workflow.mjs:254-256 (+ test :248, :988). Needs only packages/web/src + node_modules — no SPA build. Untouched.
- **desktop-auth server side**: internal/api/desktopauth.go:29-30 mux routes; web.go:306 Fallback special case for `/desktop-auth` when navigation != SPA — keep that line in whatever fallback remains. app.css (internal/desktopauth/app.css) uses the system mono (no SPA font). Passkeys need `auth.webauthn.rp_origins` = the instance origin — unchanged.
- **gen-gpui-strings** (packages/web/src/strings/en.ts -> dilla-desktop/src/strings.rs) and **gen-gpui-tokens** (packages/design-tokens/src/tokens.ts, packages/ui/src/Icon/icons.ts, dist/tokens.css) read sources only — unaffected. `packages/design-tokens/dist/tokens.css` is committed and pinned by TestLandingTokensMatchSignal (invites_test.go:881) and desktopauth style_test.go — keep.
- **Download page** (`/`, `/index.html`, `/download`, `/desktop-download.css`, `/desktop-download-brand.png`, `GET /download/{platform}` via internal/desktopdownload): must become unconditional. Font fallback: today `@font-face` only if the SPA tree holds the woff2 (downloads.go:41-53).
- **Invite page** `/i/{code}`: desktop actions only (`dilla://invite/...`, `/download`, "paste this invite code").
- **CT 144 / Homelab**: deploy/install.sh has no web references; `dillad doctor` (cmd/dillad/doctor.go) has no web check. The unit's ExecStart carries `--desktop-downloads --desktop-downloads-channel=gpui-alpha` (docs/deploy/README.md:477-484): keep both flags parseable. After the upgrade, browser users lose the SPA immediately (decision already taken); open tabs on the old SPA will hit `/app-version.json` 404 and the download page.
- **dillad init / install.sh first account**: the bootstrap invite `https://<domain>/i/<code>` (init.go:201) now leads to "Open Dilla desktop" — first account must be made in the desktop app. docs/deploy/README.md:557-558 says "open it in a browser to make the first account ... from the web client" — rewrite.

## 5. Docs
- docs/deploy/README.md: :13 (browser tab reloads on wire 6), :70-71 (upgrade dillad "with its web client" first, reload tabs), :92 (browsers on wire 4), :103-149 (mermaid upgrade diagram with Open browser tab / IndexedDB, `/app-version.json` updater), :365-391 **"Build dillad with the web client from source"** (whole section: placeholder, npm build, webdist test, cp into internal/web/dist) — delete, :455-485 **"Desktop downloads" flag section** — rewrite (always on; `--desktop-downloads` deprecated no-op; channel flag alone now effective), :557-558 bootstrap invite, :1031-1203 second-factor/rp_origins/domain-move/"browser store schema" table — reword "browser" to desktop where it refers to the SPA; the store-schema table (:1176-1203) is SPA history (keep as history or trim).
- docs/dev/ci-local.md :11 (core-integration example ok), :21 web-build shared job, :115-116 go-release/internal/web/dist.
- docs/dev/testing.md: no SPA content (only GPUI tiers + release gate; :94 "Server (v*)" gate — check it doesn't mention web-build).
- CONTRIBUTING.md :56 "re-shoot the screenshots: `npm run build -w @dilla/web`, then ..." (docs shots from the SPA) — replace with the desktop screenshot path; :92-93 browser jobs list (browser-spike, browser-media, core-integration) — still true.
- README.md "what works" rows describing the browser client (e.g. :25) — per Jonas's user-docs rule.
- docs/user/getting-started.md:588 invite landing screenshot "Open dilla in this browser" (screenshots/invite-landing.png) — reshoot/reword.
- protocol/09-http-api.md :2020-2097 **"Web client and content manifest"** (reserved prefixes, SPA routing rules, `/welcome`, `/c/...`, manifest, `/app-version.json` :2122) and :2095-2096 invite landing -> `/welcome?invite=` — rewrite to "Download and invite pages" + changelog row in protocol/07-versioning.md (behaviour change; cf. 2026-10-05 row :236). protocol/02-delivery-service.md:455 (`/app-version.json` hint) — informative, reword. `npm run check:docs` (scripts/check-protocol-docs.mjs) and TestEveryMountedRouteIsADocumentedRoute (factor_owed_test.go:246) only see mux routes — the fallback paths are not mux routes, so no route-list breakage.

---

## Decisions to put to Jonas (AskUserQuestion)
1. **Browser OIDC sign-in**: `GET /v1/auth/oidc/start` without `desktop_request` returns to `https://<domain>/#assertion=...` (auth.go:1306), the SPA. Refuse it (E_NOT_FOUND / 400) without desktop_request, or leave it (lands on the download page, assertion wasted)? Recommend refuse.
2. **Font on download + invite pages**: embed the 40 404 B JetBrains Mono woff2 (OFL; licence note needed) into internal/web/downloads and serve it at a fixed path, or accept the system monospace (desktop-auth page already does)? Recommend system mono (smaller, no licence file), unless pixel parity with the Signal look matters here.
3. **Non-page paths** (`/welcome`, `/c/...`, `/settings`): download page (today's DownloadsFallback) or 404? Recommend download page for extensionless navigation, 404 for anything with an extension.
4. **`--desktop-downloads`**: keep as a deprecated no-op with a startup warning (recommended; removal = exit-2 restart loop on CT 144) — and later remove after the Homelab drop-in is edited.
5. **web-build rename** in desktop-ci to `core-harness` (clearer, ~6 files) vs keep the name (less churn).
6. Delete SPA-only e2e tooling now (web/music/demo configs, native-interop, ux/, docs:shots) or leave dormant?

## Proposed task split

### T1 — Go: production stops serving the SPA (opus; security boundary: headers/CSP/fallback)
Files: internal/web/web.go, internal/web/downloads.go, internal/web/downloads/index.html, internal/web/dist/ (delete) + .gitignore:36-38, internal/web/{placeholder,webdist}_test.go (delete), internal/web/{web,downloads}_test.go, internal/dillad/server.go (132-139, 209-211, 724-740), internal/dillad/options.go (86-97), internal/dillad/{server,downloads,downloads_routes}_test.go, internal/dillad/dilladtest/{host.go:114-118,428-430, web_test.go}, cmd/dillad/serve.go:136-144,339 + serve_downloads_test.go.
Changes: remove `go:embed dist`/`Embedded()`; downloads navigation is the only production fallback (always on); `/download/{platform}` always the real routes; drop `/browser-migration*`, cookie, SPA pass-through, `/app-version.json`, SPA CSP; give the download page a page-only CSP (`default-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`) and `Permissions-Policy` all-off; keep reserved-prefix + belowRoot + `/desktop-auth` handling; keep `Options.Web` + `web.New` manifest check as a test-only seam that serves the given tree (dilladtest/testhost -web-root: harness-dist, parity dist-desktop-strings, one-page LiveKit trees); `--desktop-downloads` -> deprecated no-op with a warning; channel flag always effective.
Tests that prove it: GET `/`, `/download`, `/welcome`, `/c/<hex>` -> download page with new headers; `/assets/x.js`, `/app-version.json`, `/browser-migration` -> 404; `/v1/...`, `/i/...`, `/desktop-auth/...`, `/healthz`, `/metrics` reach the mux; `/x/v1/instance` -> E_NOT_FOUND path error; dilladtest with WebRoot still serves its tree and refuses a bad manifest; `dillad serve --desktop-downloads` still parses (exit 0 path) and warns; `go build ./...` without any dist dir; `go test ./internal/web/... ./internal/dillad/... ./cmd/dillad/... ./cmd/dilla-testhost/...`.
Risks: CT 144 ExecStart (flag kept); a GPUI Rust test relying on the testhost placeholder at `/` (grep found none — they use API only); the go-harness/testkit scenario harness uses dilladtest without WebRoot (now download page instead of placeholder — check nothing asserts the placeholder: only the tests listed above do).

### T2 — Go: invite landing desktop-only (+ OIDC per decision 1) (sonnet if recipe is fixed; opus if OIDC refusal is in)
Files: internal/api/invites.go (31-38, 75-81, 94, 149-163, 176, 258), internal/api/api.go:51-56, internal/dillad/server.go:209-211, internal/api/invites_test.go (605-614, 713, 727-743, 763), invites_desktop_test.go, (auth.go:1306 + tests if decision 1 = refuse).
Changes: drop the `/welcome?invite=` branch and `DesktopDownloads` field; one primary action "Open Dilla desktop" + "Download Dilla desktop" + paste-the-code note; font per decision 2 (LandingFont from embedded asset or removed; CSP drops font-src if removed).
Tests: landing has no `/welcome`, no script/form, the dilla:// link uses the configured domain only, tokens test still green, 44px target on phone.
Risk: path-scoped instances have no dilla:// link (desktopInviteURL returns "") — page must still read sensibly with only Download + code.
Depends on T1 (server.go wiring); can be done in the same branch by the same builder or after T1.

### T3 — CI/release: no SPA build (sonnet; checker-driven)
Files: .github/workflows/server.yml (web-build :222-305 removed; go-release :654-701; aggregates :704, :713), .github/workflows/desktop-ci.yml (web-build :530-612 -> harness-only/renamed; core-integration :614-688; aggregates :691, :700), .github/workflows/release.yml (header :2-3, web-build :86-127, go-release :196-254), scripts/check-ci-workflow.mjs (:25-70, :115-163, :301-319, :401-402, :582, :1062-1086, :1174) + check-ci-workflow.test.mjs, scripts/ci-areas.mjs (:57, :95, :104, :117, :127) + ci-areas.test.mjs:238, scripts/ci-local-plan.mjs (:10, :31-78) + ci-local.mjs:897 + ci-local.test.mjs.
Changes: as in section 3; core-integration keeps harness-dist only.
Tests: `npm run check:ci`, `npm run test:ci-check`, `node --test scripts/ci-areas.test.mjs scripts/ci-local.test.mjs`, `npm run ci:local -- --only go-release,core-integration` (go-release builds without internal/web/dist; core suite green on harness-dist alone).
Risks: aggregates' needs vs checker lists drift => red server-ok/desktop-ok on every PR; release.yml change only provable on a tag (dry-run via check-ci-workflow's release rules); test:noise-release (release-noise-mode test) leaves CI with the SPA build — confirm it is only meaningful for the SPA (it is: experiments/noise build gate for `@dilla/web`).
Must land in the same PR as T1 (go-release would otherwise still copy a real client into a dir that no longer embeds; the webdist test file would be gone).

### T4 — e2e cleanup (haiku/sonnet; optional per decision 6)
Files: e2e/playwright.web.config.ts, playwright.demo.config.ts, playwright.music.config.ts, e2e/web/native-interop.spec.ts, e2e/web/demo.ts, e2e/music/, e2e/ux/, e2e/scripts/shoot-docs.mjs, e2e/package.json scripts (test:e2e:web:*, test:e2e:music*, demo:web, docs:shots, ux:*), e2e/tsconfig.json include, scripts/e2e-test-tools.mjs:91, e2e/web/support/host.ts error text :396-397.
Tests: `npm run typecheck:e2e`, `npm test -w @dilla/e2e`, `npm run test:e2e:core:chromium -w @dilla/e2e` still green.
Risk: e2e/unit tests import e2e/web/support/* — keep support files they use.

### T5 — Docs (haiku for prose, sonnet for protocol/09)
Files: docs/deploy/README.md (:13, :70-71, :92-149, :365-391, :455-485, :557-558, browser wording 1031-1203), docs/dev/ci-local.md (:21, :115-116), CONTRIBUTING.md (:56, :92-93), README.md "what works", docs/user/getting-started.md:588 + screenshots/invite-landing.png (reshoot from the new page), protocol/09-http-api.md :2020-2097/:2122, protocol/02-delivery-service.md:455, protocol/07-versioning.md new changelog row.
Tests: `npm run check:docs`, `npm test` (docs/brief/copy gates).
Risk: check-ui-copy / flows `## Copy` tables under docs/design/flows compare with packages/web en.ts — only touch if the landing copy lives there (it doesn't; it's Go template text).

Order: T1+T2+T3 in one branch/PR (fewer PRs; they break each other apart), T4/T5 in the same PR as follow-up commits. Then tell the Homelab session to upgrade CT 144 (flags unchanged).
