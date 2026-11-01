# YouTube metadata rejection investigation, 2026-10-31

**Proven cause of the text-search rejection:** pinned yt-dlp returns
`extractor_key: "YoutubeSearch"` for `ytsearch5:Ullared Morgan Tjuklad`.
The previous `parseTracks` validator accepted only `"Youtube"` as a search root,
so it rejected the successful extraction before reading any entries. This was
reproduced with the bot's real sandboxed resolver, both against HEAD's original
validator (`ErrMetadataInvalid`) and the initial diagnostic changes
(`invalid_metadata:extractor`). The real search's first result is precisely the
reported video, `LvH_ft4XR1Q`.

The operator confirmed the production refusals were operation 11
(`OpMusicSearch`), which used the same `Resolve` path as enqueue. The query above
reproduces that path and error; the exact historical autocomplete keystrokes are
not known. Directly resolving the complete video URL succeeds and did not
reproduce an enqueue failure.

The captured search root is `_type: "playlist"`,
`extractor_key: "YoutubeSearch"`, `playlist_count: 5`, with five `_type: "url"`
entries whose `ie_key` is `"Youtube"`. Their durations are all present and valid:
70, 203, 51, 33 and 22 seconds. **Missing duration was not the cause of this
reproduction.** The first flat result's 70-second estimate differs from the
fully extracted video's 69 seconds; selected videos are resolved again before
enqueue.

## Exact search and input probes

All subprocesses used the pinned dependency environment and exact resolver
argument sequence described below. All direct extractions in this table exited
0 with empty stderr.

| Input | NormalizeInput | Before fix, actual sandbox | After fix, actual sandbox |
| --- | --- | --- | --- |
| `Ullared Morgan Tjuklad` | search; `ytsearch5:Ullared Morgan Tjuklad` | HEAD: `metadata_invalid`; initial diagnostic version: `invalid_metadata:extractor` | `Search` and strict `Resolve`: 5 tracks, first duration 70,000 ms |
| `https://www.youtube.com/watch?v=LvH_` | `ErrInvalidInput`; incomplete video ID | rejected before subprocess | `Search` and `Resolve`: rejected before subprocess |
| `https://www.youtube.com/watch?v=LvH_ft4XR1Q&si=shared&feature=share` | video; `https://www.youtube.com/watch?v=LvH_ft4XR1Q` | 1 track, 69,000 ms | `Search` and `Resolve`: 1 track, 69,000 ms |

The direct plain-query extraction returned 5,406 stdout bytes in 0.75 seconds;
the full-share extraction returned 61,965 bytes in 1.96 seconds. The old real
sandbox probe took 3.316 seconds; the initial diagnostic version took 3.048
seconds; the fixed probe calling both methods took 5.746 seconds. No sandbox or
environment failures occurred.

Empty subprocess stderr is expected here: yt-dlp finishes successfully and
returns valid JSON; Go subsequently rejects the root extractor. The metadata
error has no subprocess failure stderr to attach. The new safe field class
identifies this boundary without logging extractor stdout, titles or URLs.

A second pinned `ytsearch5` probe with a unique quoted query returned
`_type: "playlist"`, `extractor_key: "YoutubeSearch"`, `playlist_count: 0`, and
`entries: []`, exit 0, empty stderr (712 stdout bytes). Search therefore accepts
that demonstrated empty-result shape. Strict enqueue expansion still refuses an
empty result.

## Search and enqueue validation profiles

`MetadataResolver.Search(ctx, input)` and `Resolve(ctx, input)` share extraction,
normalization, sandboxing and bounded parsing. Both recognize the real
`YoutubeSearch` root only for normalized text searches, require the playlist
root type, and verify the reported count against the actual entries and the
five-result limit. Explicit playlist inputs retain their existing `YoutubeTab`
validation and 500-item bound.

Only `Search` text-search entries may have missing/null duration, represented as
zero in a suggestion. This is a separate suggestion-profile requirement, tested
with controlled mutations of the real fixture; the captured five search
results all had durations. `Resolve` remains strict for every queue descriptor,
including plain text expanded through search. Calling `Search` with a video or
playlist also retains strict duration validation. Non-YouTube entries, invalid
IDs/titles, private/unavailable entries, live/upcoming/post-live entries, invalid
numeric durations and durations over 24 hours remain rejected.

The service routes operation 11 through the optional `Search` capability;
enqueue continues to call `Resolve`. Neither the protocol nor the wire result
format changes. A suggestion is resolved through its canonical video URL before
enqueue, obtaining complete video metadata.

## Environment and extraction

Host execution in `/home/thim/.cache/dilla-combined`, using:

- `/home/thim/.cache/dilla-tmp/yt-dlp-venv/bin/yt-dlp`: `2026.10.21`;
  installed `yt-dlp-ejs`: `0.8.0` (the versions locked by
  `experiments/bot-providers/youtube-requirements.txt`).
- `/home/thim/.cache/dilla-tmp/youtube-tools/bin/yt-dlp`: also `2026.10.21`.
- Node `/home/thim/.local/share/nvm/v24.15.0/bin/node`: `v24.15.0`;
  host Python `3.14.8`; `/usr/bin/bwrap`.

Direct extraction used exactly the resolver's arguments, including
`--ignore-config --no-plugin-dirs --no-remote-components --flat-playlist`,
configured Node, `--no-warnings --no-progress --color never`,
`--socket-timeout 10 --extractor-retries 1 --playlist-end 501 --dump-single-json`,
then the normalized search expression or canonical watch URL. No cookies, authentication, extra
extractor arguments, or runtime component downloads were added.

A temporary Go test then constructed the real `NewMetadataResolver` with those
paths and called `Resolve` for all five inputs. The temporary test was removed
after recording the results. It reported counts, durations and safe failure
classes only; extractor stdout was never logged.

| Input | Direct extraction | Actual sandboxed resolver |
| --- | --- | --- |
| Reported `LvH_ft4XR1Q` watch URL | exit 0; 61,936 stdout bytes; 0 stderr bytes; 1.86 s | 1 track, 69,000 ms; 2.43 s |
| Ordinary CC video `aqz-KE-bpKQ` | exit 0; 175,686 stdout bytes; 41 stderr bytes; 2.25 s | 1 track, 635,000 ms; 2.32 s |
| `https://youtu.be/aqz-KE-bpKQ?si=shared` | canonical watch URL; exit 0; 175,704 stdout bytes; 41 stderr bytes; 2.06 s | 1 track, 635,000 ms; 2.30 s |
| Music video `9bZkp7q19f0` | exit 0; 642,327 stdout bytes; 2.44 s | 1 track, 252,000 ms; 2.53 s |
| Actual Short `https://www.youtube.com/shorts/etcRgjG4qRo` | canonical watch URL; exit 0; 132,954 stdout bytes; 2.51 s | 1 track, 21,000 ms; 2.80 s |

The ordinary/share direct probes emitted the nonfatal
`WARNING: Unknown codec iamf.001.001.Opus` (41 stderr bytes), despite
`--no-warnings`. Reported/music/actual-Short direct probes had empty stderr.

The actual Short was discovered in the real flat response for
`https://www.youtube.com/@YouTube/shorts`; it is distinct from an additional
19-second ordinary-video probe (`jNQXAC9IVRw`), which also extracted successfully.
The Short tab's flat entries had null durations; extracting the individual Short
returned its 21-second duration. No environment or sandbox failures occurred in
these metadata probes. These observations describe this development host,
not the production container. Metadata success does not prove PCM/media startup.

## Direct-video fixtures and safe diagnostic classes

The direct reported-video response has `_type: "video"`,
`extractor_key: "Youtube"`, a valid 11-character ID, a 48-byte title and 16-byte
uploader, `duration: 69`, `availability: "public"`, `live_status: "not_live"`,
and `is_live: false`. Neither `entries` nor `playlist_count` is present. Its
recorded fixture and the CC-video fixture pass the original HEAD validator.
The additional search fixture preserves the different real root that caused
the operation 11 rejection. Fixtures contain unchanged public field values;
signed media URLs, descriptions and unused response data are omitted.

The earlier duration/live regression tests separately demonstrated that
missing/null duration was accepted as zero and live/upcoming/post-live data
with a positive duration was accepted. Queue validation now refuses these
cases. Search's intentional unknown-duration allowance is limited to its
suggestion profile, after the other entry checks pass.

Metadata validation errors retain `errors.Is(err, ErrMetadataInvalid)` and
carry fixed, content-free classes:
`invalid_metadata:json`, `:type`, `:extractor`, `:playlist_count`, `:id`, `:title`,
`:uploader`, `:availability`, `:live`, `:missing_duration`, `:invalid_duration`,
and `:duration_limit` (each suffix is prefixed with `invalid_metadata`).
`FailureClass` preserves these through wrapping. Existing subprocess stderr
redaction is unchanged. No extractor stdout or raw metadata value is added to
production logs or error strings.

## Verification

- Original HEAD baseline: temporarily restored `youtube.go` and
  `diagnostics.go` from HEAD, ran
  `go test -p 1 ./internal/bot/music/youtube -run
  'TestResolveRecordedPinnedSearch$|TestLocalPinnedSearchProbe' -count=1 -v`,
  and restored all edits in a `finally` block. The recorded real search test
  failed with `ErrMetadataInvalid`; the real sandbox plain-query probe returned
  the same error, while the partial URL was invalid input and the full-share
  video succeeded.
- Initial diagnostic version RED: the real search test and sandbox query
  returned `invalid_metadata:extractor`; tests also failed because the separate
  `Search` capability was absent.
- Search GREEN: `go test -p 1 ./internal/bot/music/youtube -run
  'TestResolveRecordedPinnedSearch$|TestSearch' -count=1` passed after the root
  shape correction and separate validation profiles. Tests cover the real
  fixture, optional search duration versus strict enqueue, empty results,
  malformed root/count/entry data, private/live/duration-limit rejection, and
  exact normalization of all three reported inputs.
- Final package: `go test -p 1 ./internal/bot/music/youtube -count=1` passed
  (`0.608s`), including existing redaction/sandbox tests and all earlier
  direct-video regressions.
- Fixed actual sandbox probe called both `Search` and `Resolve` for all three
  inputs and returned the results above (`5.746s`). Temporary network probe
  tests were removed after recording results; ordinary fixture tests are
  deterministic and require no network.

The earlier direct-video, duration and safe-class regressions also followed
RED-to-GREEN. Those exploratory results are preserved in the table above; they
do not replace the reproduced search-root failure.
