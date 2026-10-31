import test from 'node:test';
import assert from 'node:assert/strict';
import { validateSpec, storyUrl, patchDataProps, DEFAULT_GLOBALS, padClip, localFontCss, appUrl, sideOf, pairFile, parseArgs, expandText, orderPairs, uploadFixture } from './shoot.mjs';

const good = { name: 'a', viewport: { width: 1280, height: 800 }, prototype: {}, story: { id: 'x--y' } };

test('validateSpec accepts a minimal pair', () => {
  assert.equal(validateSpec([good]).length, 1);
});

test('validateSpec rejects a pair without name or story.id', () => {
  assert.throws(() => validateSpec([{ ...good, name: undefined }]), /name is required/);
  assert.throws(() => validateSpec([{ ...good, story: {} }]), /story\.id is required/);
});

test('validateSpec rejects empty specs, path-like names, duplicates, bad viewports and bad steps', () => {
  assert.throws(() => validateSpec([]), /non-empty/);
  assert.throws(() => validateSpec([{ ...good, name: '../x' }]), /file stem/);
  assert.throws(() => validateSpec([good, good]), /duplicate/);
  assert.throws(() => validateSpec([{ ...good, viewport: { width: 0, height: 1 } }]), /viewport/);
  assert.throws(() => validateSpec([{ ...good, prototype: { steps: [{ click: 'a', hover: 'b' }] } }]), /exactly one/);
  assert.throws(() => validateSpec([{ ...good, prototype: { steps: [{ type: ['a'] }] } }]), /\[selector, text\]/);
});

test('storyUrl builds the iframe URL with the default globals encoded', () => {
  const u = new URL(storyUrl('http://127.0.0.1:6006', { id: 'shell-appshell--shell' }));
  assert.equal(u.pathname, '/iframe.html');
  assert.equal(u.searchParams.get('id'), 'shell-appshell--shell');
  assert.equal(u.searchParams.get('viewMode'), 'story');
  assert.equal(u.searchParams.get('globals'), DEFAULT_GLOBALS);
  assert.match(storyUrl('http://127.0.0.1:6006', { id: 'a' }), /globals=theme%3Asignal%3Bmotion%3Areduced/);
});

test('storyUrl encodes custom globals and ids', () => {
  const href = storyUrl('http://127.0.0.1:6006/', { id: 'a b', globals: 'theme:light;density:cozy' });
  assert.match(href, /id=a\+b/);
  assert.match(href, /globals=theme%3Alight%3Bdensity%3Acozy/);
});

test('patchDataProps sets defaults on known props and adds unknown ones, keeping the rest of the metadata', () => {
  const raw = JSON.stringify({ startAt: { editor: 'enum', default: 'signin' }, device: { editor: 'enum', default: 'auto' } });
  const out = JSON.parse(patchDataProps(raw, { startAt: 'app', extra: 1 }));
  assert.deepEqual(out.startAt, { editor: 'enum', default: 'app' });
  assert.deepEqual(out.device, { editor: 'enum', default: 'auto' });
  assert.deepEqual(out.extra, { default: 1 });
});

test('patchDataProps tolerates missing or broken metadata', () => {
  assert.deepEqual(JSON.parse(patchDataProps(null, { a: 1 })), { a: { default: 1 } });
  assert.deepEqual(JSON.parse(patchDataProps('{nope', { a: 1 })), { a: { default: 1 } });
});

test('padClip grows a box by the pad and stays on the page', () => {
  assert.deepEqual(padClip({ x: 20, y: 30, width: 10, height: 12 }, 8), { x: 12, y: 22, width: 26, height: 28 });
  assert.deepEqual(padClip({ x: 3, y: 0, width: 10, height: 12 }, 8), { x: 0, y: 0, width: 21, height: 20 });
});

test('localFontCss declares JetBrains Mono 400-700 and DM Serif Display italic from local files', () => {
  const seen = [];
  const css = localFontCss(f => { seen.push(f); return 'QQ=='; });
  assert.match(css, /font-family:'JetBrains Mono';font-style:normal;font-weight:400 700/);
  assert.match(css, /font-family:'DM Serif Display';font-style:italic;font-weight:400/);
  assert.ok(seen.some(f => f.includes('jetbrains-mono')) && seen.some(f => f.includes('dm-serif-display')));
  assert.doesNotMatch(css, /https?:/);
});

test('prototype Home loader resolves its original static URL through the official House alias', async () => {
  const { chromium } = await import('playwright');
  const { routePrototypeAssets } = await import('./shoot.mjs');
  const browser = await chromium.launch({ headless: true });
  const homeUrl = 'https://cdn.jsdelivr.net/npm/lucide-static@0.460.0/icons/home.svg';
  try {
    const page = await browser.newPage();
    const requested = [];
    await page.route('https://cdn.jsdelivr.net/**', route => {
      requested.push(route.request().url());
      return route.fulfill({ status: 404, headers: { 'access-control-allow-origin': '*' }, body: 'Missing static alias' });
    });
    const load = async () => {
      await page.setContent('<nav style="color:#E4E8EE"><d-icon name="home" size="19"></d-icon></nav>');
      await page.addScriptTag({ path: new URL('../prototype/dilla-icons.js', import.meta.url).pathname });
      await page.waitForFunction(() => customElements.get('d-icon'));
    };
    const missing = page.waitForResponse(response => response.url() === homeUrl);
    await load();
    await missing;
    assert.deepEqual(requested, [homeUrl], 'the unmodified prototype requests the obsolete home.svg URL');
    assert.equal(await page.locator('d-icon svg path').count(), 0, 'a 404 leaves the real loader with a blank Home glyph');
    // Reload the document to clear the loader's cached failed request.
    await page.goto('about:blank');
    await routePrototypeAssets(page);
    await load();
    await page.locator('d-icon svg path').first().waitFor({ timeout: 1000 });
    const glyph = await page.locator('d-icon svg').evaluate(svg => ({
      paths: [...svg.querySelectorAll('path')].map(path => path.getAttribute('d')),
      width: svg.getAttribute('width'), height: svg.getAttribute('height'),
      stroke: svg.getAttribute('stroke'), strokeWidth: svg.getAttribute('stroke-width'),
      color: getComputedStyle(svg).color,
    }));
    assert.deepEqual(glyph, {
      paths: [
        'M15 21v-8a1 1 0 0 0-1-1h-4a1 1 0 0 0-1 1v8',
        'M3 10a2 2 0 0 1 .709-1.528l7-5.999a2 2 0 0 1 2.582 0l7 5.999A2 2 0 0 1 21 10v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z',
      ],
      width: '19', height: '19', stroke: 'currentColor', strokeWidth: '2', color: 'rgb(228, 232, 238)',
    });
    assert.equal(await page.evaluate(async () => (await fetch('https://cdn.jsdelivr.net/npm/lucide-static@0.460.0/icons/unknown.svg')).status), 404, 'the alias route must leave other assets alone');
  } finally { await browser.close(); }
});

test('prototype icon readiness holds the capture until the visible Home glyph finishes loading', async () => {
  const { chromium } = await import('playwright');
  const { routePrototypeAssets, waitForPrototypeIcons } = await import('./shoot.mjs');
  const browser = await chromium.launch({ headless: true });
  let release;
  const held = new Promise(resolve => { release = resolve; });
  try {
    const page = await browser.newPage();
    await routePrototypeAssets(page);
    await page.route('https://cdn.jsdelivr.net/npm/lucide-static@0.460.0/icons/home.svg', async route => { await held; await route.fallback(); });
    await page.setContent('<d-icon name="home" size="19"></d-icon>');
    await page.addScriptTag({ path: new URL('../prototype/dilla-icons.js', import.meta.url).pathname });
    let ready = false;
    const pending = waitForPrototypeIcons(page).then(() => { ready = true; });
    await page.waitForTimeout(70);
    assert.equal(ready, false, 'the document and its empty SVG can be ready before the glyph response');
    release();
    await pending;
    assert.equal(await page.locator('d-icon svg path').count(), 2);
  } finally { release(); await browser.close(); }
});

const app = { name: 'b', viewport: { width: 1280, height: 800 }, prototype: {}, app: { who: 'ada' } };

test('validateSpec accepts a pair with app and no story, and rejects both or neither', () => {
  assert.equal(validateSpec([app]).length, 1);
  assert.equal(validateSpec([app, good.name === 'a' ? { ...good, name: 'c' } : good]).length, 2);
  assert.throws(() => validateSpec([{ ...app, story: { id: 'x--y' } }]), /exactly one of story or app/);
  const { story, ...neither } = good;
  assert.throws(() => validateSpec([neither]), /exactly one of story or app/);
});

test('validateSpec rejects an unknown who and accepts every known one', () => {
  assert.throws(() => validateSpec([{ ...app, app: { who: 'liv' } }]), /who must be one of/);
  assert.throws(() => validateSpec([{ ...app, app: {} }]), /who must be one of/);
  for (const who of ['ada', 'bjorn', 'mira', 'phone', 'fresh', 'freshPhone']) assert.equal(validateSpec([{ ...app, app: { who } }]).length, 1);
});

test('validateSpec accepts goto and totp steps and rejects a malformed totp or goto', () => {
  const steps = [{ goto: '/welcome' }, { totp: ['input', 'ada'] }, { click: 'button' }];
  assert.equal(validateSpec([{ ...app, app: { who: 'fresh', path: '/x', steps, clip: '.a', reset: '/y' } }]).length, 1);
  assert.throws(() => validateSpec([{ ...app, app: { who: 'fresh', steps: [{ totp: ['input'] }] } }]), /totp is \[selector, who\]/);
  assert.throws(() => validateSpec([{ ...app, app: { who: 'fresh', steps: [{ totp: ['input', 'zed'] }] } }]), /totp is \[selector, who\]/);
  assert.throws(() => validateSpec([{ ...app, app: { who: 'fresh', steps: [{ goto: 3 }] } }]), /goto is a path/);
  assert.throws(() => validateSpec([{ ...app, app: { who: 'ada', path: 3 } }]), /app\.path/);
});

test('appUrl joins the public URL and expands {invite} from state.invitePath', () => {
  const state = { publicUrl: 'http://127.0.0.1:8611', invitePath: '/i/abc%20d' };
  assert.equal(appUrl(state, '/c/x?i={invite}'), 'http://127.0.0.1:8611/c/x?i=/i/abc%20d');
  assert.equal(appUrl(state, '{invite}'), 'http://127.0.0.1:8611/i/abc%20d');
  assert.equal(appUrl(state, '/'), 'http://127.0.0.1:8611/');
  assert.throws(() => appUrl({ publicUrl: 'http://x' }, '{invite}'), /invitePath/);
  assert.equal(appUrl({ ...state, serverPath: '/c/a/b' }, '{server}'), 'http://127.0.0.1:8611/c/a/b');
  assert.throws(() => appUrl(state, '{server}'), /serverPath/);
  assert.equal(appUrl({ ...state, serverPath: '/c/a/b' }, '{community}'), 'http://127.0.0.1:8611/c/a');
});

test('output file stems are .app.png for app pairs and .story.png for story pairs', () => {
  assert.equal(sideOf(app), 'app');
  assert.equal(sideOf(good), 'story');
  assert.equal(pairFile('out', app, sideOf(app)), 'out/b.app.png');
  assert.equal(pairFile('out', good, 'prototype'), 'out/a.prototype.png');
  assert.equal(pairFile('out', good, 'pair'), 'out/a.pair.png');
});

test('parseArgs reads --storybook and --app with an optional state file', () => {
  assert.deepEqual(parseArgs(['s.json', 'out']), { specFile: 's.json', outDir: 'out', storybook: 'http://127.0.0.1:6006', app: null });
  assert.equal(parseArgs(['s.json', 'out', '--app']).app, 'target/ui-screens/signal-b/state.json');
  assert.equal(parseArgs(['s.json', 'out', '--app', 'x/state.json']).app, 'x/state.json');
  assert.equal(parseArgs(['s.json', 'out', '--storybook', 'http://h:1']).storybook, 'http://h:1');
});

test('expandText fills {password:who} and {recoveryKey:who} and leaves other text alone', () => {
  const state = { people: { mira: { password: 'pw-mira', recoveryKey: 'AAAA BBBB' } } };
  assert.equal(expandText(state, 'x {password:mira} y'), 'x pw-mira y');
  assert.equal(expandText(state, '{recoveryKey:mira}'), 'AAAA BBBB');
  assert.equal(expandText(state, 'plain {other}'), 'plain {other}');
});

test('expandText throws on a missing key and does not echo any value', () => {
  const state = { people: { mira: { password: 'pw-mira-secret' } } };
  assert.throws(() => expandText(state, '{recoveryKey:mira}'), err => /recoveryKey for mira/.test(err.message) && !/pw-mira-secret/.test(err.message));
  assert.throws(() => expandText(state, '{password:bjorn}'), /password for bjorn/);
  assert.throws(() => expandText({}, '{password:mira}'), /password for mira/);
});

test('validateSpec accepts a dispatch step and a mutates flag, and rejects a non-boolean mutates', () => {
  assert.equal(validateSpec([{ ...app, mutates: true, app: { who: 'ada', steps: [{ dispatch: '.d-code-block__copy' }] } }]).length, 1);
  assert.throws(() => validateSpec([{ ...app, mutates: 'yes' }]), /mutates must be true or false/);
  assert.throws(() => validateSpec([{ ...app, app: { who: 'ada', steps: [{ dispatch: '.a', click: '.b' }] } }]), /exactly one/);
});

test('orderPairs runs mutating pairs last and keeps the order inside each group', () => {
  const p = (name, mutates) => ({ name, ...(mutates ? { mutates } : {}) });
  assert.deepEqual(orderPairs([p('a', true), p('b'), p('c', true), p('d')]).map(x => x.name), ['b', 'd', 'a', 'c']);
  assert.deepEqual(orderPairs([p('a'), p('b')]).map(x => x.name), ['a', 'b']);
});

test('validateSpec accepts an upload step with files and rejects one without', () => {
  assert.equal(validateSpec([{ ...app, app: { who: 'ada', steps: [{ upload: ['input[type=file]', 'a.png', 'b.txt'] }] } }]).length, 1);
  assert.throws(() => validateSpec([{ ...app, app: { who: 'ada', steps: [{ upload: ['input[type=file]'] }] } }]), /upload is \[selector, file/);
});

// Wave B task 3b: a step that reads the setup key a new account is shown and fills its code.
test('validateSpec takes a totpFrom step of two selectors and refuses any other shape', () => {
  const pair = { name: 'p', viewport: { width: 1280, height: 800 }, prototype: {}, app: { who: 'fresh', steps: [{ totpFrom: ['#factor-totp-key', '#factor-totp-code'] }] } };
  validateSpec([pair]);
  assert.throws(() => validateSpec([{ ...pair, app: { who: 'fresh', steps: [{ totpFrom: ['#k'] }] } }]), /totpFrom is \[setup key selector, code selector\]/);
  assert.throws(() => validateSpec([{ ...pair, app: { who: 'fresh', steps: [{ totpFrom: ['#k', ''] }] } }]), /totpFrom/);
});

test('Task 8 generated upload fixtures preserve exact cap sizes without committed binaries', () => {
  const file = uploadFixture('@bytes/26214401/huge.bin');
  assert.equal(file.name, 'huge.bin');
  assert.equal(file.buffer.length, 25 * 1024 * 1024 + 1);
  assert.equal(file.mimeType, 'application/octet-stream');
  assert.throws(() => uploadFixture('@bytes/0/empty'), /fixture/);
});

test('Task 8 manifest covers both viewports, conversation findings and standing screens', async () => {
  const { readFileSync } = await import('node:fs');
  const pairs = validateSpec(JSON.parse(readFileSync(new URL('./app/task-8.json', import.meta.url), 'utf8')));
  for (const state of ['threads-direct', 'over-25-mb-toast', 'waiting-toast', 'reaction-tooltip']) {
    for (const size of ['desktop', 'phone']) assert.ok(pairs.some(pair => pair.name === `task8-${state}-${size}`));
  }
  for (const gap of ['gap44', 'gap47', 'gap49', 'gap52', 'gap53', 'gap89', 'gap90', 'gap97']) {
    for (const width of [390, 1280]) assert.ok(pairs.some(pair => pair.name.startsWith(gap) && pair.viewport.width === width));
  }
  assert.equal(pairs.filter(pair => pair.name.startsWith('standing-')).length, 5);
});

test('rejects a null app before reading its upload options and names the pair', () => {
  assert.throws(() => validateSpec([{ name: 'null-app', viewport: { width: 390, height: 844 }, prototype: {}, app: null }]), /null-app.*app.who/);
});

// Regression: the Bot may post well after the normal UI timeout; the wait is for
// the actual card, not an arbitrary sleep which can capture a still-empty feed.
test('waitFor accepts a bounded per-step timeout and rejects malformed waits', () => {
  const pair = waitFor => ({ ...app, app: { who: 'ada', steps: [{ waitFor }] } });
  validateSpec([pair({ selector: '.d-poll-card', timeout: 120_000 })]);
  for (const wait of [null, 3, {}, { selector: '', timeout: 1 }, { selector: '.x', timeout: 0 }, { selector: '.x', timeout: Infinity }]) {
    assert.throws(() => validateSpec([pair(wait)]), /waitFor/);
  }
});

test('every app manifest validates, including signin-only task 3', async () => {
  const { readdirSync, readFileSync } = await import('node:fs');
  for (const name of readdirSync(new URL('./app/', import.meta.url)).filter(n => n.endsWith('.json'))) {
    assert.doesNotThrow(() => validateSpec(JSON.parse(readFileSync(new URL(`./app/${name}`, import.meta.url)))), name);
  }
});

test('capture readiness waits for visible loading in the clip and preserves requested hover', async () => {
  const { chromium } = await import('playwright');
  const { waitForCaptureReady, runSteps } = await import('./shoot.mjs');
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    await assert.rejects(runSteps(page, [{ waitFor: { selector: '.not-posted', timeout: 40 } }]), /Timeout 40ms/);
    await page.setContent('<aside>Loading members…</aside><main aria-busy="true"><span>Loading reminders…</span><button>Ready</button></main><style>button:hover{color:rgb(255,0,0)}</style>');
    let settled = false;
    const ready = waitForCaptureReady(page, 'main').then(() => { settled = true; });
    await page.waitForTimeout(80);
    assert.equal(settled, false, 'loading reminders must block the capture');
    await page.locator('main').evaluate(el => { el.removeAttribute('aria-busy'); el.querySelector('span').textContent = 'No reminders due'; });
    await ready;
    assert.equal(settled, true, 'loading outside the crop must not block it');
    await page.evaluate(() => document.addEventListener('mousemove', event => { window.pointer = { x: event.clientX, y: event.clientY }; }));
    await runSteps(page, [{ hover: 'button' }, { click: 'button' }]);
    assert.equal(await page.locator('button').evaluate(el => { const rect = el.getBoundingClientRect(); return window.pointer.x >= rect.left && window.pointer.x <= rect.right && window.pointer.y >= rect.top && window.pointer.y <= rect.bottom; }), true);
    await runSteps(page, [{ click: 'button' }]);
    assert.deepEqual(await page.evaluate(() => window.pointer), { x: 0, y: 0 }, 'incidental click hover must be cleared');
  } finally { await browser.close(); }
});

test('voice focus captures refuse a world without its shared-screen fixture', async () => {
  const { validateWorldForPair } = await import('./shoot.mjs');
  for (const name of ['standing-voice-focus', 'gap75-focused-tile-fullscreen']) {
    assert.throws(() => validateWorldForPair({}, { name }), /screen share/);
    assert.doesNotThrow(() => validateWorldForPair({ miraShares: true }, { name }));
  }
  assert.doesNotThrow(() => validateWorldForPair({}, { name: 'ordinary-chat' }));
});

test('standing pairs capture the fresh voice fixture before any browser reloads or mutations', () => {
  const pairs = [
    { name: 'command-list' },
    { name: 'post-reminder', mutates: true },
    { name: 'standing-desktop-chat' },
    { name: 'poll-dialog' },
    { name: 'standing-voice-focus' },
    { name: 'post-poll', mutates: true },
    { name: 'standing-phone-conversation' },
  ];
  assert.deepEqual(orderPairs(pairs).map(pair => pair.name), [
    'standing-desktop-chat', 'standing-voice-focus', 'standing-phone-conversation',
    'command-list', 'poll-dialog', 'post-reminder', 'post-poll',
  ]);
  assert.equal(pairs[0].name, 'command-list', 'ordering must not rewrite the input manifest');
});

test('signed-in captures wait past Starting dilla until the app shell is visible, while fresh auth does not', async () => {
  const { chromium } = await import('playwright');
  const { waitForAppReady } = await import('./shoot.mjs');
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    await page.setContent('<h1>Starting dilla</h1><div class="d-app-shell" hidden>Channel list</div>');
    await page.evaluate(() => { document.documentElement.dataset.theme = 'signal'; });
    let ready = false;
    const loading = waitForAppReady(page, 'phone').then(() => { ready = true; });
    await page.waitForTimeout(80);
    assert.equal(ready, false, 'an attached but hidden shell is still the startup splash');
    await page.locator('.d-app-shell').evaluate(el => { el.hidden = false; });
    await loading;
    assert.equal(ready, true);
    await page.setContent('<h1>Sign in</h1><input autocomplete="username">');
    await page.evaluate(() => { document.documentElement.dataset.theme = 'signal'; });
    await waitForAppReady(page, 'fresh');
    await waitForAppReady(page, 'freshPhone');
    assert.equal(await page.locator('.d-app-shell').count(), 0, 'signed-out flows do not require a shell');
  } finally { await browser.close(); }
});

test('fresh invite entry can click through the server landing before requiring the app theme', async () => {
  const { chromium } = await import('playwright');
  const { waitForAppReady } = await import('./shoot.mjs');
  const browser = await chromium.launch({ headless: true });
  try {
    for (const who of ['fresh', 'freshPhone']) {
      const page = await browser.newPage();
      await page.setContent('<a href="#app">Open dilla in this browser</a>');
      let entered = false;
      const entry = waitForAppReady(page, who, { beforeSteps: true }).then(() => { entered = true; }).catch(() => {});
      await page.waitForTimeout(80);
      assert.equal(entered, true, 'the landing page has no app theme yet');
      await entry;
      let ready = false;
      const capture = waitForAppReady(page, who).then(() => { ready = true; }).catch(() => {});
      await page.waitForTimeout(80);
      assert.equal(ready, false, 'the final capture still waits for the real app theme');
      await page.getByRole('link').click();
      await page.evaluate(() => { document.documentElement.dataset.theme = 'signal'; });
      await capture;
      assert.equal(ready, true);
      await page.close();
    }
  } finally { await browser.close(); }
});

test('shrug capture selects the published backslash-containing row rather than the private result', async () => {
  const { chromium } = await import('playwright');
  const { readFileSync } = await import('node:fs');
  const pair = JSON.parse(readFileSync(new URL('./app/task-2.json', import.meta.url))).find(pair => pair.name === 'cmd-result-shrug');
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    await page.setContent(String.raw`<article class="d-message-row" id="published">¯\_(ツ)_/¯</article><article class="d-message-row" id="private"><div class="d-bot-result">Done: ¯\_(ツ)_/¯</div></article>`);
    assert.equal(await page.locator(pair.app.clip).count(), 1, 'the literal shrug must be found despite its backslash');
    assert.equal(await page.locator(pair.app.clip).getAttribute('id'), 'published');
    const readySelector = pair.app.steps.findLast(step => step.waitFor).waitFor;
    assert.equal(await page.locator(readySelector).getAttribute('id'), 'published', 'the readiness wait must select the same public message');
  } finally { await browser.close(); }
});

test('fresh capture authenticator supports an empty conditional passkey request without a failure-kind browser error', async () => {
  const { chromium } = await import('playwright');
  const { prepareFreshPage } = await import('./shoot.mjs');
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    await prepareFreshPage(page);
    await page.route('http://localhost:9876/**', route => route.fulfill({ contentType: 'text/html', body: '<input autocomplete="username webauthn">' }));
    await page.goto('http://localhost:9876/');
    const errorName = await page.evaluate(async () => {
      const abort = new AbortController();
      const timer = setTimeout(() => abort.abort(), 200);
      try {
        await navigator.credentials.get({ publicKey: { challenge: new Uint8Array(32), rpId: 'localhost', userVerification: 'required' }, mediation: 'conditional', signal: abort.signal });
        return 'credential';
      } catch (error) { return error.name; }
      finally { clearTimeout(timer); }
    });
    assert.ok(['NotAllowedError', 'AbortError'].includes(errorName), `empty conditional request must cancel normally, got ${errorName}`);
  } finally { await browser.close(); }
});

test('fresh auth URL expansion uses localhost while connected RTC browsers retain the numeric world origin', async () => {
  const { stateForApp } = await import('./shoot.mjs');
  const state = { publicUrl: 'http://127.0.0.1:9311', authUrl: 'http://localhost:9311', invitePath: '/i/abc%20d', serverPath: '/c/community/general' };
  for (const who of ['fresh', 'freshPhone']) {
    const auth = stateForApp(state, who);
    assert.equal(appUrl(auth, '{invite}'), 'http://localhost:9311/i/abc%20d');
    assert.equal(appUrl(auth, '{server}'), 'http://localhost:9311/c/community/general');
    assert.equal(appUrl(auth, '/welcome'), 'http://localhost:9311/welcome');
  }
  for (const who of ['ada', 'mira', 'phone']) assert.equal(appUrl(stateForApp(state, who), '{server}'), 'http://127.0.0.1:9311/c/community/general');
  assert.equal(state.publicUrl, 'http://127.0.0.1:9311', 'auth routing must not rewrite the saved world state');
  assert.equal(stateForApp({ publicUrl: state.publicUrl }, 'fresh').publicUrl, state.publicUrl, 'legacy state files retain their existing origin');
});

test('side settleMs accepts zero and rejects non-finite or negative durations', () => {
  validateSpec([{ ...good, prototype: { settleMs: 0 }, story: { id: 'a', settleMs: 0 } }]);
  for (const settleMs of [-1, Infinity, '0']) {
    assert.throws(() => validateSpec([{ ...good, prototype: { settleMs } }]), /settleMs/);
    assert.throws(() => validateSpec([{ ...app, app: { who: 'ada', settleMs } }]), /settleMs/);
  }
});

test('zero post-step settling preserves a pending result while the default waits for it to resolve', async () => {
  const { chromium } = await import('playwright');
  const { runSteps } = await import('./shoot.mjs');
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    const pending = async () => {
      await page.setContent('<p id="result">Working</p>');
      await page.evaluate(() => setTimeout(() => { document.querySelector('#result').textContent = 'Done'; }, 350));
    };
    await pending();
    await runSteps(page, [{ wait: 30 }], { settleMs: 0 });
    assert.equal(await page.locator('#result').textContent(), 'Working');
    await pending();
    await runSteps(page, [{ wait: 30 }]);
    assert.equal(await page.locator('#result').textContent(), 'Done');
  } finally { await browser.close(); }
});

test('app readiness waits for delayed light-to-Signal restoration without overriding the preference', async () => {
  const { chromium } = await import('playwright');
  const { waitForAppReady } = await import('./shoot.mjs');
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    await page.setContent('<div class="d-app-shell">Ready shell</div>');
    await page.evaluate(() => { document.documentElement.dataset.theme = 'light'; });
    let ready = false;
    const restoring = waitForAppReady(page, 'phone').then(() => { ready = true; });
    await page.waitForTimeout(80);
    assert.equal(ready, false);
    assert.equal(await page.locator('html').getAttribute('data-theme'), 'light', 'readiness must not force a theme');
    await page.evaluate(() => { document.documentElement.dataset.theme = 'signal'; });
    await restoring;
    assert.equal(ready, true);
  } finally { await browser.close(); }
});

test('capture restores dark reduced OS media after another CDP session resets it to light', async () => {
  const { chromium } = await import('playwright');
  const { prepareAppCapture } = await import('./shoot.mjs');
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    await page.emulateMedia({ colorScheme: 'dark', reducedMotion: 'reduce' });
    await page.setContent('<div class="d-app-shell">Ready shell</div>');
    await page.evaluate(() => {
      const reflect = () => { document.documentElement.dataset.theme = matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'signal'; };
      matchMedia('(prefers-color-scheme: light)').addEventListener('change', reflect);
      reflect();
    });
    const other = await page.context().newCDPSession(page);
    await other.send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: 'light' }, { name: 'prefers-reduced-motion', value: 'no-preference' }] });
    await page.waitForSelector('html[data-theme="light"]', { state: 'attached' });
    await prepareAppCapture(page, 'phone');
    assert.equal(await page.locator('html').getAttribute('data-theme'), 'signal');
    assert.equal(await page.evaluate(() => matchMedia('(prefers-reduced-motion: reduce)').matches), true);
    await other.detach();
  } finally { await browser.close(); }
});

test('poll vote waits require the clicked answer selected and aria-enabled, not merely native enabled', async () => {
  const { chromium } = await import('playwright');
  const { readFileSync } = await import('node:fs');
  const pair = JSON.parse(readFileSync(new URL('./app/task-7.json', import.meta.url))).find(pair => pair.name === 'task7-card-closed-winner-desktop');
  const click = pair.app.steps.findIndex(step => step.click?.includes('role=button[name=/^the bridge/]'));
  const selector = pair.app.steps[click + 1].waitFor.selector;
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    await page.setContent('<section class="d-poll-card"><h3>What should we raid on Friday? card-closed-winner</h3><button class="d-poll-card__option" data-mine="false" aria-disabled="true">the bridge</button><button class="d-poll-card__option" data-mine="true" aria-disabled="false">the harbour</button></section>');
    assert.equal(await page.locator(selector).count(), 0, 'busy native-enabled answer and a different selected answer must not settle the vote');
    await page.getByRole('button', { name: 'the bridge' }).evaluate(el => { el.dataset.mine = 'true'; el.setAttribute('aria-disabled', 'false'); });
    assert.equal(await page.locator(selector).count(), 1);
  } finally { await browser.close(); }
});

test('signed-in frame waits for visible delayed server, identity and online metadata while allowing blank custom status', async () => {
  const { chromium } = await import('playwright');
  const { waitForAppMetadata } = await import('./shoot.mjs');
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    await page.setContent('<div class="d-app-shell"><nav class="d-channel-list"><h1 class="d-channel-list__title"></h1><p class="d-channel-list__stats"><span>0 in voice</span></p></nav><div class="d-user-panel"><span class="d-user-panel__name">–</span><span class="d-user-panel__username"></span></div></div>');
    let ready = false;
    const pending = waitForAppMetadata(page, 'phone').then(() => { ready = true; });
    await page.waitForTimeout(60);
    assert.equal(ready, false, 'an empty heading must not count as a ready shell');
    await page.locator('h1').evaluate(el => { el.textContent = 'A real server'; });
    await page.waitForTimeout(60);
    assert.equal(ready, false, 'the account identity placeholder must settle too');
    await page.locator('.d-user-panel__name').evaluate(el => { el.textContent = 'A real person'; });
    await page.waitForTimeout(60);
    assert.equal(ready, false, 'an existing server stat line without its online part is still loading');
    await page.locator('.d-channel-list__stats').evaluate(el => { el.insertAdjacentHTML('afterbegin', '<span class="d-channel-list__stat" data-live="true">0 online</span>'); });
    await pending;
    assert.equal(ready, true, 'zero online and a blank custom status are valid');
  } finally { await browser.close(); }
});

test('metadata readiness stays within the visible capture region and leaves fresh or missing-membership frames valid', async () => {
  const { chromium } = await import('playwright');
  const { waitForAppMetadata } = await import('./shoot.mjs');
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    await page.setContent('<aside><h1 class="d-channel-list__title"></h1><span class="d-user-panel__name">–</span></aside><main id="card">Settled card</main>');
    await waitForAppMetadata(page, 'ada', '#card');
    await waitForAppMetadata(page, 'fresh');
    await waitForAppMetadata(page, 'freshPhone');
    await page.locator('aside').evaluate(el => { el.style.display = 'none'; });
    await waitForAppMetadata(page, 'phone');
    await page.setContent('<nav class="d-channel-list"><h1 class="d-channel-list__title">Home</h1></nav>');
    await waitForAppMetadata(page, 'phone');
    await page.setContent('<main>Signed out</main>');
    await waitForAppMetadata(page, 'ada');
  } finally { await browser.close(); }
});

test('Task 4 captures the volatile Music Bot menu before own voice and AFK navigation', async () => {
  const { readFileSync } = await import('node:fs');
  const pairs = JSON.parse(readFileSync(new URL('./app/task-4.json', import.meta.url)));
  const names = orderPairs(pairs).map(pair => pair.name);
  const bot = names.indexOf('t4-bot-menu-desktop');
  assert.notEqual(bot, -1);
  for (const name of names.filter(name => name.startsWith('t4-own-menu-') || name.startsWith('t4-afk-'))) {
    assert.ok(bot < names.indexOf(name), `Music Bot menu must precede ${name}`);
  }
});

test('wheel step requires a target and two finite native wheel deltas', () => {
  const pair = wheel => ({ ...good, prototype: { steps: [{ wheel }] } });
  validateSpec([pair(['main', 0, -180])]);
  for (const wheel of [[], ['main', 0], ['', 0, 1], ['main', '0', 1], ['main', 0, Infinity], ['main', 0, 1, 2]]) {
    assert.throws(() => validateSpec([pair(wheel)]), /wheel/);
  }
});

test('targeted wheel delivers trusted manual scrolling and clears its incidental hover', async () => {
  const { chromium } = await import('playwright');
  const { runSteps } = await import('./shoot.mjs');
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    await page.setContent('<style>#log{width:300px;height:160px;overflow:auto}#content{height:1800px}</style><main id="log"><div id="content">History</div></main>');
    await page.evaluate(() => {
      window.events = []; window.pointer = null;
      document.addEventListener('mousemove', e => { window.pointer = { x: e.clientX, y: e.clientY }; });
      document.querySelector('#log').addEventListener('wheel', e => { window.events.push({ trusted: e.isTrusted, deltaX: e.deltaX, deltaY: e.deltaY }); });
    });
    await runSteps(page, [{ wheel: ['#log', 0, 180] }], { settleMs: 0 });
    await page.waitForFunction(() => document.querySelector('#log').scrollTop > 0, null, { timeout: 1000 });
    assert.deepEqual(await page.evaluate(() => window.events), [{ trusted: true, deltaX: 0, deltaY: 180 }]);
    assert.deepEqual(await page.evaluate(() => window.pointer), { x: 0, y: 0 });
  } finally { await browser.close(); }
});

test('poll votes use native initial scrolling and phone multichoice keeps consecutive taps', async () => {
  const { readFileSync } = await import('node:fs');
  const polls = JSON.parse(readFileSync(new URL('./app/task-7.json', import.meta.url)));
  let votes = 0;
  for (const pair of polls) {
    let previousVote = -1;
    for (const [i, step] of (pair.app.steps ?? []).entries()) {
      if (!step.click?.includes('.d-poll-card:') || !step.click.includes('role=button[name=/^the ')) continue;
      votes++;
      if (pair.name === 'task7-card-multi-phone' && previousVote !== -1) {
        // A fresh wheel dismisses the first reveal (R30), changing the second tap's meaning.
        assert.equal(pair.app.steps.slice(previousVote + 1, i).some(step => step.wheel), false, 'phone votes must be consecutive taps, like the prototype');
      } else assert.deepEqual(pair.app.steps[i - 1].wheel, [step.click.split(' >> ')[0], 0, -180], pair.name);
      previousVote = i;
      assert.equal(pair.app.steps.some(step => step.hover), false, 'polls must clear incidental wheel hover');
    }
    if (pair.name === 'task7-card-multi-phone') assert.equal(pair.app.steps.filter(step => step.wheel).length, 1, 'only the initial native scroll precedes the phone votes');
  }
  assert.equal(votes, 8);
  const toolbar = JSON.parse(readFileSync(new URL('./app/task-8.json', import.meta.url))).find(pair => pair.name === 'gap89-phone-message-toolbar-desktop');
  for (const side of ['prototype', 'app']) {
    const i = toolbar[side].steps.findIndex(step => step.hover);
    assert.deepEqual(toolbar[side].steps[i - 1].wheel, [toolbar[side].steps[i].hover, 0, -200]);
  }
});
