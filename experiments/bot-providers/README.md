# Development-only YouTube provider qualification

The Python harness checks YouTube prerequisites and synthetic Opus encoding.
The YouTube probe exercises bounded metadata extraction and audio capture under
network and process isolation. Local checks do not replace intended-host playback
and encrypted client acceptance.

## Local checks

From this directory, with Python 3.10+ and FFmpeg:

```sh
python3 -m unittest -v
python3 provider_harness.py doctor
python3 provider_harness.py opus-smoke
```

`doctor` checks yt-dlp, ffmpeg, ffprobe, and a JavaScript runtime (Deno, Node or Bun).
The YouTube probe specifically uses Node. The checker prints sanitized version
tokens and exits nonzero for missing prerequisites. Executable overrides are
`--yt-dlp-bin`, `--ffmpeg-bin`, `--ffprobe-bin`, and `--js-runtime-bin`.

`opus-smoke` encodes a synthetic tone with libopus: 48 kHz stereo, 20 ms frames,
96 kbps CBR, DTX disabled. It checks stream metadata and packet sizes with ffprobe.
The Python process runner bounds output/time and cleans up its process group.
This proves local encoding only.

## YouTube development probe

Install the hash-pinned candidate dependencies into the probe's dedicated virtual
environment. They are separate from Dilla's application dependencies:

```sh
python3 -m venv .youtube-venv
.youtube-venv/bin/python -m pip install --require-hashes --only-binary=:all: --no-deps -r youtube-requirements.txt
python3 -m unittest -v test_provider_harness test_youtube_probe test_youtube_sandbox
python3 provider_harness.py doctor --yt-dlp-bin .youtube-venv/bin/yt-dlp
```

The pinned pair is yt-dlp 2026.10.21 and yt-dlp-ejs 0.8.0. The wheel hashes are
in `youtube-requirements.txt`; yt-dlp 2026.10.21's upstream project metadata pins
that EJS version. A direct test with 2026.10.21 succeeded after 2025.09.26 failed
in the integrated Bot path. That is dependency evidence only, not full provider or
cross-server qualification. The probe disables
remote component downloads and explicitly selects Node. Its packaged EJS setup
follows the [upstream instructions](https://github.com/yt-dlp/yt-dlp/wiki/EJS).
Use Node 22 or newer on PATH. The sandbox mounts that single executable at
`/usr/bin/node`; the host's `/usr/bin/node` may be a different version. The local
checks used Node 24.15.0 and FFmpeg/ffprobe n9.0.2. This set successfully resolved
a video, five search results and a one-video playlist, and captured a real
ten-second audio sample. See the
[qualification record](../../docs/superpowers/plans/2026-10-21-dilla-bot-qualification.md)
for evidence and remaining checks.

The Linux runner requires bubblewrap and working unprivileged namespaces. It gives
the extractor a private filesystem and network namespace. HTTPS goes through a
bounded CONNECT broker that checks allowed YouTube hosts and all resolved IP
addresses, then connects to the validated numeric address. Direct network access,
private addresses, personal configuration, browser cookies and arbitrary plugins
are unavailable. A sandbox failure stops the operation. There is no unrestricted
fallback. Process, output and resource limits apply to these development probes;
production service packaging remains a later task.

```sh
python3 youtube_probe.py metadata 'instrumental study music'
python3 youtube_probe.py metadata 'https://www.youtube.com/watch?v=aqz-KE-bpKQ'
```

Metadata output contains bounded video descriptors. Playlist validation completes
before anything is printed; incomplete or oversized collections fail without
returning a partial list. Flat enumeration avoids downloading every item's format
list. A descriptor may omit an unknown duration; playback resolves the selected
video separately. Large real playlists still need qualification. Diagnostics omit
provider output and signed media URLs.

`sample INPUT` writes up to ten seconds of Ogg Opus to stdout and refuses a
terminal. Redirect it to a private file for browser testing. The sample uses
48 kHz stereo, 20 ms frames, 96 kbps CBR and disabled DTX. Failed extraction
returns nonzero; an empty or failed artifact is not playback evidence.

For example, create a new private artifact without overwriting an existing file:

```sh
umask 077
set -o noclobber
python3 youtube_probe.py sample 'https://www.youtube.com/watch?v=aqz-KE-bpKQ' > /absolute/private/path/youtube-sample.ogg
```

Use the file only after the command exits successfully. The probe buffers the
bounded result before writing it; failed extraction can leave an empty redirected
file.

## Browser audio verification

`e2e/media/provider-opus.spec.ts` uses the existing Go media peer, MLS test driver,
LiveKit SFU and browser encryption worker. It requires authenticated frames for
the expected publisher, microphone track and epoch, decoded samples and nonzero
rendered audio. Its source is an explicit Ogg file, not a live provider pipe.

Build the existing media prerequisites from the repository root:

```sh
cargo build -p dilla-testkit --release --locked
npm run build:wasm -w @dilla/media
go build -o target/dilla-mediabot ./cmd/dilla-mediabot
```

The generated WASI fixture must also exist at
`internal/mlswasi/testdata/dilla_core_wasi.wasm`, as described in the baseline
record. Install the repository's Playwright Chromium and Firefox builds. Firefox
needs a working audio backend; the existing CI setup is
`scripts/ci-audio-server.sh`. A private null sink suffices for this check.

Set `DILLA_PROVIDER_OPUS_FILE` to the absolute path of the captured file and
`DILLA_PROVIDER_NAME` to `youtube`, then run:

```sh
npx playwright test --config e2e/playwright.media.config.ts --project=chromium-media --project=firefox-media provider-opus.spec.ts
```

Use `DILLA_PROVIDER_NAME=synthetic` for harness checks. Synthetic results are
labeled `harness-smoke-only`; missing inputs skip rather than qualify a provider.
The provider label is operator-supplied provenance, not independent proof of the
file's origin. Preserve the actual capture result with the file hash. The browser
test suppresses traces containing call credentials and passes the room token via
a private file and the SFrame key via stdin.

The qualification record distinguishes synthetic browser checks from real source
playback and records the encryption-removal negative control. This file-based
check does not qualify live queues, provider control, rekeying, restart or the
shipping bot UI.
