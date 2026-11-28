#!/usr/bin/env node
// Every screenshot in docs/user/screenshots is linked from a doc, and made (manifest `shots`) or declared by hand
// (manifest `manual`); every doc image link resolves to a file by its exact name.
import { existsSync, readdirSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const SHOTS = 'docs/user/screenshots';

function docFiles(root) {
  const docs = [];
  if (existsSync(path.join(root, 'README.md'))) docs.push('README.md');
  const dir = path.join(root, 'docs/user');
  if (existsSync(dir)) {
    for (const f of readdirSync(dir).sort()) if (f.endsWith('.md')) docs.push(`docs/user/${f}`);
  }
  return docs;
}

function imageLinks(text) {
  const links = [];
  for (const m of text.matchAll(/\]\(\s*<?([^)\s>]*screenshots\/[^)\s>]*?\.png)>?(?:\s+"[^"]*")?\s*\)/gi)) links.push(m[1]);
  for (const m of text.matchAll(/<img\b[^>]*?\bsrc\s*=\s*["']([^"']*screenshots\/[^"']*?\.png)["']/gi)) links.push(m[1]);
  return links;
}

export function checkDocsShots(root) {
  const problems = [];
  const shotsDir = path.join(root, SHOTS);
  const files = existsSync(shotsDir) ? readdirSync(shotsDir).sort() : [];
  const pngs = files.filter((f) => f.endsWith('.png'));
  for (const f of files) {
    if (!f.endsWith('.png') && f !== 'manifest.json') problems.push(`${SHOTS}/${f}: not a .png (only images and manifest.json belong here)`);
  }

  let manifest = { shots: {}, manual: {} };
  const manifestPath = path.join(shotsDir, 'manifest.json');
  if (existsSync(manifestPath)) {
    try {
      const m = JSON.parse(readFileSync(manifestPath, 'utf8'));
      manifest = { shots: m.shots ?? {}, manual: m.manual ?? {} };
    } catch (e) {
      problems.push(`${SHOTS}/manifest.json: not valid JSON (${e.message})`);
    }
  } else {
    problems.push(`${SHOTS}/manifest.json: missing`);
  }

  const linked = new Set();
  for (const doc of docFiles(root)) {
    const text = readFileSync(path.join(root, doc), 'utf8');
    for (const link of imageLinks(text)) {
      const target = path.posix.normalize(path.posix.join(path.posix.dirname(doc), link.split(/[?#]/)[0]));
      const dir = path.posix.dirname(target);
      const name = path.posix.basename(target);
      if (dir === SHOTS && pngs.includes(name)) linked.add(name);
      else problems.push(`${doc}: missing image ${link}`);
    }
  }

  for (const f of pngs) {
    if (!linked.has(f)) problems.push(`${SHOTS}/${f}: unlinked (no doc shows it)`);
    if (!(f in manifest.shots) && !(f in manifest.manual)) {
      problems.push(`${SHOTS}/${f}: no manifest entry (shots or manual)`);
    }
  }
  for (const name of Object.keys(manifest.shots)) {
    if (!pngs.includes(name)) problems.push(`${SHOTS}/${name}: in the manifest but not shot (run npm run docs:shots:desktop)`);
  }
  for (const name of Object.keys(manifest.manual)) {
    if (name in manifest.shots) problems.push(`${SHOTS}/${name}: in both shots and manual`);
  }
  return problems;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
  const problems = checkDocsShots(root);
  for (const p of problems) console.error(p);
  if (problems.length) process.exit(1);
  console.log('docs screenshots ok');
}
