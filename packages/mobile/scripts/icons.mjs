// Renders the iOS and Android app icons and splash screens from assets/*.svg.
// Run with `npm run icons -w @dilla/mobile` after changing an SVG; the PNGs are committed.
import { Resvg } from '@resvg/resvg-js';
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { deflateSync } from 'node:zlib';

const root = fileURLToPath(new URL('..', import.meta.url));
const asset = name => readFileSync(join(root, 'assets', name), 'utf8');
const icon = asset('icon.svg');
const foreground = asset('icon-foreground.svg');
const background = asset('icon-background.svg');

// The glyph spans 584 of 1024 units at scale 0.624762 in icon-foreground.svg.
const FOREGROUND_GLYPH_WIDTH = (584 * 0.624762) / 1024;
const TOP = '#0B6AB3';
const BOTTOM = '#005799';

/** Places a 1024-unit SVG document at (x, y) with size w x h inside another SVG. */
const nest = (svg, x, y, w, h) =>
  svg.replace(/<svg\b[^>]*>/, `<svg x="${x}" y="${y}" width="${w}" height="${h}" viewBox="0 0 1024 1024">`);

const wrap = (w, h, body) =>
  `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${w} ${h}" width="${w}" height="${h}">${body}</svg>`;

const render = (svg, width) => new Resvg(svg, { fitTo: { mode: 'width', value: width }, background: undefined }).render();

const crcTable = Array.from({ length: 256 }, (_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});
const crc32 = bytes => {
  let c = 0xffffffff;
  for (const b of bytes) c = crcTable[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
};
const chunk = (type, data) => {
  const out = Buffer.alloc(12 + data.length);
  out.writeUInt32BE(data.length, 0);
  out.write(type, 4, 'ascii');
  data.copy(out, 8);
  out.writeUInt32BE(crc32(out.subarray(4, 8 + data.length)), 8 + data.length);
  return out;
};

/** Encodes an opaque render as an RGB PNG without an alpha channel (the App Store rejects icons with alpha). */
const opaquePng = image => {
  const { width, height, pixels } = image;
  const raw = Buffer.alloc((width * 3 + 1) * height);
  for (let y = 0; y < height; y++) {
    for (let x = 0; x < width; x++) {
      const i = (y * width + x) * 4;
      if (pixels[i + 3] !== 255) throw new Error(`pixel ${x},${y} is not opaque`);
      pixels.copy(raw, y * (width * 3 + 1) + 1 + x * 3, i, i + 3);
    }
  }
  const header = Buffer.alloc(13);
  header.writeUInt32BE(width, 0);
  header.writeUInt32BE(height, 4);
  header.set([8, 2, 0, 0, 0], 8); // 8-bit, colour type 2 (RGB), no interlace
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', header),
    chunk('IDAT', deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ]);
};

const write = (path, bytes) => {
  const target = join(root, path);
  mkdirSync(dirname(target), { recursive: true });
  writeFileSync(target, bytes);
  console.log(path);
};

/** Gradient background with the glyph centred, glyph width = glyph px. */
const splash = (w, h, glyph) => {
  const size = glyph / FOREGROUND_GLYPH_WIDTH;
  return wrap(
    w,
    h,
    `<defs><linearGradient id="s" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="${TOP}"/><stop offset="1" stop-color="${BOTTOM}"/></linearGradient></defs>` +
      `<rect width="${w}" height="${h}" fill="url(#s)"/>${nest(foreground, (w - size) / 2, (h - size) / 2, size, size)}`,
  );
};

// iOS
const ios = 'ios/App/App/Assets.xcassets';
rmSync(join(root, ios, 'AppIcon.appiconset/AppIcon-512@2x.png'), { force: true });
write(`${ios}/AppIcon.appiconset/AppIcon-1024.png`, opaquePng(render(icon, 1024)));
write(
  `${ios}/AppIcon.appiconset/Contents.json`,
  JSON.stringify(
    { images: [{ filename: 'AppIcon-1024.png', idiom: 'universal', platform: 'ios', size: '1024x1024' }], info: { author: 'xcode', version: 1 } },
    null,
    2,
  ) + '\n',
);
// LaunchScreen.storyboard aspect-fills a 2732 square; on a portrait phone about 1260 px of its width shows.
const iosSplash = opaquePng(render(splash(2732, 2732, 440), 2732));
for (const name of ['splash-2732x2732.png', 'splash-2732x2732-1.png', 'splash-2732x2732-2.png'])
  write(`${ios}/Splash.imageset/${name}`, iosSplash);

// Android
const res = 'android/app/src/main/res';
const densities = { mdpi: 1, hdpi: 1.5, xhdpi: 2, xxhdpi: 3, xxxhdpi: 4 };
const monochrome = foreground.replace('fill="#8CC4F0"', 'fill="#fff"');
const legacySquare = wrap(
  1024,
  1024,
  `<defs><clipPath id="c"><rect x="43" y="43" width="938" height="938" rx="170"/></clipPath></defs><g clip-path="url(#c)">${nest(icon, 43, 43, 938, 938)}</g>`,
);
const legacyRound = wrap(
  1024,
  1024,
  `<defs><clipPath id="c"><circle cx="512" cy="512" r="469"/></clipPath></defs><g clip-path="url(#c)">${nest(icon, 43, 43, 938, 938)}</g>`,
);
for (const [density, scale] of Object.entries(densities)) {
  const legacy = 48 * scale;
  const adaptive = 108 * scale;
  write(`${res}/mipmap-${density}/ic_launcher.png`, render(legacySquare, legacy).asPng());
  write(`${res}/mipmap-${density}/ic_launcher_round.png`, render(legacyRound, legacy).asPng());
  write(`${res}/mipmap-${density}/ic_launcher_foreground.png`, render(foreground, adaptive).asPng());
  write(`${res}/mipmap-${density}/ic_launcher_background.png`, opaquePng(render(background, adaptive)));
  write(`${res}/mipmap-${density}/ic_launcher_monochrome.png`, render(monochrome, adaptive).asPng());
}
// The launch theme stretches drawable/splash over the window; the sizes match Capacitor's template.
const splashes = {
  drawable: [480, 320],
  'drawable-land-mdpi': [480, 320],
  'drawable-land-hdpi': [800, 480],
  'drawable-land-xhdpi': [1280, 720],
  'drawable-land-xxhdpi': [1600, 960],
  'drawable-land-xxxhdpi': [1920, 1280],
  'drawable-port-mdpi': [320, 480],
  'drawable-port-hdpi': [480, 800],
  'drawable-port-xhdpi': [720, 1280],
  'drawable-port-xxhdpi': [960, 1600],
  'drawable-port-xxxhdpi': [1280, 1920],
};
for (const [dir, [w, h]] of Object.entries(splashes))
  write(`${res}/${dir}/splash.png`, opaquePng(render(splash(w, h, Math.round(Math.min(w, h) * 0.35)), w)));
