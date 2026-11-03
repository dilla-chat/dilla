# Dilla Bot qualification record

> Historical document: Spotify integration was removed from Dilla on 2026-11-01.
> Provider setup, playback claims, and release gates below describe the earlier implementation.

Date: 2026-10-21. Scope: Task 0 baseline and Task 1 development probes.
Provider revision: the user approved evaluating librespot instead of Soloist.
The Soloist observations below are retained as historical evidence.
Status: **not qualified; release gate remains closed; implementation may continue**.
Latest evidence: isolated YouTube extraction and real audio through Chromium and
Firefox pass. Removing the encryption hook makes both browser checks fail.
Dedicated-account Spotify playback and the intended service environment remain
unverified. The intended host is the existing Dilla server. The user's later
correction moves account connection into server settings and supersedes the
earlier implementation blocker; see the final section. Earlier gate statements
below are the historical record, not current instructions to stop development.
See [implementation plan](2026-10-21-dilla-bot.md) and
[design](../specs/2026-10-21-dilla-bot.md).

## Repository baseline

`git fetch origin` succeeded. The branch starts at
`d5f1bb00dfa8af751c06a1a395adfb55c9e5502e`, exactly one commit after the
planning baseline `98b651aa987d2012c753ca44e7b3dfee5eaedf3a`.
That commit is web3.10 profile-card and voice/sidebar polish (#42); bot UI work
must use the new VoiceView, MemberCardHost, ProfileCard and settings layout.
No backend/protocol changes occurred between those revisions. Wire version is
still 2, permission bit 23 is manage_nicknames, bit 24 remains available.

Worktree: `/home/thim/.cache/dilla-tmp/dilla-bot`, branch `feat/dilla-bot`.
The native tool initially placed it outside writable roots; the same worktree
was moved with explicit filesystem approval, without creating a duplicate.
Original `/home/thim/Repositories/dilla` stays on local main `3ed7234b` with its
untracked `.claude/` untouched.

## Initial local prerequisites (before the librespot revision)

| Item | Observation |
| --- | --- |
| Soloist | Neither `soloist` nor `spotify-soloist` on PATH; no `/opt/soloist` |
| Dedicated bot service | No `/etc/dillabot`, `/var/lib/dillabot`, or runtime credential directory |
| Credentials | No Spotify/Soloist environment-variable names; no secret values inspected |
| Desktop Spotify | `/opt/spotify/spotify` exists; not used as a bot or as account authorization |
| yt-dlp | Not installed on PATH |
| Deno | Not installed on PATH |
| Node | v24.15.0 (not yet qualified with yt-dlp JS components) |
| ffmpeg / ffprobe | n9.0.2; ffmpeg built with libopus and libpulse |
| Audio tools | pipewire and pactl available; pulseaudio daemon absent |
| Isolation tool | bwrap present; service/provider isolation not qualified |
| Toolchains | Go 1.27.0, Cargo 1.98.1 |

This is the development workstation, not a verified intended service environment.
No real provider playback, browser decryption, isolated capture, pairing, or reboot
test has been performed. Installed tools alone are not qualification evidence.

## Historical Soloist constraints

The currently documented Soloist startup uses `--api-key VALUE`; no protected
file or inherited-descriptor input is documented there. This conflicts with the
approved design's prohibition on credentials in argv. A private environment file
followed by expansion into an argument still places the credential in argv. We
have not inspected an operator-supplied binary and cannot conclude that an
undocumented supported input exists. Do not invent a flag or weaken the boundary.
Source: [Soloist authentication](https://developer.spotify.com/documentation/soloist/concepts/authentication).

Its WebSocket API has no client authorization or browser-origin checks. A command
result only acknowledges dispatch. `get_queue` can request all *available* entries;
that does not prove complete expansion of an arbitrary playlist without changing
playback. Both isolation and metadata-only complete expansion need real evidence.
Source: [Soloist WebSocket reference](https://developer.spotify.com/documentation/soloist/reference/websocket-api).

Version output and exit code 10 can inform expiry diagnostics. The original
plan's 90-day lifetime remains a qualification assumption to verify against the
specific supplied build. Do not redistribute the executable.
Sources: [command line](https://developer.spotify.com/documentation/soloist/reference/command-line),
[downloads and updates](https://developer.spotify.com/documentation/soloist/reference/downloads-and-updates).

The stated external broadcast restriction remains unresolved; the published policy
describes one source serving simultaneous listeners as prohibited. Technical
qualification alone does not resolve that constraint.
Source: [Spotify developer policy, section III](https://developer.spotify.com/policy).

YouTube qualification requires yt-dlp with its JS support and a supported runtime,
network isolation, actual extraction and bounded decoding. Node availability by
itself proves none of these.
Source: [yt-dlp documentation](https://github.com/yt-dlp/yt-dlp).

## Optional developer account qualification (historical manual flow)

The original probe used protected-file PKCE authorization and requested a test
host/service identity. The user has since selected the existing Dilla host and
clarified that Spotify authorization belongs in the shipping server settings.
The manual probe is optional developer tooling, not a prerequisite for continuing
implementation. Never provide credentials or the redirected authorization URL in
chat. Soloist and its API key are no longer required. Production deployment
remains a separate action. See the development
[probe instructions](../../../experiments/bot-providers/README.md).

Task 1 then needs actual evidence for headless authorization/direct PCM output, restart
and reboot, complete track/album/playlist resolution without playback, pause/seek/
transition/completion behavior, bounded Opus output and browser SFrame decryption.
The development test peer now accepts captured Opus; real YouTube audio has passed
it in both browsers. Spotify still needs actual account audio through this path.
Mocks and command acknowledgements cannot pass that row.

## Initial baseline failures

- `cargo test -p dilla-core` fails at `vectors_native.rs`: `vectors` is
  feature-gated. Existing CI enables all features; focused command must include
  `--features vectors`. This is a plan-command correction, not a source change.
- API/DS Go tests initially fail because
  `internal/mlswasi/testdata/dilla_core_wasi.wasm` is generated and absent from a
  fresh worktree. Build it with the existing locked WASI command and copy the
  artifact before rerunning. Gateway/media/SFrame pass on the initial run.
- `npm ci` installs the locked dependency set successfully; its audit reports
  55 existing findings (17 moderate, 34 high, 4 critical). No dependency versions
  were changed as part of this preparation.

## Baseline results after prerequisite setup

| Command | Result |
| --- | --- |
| `cargo test -p dilla-core --features vectors` | Passed: 618 tests, 4 ignored across 20 summaries |
| `cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked` | Passed; generated artifact copied to ignored testdata path |
| `go test ./internal/api ./internal/ds ./internal/gateway ./internal/media ./internal/sframe` | All five packages passed after fixture setup |
| `npm test -w @dilla/client-core` | 1,987 tests / 51 files passed |
| `npm test -w @dilla/web` | 2,187 tests / 110 files passed |
| Both workspace `npm run typecheck` commands | Passed |
| `npm run lint` | Passed |
| `npm run check:docs` | Passed |
| `npm run vectors` plus tracked-vector diff | Passed; no vector changes |

These establish the baseline, not bot acceptance. No production Rust/Go/TypeScript
code was changed. PostgreSQL-specific, race and browser release matrices have not
been run for this bot; they remain future release gates. dilla-bot-core,
internal/bot and cmd/dillabot do not yet exist.

The development-only [provider harness](../../../experiments/bot-providers/README.md)
adds dependency/version checks and local synthetic Opus encoding. It does not
launch providers or consume credentials. Its unit tests and real ffmpeg smoke
check are preparation for Task 1, not completion of Task 1.

## Initial development harness verification (before the librespot revision)

After commit `ea0c9a96`, the coordinator independently ran:

```sh
cd experiments/bot-providers
python3 -m unittest -v
python3 provider_harness.py opus-smoke
python3 provider_harness.py doctor
```

The 13 unit tests passed. The real local ffmpeg/ffprobe check produced 51 Opus
packets at 48 kHz stereo, 20 ms frame cadence and 96 kbps CBR. It explicitly
reports provider playback and browser SFrame decryption unverified. `doctor`
returns the expected exit 1 for missing Soloist and yt-dlp and reports qualification
unverified, even though local encoding works.

An independent review found that a descendant holding stdout could defeat the
deadline and a SIGTERM-ignoring child could survive cleanup. Both were reproduced
before fixing; the coordinator observed a 0.05-second timeout return success after
1.019 seconds. The fixes preserve readiness-based reads and terminate the spawned
process group independently of its leader. New tests check elapsed time and actual
descendant termination. The reviewer's scoped second pass approved both fixes,
with no new Important findings. This review covers the bounded harness only.

Task 0 is complete. Task 1 is **blocked/unqualified**, with the preparatory harness
implemented and verified. Tasks 2–16 have not begun. No production bot service,
UI, APIs, protocol change, provider integration, feature release, PR, or live
deployment is represented by this preparation. Resume from the current operator inputs
and qualification matrix; do not bypass the qualification gate.

## Librespot development probe

The user approved replacing Soloist with librespot for qualification. The
standalone Rust probe in `experiments/bot-providers/librespot-probe` pins
librespot 0.8.0 with default audio/discovery features disabled. A private PCM pipe
replaces the virtual sink and unauthenticated control socket. The community
identity/MLS helper remains a separate planned component.

The nested Cargo.lock also fixes `vergen` at 9.0.6 with `vergen-gitcl` 1.0.5.
Initial dependency resolution selected vergen 9.1.0, which mixed incompatible
vergen-lib traits in librespot-core's build script. The compatible lockfile
builds without modifying upstream source or the main workspace's dependencies.

Implemented development commands are authorization begin/completion, metadata
resolution, bounded track playback, and a synthetic PCM smoke check. Controls
cover pause/resume/seek/volume/stop; queues remain a future runtime feature.
OAuth uses PKCE with state, exact callback and expiry validation. Files and
callbacks are size-bounded; secret files require mode 0600 and the state directory
0700. Metadata resolution constructs no player and rejects incomplete, truncated,
non-track or oversized collections before returning descriptors.

Independent OAuth-module review approved the implementation. The integrated
helper review found a post-login cleanup gap when credential persistence fails.
Commit `5db759ae` fixes it with a session shutdown guard, preserves shutdown before
joining the player thread, and adds an offline lifecycle regression using a real
librespot Session without connecting. The coordinator also reproduced and fixed
a test fixture that depended on the ambient umask. The scoped re-review approved
both fixes with no remaining findings.

The coordinator independently verified the final integrated helper:

| Check | Observed result |
| --- | --- |
| Locked Rust tests | 20 unit and 2 subprocess tests passed after the cleanup fix |
| Formatting and clippy with warnings denied | Passed |
| Shared production sink and converter, synthetic input | 176,400 bytes: one second of 44.1 kHz stereo signed 16-bit little-endian PCM |
| That PCM through FFmpeg and ffprobe | 51 Opus packets, 48 kHz stereo, 20 ms frame cadence, 96 kbps CBR |
| Local authorization setup, without browser/account | Created mode-0700 state and mode-0600 pending file |
| Synthetic wrong-state callback | Rejected without token exchange, consumed pending state, allowed a new attempt |
| Doctor with built probe path | Helper available; yt-dlp absent; exit 1 and qualification explicitly unverified |
| Private-umask fixture check | Passed under umask 077 after making the loose-mode fixture explicit |

These tests use synthetic audio and locally generated authorization state only.
No account token exchange, cached-account login, real Spotify audio, service host
restart/reboot or browser decryption was tested. Actual provider qualification
remains open, including complete real playlists and upstream audio-key behavior
for the selected account. The known compatibility issue is tracked
[upstream](https://github.com/librespot-org/librespot/issues/1649).

The Python checker revision also passed all 14 tests; protocol documentation
checks passed. No root-workspace Rust/Go/TypeScript implementation was changed.
The isolated worktree retains signed-off commits; original main and `.claude/`
remain untouched. The development probe and its review are complete; this does
not complete Task 1's real provider qualification.

Use the [probe guide](../../../experiments/bot-providers/README.md) for the next
account test. Soloist binaries, API keys, sink setup and build-expiry requirements
are superseded. Task 0 is complete; Task 1 remains unqualified; Tasks 2–16 remain
unstarted. No production bot release or live deployment has occurred.

## Isolated YouTube extraction and browser audio

Continued work used `gpt-6-luna` implementers and independent reviewers, with the
coordinator integrating and verifying the results. All changes remain development
probes or test infrastructure; no `dillabot` runtime or bot UI is delivered.

The pinned Python virtual environment contains yt-dlp 2026.8.19 and yt-dlp-ejs
0.8.0, installed with hashes from `youtube-requirements.txt`. The actual sandbox
used Node 24.15.0 and FFmpeg/ffprobe n9.0.2. The runner mounts the selected Node
executable individually: the host's `/usr/bin/node` was 20.20.2 and would not meet
the upstream EJS requirement. The packaged JavaScript setup follows the
[yt-dlp EJS instructions](https://github.com/yt-dlp/yt-dlp/wiki/EJS).

The Linux runner uses bubblewrap with private filesystem, process and network
namespaces. Its private Unix-socket broker permits CONNECT to approved YouTube
domains on port 443 only, rejects non-public DNS answers, and connects to the
validated numeric address. Tests exercise actual namespaces, including direct
network denial, hidden host files, output limits and process cleanup. The virtual
environment mount is restricted to the probe's dedicated directory; personal
configuration, cookies and plugins are absent. Missing isolation fails closed.

Integration exposed three compatibility issues that were reproduced and fixed:

- The system CA symlink pointed outside the mounted certificate directory. The
  resolved certificate bundle is now mounted explicitly; certificate validation
  remains enabled.
- Full search-result format lists exceeded the 2 MiB metadata bound. Flat playlist
  enumeration now produces bounded descriptors, with duration optional when
  unknown. Individual playback still resolves the selected video fully.
- FFmpeg's CONNECT request uses `Host: 127.0.0.1:3128`, naming its proxy. The parser
  accepts that exact value while retaining the CONNECT authority as the sole
  destination for allowlist, DNS and routing checks. Wrong proxy ports, arbitrary
  Host values and private CONNECT destinations remain rejected.

Independent review also found an overly broad optional tools mount, an input
control-character normalization gap, an incorrect default media-peer path, a
timeout test that could terminate before installing its signal handler, and a
malformed flat playlist-entry type that could pass as a track. Each finding was
fixed with focused regression coverage and returned for review. Entry validation
now distinguishes full videos from flat YouTube URLs and rejects nested playlists.
The 500-entry accepted boundary and 501-entry atomic rejection have unit coverage.
The final scoped reviews approved the probe, egress runner and browser test; the
final evidence/documentation review found no Important or critical findings.

The coordinator observed these live extraction results on the development
workstation, without cookies or a personal account:

| Check | Observed result |
| --- | --- |
| Video `aqz-KE-bpKQ` | One descriptor, duration 635 seconds |
| Search `Big Buck Bunny Blender` | Five descriptors with durations |
| Publicly reachable unlisted playlist `PLt5yu3-wZAlQLfIN0MMgp0wVV6MP3bM4_` | Complete one-video enumeration |
| Older yt-dlp test video `BaW_jenozKc` | Unavailable; probe returned nonzero with sanitized error |
| Ten-second sample from `aqz-KE-bpKQ` | 121,173-byte Ogg Opus file; capture completed in 2.22 seconds |
| Packet inspection of that sample | 501 packets, 240 bytes each, 48 kHz stereo; 20 ms interior cadence with Ogg edge trimming |
| Dependency doctor with explicit probe paths | All tools available; qualification still explicitly unverified |

The captured file's SHA-256 is
`c101d5ff7a941f9e376aa81cce9d0d8baed5859c5ade7a319b596d2282fa16c7`.
It remains a private local artifact, not a committed media fixture. The
[probe guide](../../../experiments/bot-providers/README.md) gives installation,
capture and browser commands. This short real playlist does not prove the large
playlist qualification row.

`e2e/media/provider-opus.spec.ts` publishes the captured file through the existing
Go peer, LiveKit SFU and browser SFrame worker, using MLS-derived test-call keys.
The peer's new protected token-file input avoids room tokens in arguments; keys
travel through stdin. The test disables traces that could retain those values.
It checks the expected publisher, microphone track, received RTP, advancing
authenticated-frame count for the expected epoch, decoded samples and nonzero
audio RMS. A provider label alone cannot establish the source of an arbitrary
file: the actual capture and matching hash above supply that provenance here.

| Browser | Authenticated frames at observation | Decoded samples | Audio RMS |
| --- | --- | --- | --- |
| Chromium | 63 | 59,520 | 0.005740 |
| Firefox | 57 | 54,240 | 0.005562 |

All six tests passed: one real-audio test and two subprocess-boundary tests per
browser. The subprocess tests prove bounded output and TERM-to-KILL escalation
with process reaping. Firefox used a temporary private PipeWire null sink with
WirePlumber's policy-only profile; no physical sound device was required.

For the negative control, a separate binary was built using a Go overlay that
removed only `lksdk.WithFrameEncryptor(enc)` in `internal/media/publish.go`.
Tracked production source was unchanged. The same real YouTube artifact reached
both browser receivers as RTP, but both tests failed at the authenticated-frame
assertion with zero frames. This demonstrates that a connected plaintext sender
cannot pass the check. A synthetic fixture had separately passed both browsers
and failed the same negative control; it was recorded as harness evidence only.

This file-based test covers one epoch. It does not establish a production live
provider, continuous key rotation, removal of a listener, independent rooms,
queue recovery, service-host lifecycle or the shipping bot UI. Those acceptance
requirements remain open. In particular, no real Spotify authorization, audio,
complete playlist or restart/reboot test has occurred.

Task 0 remains complete. Task 1 now has real isolated YouTube capture and browser
decryption evidence, but is still unqualified pending the dedicated Spotify
account and intended Linux host/service user. Tasks 2–16 remain unstarted under
the explicit provider gate. No complete bot release, PR or live deployment has
occurred.

Final coordinator checks for this increment:

| Command | Result |
| --- | --- |
| `python3 -m unittest -v test_provider_harness test_youtube_probe test_youtube_sandbox` from the probe directory | 46 passed, including actual namespace checks |
| `go test -race ./cmd/dilla-mediabot` | Passed |
| `npm run typecheck:e2e` | Passed |
| `npm run lint` | Passed (existing client-core/web scope) |
| `npm run check:docs` and `git diff --check` | Passed |

After the final entry-kind fix, the coordinator repeated live video/search/playlist
resolution and sample extraction successfully. The browser test and media peer
were unchanged by that metadata fix. Full production acceptance remains pending.

## Go YouTube adapter live smoke (2026-10-22)

The new Go provider adapter was exercised against the same public video,
`https://www.youtube.com/watch?v=aqz-KE-bpKQ`, on the Linux development
workstation. The pinned tools were yt-dlp 2026.08.19 and yt-dlp-ejs 0.8.0 from
`experiments/bot-providers/.youtube-venv`, Node 24.15.0 from the pinned nvm
installation, Python 3, bubblewrap 0.13.1, and FFmpeg 9.0.2. The dependency
doctor was run with explicit paths for those tools and the built
`dilla-librespot-probe`; each dependency reported available and the doctor
correctly retained `provider qualification: unverified`.

The first Go run failed because the sandbox mounted the resolved system CA bundle
as `/etc/ssl/cert.pem` but set `SSL_CERT_FILE` to
`/etc/ssl/certs/ca-certificates.crt`. On this host that path resolves outside
the mounted certificate directory, so yt-dlp rejected YouTube's TLS certificate.
The bubblewrap command and embedded worker now both direct child tools to the
mounted bundle; a regression test covers both settings.

A bounded temporary Go smoke used `NewMetadataResolver` and `OpusStarter` with
that URL, a 90-second overall deadline, a 30-second PCM duration cap, a 16 MiB
PCM cap, and an eight-sample queue. Resolution returned the expected title
("Big Buck Bunny 60fps 4K - Official Blender Foundation Short Film") and 635 s
duration. The PCM-to-Opus bridge emitted ten 240-byte Opus packets, each marked
20 ms, in 3.19 s. It created no LiveKit publisher or voice call. The temporary
smoke source was removed after completion.

This proves unauthenticated public extraction and local PCM-to-Opus operation
through the Go adapter on this workstation. It does not qualify the intended
Dilla service host/account, the bot lifecycle, browser SFrame decryption, or
continuous playback. The earlier Python probe's Chromium/Firefox evidence is a
separate file-based path and is not evidence that the Go output was played in a
browser.

## User correction: connect Spotify in server settings

The user clarified that the bot belongs on the existing Dilla server and that
Spotify authorization should be part of server settings. The request for manual
prototype authorization as a prerequisite to further development was premature.
The approved plan and design now put the connection flow in Server Settings >
Bot > Music. It will include connection progress, cancellation/errors, readiness,
reconnection and disconnection. Because the account is installation-wide, account
changes require instance-admin authority; community managers receive only the
status needed to configure music.

Task 1 remains open and its real Spotify observations are still required before
release. Missing account authorization no longer blocks Tasks 2–15. The next work
is the shared contracts and application setup flow, using controlled responses
for development tests and keeping real-account qualification explicitly pending.
No Spotify account setup, service deployment or production feature completion is
claimed by this correction. The development probe's fixed loopback redirect must
not be assumed to support automatic callbacks from remote browsers; verify the
shipping redirect mechanism against the registered client's capabilities.
