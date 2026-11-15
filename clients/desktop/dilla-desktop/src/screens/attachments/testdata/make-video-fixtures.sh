#!/bin/bash
# The inline-video test fixtures (parity plan Package 12), made with FFmpeg 8 (libvpx, libaom, libx264, libx265,
# libtheora, libopus): 2 s of 96x64 at 10 fps whose luma steps by 10 each frame (16 + 100 t), so a decoded picture tells
# which frame it is; a 440 Hz tone where the container carries sound. Run from this directory: bash make-video-fixtures.sh
set -e
cd "$(dirname "$0")"
V=(-f lavfi -i "nullsrc=s=96x64:r=10:d=2,format=yuv420p,geq=lum='16+100*T':cb=128:cr=128")
A=(-f lavfi -i sine=frequency=440:sample_rate=48000:duration=2)
Q=(-nostdin -hide_banner -loglevel error -y -fflags +bitexact -flags:v +bitexact -flags:a +bitexact)
ffmpeg "${Q[@]}" "${V[@]}" "${A[@]}" -c:v libvpx -b:v 80k -g 10 -auto-alt-ref 0 -c:a libopus -b:a 16k video-vp8-opus.webm
ffmpeg "${Q[@]}" "${V[@]}" -c:v libvpx-vp9 -b:v 80k -g 10 video-vp9.webm
ffmpeg "${Q[@]}" "${V[@]}" -c:v libaom-av1 -cpu-used 8 -b:v 80k -g 10 video-av1.webm
ffmpeg "${Q[@]}" "${V[@]}" "${A[@]}" -c:v libx264 -profile:v high -bf 2 -g 10 -c:a aac -b:a 24k -movflags +faststart video-h264-aac.mp4
ffmpeg "${Q[@]}" "${V[@]}" -c:v libaom-av1 -cpu-used 8 -b:v 80k -g 10 video-av1.mp4
ffmpeg "${Q[@]}" "${V[@]}" -c:v libx265 -x265-params log-level=none -g 10 -tag:v hvc1 video-hevc.mp4
ffmpeg "${Q[@]}" "${V[@]}" -c:v libtheora -g 10 video-theora.ogv
# A phone's portrait clip: left half black, right half white, stored landscape with a 90 degree anticlockwise display
# matrix (FFmpeg shows the white half on top).
ffmpeg "${Q[@]}" -f lavfi -i "nullsrc=s=96x64:r=10:d=1,format=yuv420p,geq=lum='if(lt(X,48),16,235)':cb=128:cr=128" \
  -c:v libx264 -bf 0 -g 10 rot-src.mp4
ffmpeg -nostdin -hide_banner -loglevel error -y -display_rotation 90 -i rot-src.mp4 -c copy video-h264-rot90.mp4
rm rot-src.mp4
# The parity shot's clip (tests/video_player_shot.rs): 4 s of FFmpeg's test pattern at 640x360 with a tone.
ffmpeg "${Q[@]}" -f lavfi -i "testsrc2=s=640x360:r=25:d=4" -f lavfi -i "sine=frequency=440:duration=4" \
  -c:v libvpx -b:v 120k -g 25 -c:a libopus -b:a 16k video-shot.webm
