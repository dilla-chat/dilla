import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  secretReason, validateJudge, planPrepare, withinLimits, boxToCss, regionCrop, buildQuestions, requestGroups, buildBody,
  decidePresence, decideState, decideCopy, decideTone, decideGeometry, classifyDeltas, decide, pairVerdict, parseJudgeArgs,
  pngSize, send, REDACTED_HEADERS, qid, boxQuantum, reviewerLine, compareOf, SCALE,
} from './judge.mjs';
import { POS_TOL, MODEL, posTol, sizeTol } from './judge-questions.mjs';
import { parseArgs, SCALE as SHOOT_SCALE } from './shoot.mjs';

// No test asserts a model probability: the answers below are synthetic, to exercise the code's truth table.

test('presence rows P1-P9', () => {
  assert.equal(decidePresence(0.9, 0.9).row, 'P1');
  assert.deepEqual([decidePresence(0.9, 0.1).facet, decidePresence(0.9, 0.1).reason], ['differs', 'missing_in_app']);
  assert.deepEqual([decidePresence(0.1, 0.9).facet, decidePresence(0.1, 0.9).reason], ['not-judged', 'spec_mismatch_prototype']);
  assert.deepEqual([decidePresence(0.1, 0.1).facet, decidePresence(0.1, 0.1).reason], ['not-judged', 'not_found_either']);
  assert.deepEqual([decidePresence(0.1, 0.1, 'absent').facet, decidePresence(0.1, 0.1, 'absent').row], ['match', 'P5']);
  assert.deepEqual([decidePresence(0.1, 0.9, 'absent').facet, decidePresence(0.1, 0.9, 'absent').reason], ['differs', 'extra_in_app']);
  assert.equal(decidePresence(0.9, 0.5, 'absent').row, 'P7');
  assert.equal(decidePresence(0.5, 0.9).reason, 'presence_unsettled_prototype');
  assert.equal(decidePresence(0.5, 0.9, 'absent').reason, 'presence_unsettled_prototype');
  assert.equal(decidePresence(0.9, 0.5).reason, 'presence_unsettled_app');
  assert.equal(decidePresence(0.1, 0.5, 'absent').reason, 'presence_unsettled_app');
  // The thresholds are inclusive: exactly 0.7 reads yes and exactly 0.3 reads no.
  assert.equal(decidePresence(0.7, 0.7).row, 'P1');
  assert.equal(decidePresence(0.7, 0.3).row, 'P2');
  // A missing answer is never a "no".
  assert.equal(decidePresence(undefined, 0.1).reason, 'no_answer');
});

test('state rows S1-S5 and copy', () => {
  assert.equal(decideState(0.9, 0.8, 'selected').row, 'S1');
  assert.equal(decideState(0.1, 0.2, 'selected').row, 'S2');
  assert.equal(decideState(0.9, 0.1, 'selected').reason, 'state_lost:selected');
  assert.equal(decideState(0.1, 0.9, 'focused').reason, 'state_extra:focused');
  assert.equal(decideState(0.5, 0.9, 'focused').reason, 'state_unsettled:focused');
  assert.equal(decideState(0.9, 0.5, 'focused').facet, 'not-judged');
  assert.equal(decideCopy(0.9, 0.9).facet, 'match');
  assert.equal(decideCopy(0.9, 0.1).reason, 'copy_differs');
  assert.equal(decideCopy(0.1, 0.9).reason, 'spec_mismatch_prototype');
  assert.equal(decideCopy(0.9, 0.5).reason, 'copy_unsettled');
});

test('tone: both sure and equal match, both sure and different differ, else not judged', () => {
  const t = (name, p) => ({ type: 'choice', choice: name, confidence: p, probabilities: { muted_grey_text: name === 'muted_grey_text' ? p : 1 - p, bright_white_text: name === 'bright_white_text' ? p : 1 - p } });
  assert.equal(decideTone(t('muted_grey_text', 0.9), t('muted_grey_text', 0.8)).facet, 'match');
  assert.equal(decideTone(t('muted_grey_text', 0.9), t('bright_white_text', 0.8)).facet, 'differs');
  assert.equal(decideTone(t('muted_grey_text', 0.9), t('bright_white_text', 0.6)).facet, 'not-judged');
});

test('geometry is a lead (G1 none, G2 strong, G3 weak, G4 no alignment), in Box steps, aligned per edge group', () => {
  assert.equal(classifyDeltas([{ kind: 'moved', axis: 'y', value: POS_TOL, tol: POS_TOL, step: 3 }]).level, 'none');
  assert.equal(classifyDeltas([{ kind: 'moved', axis: 'y', value: POS_TOL + 1, tol: POS_TOL, step: 3 }]).level, 'weak');
  const strong = classifyDeltas([{ kind: 'moved', axis: 'y', value: -(2 * POS_TOL + 1), tol: POS_TOL, step: 8 }]);
  assert.equal(strong.level, 'strong');
  assert.equal(strong.lead, 'moved y-13px (1.6 steps)');
  const b = (x, y, w, h) => ({ x0: x, y0: y, x1: x + w, y1: y + h });
  const els = [{ id: 'a' }, { id: 'b' }, { id: 'c' }, { id: 'd', anchor: 'c' }, { id: 'e', compare: ['presence', 'size'] }, { id: 'f', data: true }];
  // App frame is shifted 17 px down: a, b, c follow it, d moved 30 px further but its anchor c did not.
  const css = {
    a: { prototype: b(10, 10, 100, 20), app: b(10, 27, 100, 20) },
    b: { prototype: b(10, 50, 100, 20), app: b(10, 67, 100, 20) },
    c: { prototype: b(10, 90, 100, 20), app: b(10, 107, 100, 20) },
    d: { prototype: b(10, 130, 100, 20), app: b(10, 177, 100, 20) },
    e: { prototype: b(10, 200, 100, 20), app: b(300, 400, 100, 60) },
    f: { prototype: b(10, 300, 100, 20), app: b(10, 500, 100, 200) },
  };
  const g = decideGeometry(els, new Set(['a', 'b', 'c', 'd', 'e', 'f']), css);
  assert.equal(g.a.row, 'G1');
  assert.equal(g.a.offset_css.y, 17);
  assert.equal(g.d.level, 'strong');
  assert.equal(g.d.dy_css, 30);
  assert.equal(g.e.level, 'strong'); // size only: 40 px taller; its position is not compared
  assert.equal(g.e.dx_css, undefined);
  assert.equal(g.f, undefined); // data-dependent: presence-only, no lead
  const lone = decideGeometry([{ id: 'a' }], new Set(['a']), { a: css.a });
  assert.match(lone.a.note, /G4/);
  // Phone bezel: the top group moved down 17, the bottom group up 17; aligned separately, all four are G1.
  const phone = [{ id: 't1', edge: 'top' }, { id: 't2', edge: 'top' }, { id: 'b1', edge: 'bottom' }, { id: 'b2', edge: 'bottom' }];
  const pcss = { t1: { prototype: b(0, 0, 50, 20), app: b(0, 17, 50, 20) }, t2: { prototype: b(0, 40, 50, 20), app: b(0, 57, 50, 20) },
    b1: { prototype: b(0, 700, 50, 20), app: b(0, 683, 50, 20) }, b2: { prototype: b(0, 760, 50, 20), app: b(0, 743, 50, 20) } };
  const pg = decideGeometry(phone, new Set(['t1', 't2', 'b1', 'b2']), pcss);
  assert.deepEqual(['t1', 't2', 'b1', 'b2'].map(id => pg[id].row), ['G1', 'G1', 'G1', 'G1']);
  assert.equal(pg.b1.offset_css.from, 'median:bottom');
});

test('geometry never decides: a strong lead leaves the element verdict to its Nouls', () => {
  const judge = { elements: [{ id: 'a', name: 'a' }, { id: 'b', name: 'b' }, { id: 'c', name: 'c' }] };
  const n = v => ({ type: 'noul', noul: v });
  const answers = { prototype: { a__present: n(0.9), b__present: n(0.9), c__present: n(0.9) }, app: { a__present: n(0.9), b__present: n(0.9), c__present: n(0.9) } };
  const b = (x, y, w, h) => ({ x0: x, y0: y, x1: x + w, y1: y + h });
  const css = { a: { prototype: b(0, 0, 50, 20), app: b(0, 0, 50, 20) }, b: { prototype: b(0, 100, 50, 20), app: b(0, 100, 50, 20) }, c: { prototype: b(0, 200, 50, 20), app: b(0, 400, 300, 20) } };
  const v = decide(judge, answers, css);
  const c = v.elements.find(e => e.id === 'c');
  assert.equal(c.verdict, 'match');
  assert.match(c.lead, /^strong: moved x\+125px .*moved y\+200px/);
  assert.equal(v.verdict, 'match');
  assert.equal(reviewerLine({ ...v, pair: 'p' }), 'ok; leads: c');
});

test('reviewerLine: the short judge column', () => {
  const e = (id, verdict) => ({ id, verdict, reasons: [] });
  assert.equal(reviewerLine({ verdict: 'differs', elements: [e('a', 'differs'), e('b', 'not-judged'), e('c', 'match')] }), 'CHECK a; unsure b');
  assert.equal(reviewerLine({ verdict: 'match (accepted: 1)', elements: [e('a', 'accepted')] }), 'ok (accepted a)');
  assert.equal(reviewerLine({ verdict: 'not-judged:excluded' }), 'skip (secret)');
  assert.equal(reviewerLine({ verdict: 'not-judged:no-elements' }), 'no list');
  assert.equal(reviewerLine({ verdict: 'error' }), 'ERROR');
});

test('element-list rules: data is presence-only, small needs a region, phone positions need an anchor or an edge', () => {
  const desk = els => ({ name: 'p', viewport: { width: 1280, height: 800 }, judge: { elements: els } });
  const phone = els => ({ name: 'p', viewport: { width: 390, height: 844 }, judge: { elements: els } });
  validateJudge(desk([{ id: 'a', name: 'x', data: true }]));
  assert.throws(() => validateJudge(desk([{ id: 'a', name: 'x', data: true, compare: ['presence', 'size'] }])), /presence-only/);
  assert.throws(() => validateJudge(desk([{ id: 'a', name: 'x', small: true }])), /region/);
  validateJudge(desk([{ id: 'a', name: 'x', small: true, compare: ['presence'], region: { x: 0, y: 0, width: 200, height: 200 } }]));
  assert.throws(() => validateJudge(phone([{ id: 'a', name: 'x' }])), /anchor or an edge/);
  validateJudge(phone([{ id: 'a', name: 'x', edge: 'top' }, { id: 'b', name: 'y', anchor: 'a' }, { id: 'c', name: 'z', compare: ['presence', 'size'] }, { id: 'd', name: 'w', expect: 'absent' }]));
  assert.throws(() => validateJudge(phone([{ id: 'a', name: 'x', edge: 'middle' }])), /edge/);
  assert.deepEqual(compareOf({ data: true, compare: ['presence'] }), ['presence']);
  assert.deepEqual(Object.keys(buildQuestions([{ id: 'a', name: 'x', data: true }])), ['a__present']);
});

test('decide: element and pair verdicts, speculative answers ignored, accepted deviations', () => {
  const judge = { elements: [
    { id: 'title', name: 'the title', copy: 'Hi', compare: ['presence'] },
    { id: 'row', name: 'the first row', compare: ['presence'], state: { selected: { phrase: 'selected' } }, accepted: [{ facet: 'state', why: 'Jonas, test' }] },
    { id: 'gone', name: 'a loading line', expect: 'absent' },
    { id: 'icon', name: 'the icon', compare: ['presence'], copy: 'x' },
  ] };
  const n = v => ({ type: 'noul', noul: v });
  const answers = {
    prototype: { [qid.present('title')]: n(0.9), [qid.copy('title')]: n(0.9), [qid.present('row')]: n(0.9), [qid.state('row', 'selected')]: n(0.9), [qid.present('gone')]: n(0.05), [qid.present('icon')]: n(0.9), [qid.copy('icon')]: n(0.9) },
    app: { [qid.present('title')]: n(0.9), [qid.copy('title')]: n(0.1), [qid.present('row')]: n(0.9), [qid.state('row', 'selected')]: n(0.1), [qid.present('gone')]: n(0.95), [qid.present('icon')]: n(0.1), [qid.copy('icon')]: n(0.95) },
  };
  const v = decide(judge, answers, {});
  const by = Object.fromEntries(v.elements.map(e => [e.id, e]));
  assert.equal(by.title.verdict, 'differs');
  assert.deepEqual(by.title.reasons, ['copy_differs']);
  assert.equal(by.row.verdict, 'accepted');
  assert.equal(by.gone.reasons[0], 'extra_in_app');
  assert.equal(by.icon.verdict, 'differs');
  assert.equal(by.icon.copy, undefined); // the copy answer is speculative: presence failed, so it is never read
  assert.equal(v.verdict, 'differs');
  assert.deepEqual(v.counts, { match: 0, differs: 3, 'not-judged': 0, accepted: 1 });
});

test('pairVerdict order: differs, not-judged (incl. a third spec mismatches), match with accepted', () => {
  const r = (verdict, reasons = []) => ({ verdict, reasons });
  assert.equal(pairVerdict([r('match'), r('differs')]).verdict, 'differs');
  assert.equal(pairVerdict([r('match'), r('not-judged')]).verdict, 'not-judged');
  assert.equal(pairVerdict([r('match'), r('accepted')]).verdict, 'match (accepted: 1)');
  assert.equal(pairVerdict([r('match'), r('match')]).verdict, 'match');
});

test('planPrepare: desktop and phone shots scale down to the documented limits', () => {
  const d = planPrepare(2560, 1600);
  assert.deepEqual([d.outW, d.outH, d.padX, d.padY], [1568, 980, 0, 0]);
  assert.ok(Math.abs(d.scaleX - 0.6125) < 1e-9);
  const p = planPrepare(780, 1688);
  assert.deepEqual([p.outW, p.outH], [Math.round(780 * 1568 / 1688), 1568]);
  assert.ok(withinLimits(p.outW, p.outH));
  const ok = planPrepare(1462, 636);
  assert.deepEqual([ok.outW, ok.outH, ok.scaleX], [1462, 636, 1]);
});

test('planPrepare: thin strips are padded, tiny squares scaled up, wide strips scaled then padded', () => {
  const s = planPrepare(1526, 241);
  assert.deepEqual([s.outW, s.outH, s.scaleX, s.padX, s.padY], [1526, 280, 1, 0, 19]);
  const t = planPrepare(132, 132);
  assert.deepEqual([t.outW, t.outH, t.padX, t.padY], [280, 280, 0, 0]);
  assert.ok(Math.abs(t.scaleX - 280 / 132) < 1e-9);
  const w = planPrepare(1976, 144);
  assert.equal(w.contentW, 1568);
  assert.equal(w.outH, 280);
  assert.ok(w.padY > 0);
  for (const [a, b] of [[2560, 1600], [780, 1688], [1526, 192], [1976, 144], [132, 132], [300, 3000], [5000, 5000]]) {
    const q = planPrepare(a, b);
    assert.ok(withinLimits(q.outW, q.outH), `${a}x${b} -> ${q.outW}x${q.outH}`);
  }
});

test('boxToCss maps a Box answer back to CSS px through scale, pad and crop origin', () => {
  const d = planPrepare(2560, 1600); // a whole desktop frame at scale 2
  const css = boxToCss({ left: 0.5, right: 0.75, top: 0.25, bottom: 0.5 }, d);
  assert.deepEqual(Object.values(css).map(v => Math.round(v * 1000) / 1000), [640, 200, 960, 400]);
  const s = planPrepare(1526, 241); // padded strip: the box edge at the pad is the frame's top
  const top = boxToCss({ left: 0, right: 1, top: s.padY / s.outH, bottom: (s.padY + 241) / s.outH }, s);
  assert.ok(Math.abs(top.y0) < 1e-9 && Math.abs(top.y1 - 120.5) < 1e-9);
  const crop = regionCrop({ x: 880, y: 40, width: 380, height: 190 }, 2560, 1600);
  assert.deepEqual(crop, { x: 1760, y: 80, w: 760, h: 380 });
  const r = planPrepare(crop.w, crop.h);
  const inCrop = boxToCss({ left: 0, right: 0.5, top: 0, bottom: 1 }, r, crop);
  assert.deepEqual([inCrop.x0, inCrop.y0, inCrop.x1, inCrop.y1], [880, 40, 1070, 230]);
});

test('secretReason excludes passwords, recovery keys, one-time codes and the recovery-key screen', () => {
  const pair = steps => ({ name: 'p', prototype: {}, app: { who: 'fresh', steps } });
  assert.match(secretReason(pair([{ type: ['#signin-password', '{password:mira}'] }])), /secret/);
  assert.match(secretReason(pair([{ type: ['input[name=key]', '{recoveryKey:bjorn}'] }])), /secret/);
  assert.match(secretReason(pair([{ totp: ['input', 'mira'] }])), /secret/);
  assert.match(secretReason(pair([{ waitFor: 'text=Your recovery key' }])), /secret/);
  assert.match(secretReason(pair([{ type: ['#signin-password', 'literal'] }])), /secret/);
  assert.equal(secretReason(pair([{ type: ['#signin-username', 'mira'] }, { click: 'x' }])), null);
  assert.equal(secretReason({ name: 's', prototype: {}, story: { id: 'a--b' } }), null);
});

test('the secret rule matches the three excluded wave A pairs and not their empty-form sibling', async () => {
  const fs = await import('node:fs');
  const spec = JSON.parse(fs.readFileSync(new URL('./app/wave-a-gaps.json', import.meta.url), 'utf8'));
  const hit = spec.filter(p => secretReason(p)).map(p => p.name).sort();
  assert.deepEqual(hit, ['gap03-signin-recover-desktop', 'gap04-signin-done-desktop', 'gap11-auth-phone-code']);
});

test('validateJudge rejects unknown keys, bad anchors and "the X or the Y" names', () => {
  const p = judge => ({ name: 'p', judge });
  validateJudge(p({ elements: [{ id: 'a', name: 'the send button' }, { id: 'b', name: 'the field', anchor: 'a' }] }));
  validateJudge(p({ exclude: 'secret' }));
  assert.throws(() => validateJudge(p({ elements: [{ id: 'a', name: 'x', colour: 'red' }] })), /unknown key colour/);
  assert.throws(() => validateJudge(p({ elements: [{ id: 'a', name: 'x', anchor: 'zz' }] })), /anchor/);
  assert.throws(() => validateJudge(p({ elements: [{ id: 'a', name: 'the bell or the gear' }] })), /one thing/);
  assert.throws(() => validateJudge(p({ elements: [{ id: 'a', name: 'x', copy: 3 }] })), /copy/);
  assert.throws(() => validateJudge(p({ mask: [] })), /unknown key mask/);
});

test('questions: identical for both sides, absent elements ask presence only, no side names', () => {
  const els = [{ id: 'a', name: 'the send button', copy: 'Send', tone: true, state: { focused: { phrase: 'focused', true: 't', false: 'f' } } }, { id: 'b', name: 'a loading line', expect: 'absent' }];
  const qs = buildQuestions(els);
  assert.deepEqual(Object.keys(qs).sort(), ['a__box', 'a__copy', 'a__present', 'a__state__focused', 'a__tone', 'b__present']);
  assert.ok(!/prototype|app\b|story/i.test(JSON.stringify(qs)));
  const pair = { name: 'p', viewport: { width: 390, height: 844 }, prototype: {}, app: { who: 'phone' } };
  const body = buildBody(pair, requestGroups(els)[0], 'ab'.repeat(32));
  assert.equal(body.model, MODEL);
  assert.equal(body.state.view, 'A phone screen of a dark-themed chat app.');
  assert.equal(body.assets[body.state.screen].part, 'screen-image');
  const g = requestGroups([{ id: 'x', name: 'x' }, { id: 'y', name: 'y', region: { x: 0, y: 0, width: 200, height: 200 } }]);
  assert.deepEqual(g.map(x => x.elements.map(e => e.id)), [['x'], ['y']]);
});

test('send: the key goes only in the Authorization header; logs use the redacted headers; 429 is retried', async () => {
  const calls = [];
  const fake = async (url, init) => {
    calls.push(init);
    return calls.length === 1 ? new Response('{}', { status: 429 }) : new Response(JSON.stringify({ model: 'jev-1.14.0', answers: { a: { type: 'noul', noul: 0.5 } }, usage: { input_tokens: 10, output_tokens: 1 } }), { status: 200 });
  };
  const res = await send({ model: 'm', state: {}, questions: {} }, Buffer.from([1, 2, 3]), 'sk-test-secret', { fetchImpl: fake, tries: 2 });
  assert.equal(res.status, 200);
  assert.equal(calls.length, 2);
  assert.equal(calls[1].headers.Authorization, 'Bearer sk-test-secret');
  assert.ok(!JSON.stringify(res).includes('sk-test-secret'));
  assert.equal(REDACTED_HEADERS.Authorization, 'Bearer [REDACTED]');
});

test('boxQuantum: one Box step in CSS px per axis sets the geometry tolerance', () => {
  const desk = planPrepare(2560, 1600), phone = planPrepare(780, 1688);
  const qd = boxQuantum(desk, desk), qp = boxQuantum(phone, phone);
  assert.ok(Math.abs(qd.x - 12.8) < 1e-9 && Math.abs(qd.y - 8) < 1e-9);
  assert.ok(Math.abs(qp.x - 3.9) < 0.01 && Math.abs(qp.y - 8.44) < 0.01);
  assert.ok(Math.abs(posTol(qd.x) - 25.6) < 1e-9);
  assert.ok(Math.abs(posTol(qp.x) - 7.8) < 0.01);
  assert.equal(posTol(0), 6);
  assert.ok(Math.abs(sizeTol(400, qd.y) - 32) < 1e-9); // 8 % of 400 beats two 8 px steps
  const b = (x, y, w, h) => ({ x0: x, y0: y, x1: x + w, y1: y + h });
  // A one-step difference on a desktop frame is noise (G1); the old 6 px tolerance would have called it G3.
  const css = { a: { prototype: b(0, 0, 100, 20), app: b(12.8, 0, 100, 20), quantum: qd }, b: { prototype: b(0, 50, 100, 20), app: b(0, 50, 100, 20), quantum: qd }, c: { prototype: b(0, 90, 100, 20), app: b(0, 90, 100, 20), quantum: qd } };
  const g = decideGeometry([{ id: 'a' }, { id: 'b' }, { id: 'c' }], new Set(['a', 'b', 'c']), css);
  assert.equal(g.a.row, 'G1');
  assert.deepEqual(g.a.pos_tol_css, { x: 25.6, y: 16 });
});

test('shoot.mjs --judge cannot deadlock: judge.mjs never imports shoot.mjs statically (bug: unsettled top-level await)', async () => {
  // shoot.mjs runs `await main()` at top level and imports judge.mjs from inside it; a static import path from judge.mjs back
  // to shoot.mjs makes judge.mjs wait for shoot.mjs, which waits for judge.mjs, and Node exits 13.
  const fs = await import('node:fs');
  const { fileURLToPath } = await import('node:url');
  const pathMod = await import('node:path');
  const seen = new Set();
  const walk = file => {
    if (seen.has(file)) return;
    seen.add(file);
    const src = fs.readFileSync(file, 'utf8');
    for (const m of src.matchAll(/^\s*(?:import|export)\s[^'"]*?from\s+['"](\.{1,2}\/[^'"]+)['"]|^\s*import\s+['"](\.{1,2}\/[^'"]+)['"]/gm)) walk(pathMod.resolve(pathMod.dirname(file), m[1] ?? m[2]));
  };
  walk(fileURLToPath(new URL('./judge.mjs', import.meta.url)));
  assert.ok(seen.size >= 2, 'the walk found judge-questions.mjs');
  assert.ok(![...seen].some(f => f.endsWith('/shoot.mjs')), `static imports from judge.mjs reach shoot.mjs: ${[...seen].join(', ')}`);
  assert.equal(SCALE, SHOOT_SCALE);
});

test('judge.mjs CLI still validates the spec through a dynamic import of shoot.mjs (no network, no browser)', async () => {
  const { spawnSync } = await import('node:child_process');
  const fs = await import('node:fs');
  const os = await import('node:os');
  const pathMod = await import('node:path');
  const dir = fs.mkdtempSync(pathMod.join(os.tmpdir(), 'judge-cli-'));
  fs.writeFileSync(pathMod.join(dir, 'spec.json'), JSON.stringify([{ name: 'p', viewport: { width: 1280, height: 800 }, prototype: {}, story: { id: 'a--b' } }]));
  const r = spawnSync(process.execPath, [new URL('./judge.mjs', import.meta.url).pathname, pathMod.join(dir, 'spec.json'), dir, '--dry-run'], { encoding: 'utf8', timeout: 60_000 });
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /not-judged:no-elements/);
  const bad = spawnSync(process.execPath, [new URL('./judge.mjs', import.meta.url).pathname, pathMod.join(dir, 'spec.json'), dir, '--dry-run'].map((a, i) => (i === 1 ? pathMod.join(dir, 'nope.json') : a)), { encoding: 'utf8', timeout: 60_000 });
  assert.notEqual(bad.status, 0);
  fs.rmSync(dir, { recursive: true, force: true });
});

test('shoot.mjs --judge: parseArgs strips the flag and reports it', () => {
  assert.deepEqual(parseArgs(['s.json', 'out', '--judge', '--app']), { specFile: 's.json', outDir: 'out', storybook: 'http://127.0.0.1:6006', app: 'target/ui-screens/signal-b/state.json', judge: true });
  assert.equal(parseArgs(['s.json', 'out']).judge, undefined);
});

test('the probe judge blocks in the committed specs are valid', async () => {
  const fs = await import('node:fs');
  for (const f of ['./app/wave-a-gaps.json', './app/task-2.json', './app/standing.json']) {
    const spec = JSON.parse(fs.readFileSync(new URL(f, import.meta.url), 'utf8'));
    spec.forEach(validateJudge);
    assert.ok(spec.some(p => p.judge?.elements?.length > 0), f);
  }
});

test('pngSize reads IHDR; parseJudgeArgs reads the flags', () => {
  assert.deepEqual(parseJudgeArgs(['--dry-run', 's.json', '--reuse', 'shots']), { replay: undefined, specFile: 's.json', shotDir: 'shots', outDir: undefined, only: undefined, budget: undefined, dry: true, reuse: true });
  const b = Buffer.alloc(24);
  b.writeUInt32BE(0x89504e47, 0); b.write('IHDR', 12, 'ascii'); b.writeUInt32BE(1568, 16); b.writeUInt32BE(980, 20);
  assert.deepEqual(pngSize(b), { w: 1568, h: 980 });
  assert.deepEqual(parseJudgeArgs(['s.json', 'shots', '--only', 'a,b', '--budget', '0.2', '--out', 'o']), { replay: undefined, specFile: 's.json', shotDir: 'shots', outDir: 'o', only: ['a', 'b'], budget: 0.2 });
  assert.equal(parseJudgeArgs(['--replay', 'x.request.app.json']).replay, 'x.request.app.json');
});
