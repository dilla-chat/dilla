# Real yt-dlp metadata fixtures

These JSON files contain unchanged field values projected from real public
YouTube responses captured on 2026-10-31. They are not synthesized yt-dlp
responses. Signed URLs, media formats, thumbnails, captions, descriptions and
other fields unused by the queue validator are omitted. Neither direct-video
response contained `entries` or `playlist_count`.

- `yt-dlp-2026.10.21-aqz-KE-bpKQ.json`: Blender's Big Buck Bunny, with
  `license` reporting Creative Commons Attribution (reuse allowed).
- `yt-dlp-2026.10.21-LvH_ft4XR1Q.json`: the exact video reported as failing.
  Its current direct-video response passes both validators. The production
  refusal was operation 11 search, reproduced by the search fixture below.
- `yt-dlp-2026.10.21-ullared-search.json`: real `ytsearch5:Ullared Morgan Tjuklad`
  output. Its `YoutubeSearch` root is rejected by the original validator even
  though all five entry durations are valid. The first result is the reported
  video `LvH_ft4XR1Q`. The JSON retains only validator fields with unchanged
  values; the input query is documented here rather than duplicated in JSON.

The installed environment matched `experiments/bot-providers/youtube-requirements.txt`:
`yt-dlp 2026.10.21`, `yt-dlp-ejs 0.8.0`, with Node `v24.15.0`.
The extraction command used the resolver's exact argument sequence, substituting
only configured executable paths and the canonical video URL:

```sh
/home/thim/.cache/dilla-tmp/yt-dlp-venv/bin/yt-dlp \
  --ignore-config --no-plugin-dirs --no-remote-components --flat-playlist \
  --js-runtimes node:/home/thim/.local/share/nvm/v24.15.0/bin/node \
  --no-warnings --no-progress --color never --socket-timeout 10 \
  --extractor-retries 1 --playlist-end 501 --dump-single-json \
  'https://www.youtube.com/watch?v=aqz-KE-bpKQ'
```

For the reported-video fixture, only the final video ID changed to
`LvH_ft4XR1Q`. For the search fixture, the final argument was exactly
`ytsearch5:Ullared Morgan Tjuklad` (exit 0; 5,406 stdout bytes; empty stderr).
Both direct-video extractions returned exit status 0. The reported video had
empty stderr;
Big Buck Bunny emitted the nonfatal `Unknown codec iamf.001.001.Opus` warning
(41 bytes) despite `--no-warnings`. Public-video metadata is
volatile; see [the probe report](../../../../../docs/bot-youtube-metadata-2026-10-31.md)
for sandbox outcomes and the limits of this evidence.
