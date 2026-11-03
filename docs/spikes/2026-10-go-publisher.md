# SP-15 — Go publisher and decryptor smoke

Date: 2026-10-03. Commit: dc02a1d (the rig, the fixtures and this document were uncommitted
files on top of it during the run). Host: dev box, linux/amd64, Go 1.27.0, `GOMAXPROCS=4`.
Pins: `github.com/livekit/livekit-server v1.13.7`,
`github.com/livekit/protocol v1.51.1-0.20260910121219-271d9cde3897`,
`github.com/livekit/server-sdk-go/v2 v2.18.2-0.20260922130803-2088dabd3442`,
`github.com/pion/webrtc/v4 => github.com/livekit/webrtc-pion/v4 v4.2.18-warp.1`.
Rig: `internal/sfu/spike_go_publisher_test.go` (`//go:build spike`), one in-process SFU on
127.0.0.1:7880/7882, Go-SDK participants only.
Reproduce: `go test -tags spike -count=1 -v -run TestSpike ./internal/sfu/` (no `-race`: it inflates
the c-row latencies, and a video publish trips an upstream LiveKit data race).

## Question

Before `internal/media` is written: which server-sdk-go shapes publish a dilla-encrypted Opus,
VP8 and H.264 track and read one back without cgo; what `EncryptFrame` costs under the
`LocalTrack` mutex at 2 Mbps; and whether the SDK's H.264 file reader can feed `dilla-sframe/1`
directly.

## Fixtures (committed, generated once with ffmpeg n9.0.2)

| File | Content | Command |
|---|---|---|
| `internal/media/testdata/tone.ogg` | Opus 48 kHz mono, 64 kbps, 20 ms frames, 30 s of a 1 kHz tone | task 6 step 2 of the plan |
| `internal/media/testdata/bars.ivf` | VP8 640×360, 30 fps, 10 s, ≈300 kbps, GOP 60, no alt-ref | step 3 |
| `internal/media/testdata/bars.h264` | H.264 Constrained Baseline level 3.1, no B-frames, 640×360, 30 fps, 10 s, ≈300 kbps, SPS+PPS before every IDR, GOP 60 | step 4 |

What the files measured on this box:

- `tone.ogg`: 369,738 bytes (libopus VBR on a pure tone runs above the 64 kbps target: ffmpeg
  reported 98.6 kbit/s overall). `ffprobe`:
  `stream|codec_name=opus|profile=unknown|sample_rate=48000|channels=1|r_frame_rate=0/0`.
- `bars.ivf`: 387,675 bytes, 300 frames, 310.1 kbit/s; bytes 8–11 are `VP80`; the header's
  timebase is 1/30 (denominator 30 at bytes 16–19, numerator 1 at bytes 20–23). `ffprobe`:
  `stream|codec_name=vp8|width=640|height=360|r_frame_rate=30/1`.
- `bars.h264`: 425,301 bytes, 340.2 kbit/s, starts `00 00 00 01 67 42 c0 1f`. `ffprobe`:
  `stream|codec_name=h264|profile=Constrained Baseline|width=640|height=360|has_b_frames=0|r_frame_rate=60/1`.
  The `r_frame_rate=60/1` is ffprobe's raw-H.264 reading of the SPS VUI, not the frame rate:
  `trace_headers` shows `timing_info_present_flag=1`, `num_units_in_tick=1`, `time_scale=60`,
  `fixed_frame_rate_flag=1`, which is 60 / (2 × 1) = 30 fps; `ffprobe -count_frames` reads
  `nb_read_frames=300` over the 10 s source. A start-code scan counts 1,511 NAL units:
  5 SPS, 5 PPS, 1 SEI (x264's version string, after the first PPS), 25 IDR slices and 1,475
  non-IDR slices, i.e. 5 IDR pictures every 60 frames, each preceded by SPS+PPS, and every
  picture split into 5 slices (x264 `-tune zerolatency` uses sliced threads).

## Results

| # | Measurement | Criterion | Value | Pass |
|---|---|---|---|---|
| a1 | `EncryptFrame` calls per second, Ogg Opus file track (`SPIKE opus_encrypt_calls_per_second`) | 40–60 (one per 20 ms packet) | 50.0 | PASS |
| a2 | Publication encryption in `ListParticipants` (`SPIKE opus_track_encryption`) | `CUSTOM` | CUSTOM | PASS |
| b1 | VP8 frames read through `e2ee.NewTrackDecryptor` in 5 s (`SPIKE vp8_frames_read`) | ≥ 60 | 107 | PASS |
| b2 | VP8 key frames read (`SPIKE vp8_key_frames_read`) | ≥ 1 | 2 | PASS |
| b3 | Subscriber's view of the publication (`SPIKE vp8_subscriber_sees_encryption`) | `CUSTOM` | CUSTOM | PASS |
| c1 | `WriteSample` p50 at 8,333-byte frames (`SPIKE writesample_p50`) | recorded | 94.64µs | PASS |
| c2 | `WriteSample` p99 (`SPIKE writesample_p99`) | < 5 ms budget; hard limit 33 ms | 187.729µs | PASS |
| c3 | `WriteSample` max (`SPIKE writesample_max`) | recorded | 238.038µs | PASS |
| d1 | H.264 file samples starting with a start code (`SPIKE h264_file_samples_with_start_code`) | recorded; 0 means bare NALs | 0 | PASS |
| d2 | NAL types of the first samples (`SPIKE h264_file_first_nal_types`) | recorded | [7 8 5 5 5 5 5 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1] | PASS |
| e1 | cgo media packages in the spike graph (`SPIKE cgo_media_packages_in_spike_graph`) | 0 | 0 | PASS |

### Raw output (the run the table quotes)

```
=== RUN   TestSpikeAnOggOpusFileTrackEncryptsOncePerPacket
    spike_go_publisher_test.go:195: SPIKE opus_encrypt_calls=199 opus_encrypt_span=3.960852963s opus_encrypt_calls_per_second=50.0
    spike_go_publisher_test.go:212: SPIKE opus_track_encryption=CUSTOM
--- PASS: TestSpikeAnOggOpusFileTrackEncryptsOncePerPacket (4.12s)
=== RUN   TestSpikeASingleLayerVP8PublicationDecryptsInAGoSubscriber
    spike_go_publisher_test.go:267: SPIKE vp8_frames_read=107 vp8_key_frames_read=2 vp8_subscriber_sees_encryption=CUSTOM
--- PASS: TestSpikeASingleLayerVP8PublicationDecryptsInAGoSubscriber (5.18s)
=== RUN   TestSpikeWriteSampleLatencyAtTwoMegabitsWithAEADAndEscaping
    spike_go_publisher_test.go:318: SPIKE writesample_frame_bytes=8333 writesample_p50=94.64µs writesample_p99=187.729µs writesample_max=238.038µs
--- PASS: TestSpikeWriteSampleLatencyAtTwoMegabitsWithAEADAndEscaping (10.41s)
=== RUN   TestSpikeTheH264FileReaderHandsTheEncryptorBareNALs
    spike_go_publisher_test.go:355: SPIKE h264_file_samples=32 h264_file_samples_with_start_code=0 h264_file_first_nal_types=[7 8 5 5 5 5 5 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1]
--- PASS: TestSpikeTheH264FileReaderHandsTheEncryptorBareNALs (3.12s)
=== RUN   TestSpikeTheSpikeBuildPullsNoCgoMediaPackage
    spike_go_publisher_test.go:389: SPIKE cgo_media_packages_in_spike_graph=0
--- PASS: TestSpikeTheSpikeBuildPullsNoCgoMediaPackage (0.08s)
PASS
ok  	github.com/dilla-chat/dilla/internal/sfu	22.916s
```

An earlier run on the same tree also passed all five tests (`ok … 23.074s`), with
`writesample_p50=86.569µs writesample_p99=162.069µs writesample_max=219.329µs` and the same
d and e lines. After a lint-only rename in the rig (the AEAD encryptor's `const clear` became
`clearLen`; golangci-lint's `predeclared` check), a confirming run on the committed file passed
all five (`ok … 22.918s`): `opus_encrypt_calls_per_second=50.0`, `opus_track_encryption=CUSTOM`,
`vp8_frames_read=107 vp8_key_frames_read=2 vp8_subscriber_sees_encryption=CUSTOM`,
`writesample_p50=86.65µs writesample_p99=140.789µs writesample_max=149.099µs`, the same d line
and `cgo_media_packages_in_spike_graph=0`. The table keeps the first saved run's figures, the
highest of the three p99s.

### What the rows mean

- **d1/d2.** The rig keeps the first 32 `EncryptFrame` payloads (`h264_file_samples` is that
  capped count, not the total): none starts with a start code, and only 31 are non-empty. One
  call carried an **empty payload**: the SDK's H.264 reader turns the SEI NAL into a sample with
  `Data = nil` (`readersampleprovider.go:381-393`), and `LocalTrack.WriteSample` hands every
  sample to the encryptor with no empty check (`localtrack.go:527-535`). The types list skips
  empty payloads, so it reads `[7 8 5 …]` although the file's third NAL is the SEI. Each IDR
  slice and each P slice is its own sample (5 per picture).
- **b1/b2.** The first 150 frames of the fixture hold 3 key frames (frames 0, 60, 120); the
  subscriber recognised 2 and read 107 frames in total. The shortfall against 150 was not
  investigated (the criteria are ≥ 1 and ≥ 60).
- **The fixed IVF duration in b is required (measured).** A throwaway variant of b without
  `ReaderTrackWithFrameDuration` (run once, not committed) printed
  `vp8_nodur_encrypt_calls=6 span=4.000134479s frames_read=0`: six frames in 4 s and nothing at
  the subscriber. The cause, read from the pinned source: the webrtc-pion fork's `ivfreader`
  already scales the IVF pts by denominator / numerator (`ivfreader.go:91-92`, ×30 here), and the
  SDK multiplies the delta by numerator / denominator × 1000 ms again
  (`readersampleprovider.go:519-526`), so every frame of a 1/30-timebase file lasts 1 s.

## Decision (consumed by task 7)

1. **Publish shapes.** Ogg Opus and VP8 IVF publish through
   `lksdk.NewLocalFileTrack(path, lksdk.ReaderTrackWithSampleOptions(lksdk.WithFrameEncryptor(enc)))`
   (IVF also with `lksdk.ReaderTrackWithFrameDuration`: the SDK's timestamp-derived duration is
   1 s per frame on a 1/30-timebase IVF, measured above) and `PublishTrack` with
   `TrackPublicationOptions{Encryption: livekit.Encryption_CUSTOM}`, single layer (DEV-40 ruling
   b). H.264 publishes through `lksdk.NewLocalTrack` with `WithFrameEncryptor` and `StartWrite`
   over dilla's own access-unit provider (item 4). a1 and a2 passed, so task 7 proceeds.
2. **Decrypt shape.** `internal/media` runs its own loop over `samplebuilder.New(150, …)` and
   calls `DecryptFrame` per reassembled frame: `TrackDecryptor.ReadSample` returns the first
   decrypt error (`trackdecryptor.go:74-101`, the `fmt.Errorf("decrypt frame: %w", err)` return
   at 79-82) and a rig must count every drop by reason instead. b1–b3 show the SDK's reassembly
   delivers whole VP8 frames to a frame-level hook. Two further points carried from the plan
   review of 2026-10-03 were **not measured by this spike** (no H.264 subscriber and no
   padding counter in the rig): the STAP-A carrying SPS+PPS and the IDR reaching the
   samplebuilder as two samples with one RTP timestamp, and LiveKit's probe padding reaching it
   as zero-filled "frames". Task 7's loop joins samples by RTP timestamp and empties padded
   packets, and its own tests are where those two claims get measured.
3. **`WriteSample` budget.** c2 is 187.729µs, under the 5 ms budget: the stdlib AES-GCM plus
   escaping runs inline in `EncryptFrame`.
4. **H.264 access-unit builder.** Required, d1 is 0: the reader hands `EncryptFrame` one bare
   NAL per sample (SPS and PPS as their own samples, and each of a picture's 5 slices as its own
   sample), so no `dilla-sframe/1` H.264 prefix (which starts with a start code and runs through
   the first VCL NAL's slice header) can be formed. Task 7 writes `h264AUProvider`:
   `00 00 00 01 SPS 00 00 00 01 PPS 00 00 00 01 IDR` for a key frame, `00 00 00 01 slice`
   otherwise, a picture's slices grouped into one access unit, no AUD, filler or SEI, every SPS
   made libwebrtc-canonical, one `Packetize` per access unit. The SDK passes empty samples to
   the encryptor (d1/d2 above), so the provider never emits one and the encryptor must not
   produce a frame from an empty payload.
5. **No cgo.** e1 is the guard task 7 makes permanent in `TestNothingInTheModuleImportsTheCgoMediaPackage`.
   No cgo boundary is needed: every shape above builds without cgo.

## Correction to `docs/spikes/2026-09-livekit.md`

Its "Limits of this spike" said `media-sdk/opus` is "a single file with `#cgo pkg-config: opus`
and no build-tag fallback". It is two files, `opus.go` and `codec.go`, both `//go:build cgo`;
`opus.go` imports `gopkg.in/hraban/opus.v2` and carries `#cgo pkg-config: opus`
(`media-sdk@v0.1.1/opus/opus.go:15,25,35,38`, `codec.go:15`; checked against the module source
downloaded on 2026-10-03, `h1:d0glitmWU5ncIgsW/6wG4Q4iOEcpwINGl85mQjwnKuM=`). The conclusion
stands: the package has no buildable file under `CGO_ENABLED=0`.
