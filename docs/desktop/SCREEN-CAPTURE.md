# ADR: Isolate desktop video capture from the chat renderer

**Status:** Accepted for implementation; native acceptance remains open.
**Date:** 2026-11-01
**Decider:** Desktop implementation, within the approved sandboxed Electron plan.

## Context

Electron 45 reports modern and legacy desktop capture under the same
`display-capture` permission. Its permission callback does not expose the request
subtype or requested source. Modern `getDisplayMedia` proceeds through the display
request handler, while legacy `getUserMedia` desktop constraints bypass it.

A real sandboxed-renderer regression test reproduced desktop video without a
gesture or picker after a broad permission grant. Window focus and a short grant
lifetime cannot bind that grant to a selected source. The chat renderer must
therefore receive no direct desktop-capture permission.

## Decision

Main selects a real source through Dilla's native picker and, on Wayland, the
existing portal selection. Only then may it create a hidden, sandboxed capture
WebContents in its own temporary Session. This page contains immutable bundled
code, no account or encryption keys, no remote content and no network access.
Only main supplies its selected native source identifier.

The capture page uses Electron's supported legacy capture operation for that
selected video source, with audio disabled. Its Session grants capture only to
that page. The chat Session denies modern and legacy desktop capture, including
legacy system audio. There remains one visible application window per instance.

A main-created MessageChannel passes ports to the capture page and its owning
renderer. Capture frames are copied to bounded RGBA ArrayBuffers and transferred
between DOM ports. Main JavaScript does not receive pixel buffers. The renderer
reconstructs a video track and publishes it through the existing encrypted screen
track. Application audio continues through its separate selected-application
capture and encrypted audio track; there is no system-audio fallback.

Requests, configuration, ownership, frame dimensions and buffer sizes are
validated. Backpressure permits at most two unacknowledged frames. Cancellation,
source ending, renderer navigation/crash, port loss and explicit track stop must
release capture and destroy its hidden WebContents. Failed acquisition remains a
visible failure rather than a broader capture fallback.

## Options considered

| Option | Assessment |
|---|---|
| Grant the chat renderer capture permission | Rejected: the real legacy capture regression bypasses source selection. |
| Renderer override or temporary permission token | Rejected: permission requests lack a browser-derived subtype or source binding. |
| Patch and distribute Electron | Provides a native subtype boundary, but requires maintaining four custom browser builds. |
| Isolated capture page and bounded pixel transfer | Uses stock Electron and keeps selection authority outside the chat renderer; adds copying and lifecycle work. |

Direct MediaStreamTrack transfer is unavailable in the pinned Chromium build.
VideoFrame transfer is restricted to an agent cluster. A local prototype instead
transferred a real 1600×1000 desktop frame as a 6.4 MB ArrayBuffer and decoded the
same checkerboard pixels through a generated video track. Both direct capture
APIs remained denied in the consumer. This proves one-frame transport only.

## Consequences and acceptance

Raw RGBA costs four bytes per pixel per frame. Resolution, frame rate, queue
depth, copying cost and failure cleanup need measurement; no performance advantage
is assumed. Native acceptance on Linux, Windows and both Mac architectures must
exercise actual selection/cancellation, sustained encrypted delivery, hostile
legacy requests, instance isolation and cleanup. The implementation and its IPC
boundary require independent security review before publication.

## Source evidence

- [Pinned Electron permission dispatch](https://github.com/electron/electron/blob/v45.2.1/shell/browser/web_contents_permission_helper.cc)
- [Electron 45 display-capture permission change](https://www.electronjs.org/docs/latest/breaking-changes)
- [Pinned VideoFrame agent-cluster restriction](https://github.com/chromium/chromium/blob/156.0.8078.75/third_party/blink/renderer/modules/webcodecs/video_frame_attachment.h)
