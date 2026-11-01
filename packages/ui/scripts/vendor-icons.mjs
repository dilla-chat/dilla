// Generates packages/ui/src/Icon/icons.ts from Lucide SVGs downloaded at a pinned release tag.
//
//   1. Download each icon and the licence at the tag, each into a new, empty directory, e.g.
//      https://raw.githubusercontent.com/lucide-icons/lucide/<tag>/icons/<name>.svg and .../<tag>/LICENSE
//   2. node packages/ui/scripts/vendor-icons.mjs <tag> <LICENSE> packages/ui/src/Icon/icons.ts <name>.svg...
//
// It checks each root's attributes (24 × 24, no fill, currentColor, stroke 2, round caps and joins), keeps only the
// child elements it knows (path, polyline, rect, circle, line, with their geometry attributes) and refuses anything else, records
// each file's SHA-256, and copies Lucide's ISC licence and, for an icon Lucide lists as Feather-derived, Feather's MIT
// notice into the header. It reads the files as data and runs nothing from them.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';

const [tag, licenceFile, out, ...svgs] = process.argv.slice(2);
if (!tag || !licenceFile || !out || svgs.length === 0) { console.error('usage'); process.exit(2); }

const ROOT_WANT = {
  viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': '2',
  'stroke-linecap': 'round', 'stroke-linejoin': 'round',
};
const ALLOWED = { path: ['d'], polyline: ['points'], rect: ['x', 'y', 'width', 'height', 'rx', 'ry'], circle: ['cx', 'cy', 'r'], line: ['x1', 'y1', 'x2', 'y2'] };
const attrs = s => Object.fromEntries([...s.matchAll(/([a-zA-Z][\w-]*)="([^"]*)"/g)].map(m => [m[1], m[2]]));

// Icons Jonas approved from another Lucide release than <tag> (the prototype's icon loader fetches lucide-static 0.460.0, and
// these two glyphs differ from 1.57.0). Pass their SVGs from that package; the header records each one's own version and
// hash, and the note is written above the entry, so a regeneration keeps both geometry and provenance.
const VERSIONS = {
  key: { from: 'lucide-static@0.460.0', note: "Fix round 3: the prototype's key (lucide-static 0.460.0, the version its icon loader fetches): the bow at the lower left." },
  youtube: { from: 'lucide-static@0.460.0', note: "Fix round 3: the prototype's YouTube provider glyph (lucide-static 0.460.0 'youtube', which its icon loader fetches)." },
};

// Other comments kept above an entry.
const NOTES = { monitor: "Task 8c: the share picker's sources." };
const noteOf = n => VERSIONS[n]?.note ?? NOTES[n];

const entries = [];
const hashes = [];
for (const file of svgs) {
  const name = path.basename(file, '.svg');
  const buf = fs.readFileSync(file);
  hashes.push([name, crypto.createHash('sha256').update(buf).digest('hex')]);
  const text = buf.toString('utf8');
  const root = /<svg([\s\S]*?)>/.exec(text);
  if (!root) throw new Error(`${name}: no <svg>`);
  const r = attrs(root[1]);
  for (const [k, v] of Object.entries(ROOT_WANT)) if (r[k] !== v) throw new Error(`${name}: ${k}=${r[k]}, want ${v}`);
  const body = text.slice(root.index + root[0].length, text.lastIndexOf('</svg>'));
  const nodes = [];
  for (const m of body.matchAll(/<([a-z]+)\s([^>]*?)\/>/g)) {
    const tagName = m[1];
    if (!(tagName in ALLOWED)) throw new Error(`${name}: unexpected <${tagName}>`);
    const a = attrs(m[2]);
    // A dot drawn with fill="currentColor" (palette) is kept as the stroked dot the Icon component draws; the fill is dropped.
    if (a.fill === 'currentColor') delete a.fill;
    for (const k of Object.keys(a)) if (!ALLOWED[tagName].includes(k)) throw new Error(`${name}: unexpected ${tagName}@${k}`);
    nodes.push([tagName, a]);
  }
  const leftover = body.replace(/<([a-z]+)\s([^>]*?)\/>/g, '').trim();
  if (leftover !== '') throw new Error(`${name}: unparsed content: ${leftover}`);
  entries.push([name, nodes]);
}

const licence = fs.readFileSync(licenceFile, 'utf8').trim();
const isc = licence.slice(0, licence.indexOf('---')).trim();
const mitStart = licence.indexOf('The MIT License');
const mit = licence.slice(mitStart).trim();
const featherList = licence.slice(licence.indexOf('derived from the Feather project:') + 'derived from the Feather project:'.length, mitStart)
  .split(',').map(s => s.trim()).filter(Boolean);
const fromFeather = entries.map(([n]) => n).filter(n => featherList.includes(n));
const comment = s => s.split('\n').map(l => (l === '' ? ' *' : ` * ${l}`)).join('\n');
const q = s => `'${s.replaceAll('\\', '\\\\').replaceAll("'", "\\'")}'`;

let ts = `/*
 * Vendored icon geometry from Lucide (https://lucide.dev), release tag ${tag}:
 * https://github.com/lucide-icons/lucide/tree/${tag}/icons
 * Generated from the upstream SVG files; only the child elements are kept. Every icon is drawn on a 24 × 24
 * viewBox with fill none, stroke currentColor, stroke width 2 and round caps and joins (the Icon component sets these).
 * Do not edit by hand: re-fetch at a pinned tag and regenerate with packages/ui/scripts/vendor-icons.mjs.
 *
 * SHA-256 of each upstream file at ${tag} (the Signal wave A icons, from smile on, are hashed from the lucide-static@${tag}
 * package files, which carry a licence banner the GitHub files lack; the geometry is the same). An icon marked "exception"
 * comes from another release and is hashed from that one:
${hashes.map(([n, h]) => ` *   icons/${n}.svg  ${h}${n in VERSIONS ? `  (exception: ${VERSIONS[n].from}, not ${tag})` : ''}`).join('\n')}
 *
 * Lucide's licence (LICENSE at ${tag}):
 *
${comment(isc)}
${fromFeather.length > 0 ? ` *
 * ${fromFeather.join(', ')} ${fromFeather.length === 1 ? 'is' : 'are'} derived from the Feather project, under:
 *
${comment(mit)}
` : ''} */

export type IconElement =
  | readonly ['path', { readonly d: string }]
  | readonly ['polyline', { readonly points: string }]
  | readonly ['rect', { readonly x?: string; readonly y?: string; readonly width: string; readonly height: string; readonly rx?: string; readonly ry?: string }]
  | readonly ['circle', { readonly cx: string; readonly cy: string; readonly r: string }]
  | readonly ['line', { readonly x1: string; readonly y1: string; readonly x2: string; readonly y2: string }];

export const ICONS = {
${entries.map(([n, nodes]) => `${noteOf(n) === undefined ? '' : `  // ${noteOf(n)}\n`}  ${q(n)}: [\n${nodes.map(([t, a]) => `    [${q(t)}, { ${Object.entries(a).map(([k, v]) => `${k}: ${q(v)}`).join(', ')} }],`).join('\n')}\n  ],`).join('\n')}
} as const satisfies Record<string, readonly IconElement[]>;

export type IconName = keyof typeof ICONS;
`;
fs.writeFileSync(out, ts);
console.log(`wrote ${entries.length} icons to ${out}`);
