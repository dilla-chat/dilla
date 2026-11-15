#!/usr/bin/env node
// Shoots the reference of the native client's inline video player (parity plan Package 12): Chromium's own
// <video controls> as the web client drew a ready video (MediaPlayer.css `.d-media-player__video`: 480 x 270, the stage
// tile, --r-xl corners), paused at 0:03 of clips/video-shot.webm, at deviceScaleFactor 2, the pointer away.
//   node clients/desktop/parity/references/linux/attachments/video-player/shot.mjs
// writes ../video-player-ready-2x.png. The GPUI side is dilla-desktop's tests/video_player_shot.rs.
import { chromium } from 'playwright';
import { copyFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, '../../../../../../..');
copyFileSync(join(REPO, 'clients/desktop/dilla-desktop/src/screens/attachments/testdata/video-shot.webm'), join(HERE, 'clip.webm'));
const b = await chromium.launch();
const p = await b.newPage({ deviceScaleFactor: 2, viewport: { width: 520, height: 310 } });
await p.goto('file://' + join(HERE, 'page.html'));
await p.waitForFunction(() => document.getElementById('v').readyState >= 2);
await p.evaluate(async () => { const v = document.getElementById('v'); v.currentTime = 3; await new Promise(r => { v.onseeked = r; }); });
// Chromium's loading ring goes after the seek settles.
await p.waitForTimeout(1500);
await p.screenshot({ path: join(HERE, '..', 'video-player-ready-2x.png') });
await b.close();
