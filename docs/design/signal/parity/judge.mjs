#!/usr/bin/env node
// TypeSafe parity judge: asks Jev the same element questions about the prototype frame and the app (or story) frame of a
// pair, one request per image, and decides match / differs / not-judged in code from the Noul and Choice answers only (the
// truth table below). Box geometry is reported as a lead, never as a difference. Advisory: it sits next to pixel diff and
// the human reviewer, never in place of them. Usage and the rules: README.md, "Judge a pair".
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';
import * as Q from './judge-questions.mjs';

// No static import of shoot.mjs: `shoot.mjs --judge` imports this module while shoot.mjs is still running its top-level
// `await main()`, so a static import back into shoot.mjs is a cycle that waits on itself and Node exits with "unsettled
// top-level await" (exit 13). shoot.mjs is imported dynamically, only by this module's own CLI (judge.test.mjs checks both).
/** Device pixels per CSS px of the captured frames; equal to shoot.mjs SCALE (asserted in judge.test.mjs). */
export const SCALE = 2;
/** Which second side a pair has (the same rule as shoot.mjs sideOf). Pure. */
const sideOf = pair => (pair.app !== undefined ? 'app' : 'story');

export const DEFAULT_KEY_FILE = path.join(process.env.HOME ?? '', '.config/typesafe/api-key');
const isObject = v => typeof v === 'object' && v !== null && !Array.isArray(v);
const ELEMENT_KEYS = new Set(['id', 'name', 'expect', 'criteria', 'compare', 'anchor', 'region', 'state', 'copy', 'tone', 'accepted', 'data', 'edge', 'small']);
/** The facets an element is compared on: a data-dependent element (`data: true`) is presence-only. Pure. */
export const compareOf = el => (el.data === true ? ['presence'] : el.compare ?? COMPARE);
const COMPARE = ['presence', 'position', 'size'];
/** A phone pair: the prototype draws a phone frame whose bezel insets the top and the bottom. Pure. */
export const isPhone = pair => pair.viewport.width < Q.PHONE_MAX_WIDTH;

// ---------------------------------------------------------------------------------------------------------------------
// Data rules: which pairs are never sent, and the shape of a `judge` block.

/**
 * Why a pair must never be sent, or null. A rule in code, not only a list: any step (either side) that types a
 * `{password:…}` or `{recoveryKey:…}`, types into a password, recovery-key or one-time-code field, fills a `totp`, or waits
 * for the recovery-key screen. A partial capture of such a pair can show a typed secret. Pure.
 */
export function secretReason(pair) {
  const steps = [...(pair.prototype?.steps ?? []), ...(pair.app?.steps ?? []), ...(pair.story?.steps ?? [])];
  for (const s of steps) {
    if (!isObject(s)) continue;
    if (s.totp !== undefined) return 'secret: fills a one-time code';
    if (Array.isArray(s.type)) {
      if (/\{(password|recoveryKey):/.test(String(s.type[1]))) return 'secret: types a password or recovery key';
      if (/password|recovery|one-time-code|totp/i.test(String(s.type[0]))) return 'secret: types into a secret field';
    }
    if (typeof s.waitFor === 'string' && /recovery key|one-time-code/i.test(s.waitFor)) return 'secret: waits for a secret entry screen';
  }
  return null;
}

/** Checks one pair's `judge` block (absent is fine). Throws naming the first problem. Pure. */
export function validateJudge(pair) {
  const j = pair.judge;
  if (j === undefined) return;
  const at = `${pair.name}: judge`;
  if (!isObject(j)) throw new Error(`${at} must be an object`);
  for (const k of Object.keys(j)) if (!['exclude', 'elements'].includes(k)) throw new Error(`${at}: unknown key ${k}`);
  if (j.exclude !== undefined && (typeof j.exclude !== 'string' || j.exclude === '')) throw new Error(`${at}.exclude must be a reason string`);
  if (j.elements === undefined) return;
  if (!Array.isArray(j.elements)) throw new Error(`${at}.elements must be an array`);
  const ids = new Set(j.elements.map(e => e?.id));
  const seen = new Set();
  j.elements.forEach((e, i) => {
    const w = `${at}.elements[${i}]`;
    if (!isObject(e)) throw new Error(`${w} must be an object`);
    for (const k of Object.keys(e)) if (!ELEMENT_KEYS.has(k)) throw new Error(`${w}: unknown key ${k}`);
    if (typeof e.id !== 'string' || !/^[a-z][a-z0-9_]*$/.test(e.id)) throw new Error(`${w}: id must be lower_snake`);
    if (seen.has(e.id)) throw new Error(`${w}: duplicate id ${e.id}`);
    seen.add(e.id);
    if (typeof e.name !== 'string' || e.name === '') throw new Error(`${w} (${e.id}): name is required`);
    if (/\bor the\b/i.test(e.name)) throw new Error(`${w} (${e.id}): a name picks out one thing ("the X or the Y" is a spec error)`);
    if (e.expect !== undefined && !['present', 'absent'].includes(e.expect)) throw new Error(`${w} (${e.id}): expect is present or absent`);
    if (e.criteria !== undefined && !(isObject(e.criteria) && typeof e.criteria.true === 'string' && typeof e.criteria.false === 'string')) throw new Error(`${w} (${e.id}): criteria is {true, false} strings`);
    if (e.compare !== undefined && !(Array.isArray(e.compare) && e.compare.length > 0 && e.compare.every(c => COMPARE.includes(c)))) throw new Error(`${w} (${e.id}): compare is a non-empty list of ${COMPARE.join(', ')}`);
    if (e.anchor !== undefined && (e.anchor === e.id || !ids.has(e.anchor))) throw new Error(`${w} (${e.id}): anchor must name another element of the pair`);
    // Element-list rules (judge-questions.mjs, the comment above PHONE_MAX_WIDTH).
    if (e.data !== undefined && typeof e.data !== 'boolean') throw new Error(`${w} (${e.id}): data is true or false`);
    if (e.data === true && e.compare !== undefined && e.compare.some(c => c !== 'presence')) throw new Error(`${w} (${e.id}): a data-dependent element is presence-only (drop compare or use ["presence"])`);
    if (e.small !== undefined && typeof e.small !== 'boolean') throw new Error(`${w} (${e.id}): small is true or false`);
    if (e.small === true && e.region === undefined) throw new Error(`${w} (${e.id}): a small element needs a region (close-up crop)`);
    if (e.edge !== undefined && !['top', 'bottom'].includes(e.edge)) throw new Error(`${w} (${e.id}): edge is top or bottom`);
    if (pair.viewport && isPhone(pair) && (e.expect ?? 'present') === 'present' && compareOf(e).includes('position') && e.anchor === undefined && e.edge === undefined) throw new Error(`${w} (${e.id}): on a phone pair a positioned element needs an anchor or an edge (top or bottom)`);
    if (e.region !== undefined && !(isObject(e.region) && ['x', 'y', 'width', 'height'].every(k => Number.isFinite(e.region[k])) && e.region.width > 0 && e.region.height > 0)) throw new Error(`${w} (${e.id}): region is {x, y, width, height} in CSS px`);
    if (e.state !== undefined) {
      if (!isObject(e.state)) throw new Error(`${w} (${e.id}): state is {facet: {phrase, true, false}}`);
      for (const [f, s] of Object.entries(e.state)) if (!/^[a-z_]+$/.test(f) || !isObject(s) || typeof s.phrase !== 'string') throw new Error(`${w} (${e.id}): state.${f} needs a phrase`);
    }
    if (e.copy !== undefined && typeof e.copy !== 'string') throw new Error(`${w} (${e.id}): copy is a string`);
    if (e.tone !== undefined && typeof e.tone !== 'boolean') throw new Error(`${w} (${e.id}): tone is true or false`);
    if (e.accepted !== undefined && !(Array.isArray(e.accepted) && e.accepted.every(a => isObject(a) && typeof a.facet === 'string' && typeof a.why === 'string' && a.why !== ''))) throw new Error(`${w} (${e.id}): accepted is [{facet, why}]`);
  });
}

// ---------------------------------------------------------------------------------------------------------------------
// Image preparation (sizes are pure; the pixel work runs in Chromium's canvas, which the parity tool already uses).

/** Width and height of a PNG from its IHDR chunk. Pure. */
export function pngSize(buf) {
  if (buf.length < 24 || buf.readUInt32BE(0) !== 0x89504e47 || buf.toString('ascii', 12, 16) !== 'IHDR') throw new Error('not a PNG');
  return { w: buf.readUInt32BE(16), h: buf.readUInt32BE(20) };
}

/**
 * How a w x h source becomes an image the API accepts (each side 280..1568). Too big: scale the longest side down to 1568.
 * Both sides under 280: scale up until the shorter side is 280. One side under 280 (a wide strip): pad that side with the
 * frame's corner colour instead of resampling text a second time. Content is centred; pads are recorded. Pure.
 */
export function planPrepare(w, h, limits = Q.LIMITS) {
  const { minSide, maxSide } = limits;
  let scale = Math.min(1, maxSide / Math.max(w, h));
  if (w * scale < minSide && h * scale < minSide) scale = Math.min(minSide / Math.min(w, h), maxSide / Math.max(w, h));
  const contentW = Math.min(maxSide, Math.max(1, Math.round(w * scale)));
  const contentH = Math.min(maxSide, Math.max(1, Math.round(h * scale)));
  const outW = Math.max(minSide, contentW), outH = Math.max(minSide, contentH);
  return { srcW: w, srcH: h, scaleX: contentW / w, scaleY: contentH / h, contentW, contentH, outW, outH, padX: Math.floor((outW - contentW) / 2), padY: Math.floor((outH - contentH) / 2) };
}

/** True when an image of this size is inside the API's limits. Pure. */
export const withinLimits = (w, h, l = Q.LIMITS) => w >= l.minSide && h >= l.minSide && w <= l.maxSide && h <= l.maxSide && w * h >= l.minPixels && w * h <= l.maxPixels;

/**
 * A Box answer (fractions of the sent image) in CSS px of the captured frame: fraction -> sent px -> minus pad -> / scale ->
 * device px (+ crop origin) -> / deviceScale. `origin` is the crop's top-left in device px (a region request). Pure.
 */
export function boxToCss(box, plan, origin = { x: 0, y: 0 }, deviceScale = SCALE) {
  const x = f => ((f * plan.outW - plan.padX) / plan.scaleX + origin.x) / deviceScale;
  const y = f => ((f * plan.outH - plan.padY) / plan.scaleY + origin.y) / deviceScale;
  return { x0: x(box.left), y0: y(box.top), x1: x(box.right), y1: y(box.bottom) };
}

/** The device-px crop of a CSS region, clamped to the frame. Pure. */
/**
 * One step of a Box edge in CSS px, per axis: Box answers come back in steps of Q.BOX_STEP of the sent image, so a 1568 px
 * wide desktop frame cannot place an edge finer than 12.8 CSS px. The larger of the two sides' steps. Pure.
 */
export function boxQuantum(planP, planA, deviceScale = SCALE) {
  const step = (plan, out, scale) => (Q.BOX_STEP * plan[out]) / plan[scale] / deviceScale;
  return { x: Math.max(step(planP, 'outW', 'scaleX'), step(planA, 'outW', 'scaleX')), y: Math.max(step(planP, 'outH', 'scaleY'), step(planA, 'outH', 'scaleY')) };
}

export function regionCrop(region, w, h, deviceScale = SCALE) {
  const x = Math.max(0, Math.round(region.x * deviceScale)), y = Math.max(0, Math.round(region.y * deviceScale));
  return { x, y, w: Math.min(w - x, Math.round(region.width * deviceScale)), h: Math.min(h - y, Math.round(region.height * deviceScale)) };
}

let browserPromise = null;
async function browser() {
  if (!browserPromise) browserPromise = import('playwright').then(({ chromium }) => chromium.launch({ headless: true }));
  return browserPromise;
}
async function closeBrowser() { if (browserPromise) { const b = await browserPromise; browserPromise = null; await b.close(); } }

/** Prepares one frame (or a crop of it) for sending. Returns { buf, plan, crop, sha256 }. */
export async function prepareImage(file, crop = null) {
  const src = fs.readFileSync(file);
  const { w, h } = pngSize(src);
  const c = crop ?? { x: 0, y: 0, w, h };
  const plan = planPrepare(c.w, c.h);
  const page = await (await browser()).newPage();
  try {
    const b64 = await page.evaluate(async ({ data, c, plan }) => {
      const img = new Image();
      img.src = `data:image/png;base64,${data}`;
      await img.decode();
      const corner = document.createElement('canvas');
      corner.width = 1; corner.height = 1;
      const cg = corner.getContext('2d');
      cg.drawImage(img, c.x, c.y, 1, 1, 0, 0, 1, 1);
      const [r, g, b] = cg.getImageData(0, 0, 1, 1).data;
      const canvas = document.createElement('canvas');
      canvas.width = plan.outW; canvas.height = plan.outH;
      const ctx = canvas.getContext('2d');
      ctx.fillStyle = `rgb(${r},${g},${b})`;
      ctx.fillRect(0, 0, plan.outW, plan.outH);
      ctx.imageSmoothingEnabled = true;
      ctx.imageSmoothingQuality = 'high';
      ctx.drawImage(img, c.x, c.y, c.w, c.h, plan.padX, plan.padY, plan.contentW, plan.contentH);
      return canvas.toDataURL('image/png').split(',')[1];
    }, { data: src.toString('base64'), c, plan });
    const buf = Buffer.from(b64, 'base64');
    const got = pngSize(buf);
    if (got.w !== plan.outW || got.h !== plan.outH || !withinLimits(got.w, got.h)) throw new Error(`prepared ${file} is ${got.w}x${got.h}, outside the limits`);
    return { buf, plan, crop: c, sha256: sha256(buf) };
  } finally { await page.close(); }
}

const sha256 = b => crypto.createHash('sha256').update(b).digest('hex');

// ---------------------------------------------------------------------------------------------------------------------
// Questions and requests.

/** Question IDs (never sent to the model as text; the API returns answers under them). Pure. */
export const qid = {
  present: id => `${id}__present`,
  box: id => `${id}__box`,
  state: (id, f) => `${id}__state__${f}`,
  copy: id => `${id}__copy`,
  tone: id => `${id}__tone`,
};

/** Every question for a set of elements; identical for both images. Speculative fan-out: all asked, code reads what applies. Pure. */
export function buildQuestions(elements) {
  const qs = {};
  for (const el of elements) {
    qs[qid.present(el.id)] = Q.presentQuestion(el);
    if ((el.expect ?? 'present') === 'absent') continue;
    const compare = compareOf(el);
    if (compare.includes('position') || compare.includes('size')) qs[qid.box(el.id)] = Q.boxQuestion(el);
    for (const [f, s] of Object.entries(el.state ?? {})) qs[qid.state(el.id, f)] = Q.stateQuestion(el, s);
    if (el.copy !== undefined) qs[qid.copy(el.id)] = Q.copyQuestion(el);
    if (el.tone === true) qs[qid.tone(el.id)] = Q.toneQuestion(el);
  }
  return qs;
}

/** Elements split into requests: the whole frame, plus one request per distinct `region` ("look closer when needed"). Pure. */
export function requestGroups(elements) {
  const groups = [{ key: 'main', region: null, elements: [] }];
  for (const el of elements) {
    if (!el.region) { groups[0].elements.push(el); continue; }
    const key = `region-${el.region.x}-${el.region.y}-${el.region.width}-${el.region.height}`;
    let g = groups.find(x => x.key === key);
    if (!g) groups.push(g = { key, region: el.region, elements: [] });
    g.elements.push(el);
  }
  return groups.filter(g => g.elements.length > 0);
}

/** The `view` phrase, the same on both sides. Pure. */
export function viewPhrase(pair, region) {
  if (region) return Q.VIEW.region;
  const other = pair.app ?? pair.story ?? {};
  if (pair.prototype?.clip || other.clip) return Q.VIEW.clip;
  return isPhone(pair) ? Q.VIEW.phone : Q.VIEW.desktop;
}

/** The JSON request (the multipart `request` part). The image reference is derived from its bytes, so reruns are identical. Pure. */
export function buildBody(pair, group, imageSha) {
  const ref = `img-${imageSha.slice(0, 24)}`;
  return {
    model: Q.MODEL,
    state: { screen: ref, view: viewPhrase(pair, group.region) },
    questions: buildQuestions(group.elements),
    assets: { [ref]: { type: 'image', format: 'png', part: 'screen-image' } },
  };
}

export const REDACTED_HEADERS = { Authorization: 'Bearer [REDACTED]', 'TypeSafe-Danger-Zone-Images': 'true' };

/** The API key: TYPESAFE_API_KEY_FILE (default ~/.config/typesafe/api-key), else TYPESAFE_API_KEY. Never logged. */
export function loadKey() {
  const file = process.env.TYPESAFE_API_KEY_FILE ?? DEFAULT_KEY_FILE;
  let key = '';
  try { key = fs.readFileSync(file, 'utf8').trim(); } catch { key = (process.env.TYPESAFE_API_KEY ?? '').trim(); }
  if (key === '') throw new Error(`no TypeSafe key: put it in ${file} or TYPESAFE_API_KEY`);
  return key;
}

/** POSTs one multipart request; retries 429/529 with backoff. Returns { status, json, ms }. Errors never carry the key. */
export async function send(body, imageBuf, key, { fetchImpl = fetch, tries = 4 } = {}) {
  for (let attempt = 1; ; attempt += 1) {
    const form = new FormData();
    form.append('request', new Blob([JSON.stringify(body)], { type: 'application/json' }));
    form.append('screen-image', new Blob([imageBuf], { type: 'image/png' }), 'screen.png');
    const t0 = Date.now();
    const res = await fetchImpl(Q.API_URL, { method: 'POST', headers: { Authorization: `Bearer ${key}`, 'TypeSafe-Danger-Zone-Images': 'true' }, body: form });
    const text = await res.text();
    let json = null;
    try { json = JSON.parse(text); } catch { /* not JSON */ }
    if ((res.status === 429 || res.status === 529) && attempt < tries) { await new Promise(r => setTimeout(r, 1000 * 2 ** attempt)); continue; }
    return { status: res.status, json, text: json ? undefined : text.slice(0, 2000), ms: Date.now() - t0 };
  }
}

export const usd = inputTokens => (inputTokens * Q.USD_PER_MTOK) / 1e6;

// ---------------------------------------------------------------------------------------------------------------------
// The truth table (design §1). Everything below is pure.

export const band = p => (typeof p !== 'number' ? 'missing' : p >= Q.HI ? 'yes' : p <= Q.LO ? 'no' : 'band');

/** Presence rows P1-P9, checked in order. Returns { facet, reason, row }. */
export function decidePresence(pP, pA, expect = 'present') {
  const a = band(pP), b = band(pA);
  if (a === 'missing' || b === 'missing') return { facet: 'not-judged', reason: 'no_answer', row: '-' };
  if (expect === 'present') {
    if (a === 'yes' && b === 'yes') return { facet: 'match', reason: null, row: 'P1' };
    if (a === 'yes' && b === 'no') return { facet: 'differs', reason: 'missing_in_app', row: 'P2' };
    if (a === 'no' && b === 'yes') return { facet: 'not-judged', reason: 'spec_mismatch_prototype', row: 'P3' };
    if (a === 'no' && b === 'no') return { facet: 'not-judged', reason: 'not_found_either', row: 'P4' };
  } else {
    if (a === 'no' && b === 'no') return { facet: 'match', reason: null, row: 'P5' };
    if (a === 'no' && b === 'yes') return { facet: 'differs', reason: 'extra_in_app', row: 'P6' };
    if (a === 'yes') return { facet: 'not-judged', reason: 'spec_mismatch_prototype', row: 'P7' };
  }
  if (a === 'band') return { facet: 'not-judged', reason: 'presence_unsettled_prototype', row: 'P8' };
  return { facet: 'not-judged', reason: 'presence_unsettled_app', row: 'P9' };
}

/** State rows S1-S5 (also copy, with its own reasons and the prototype-side spec check). */
export function decideState(sP, sA, facetName) {
  const a = band(sP), b = band(sA);
  if (a === 'missing' || b === 'missing') return { facet: 'not-judged', reason: `no_answer:${facetName}` };
  if (a === 'yes' && b === 'yes') return { facet: 'match', reason: null, row: 'S1' };
  if (a === 'no' && b === 'no') return { facet: 'match', reason: null, row: 'S2' };
  if (a === 'yes' && b === 'no') return { facet: 'differs', reason: `state_lost:${facetName}`, row: 'S3' };
  if (a === 'no' && b === 'yes') return { facet: 'differs', reason: `state_extra:${facetName}`, row: 'S4' };
  return { facet: 'not-judged', reason: `state_unsettled:${facetName}`, row: 'S5' };
}

export function decideCopy(cP, cA) {
  const a = band(cP), b = band(cA);
  if (a === 'missing' || b === 'missing') return { facet: 'not-judged', reason: 'no_answer:copy' };
  if (a === 'no') return { facet: 'not-judged', reason: 'spec_mismatch_prototype' };
  if (a === 'yes' && b === 'yes') return { facet: 'match', reason: null };
  if (a === 'yes' && b === 'no') return { facet: 'differs', reason: 'copy_differs' };
  return { facet: 'not-judged', reason: 'copy_unsettled' };
}

const topOf = ans => {
  if (!ans || !isObject(ans.probabilities)) return null;
  const [name, p] = Object.entries(ans.probabilities).sort((x, y) => y[1] - x[1])[0] ?? [];
  return name === undefined ? null : { name, p };
};

export function decideTone(tP, tA) {
  const a = topOf(tP), b = topOf(tA);
  if (!a || !b) return { facet: 'not-judged', reason: 'no_answer:tone' };
  if (a.p >= Q.HI && b.p >= Q.HI) return a.name === b.name ? { facet: 'match', reason: null, prototype: a.name, app: b.name } : { facet: 'differs', reason: `tone:${a.name}->${b.name}`, prototype: a.name, app: b.name };
  return { facet: 'not-judged', reason: 'tone_unsettled', prototype: a.name, app: b.name };
}

const median = xs => { const s = [...xs].sort((a, b) => a - b); const m = s.length >> 1; return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2; };
const centre = b => ({ x: (b.x0 + b.x1) / 2, y: (b.y0 + b.y1) / 2 });
const round1 = v => Math.round(v * 10) / 10;

/**
 * Classifies Box deltas against their tolerances into a lead level (never a verdict): G1 `none` (all within), G2 `strong`
 * (any over twice the tolerance), G3 `weak` (otherwise). Each delta also reports its size in Box steps.
 */
export function classifyDeltas(deltas) {
  const fmt = d => `${d.kind === 'moved' ? 'moved' : 'resized'} ${d.axis}${d.value > 0 ? '+' : ''}${Math.round(d.value)}px (${(Math.abs(d.value) / (d.step || 1)).toFixed(1)} steps)`;
  const over2 = deltas.filter(d => Math.abs(d.value) > 2 * d.tol);
  if (over2.length) return { level: 'strong', lead: over2.map(fmt).join(', '), row: 'G2' };
  const over = deltas.filter(d => Math.abs(d.value) > d.tol);
  if (over.length === 0) return { level: 'none', lead: null, row: 'G1' };
  return { level: 'weak', lead: over.map(fmt).join(', '), row: 'G3' };
}

/**
 * Geometry leads (§1.2, adjusted 2026-10-30: Box geometry is reported, never decided) for the elements that passed P1.
 * `css` maps element id -> { prototype, app, quantum } in CSS px. Offset: the anchor's centre shift, else the median shift
 * over the P1 elements compared on position in the same `edge` group (phone pairs: the prototype's bezel insets the top
 * and the bottom differently, so `top` and `bottom` elements are aligned separately); a group needs 2. Pure.
 */
export function decideGeometry(elements, p1Ids, css) {
  const shift = id => { const p = centre(css[id].prototype), a = centre(css[id].app); return { x: a.x - p.x, y: a.y - p.y }; };
  const positioned = elements.filter(e => p1Ids.has(e.id) && css[e.id] && compareOf(e).includes('position') && !e.anchor);
  const groupOf = e => e.edge ?? 'all';
  const med = {};
  for (const g of new Set(positioned.map(groupOf))) {
    const members = positioned.filter(e => groupOf(e) === g);
    med[g] = members.length >= 2 ? { x: median(members.map(e => shift(e.id).x)), y: median(members.map(e => shift(e.id).y)) } : null;
  }
  const out = {};
  for (const el of elements) {
    if (!p1Ids.has(el.id) || !css[el.id]) continue;
    const compare = compareOf(el);
    if (!compare.includes('position') && !compare.includes('size')) continue;
    const { prototype: p, app: a } = css[el.id];
    const qx = css[el.id].quantum?.x ?? 0, qy = css[el.id].quantum?.y ?? 0;
    const g = { prototype_css: roundBox(p), app_css: roundBox(a), step_css: { x: round1(qx), y: round1(qy) }, pos_tol_css: { x: round1(Q.posTol(qx)), y: round1(Q.posTol(qy)) } };
    const deltas = [];
    let noAlign = false;
    if (compare.includes('position')) {
      let off = null;
      if (el.anchor) { if (p1Ids.has(el.anchor) && css[el.anchor]) off = shift(el.anchor); }
      else off = med[groupOf(el)] ?? null;
      if (off === null) noAlign = true;
      else {
        const pc = centre(p), ac = centre(a);
        g.offset_css = { x: round1(off.x), y: round1(off.y), from: el.anchor ?? `median:${groupOf(el)}` };
        g.dx_css = round1(ac.x - (pc.x + off.x));
        g.dy_css = round1(ac.y - (pc.y + off.y));
        deltas.push({ kind: 'moved', axis: 'x', value: g.dx_css, tol: Q.posTol(qx), step: qx }, { kind: 'moved', axis: 'y', value: g.dy_css, tol: Q.posTol(qy), step: qy });
      }
    }
    if (compare.includes('size')) {
      const pw = p.x1 - p.x0, ph = p.y1 - p.y0;
      g.dw_css = round1((a.x1 - a.x0) - pw);
      g.dh_css = round1((a.y1 - a.y0) - ph);
      deltas.push({ kind: 'resized', axis: 'w', value: g.dw_css, tol: Q.sizeTol(pw, qx), step: qx }, { kind: 'resized', axis: 'h', value: g.dh_css, tol: Q.sizeTol(ph, qy), step: qy });
    }
    Object.assign(g, deltas.length ? classifyDeltas(deltas) : { level: 'none', lead: null, row: 'G1' });
    if (noAlign) g.note = 'position not aligned (G4: no anchor and fewer than 2 elements in its edge group)';
    out[el.id] = g;
  }
  return out;
}
const roundBox = b => ({ x0: round1(b.x0), y0: round1(b.y0), x1: round1(b.x1), y1: round1(b.y1) });

const RANK = { differs: 3, 'not-judged': 2, accepted: 1, match: 0 };

/** Applies spec-declared accepted deviations: a differing facet listed in `accepted` reports `accepted`. */
function applyAccepted(el, name, f) {
  if (f?.facet !== 'differs') return f;
  const hit = (el.accepted ?? []).find(a => a.facet === name || (name.startsWith('state.') && a.facet === 'state'));
  return hit ? { ...f, facet: 'accepted', why: hit.why } : f;
}

/**
 * The whole decision for one pair: answers per side are { questionId: answer }; css per element from the Box answers.
 * Returns { verdict, elements, counts }. Element: differs if any facet differs, else not-judged if any is, else match
 * (accepted when an accepted deviation applied). Pure.
 */
export function decide(judge, answers, css) {
  const elements = judge.elements;
  const ans = (side, id) => answers[side]?.[id];
  const nouls = (id) => [ans('prototype', id)?.noul, ans('app', id)?.noul];
  const results = elements.map(el => {
    const r = { id: el.id, expect: el.expect ?? 'present' };
    const [pP, pA] = nouls(qid.present(el.id));
    r.presence = { prototype: pP, app: pA, ...decidePresence(pP, pA, r.expect) };
    return r;
  });
  const p1 = new Set(results.filter(r => r.presence.row === 'P1').map(r => r.id));
  const geo = decideGeometry(elements, p1, css);
  elements.forEach((el, i) => {
    const r = results[i];
    const facets = [['presence', r.presence]];
    if (p1.has(el.id)) {
      // Box geometry is a lead for a person or for pixel diff, never a facet of the verdict (probe 2026-10-30: Boxes are
      // quantised to 0.01 of the image, move a step between identical requests and sometimes land on the wrong element).
      if (geo[el.id]) {
        r.geometry = geo[el.id];
        r.geometry.box_confidence = { prototype: ans('prototype', qid.box(el.id))?.confidence ?? null, app: ans('app', qid.box(el.id))?.confidence ?? null };
        if (r.geometry.lead) r.lead = `${r.geometry.level}: ${r.geometry.lead}`;
      }
      r.state = {};
      for (const f of Object.keys(el.state ?? {})) {
        const [sP, sA] = nouls(qid.state(el.id, f));
        r.state[f] = { prototype: sP, app: sA, ...decideState(sP, sA, f) };
        facets.push([`state.${f}`, r.state[f]]);
      }
      if (el.copy !== undefined) {
        const [cP, cA] = nouls(qid.copy(el.id));
        r.copy = { text: el.copy, prototype: cP, app: cA, ...decideCopy(cP, cA) };
        facets.push(['copy', r.copy]);
      }
      if (el.tone === true) {
        r.tone = decideTone(ans('prototype', qid.tone(el.id)), ans('app', qid.tone(el.id)));
        facets.push(['tone', r.tone]);
      }
    }
    let worst = 'match';
    const reasons = [];
    for (const [name, f0] of facets) {
      const f = applyAccepted(el, name, f0);
      if (f !== f0) { if (name === 'presence') r.presence = f; else if (name === 'copy') r.copy = f; else if (name === 'tone') r.tone = f; else r.state[name.slice(6)] = f; }
      if (RANK[f.facet] > RANK[worst]) worst = f.facet;
      if (f.facet !== 'match' && f.reason) reasons.push(f.facet === 'accepted' ? `accepted:${f.reason}` : f.reason);
    }
    r.verdict = worst;
    r.reasons = reasons;
  });
  return { ...pairVerdict(results), elements: results };
}

/** Pair verdict from element verdicts (§1.5 steps 4-6). Pure. */
export function pairVerdict(results) {
  const counts = { match: 0, differs: 0, 'not-judged': 0, accepted: 0 };
  for (const r of results) counts[r.verdict] += 1;
  const mismatch = results.filter(r => r.reasons.includes('spec_mismatch_prototype')).length;
  let verdict;
  if (counts.differs > 0) verdict = 'differs';
  else if (counts['not-judged'] > 0 || mismatch > results.length / 3) verdict = 'not-judged';
  else verdict = counts.accepted > 0 ? `match (accepted: ${counts.accepted})` : 'match';
  return { verdict, counts, spec_mismatch: mismatch };
}

// ---------------------------------------------------------------------------------------------------------------------
// Running a spec.

class BudgetStop extends Error {}

function makeLedger(outDir, budget) {
  const file = path.join(outDir, 'judge-ledger.json');
  let book = { entries: [], total_usd: 0, total_input_tokens: 0 };
  try { book = JSON.parse(fs.readFileSync(file, 'utf8')); } catch { /* new ledger */ }
  let runUsd = 0, runTokens = 0;
  return {
    check() { if (runUsd >= budget) throw new BudgetStop(`budget $${budget} reached ($${runUsd.toFixed(6)} spent this run)`); },
    add(entry) {
      const cost = usd(entry.input_tokens);
      runUsd += cost; runTokens += entry.input_tokens;
      book.entries.push({ at: new Date().toISOString(), ...entry, usd: cost });
      book.total_usd = (book.total_usd ?? 0) + cost;
      book.total_input_tokens = (book.total_input_tokens ?? 0) + entry.input_tokens;
      fs.writeFileSync(file, JSON.stringify(book, null, 2));
    },
    get run() { return { usd: runUsd, input_tokens: runTokens }; },
    get total() { return { usd: book.total_usd, input_tokens: book.total_input_tokens }; },
  };
}

function frameFiles(shotDir, pair) {
  const proto = path.join(shotDir, `${pair.name}.prototype.png`);
  const sides = [sideOf(pair), sideOf(pair) === 'app' ? 'story' : 'app'];
  const side = sides.find(s => fs.existsSync(path.join(shotDir, `${pair.name}.${s}.png`)));
  return { proto, side, other: side ? path.join(shotDir, `${pair.name}.${side}.png`) : null };
}

async function judgePair(pair, { shotDir, outDir, key, ledger, dry, reuse }) {
  const base = { pair: pair.name, model: Q.MODEL, questions_version: Q.QUESTIONS_VERSION, spec_sha256: sha256(Buffer.from(JSON.stringify(pair))) };
  const excluded = secretReason(pair) ?? (pair.judge?.exclude ? `excluded: ${pair.judge.exclude}` : null);
  if (excluded) return { ...base, verdict: 'not-judged:excluded', reason: excluded, usage: { input_tokens: 0, usd: 0 } };
  const elements = pair.judge?.elements ?? [];
  if (elements.length === 0) return { ...base, verdict: 'not-judged:no-elements', reason: 'no judge block', usage: { input_tokens: 0, usd: 0 } };
  const { proto, side, other } = frameFiles(shotDir, pair);
  if (!fs.existsSync(proto) || !other) return { ...base, verdict: 'error', reason: 'frames missing', usage: { input_tokens: 0, usd: 0 } };
  if (/recovery/i.test(JSON.stringify(pair))) console.log(`note ${pair.name}: the spec mentions recovery; the frame shows story fixtures only`);
  const groups = requestGroups(elements);
  const answers = { prototype: {}, app: {} };
  const plans = { prototype: {}, app: {} };
  let tokens = 0;
  const jobs = [];
  for (const [label, file] of [['prototype', proto], ['app', other]]) {
    const { w, h } = pngSize(fs.readFileSync(file));
    for (const g of groups) {
      jobs.push((async () => {
        const crop = g.region ? regionCrop(g.region, w, h) : null;
        const prep = await prepareImage(file, crop);
        const stem = `${pair.name}.${label}${g.region ? `.${g.key}` : ''}`;
        const imgFile = `${stem}.judge.png`;
        fs.writeFileSync(path.join(outDir, imgFile), prep.buf);
        const body = buildBody(pair, g, prep.sha256);
        const log = { url: Q.API_URL, method: 'POST', headers: REDACTED_HEADERS, body, parts: { 'screen-image': { file: imgFile, sha256: prep.sha256, bytes: prep.buf.length, w: prep.plan.outW, h: prep.plan.outH } }, prepare: { source: path.basename(file), side: label === 'app' ? side : 'prototype', crop: prep.crop, ...prep.plan } };
        const reqFile = path.join(outDir, `${pair.name}.request.${label}${g.region ? `.${g.key}` : ''}.json`);
        const resFile = path.join(outDir, `${pair.name}.response.${label}${g.region ? `.${g.key}` : ''}.json`);
        // --reuse: a stored 200 answer to a byte-identical request (same body, same image bytes) is decided again, not resent.
        let res = null;
        if (reuse && fs.existsSync(reqFile) && fs.existsSync(resFile)) {
          const old = JSON.parse(fs.readFileSync(reqFile, 'utf8'));
          const stored = JSON.parse(fs.readFileSync(resFile, 'utf8'));
          if (JSON.stringify(old.body) === JSON.stringify(body) && old.parts?.['screen-image']?.sha256 === prep.sha256 && stored.status === 200) res = { ...stored, reused: true };
        }
        fs.writeFileSync(reqFile, JSON.stringify(log, null, 2));
        if (!res) {
          if (dry) return;
          ledger.check();
          res = await send(body, prep.buf, key);
          fs.writeFileSync(resFile, JSON.stringify(res, null, 2));
        }
        if (res.status !== 200 || !res.json?.answers) throw new Error(`HTTP ${res.status} for ${stem}: ${JSON.stringify(res.json?.detail ?? res.text ?? '').slice(0, 300)}`);
        const it = res.reused ? 0 : res.json.usage?.input_tokens ?? 0;
        tokens += it;
        if (!res.reused) ledger.add({ pair: pair.name, side: label, group: g.key, input_tokens: it, image: `${prep.plan.outW}x${prep.plan.outH}`, questions: Object.keys(body.questions).length });
        Object.assign(answers[label], res.json.answers);
        for (const el of g.elements) plans[label][el.id] = { plan: prep.plan, origin: { x: prep.crop.x, y: prep.crop.y } };
      })());
    }
  }
  const settled = await Promise.allSettled(jobs);
  const failed = settled.find(s => s.status === 'rejected');
  const usage = { input_tokens: tokens, usd: usd(tokens) };
  if (failed) {
    const budget = failed.reason instanceof BudgetStop;
    return { ...base, verdict: budget ? 'not-judged:budget' : 'error', reason: String(failed.reason?.message ?? failed.reason), usage };
  }
  if (dry) return { ...base, verdict: 'not-judged:dry-run', reason: 'requests prepared and logged, not sent', usage };
  const css = {};
  for (const el of elements) {
    const bP = answers.prototype[qid.box(el.id)]?.box, bA = answers.app[qid.box(el.id)]?.box;
    if (bP && bA) css[el.id] = { prototype: boxToCss(bP, plans.prototype[el.id].plan, plans.prototype[el.id].origin), app: boxToCss(bA, plans.app[el.id].plan, plans.app[el.id].origin), quantum: boxQuantum(plans.prototype[el.id].plan, plans.app[el.id].plan) };
  }
  return { ...base, side, ...decide(pair.judge, answers, css), usage };
}

const fmt$ = v => `$${v.toFixed(6)}`;

/**
 * The reviewer's short column: `ok`, `CHECK a, b` (elements that differ), `unsure c` (not judged), plus `leads: d` for
 * strong geometry leads (look, then decide with pixel diff). `skip`, `no list` and `ERROR` for the other pair verdicts. Pure.
 */
export function reviewerLine(v) {
  if (v.verdict === 'error') return 'ERROR';
  if (v.verdict === 'not-judged:excluded') return 'skip (secret)';
  if (v.verdict === 'not-judged:no-elements') return 'no list';
  if (v.verdict.startsWith('not-judged:')) return v.verdict.slice(11);
  const els = v.elements ?? [];
  const ids = verdict => els.filter(e => e.verdict === verdict).map(e => e.id);
  const parts = [];
  if (ids('differs').length) parts.push(`CHECK ${ids('differs').join(', ')}`);
  if (ids('not-judged').length) parts.push(`unsure ${ids('not-judged').join(', ')}`);
  if (parts.length === 0) parts.push(ids('accepted').length ? `ok (accepted ${ids('accepted').join(', ')})` : 'ok');
  const leads = els.filter(e => e.geometry?.level === 'strong').map(e => e.id);
  if (leads.length) parts.push(`leads: ${leads.join(', ')}`);
  return parts.join('; ');
}

export function summaryRows(verdicts) {
  return verdicts.map(v => {
    const c = v.counts;
    const reasons = (v.elements ?? []).filter(e => e.verdict !== 'match').map(e => `${e.id} ${e.reasons.join('; ')}`).join(' · ') || v.reason || '';
    const leads = (v.elements ?? []).filter(e => e.lead).map(e => `${e.id} ${e.lead}`).join(' · ');
    return { pair: v.pair, judge: reviewerLine(v), verdict: v.verdict, emdn: c ? `${c.match}/${c.differs}/${c['not-judged']}${c.accepted ? `/a${c.accepted}` : ''}` : '–', reasons, leads, tokens: v.usage?.input_tokens ?? 0, usd: v.usage?.usd ?? 0 };
  });
}

/** Judges every pair of a spec whose frames are in `shotDir`. Returns { verdicts, exit }. */
export async function judgeSpec(spec, shotDir, opts = {}) {
  const outDir = opts.outDir ?? shotDir;
  const budget = opts.budget ?? Q.DEFAULT_BUDGET_USD;
  fs.mkdirSync(outDir, { recursive: true });
  spec.forEach(validateJudge);
  for (const p of spec) if (secretReason(p) && !p.judge?.exclude) console.log(`warn ${p.name}: the secret rule excludes it but the spec has no judge.exclude marker`);
  const pairs = spec.filter(p => !opts.only || opts.only.includes(p.name));
  const needsKey = pairs.some(p => !secretReason(p) && !p.judge?.exclude && (p.judge?.elements ?? []).length > 0);
  const key = needsKey && !opts.dry ? loadKey() : null;
  const ledger = makeLedger(outDir, budget);
  const verdicts = [];
  try {
    const queue = [...pairs];
    const worker = async () => {
      for (let p = queue.shift(); p; p = queue.shift()) {
        let v;
        try { v = await judgePair(p, { shotDir, outDir, key, ledger, dry: opts.dry, reuse: opts.reuse }); }
        catch (e) { v = { pair: p.name, verdict: e instanceof BudgetStop ? 'not-judged:budget' : 'error', reason: String(e?.message ?? e), usage: { input_tokens: 0, usd: 0 } }; }
        fs.writeFileSync(path.join(outDir, `${p.name}.verdict.json`), JSON.stringify(v, null, 2));
        verdicts.push(v);
        console.log(`${v.verdict.padEnd(22)} ${p.name}`);
      }
    };
    await Promise.all(Array.from({ length: Math.min(4, pairs.length) }, worker));
  } finally { await closeBrowser(); }
  verdicts.sort((a, b) => pairs.findIndex(p => p.name === a.pair) - pairs.findIndex(p => p.name === b.pair));
  const rows = summaryRows(verdicts);
  const uncovered = spec.filter(p => !(p.judge?.elements ?? []).length && !secretReason(p) && !p.judge?.exclude).map(p => p.name);
  const esc = s => s.replace(/\|/g, '\\|');
  const md = ['Advisory only (Nouls decide; geometry is a lead). judge: ok / CHECK (differs) / unsure (not judged) / leads (look with pixel diff).', '',
    '| pair | judge | verdict | elements m/d/n | reasons | leads | tokens | $ |', '|---|---|---|---|---|---|---|---|',
    ...rows.map(r => `| ${r.pair} | ${esc(r.judge)} | ${r.verdict} | ${r.emdn} | ${esc(r.reasons)} | ${esc(r.leads)} | ${r.tokens} | ${fmt$(r.usd)} |`),
    '', `Run spend: ${ledger.run.input_tokens} input tokens, ${fmt$(ledger.run.usd)} (ledger total ${fmt$(ledger.total.usd ?? 0)}). Model ${Q.MODEL}, ${Q.QUESTIONS_VERSION}.`,
    '', `States with no judge block (${uncovered.length}): ${uncovered.join(', ') || 'none'}`].join('\n');
  fs.writeFileSync(path.join(outDir, 'judge-summary.md'), `${md}\n`);
  fs.writeFileSync(path.join(outDir, 'judge-summary.json'), JSON.stringify({ rows, run: ledger.run, uncovered }, null, 2));
  console.log(md);
  const exit = verdicts.some(v => v.verdict === 'error' || v.verdict === 'not-judged:budget') ? 1 : 0;
  return { verdicts, exit };
}

/** Resends a logged request byte for byte (the image is read from the logged file and checked against its sha256). */
export async function replay(logFile) {
  const log = JSON.parse(fs.readFileSync(logFile, 'utf8'));
  const part = log.parts['screen-image'];
  const buf = fs.readFileSync(path.join(path.dirname(logFile), part.file));
  if (sha256(buf) !== part.sha256) throw new Error(`${part.file} changed since it was logged`);
  const res = await send(log.body, buf, loadKey());
  const out = logFile.replace(/\.request\./, '.replay.').replace(/\.json$/, `.${new Date().toISOString().replace(/[:.]/g, '-')}.json`);
  fs.writeFileSync(out, JSON.stringify(res, null, 2));
  console.log(`HTTP ${res.status}  ${res.json?.usage?.input_tokens ?? 0} input tokens (${fmt$(usd(res.json?.usage?.input_tokens ?? 0))})  -> ${out}`);
  return res.status === 200 ? 0 : 1;
}

/** `<spec> <shotDir> [--out dir] [--only a,b] [--budget 0.50]` or `--replay <request log>`. Pure. */
export function parseJudgeArgs(argv) {
  const args = [...argv];
  const take = flag => { const i = args.indexOf(flag); if (i < 0) return undefined; const v = args[i + 1]; args.splice(i, 2); return v; };
  const replayFile = take('--replay');
  const outDir = take('--out');
  const only = take('--only');
  const budget = take('--budget');
  const dry = args.includes('--dry-run'), reuse = args.includes('--reuse');
  const [specFile, shotDir] = args.filter(x => x !== '--dry-run' && x !== '--reuse');
  return { replay: replayFile, specFile, shotDir, outDir, only: only ? only.split(',') : undefined, budget: budget === undefined ? undefined : Number(budget), ...(dry ? { dry: true } : {}), ...(reuse ? { reuse: true } : {}) };
}

export async function main(argv) {
  const a = parseJudgeArgs(argv.slice(2));
  if (a.replay) return replay(a.replay);
  if (!a.specFile || !a.shotDir || (a.budget !== undefined && !(a.budget > 0))) {
    console.error('usage: judge.mjs <spec.json> <shotDir> [--out dir] [--only name,...] [--budget 0.50] [--dry-run] | --replay <name>.request.<side>.json');
    return 2;
  }
  const { validateSpec } = await import('./shoot.mjs');
  const spec = validateSpec(JSON.parse(fs.readFileSync(a.specFile, 'utf8')));
  const { exit } = await judgeSpec(spec, a.shotDir, { outDir: a.outDir, only: a.only, budget: a.budget, dry: a.dry, reuse: a.reuse });
  return exit;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = await main(process.argv);
}
