# YouTube provider process sandbox

`PCMStarter` and `NewMetadataResolver` use the package-embedded Python worker,
host Python 3, and bubblewrap. Production images must install Python 3 and
bubblewrap and allow the service account to create user, mount, PID, and network
namespaces. `Bubblewrap` and `Python` paths may be supplied explicitly; blank
values use `PATH` lookup. If a dependency or namespace is unavailable, startup
returns `ErrSandboxUnavailable` and does not fall back to a host process.

Configure absolute executable paths for yt-dlp, FFmpeg, and Node. The sandbox
mounts the read-only system runtime trees, public CA certificates, and only the
configured tool paths outside those trees (or the dedicated `.youtube-venv`
containing yt-dlp). It does not mount `/home`, provider credentials, browser
profiles, or private TLS keys.
The extractor receives a clean environment and runs with argv; no shell is used.

The worker runs in a bubblewrap network namespace without host interfaces. It
can reach its loopback HTTP proxy, which forwards CONNECT requests over a private
Unix socket to the Go broker. The broker permits port 443 only on
`youtube.com`, `youtu.be`, and subdomains of `youtube.com`, `googlevideo.com`,
and `ytimg.com`. It resolves each hostname on the host, rejects the complete
answer set if any address is non-public, then connects to a validated numeric
address to avoid a second DNS lookup. Proxy variables are only for reaching the
broker; the network namespace supplies direct-egress isolation.

PCM output is streamed and capped at 512 MiB by default; callers can lower that
limit. Each CONNECT tunnel is capped at 512 MiB and all tunnels in one process
share a 1 GiB budget. PCM sessions end on context cancellation or after at most
24 hours by default. Metadata runs have a two-minute cap and bounded output.

`OpusStarter` accepts an already-resolved YouTube video track and explicitly
bridges its sandboxed 44.1 kHz stereo PCM into the existing bounded Opus runtime.
It returns the sample source without creating a LiveKit publisher or opening a
voice call; an owning playback lifecycle must make that separate decision.

## Warm resolver and the single ffmpeg (Bot v2 task 18)

`NewBackend` builds a `WarmResolver` and a `StreamStarter`. The resolver keeps one
long-lived worker (`sandbox_worker.py serve`) in the same bubblewrap sandbox, with
the same mounts (yt-dlp and Node) and the same egress broker as the per-request
metadata worker; `TestWarmWorkerSandboxArgvMatchesTheMetadataWorker` pins that.
A lookup is one yt-dlp run that prints the metadata and the chosen audio stream URL;
results are cached for 6 h by normalised query and by video URL. Each lookup has a
timeout; a worker that exceeds it, or exits, is killed and restarted with backoff
(100 ms doubling to 10 s), and it is replaced every 200 lookups so its 1 GiB egress
budget is never reached.

The stream URL is fetched by one ffmpeg in a new sandbox (only ffmpeg is mounted),
through the same broker, and encoded straight to 48 kHz stereo Ogg Opus in 20 ms
frames. The Bot forwards those packets (`encode.BridgeOgg`) with no second ffmpeg.
`Prefetch` starts that ffmpeg for the next track ahead of time, buffering at most
128 KiB until the track's own start claims it. `PCMStarter` and its PCM pipeline
remain for the qualification tests only.

Fix round 1: lookups and pre-buffers are partitioned by owner (`Backend.For(communityID)`;
the cache key and the pre-buffer key include it, one held stream per owner), so one community
never hits or claims another's entry. yt-dlp runs with `--no-cache-dir`, progressive `https`
audio only, and its chosen User-Agent is passed to ffmpeg, which runs with
`-protocol_whitelist https,tls,tcp,httpproxy`. A timed-out lookup is cancelled alone (the worker
kills that yt-dlp's process group); the worker is killed only when it answers nothing, and is
replaced after 200 lookups, 3 failures in a row, half its egress budget, or 1 h.

`FixtureResolver` (`-tags dilla_testhost`, `fixture_testhost.go`) serves `.opus`
files from `DILLA_BOT_MUSIC_FIXTURES` through the same ffmpeg arguments. A build
without the tag refuses to start while the variable is set, and release.yml and
server.yml fail a release whose dillad or dillabot carries the tag.

The tests launch fake extractor and FFmpeg processes in the real local
bubblewrap namespace, confirm direct public-network access fails, check the
broker hostname/IP policy, and verify argument, cancellation, and output bounds.
They do not contact YouTube or qualify live playback. Production deployment
still needs host-level namespace and tool-version qualification.
