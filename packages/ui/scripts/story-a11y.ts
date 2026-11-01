import type { Page } from 'playwright';
import { injectAxe, checkA11y } from 'axe-playwright';
import { CONVERSATION_PROTOTYPE_CONTRAST } from './prototype-contrast.ts';

const STRICT_TARGET_TITLES = ['Form/', 'Ceremony/', 'Shell/', 'Conversation/', 'Settings/', 'Search/', 'Voice/', 'Media/'];

type StoryContext = { id: string; title: string; name: string };

/**
 * The element whose border is a control's only boundary, and the colour signal draws it in. Jonas 2026-10-27:
 * signal uses the prototype's exact borders (--control-edge #2A303A, --control-edge-field #252A33 for fields,
 * --control-edge-key #3A404A for keycaps and the add tile); light and high-contrast keep --edge (tokens.test.ts).
 */
const CONTROL_EDGE = 'rgb(42, 48, 58)';
const CONTROL_EDGE_FIELD = 'rgb(37, 42, 51)';
const CONTROL_EDGE_KEY = 'rgb(58, 64, 74)';
function boundarySelector(id: string): { selector: string; colour: string } | null {
  if (id.startsWith('primitives-keyhint--')) return { selector: '.d-keyhint__kbd', colour: CONTROL_EDGE_KEY };
  if (id === 'form-toggle--off') return { selector: '.d-toggle__track', colour: CONTROL_EDGE };
  if (id.startsWith('form-segmented--')) return { selector: '.d-segmented__group', colour: CONTROL_EDGE_FIELD };
  if (id === 'form-select--default') return { selector: '.d-select__input', colour: CONTROL_EDGE_FIELD };
  if (id === 'form-textfield--empty' || id === 'form-textfield--filled') return { selector: '.d-text-field__input', colour: CONTROL_EDGE_FIELD };
  if (id === 'primitives-button--default') return { selector: '.d-btn', colour: CONTROL_EDGE };
  if (id.startsWith('shell-topbar--')) return { selector: '.d-topbar__kbd', colour: CONTROL_EDGE };
  if (id === 'shell-communityrail--signal') return { selector: '.d-community-rail__glyph', colour: CONTROL_EDGE_KEY };
  return null;
}

/** README §5.1: the column aside's settled width (members 232, thread 340, closed 0). */
const ASIDE_STORIES: Record<string, number> = {
  'shell-appshell--aside-wide': 232, 'shell-appshell--frame-members': 232, 'shell-appshell--aside-thread': 340,
  'shell-appshell--aside-closed': 0, 'shell-appshell--frame-members-closed': 0,
};
async function checkAside(page: Page, context: StoryContext, want: number): Promise<void> {
  // Let the width transition settle (the runner already waited for finite animations; this is the belt to that).
  await page.waitForFunction(() => document.getAnimations().every(a => a.playState !== 'running' || a.effect?.getTiming().iterations === Infinity), undefined, { timeout: 5_000 }).catch(() => {});
  const got = await page.evaluate(() => {
    const el = document.querySelector<HTMLElement>('#storybook-root .d-app-shell__aside');
    if (el === null) return null;
    const control = el.querySelector<HTMLElement>('button, [href], input, [tabindex]:not([tabindex="-1"])');
    control?.focus();
    return {
      width: Math.round(el.getBoundingClientRect().width * 10) / 10, visibility: getComputedStyle(el).visibility, inert: el.inert,
      // Task 6 fix round 3: a clip-path on the aside or its body would cut a fixed-position popover inside it (the profile card).
      clipped: [el, el.querySelector('.d-app-shell__aside-body')].some(n => n !== null && getComputedStyle(n).clipPath !== 'none'),
      bodyWidth: Math.round((el.querySelector('.d-app-shell__aside-body')?.getBoundingClientRect().width ?? 0) * 10) / 10,
      focused: control !== null && document.activeElement === control,
    };
  });
  const name = `${context.title} / ${context.name}`;
  if (got === null) throw new Error(`${name}: no aside`);
  if (Math.abs(got.width - want) > 0.5) throw new Error(`${name}: the aside is ${String(got.width)} wide, want ${String(want)}`);
  if (got.clipped) throw new Error(`${name}: the aside or its body has a clip-path, which would cut its fixed-position popovers`);
  if (want === 0) {
    if (got.visibility !== 'hidden' || !got.inert) throw new Error(`${name}: the closed aside is ${got.visibility} and inert=${String(got.inert)}`);
    if (got.focused) throw new Error(`${name}: a control in the closed aside takes focus`);
  } else {
    if (got.visibility !== 'visible' || got.inert) throw new Error(`${name}: the open aside is ${got.visibility} and inert=${String(got.inert)}`);
    // The body is the open width less the 1px hairline, whatever the column is doing.
    if (Math.abs(got.bodyWidth - (want - 1)) > 0.5) throw new Error(`${name}: the aside body is ${String(got.bodyWidth)} wide, want ${String(want - 1)}`);
    await page.evaluate(() => { (document.activeElement as HTMLElement | null)?.blur(); });
  }
}

/**
 * Fix round 2: the web app's close path (the content goes with the close). The same aside element animates its width to 0:
 * some animation frame of the close is between 0 and 232 wide, then it ends hidden or unmounted; a reopen animates from 0 again.
 */
async function checkAsideTransition(page: Page, context: StoryContext): Promise<void> {
  const name = `${context.title} / ${context.name}`;
  const read = () => page.evaluate(() => {
    const el = document.querySelector<HTMLElement & { __mark?: boolean }>('#storybook-root .d-app-shell__aside');
    return el === null ? null : { width: el.getBoundingClientRect().width, same: el.__mark === true, open: el.dataset.open, visibility: getComputedStyle(el).visibility };
  });
  await page.waitForTimeout(600);
  const before = await read();
  if (before === null || Math.abs(before.width - 232) > 0.5) throw new Error(`${name}: the open aside is ${String(before?.width)} wide, want 232`);
  await page.evaluate(() => { (document.querySelector('#storybook-root .d-app-shell__aside') as (HTMLElement & { __mark?: boolean }) | null)!.__mark = true; });
  // Task 11 (Task 5 minor): the toggle is clicked in the page and the width is sampled every animation frame for 400ms, so a
  // slow machine cannot miss the transition the way one read 100ms after a click could. Some frame must lie strictly between
  // closed and open, on the same element.
  const toggleAndSample = () => page.evaluate(async () => {
    const el = () => document.querySelector<HTMLElement & { __mark?: boolean }>('#storybook-root .d-app-shell__aside');
    document.querySelector<HTMLElement>('#storybook-root .story-aside-toggle')!.click();
    const samples: { width: number; same: boolean; open: string | undefined }[] = [];
    const start = performance.now();
    while (performance.now() - start < 400) {
      await new Promise(r => requestAnimationFrame(r));
      const a = el();
      samples.push(a === null ? { width: -1, same: false, open: undefined } : { width: a.getBoundingClientRect().width, same: a.__mark === true, open: a.dataset.open });
    }
    return samples;
  });
  const closing = await toggleAndSample();
  // Once closed it may unmount; while it still has width it must be the same element, marked closing.
  const wide = closing.filter(x => x.width > 0.5);
  if (wide.length === 0 || wide.some(x => !x.same)) throw new Error(`${name}: closing replaced or removed the aside at once`);
  if (wide.some(x => x.open !== 'false')) throw new Error(`${name}: the closing aside is not data-open false in every frame`);
  if (!closing.some(x => x.width > 0.5 && x.width < 231.5)) throw new Error(`${name}: no frame of the close is between 0 and 232 wide (${closing.map(x => x.width.toFixed(0)).join(', ')})`);
  await page.waitForTimeout(800);
  const after = await read();
  if (after !== null && (after.width > 0.5 || after.visibility !== 'hidden')) throw new Error(`${name}: the closed aside is ${String(after.width)} wide and ${after.visibility}`);
  const opening = await toggleAndSample();
  if (!opening.some(x => x.width > 0.5 && x.width < 231.5)) throw new Error(`${name}: no frame of the reopen is between 0 and 232 wide (${opening.map(x => x.width.toFixed(0)).join(', ')})`);
  await page.waitForTimeout(600);
  const reopened = await read();
  if (reopened === null || Math.abs(reopened.width - 232) > 0.5) throw new Error(`${name}: the reopened aside is ${String(reopened?.width)} wide, want 232`);
  await page.evaluate(() => { (document.activeElement as HTMLElement | null)?.blur(); });
}

/**
 * Task 9 (Signal §5.8, Ruling C): the desktop palette is centred by left/right 0 and margin 0 auto, never a transform: once its
 * entry animation has ended its computed transform is none, its centre is the viewport's, it is min(600px, 92%) wide at 14vh,
 * its input row is 50px (51 with its hairline), every result at least 38px of content (46 with padding), and the footer reads "Searches messages held in this browser".
 */
async function checkPalette(page: Page, context: StoryContext): Promise<void> {
  const got = await page.evaluate(async () => {
    const dialog = document.querySelector<HTMLElement>('#storybook-root dialog.d-search-palette-dialog');
    if (dialog === null) return null;
    await Promise.all(dialog.getAnimations().map(a => a.finished.catch(() => undefined)));
    const cs = getComputedStyle(dialog);
    const r = dialog.getBoundingClientRect();
    const field = document.querySelector('#storybook-root .d-search-palette__field')?.getBoundingClientRect().height ?? 0;
    const rows = Array.from(document.querySelectorAll('#storybook-root .d-search-palette [role="option"]')).map(o => o.getBoundingClientRect().height);
    return { transform: cs.transform, left: cs.left, right: cs.right, centre: r.left + r.width / 2, width: r.width, top: r.top, vw: window.innerWidth, vh: window.innerHeight,
      field, rows, note: document.querySelector('#storybook-root .d-search-palette__note')?.textContent ?? '' };
  });
  const name = `${context.title} / ${context.name}`;
  if (got === null) throw new Error(`${name}: no palette dialog`);
  const bad: string[] = [];
  if (got.transform !== 'none') bad.push(`transform ${got.transform}`);
  if (got.left !== '0px' || got.right !== '0px') bad.push(`left/right ${got.left}/${got.right}, not 0/0`);
  if (Math.abs(got.centre - got.vw / 2) > 0.5) bad.push(`centre ${String(got.centre)} vs ${String(got.vw / 2)}`);
  if (Math.abs(got.width - Math.min(600, got.vw * 0.92)) > 0.5) bad.push(`width ${String(got.width)}`);
  if (Math.abs(got.top - got.vh * 0.14) > 0.5) bad.push(`top ${String(got.top)}, not 14vh`);
  if (Math.abs(got.field - 51) > 0.5) bad.push(`input row ${String(got.field)}, not 50 + its hairline`);
  if (got.rows.length === 0 || got.rows.some(h => h < 45.5)) bad.push(`result rows ${got.rows.join(', ')}`);
  if (got.note !== 'Searches messages held in this browser') bad.push(`footer "${got.note}"`);
  if (bad.length > 0) throw new Error(`${name}: ${bad.join('; ')}`);
}

/** The top bar's geometry (README §5.2, the prototype's inline sizes), in CSS px at a 16px root. */
async function checkTopBar(page: Page, context: StoryContext): Promise<void> {
  const got = await page.evaluate(() => {
    const box = (sel: string) => {
      const el = document.querySelector(`#storybook-root ${sel}`);
      if (el === null) return null;
      const r = el.getBoundingClientRect();
      return { w: Math.round(r.width * 10) / 10, h: Math.round(r.height * 10) / 10 };
    };
    return { bar: box('.d-topbar'), search: box('.d-topbar__search'), bell: box('.d-topbar__bell .d-icon-button'), gear: box('.d-topbar__gear'),
      chip: box('.d-topbar__voice'), badge: box('.d-topbar__badge'), tile: box('.d-brand__tile'), caret: box('.d-brand__caret') };
  });
  const bad: string[] = [];
  const want = (what: string, b: { w: number; h: number } | null, w: number | null, h: number) => {
    if (b === null) { bad.push(`no ${what}`); return; }
    if (Math.abs(b.h - h) > 0.5 || (w !== null && Math.abs(b.w - w) > 0.5)) bad.push(`${what} is ${String(b.w)}x${String(b.h)}, want ${w === null ? '…' : String(w)}x${String(h)}`);
  };
  want('the bar', got.bar, null, 44);
  want('the search trigger', got.search, null, 28);
  want('the bell', got.bell, 28, 28);
  want('the gear', got.gear, 28, 28);
  want('the D tile', got.tile, 18, 18);
  want('the caret', got.caret, 6, 12);
  if (got.search !== null && got.search.w > 360.5) bad.push(`the search trigger is ${String(got.search.w)} wide, at most 360`);
  if (context.id.endsWith('--in-voice')) want('the voice chip', got.chip, null, 28);
  else if (got.chip !== null) bad.push('a voice chip outside a call');
  if (context.id.endsWith('--reminders-due')) want('the due badge', got.badge, 14, 14);
  else if (got.badge !== null) bad.push('a due badge with nothing due');
  // The gear keeps its 45° hover turn while pressed (the prototype has no press state).
  if (context.id === 'shell-topbar--default') {
    await page.hover('#storybook-root .d-topbar__gear');
    await page.mouse.down();
    await page.waitForTimeout(300);
    const turn = await page.evaluate(() => {
      const el = document.querySelector('#storybook-root .d-topbar__gear');
      return el === null ? '' : getComputedStyle(el).transform;
    });
    await page.mouse.up();
    await page.mouse.move(0, 0);
    // rotate(45deg) is matrix(0.707107, 0.707107, -0.707107, 0.707107, 0, 0); a scale would shrink the terms.
    const m = /^matrix\(([-\d.]+), ([-\d.]+)/.exec(turn);
    if (m === null || Math.abs(Number(m[1]) - Math.SQRT1_2) > 0.01 || Math.abs(Number(m[2]) - Math.SQRT1_2) > 0.01) bad.push(`the pressed gear's transform is ${turn}, want rotate(45deg)`);
  }
  if (bad.length > 0) throw new Error(`${context.title} / ${context.name}: ${bad.join('; ')}`);
}

/**
 * Task 6 (Signal §5.3-§5.5, §5.10): the geometry and the computed states of the rail, the sidebar rows, the user panel, the
 * dock, the channel header, the tab bar and the phone header, read in the browser (Ruling C). Each entry is a selector and
 * what its first match must measure; `shadow` is a substring of its computed box-shadow, `radius` its top-left radius in px.
 */
type Want = { sel: string; w?: number; h?: number; radius?: number; shadow?: string; transform?: string; color?: string; background?: string };
const LIVE_RING = '0px 0px 0px 3.5px';
const SIGNAL_GEOMETRY: Record<string, Want[]> = {
  'shell-communityrail--signal': [
    { sel: '.d-community-rail', w: 56 },
    { sel: '.d-community-rail__item[aria-current="page"]', w: 38, h: 38, radius: 10, shadow: LIVE_RING, background: 'rgb(90, 168, 230)' },
    { sel: '.d-community-rail__item:not([aria-current])', w: 38, h: 38, radius: 999, background: 'rgb(20, 23, 28)' },
    { sel: '.d-community-rail__badge .d-pill', h: 16, background: 'rgb(255, 209, 106)', color: 'rgb(26, 19, 0)' },
    { sel: '.d-community-rail__glyph', w: 38, h: 38, radius: 999 },
    { sel: '.d-community-rail__rule', w: 20, h: 1 },
  ],
  'shell-communityrail--signal-home-selected': [
    { sel: '.d-community-rail__home', w: 38, h: 38, radius: 10, background: 'rgb(23, 27, 34)', color: 'rgb(90, 168, 230)' },
  ],
  'primitives-channelrow--text': [{ sel: '.d-chrow', h: 28, radius: 5, color: 'rgb(140, 147, 158)' }, { sel: '.d-chrow__glyph', color: 'rgb(92, 99, 110)' }],
  'primitives-channelrow--active': [
    { sel: '.d-chrow', h: 28, background: 'rgb(23, 27, 34)', color: 'rgb(228, 232, 238)' },
    { sel: '.d-chrow__bar', w: 2, h: 16, transform: 'none' },
    { sel: '.d-chrow__glyph', color: 'rgb(90, 168, 230)' },
  ],
  'primitives-channelrow--unread': [{ sel: '.d-chrow', color: 'rgb(228, 232, 238)' }, { sel: '.d-chrow .d-pill', h: 16, radius: 4, background: 'rgb(90, 168, 230)' }],
  'primitives-channelrow--mention': [{ sel: '.d-chrow .d-pill', h: 16, radius: 4, background: 'rgb(255, 209, 106)', color: 'rgb(26, 19, 0)' }],
  'primitives-channelrow--collapsed-but-unread': [{ sel: '.d-channel-list__cat-head', h: 26 }],
  'primitives-channelrow--voice-joined': [{ sel: '.d-voice-row', h: 28 }, { sel: '.d-voice-row__glyph', color: 'rgb(124, 255, 142)' }],
  'voice-voicechannelrow--speaking': [
    { sel: '.d-voice-row__person', h: 26 },
    { sel: '.d-voice-row__person[data-speaking="true"] .d-avatar', w: 18, h: 18, shadow: LIVE_RING },
    { sel: '.d-voice-row__person:not([data-speaking]) .d-avatar', w: 18, h: 18 },
  ],
  'voice-voicechannelrow--with-glenn': [{ sel: '.d-voice-row__person:last-child .d-avatar[data-shape="bot"]', w: 18, radius: 5 }],
  'voice-voicechannelrow--muted-deafened': [{ sel: '.d-voice-row__state[data-state="muted"]', color: 'rgb(255, 110, 110)' }],
  'voice-voicechannelrow--camera-and-live': [{ sel: '.d-voice-row__state[data-state="screen"] .d-tag', background: 'rgb(255, 110, 110)' }],
  'shell-channellist--signal': [
    { sel: '.d-channel-list__cat-head', h: 26 },
    { sel: '.d-channel-list__live', w: 6, h: 6 },
    { sel: '.d-chrow', h: 28 },
  ],
  'shell-userpanel--signal': [{ sel: '.d-user-panel', h: 64 }, { sel: '.d-user-panel .d-icon-button', w: 30, h: 30 }, { sel: '.d-user-panel__who .d-avatar', w: 30, h: 30 }],
  'shell-userpanel--signal-muted-and-deafened': [{ sel: '.d-user-panel .d-icon-button[aria-pressed="true"]', w: 30, color: 'rgb(255, 110, 110)', background: 'rgba(255, 110, 110, 0.1)' }],
  'voice-voicedock--signal': [{ sel: '.d-voice-dock', radius: 8, background: 'rgba(124, 255, 142, 0.04)' }, { sel: '.d-voice-dock__leave', w: 28, h: 28, color: 'rgb(255, 110, 110)' }, { sel: '.d-voice-dock__bars', h: 12 }, { sel: '.d-voice-dock__bars > span', w: 2 }],
  'shell-channelheader--signal-members-open': [{ sel: '.d-channel-header', h: 44 }, { sel: '.d-channel-header__actions [aria-pressed="true"]', h: 28, color: 'rgb(90, 168, 230)', background: 'rgba(90, 168, 230, 0.12)' }],
  'shell-tabbar--signal': [{ sel: '.d-tab-bar__link', h: 56 }, { sel: '.d-tab-bar__link[aria-current="page"]', color: 'rgb(228, 232, 238)' }],
  // Jonas 2026-10-27: the prototype's phone sizes (supersedes Ruling N): Threads/Members 28 high, search 36 x 28, no "⋯".
  // Task 6b fix round 1: the prototype's heights are content plus a 1px border: a button 30 + 2 = 32, the role-name field 34 + 2 = 36.
  'primitives-button--default': [{ sel: '.d-btn', h: 32 }],
  'form-textfield--filled': [{ sel: '.d-text-field__input', h: 36 }],
  // README §6.7, the prototype's bell popover (content-box + border): 342 wide, 22px tabs, 26px row icons (--warn on a due one), 26px row actions.
  'shell-reminderspopover--due': [
    { sel: '.d-reminders', w: 342, radius: 10 }, { sel: '.d-reminders__tab', h: 22 }, { sel: '.d-reminders__bot', w: 22, h: 22 },
    { sel: '.d-reminders__row[data-due="true"] .d-reminders__icon', w: 26, h: 26, color: 'rgb(255, 209, 106)', background: 'rgba(255, 209, 106, 0.14)' },
    { sel: '.d-reminders__action', h: 26 },
  ],
  // Task 8 (Ruling C, README §5.6, the prototype's values): the 64px call bar on --bg-2, its 40px controls (7px corners, --surface-tile
  // idle, the danger tint on, --accent for Camera, Leave --danger on --danger-ink); the stage's 16:9 tiles on #0E1116 with 10px
  // corners, 64px faces and the 22px caption; the focused tile's 28px Grid and full screen over a 188px filmstrip (150 on a phone),
  // its 120px face; the preview's 48px faces and its 40px green Join voice.
  'voice-callcontrols--default': [
    { sel: '.d-call-stage__controls[data-bar]', h: 64, background: 'rgb(11, 13, 16)' },
    { sel: '.d-call-controls--bar [data-control="mute"]', h: 40, radius: 7, background: 'rgb(17, 21, 26)', color: 'rgb(228, 232, 238)' },
    { sel: '.d-call-controls--bar [data-control="leave"]', h: 40, background: 'rgb(255, 110, 110)', color: 'rgb(26, 6, 6)' },
  ],
  'voice-callcontrols--muted': [{ sel: '.d-call-controls--bar [data-control="mute"]', h: 40, color: 'rgb(255, 110, 110)', background: 'rgba(255, 110, 110, 0.1)' }],
  'voice-callcontrols--camera-on': [{ sel: '.d-call-controls--bar [data-control="camera"]', h: 40, color: 'rgb(3, 18, 30)', background: 'rgb(90, 168, 230)' }],
  'voice-callstage--grid-2': [
    { sel: '.d-call-tile[data-stage]', radius: 10, background: 'rgb(14, 17, 22)' },
    { sel: '.d-call-tile__avatar .d-avatar', w: 64, h: 64 },
    { sel: '.d-call-tile[data-stage] .d-call-tile__caption', h: 22 },
  ],
  'voice-callstage--focus-share': [
    { sel: '.d-call-tile__grid', h: 28 }, { sel: '.d-call-tile__fullscreen', w: 28, h: 28 },
    { sel: '.d-call-stage__tile[data-focused="false"]', w: 188 },
  ],
  'voice-callstage--focus-person': [{ sel: '.d-call-stage__tile[data-focused="true"] .d-call-tile__avatar .d-avatar', w: 120, h: 120 }],
  'voice-callstage--not-joined-preview': [
    { sel: '.d-call-stage__face .d-avatar', w: 48, h: 48 },
    { sel: '.d-call-stage__join', h: 40, background: 'rgb(124, 255, 142)', color: 'rgb(6, 21, 10)' },
  ],
  'voice-callstage--phone': [{ sel: '.d-call-stage__tile[data-focused="false"]', w: 150 }],
  // Fix round 1: the focused share of a speaking owner (the prototype's 0.55 edge), and the person menu (236px, 9px corners, 32px items).
  'voice-callstage--focus-share-speaking': [{ sel: '.d-call-stage__tile[data-focused="true"] > .d-call-tile', shadow: 'rgba(124, 255, 142, 0.55)' }],
  'voice-personcontextmenu--person': [
    { sel: '.d-person-menu', w: 236, radius: 9, background: 'rgb(15, 18, 22)' },
    { sel: '.d-person-menu .d-action-menu__item', h: 32 }, { sel: '.d-person-menu__head .d-avatar', w: 28, h: 28 },
  ],
  'shell-phoneheader--signal': [{ sel: '.d-phone-header', h: 52 }, { sel: '.d-phone-header__back', w: 44, h: 44 },
    { sel: '.d-phone-header__actions .d-btn[aria-label="Threads"]', w: 30, h: 28 }, { sel: '.d-phone-header__actions .d-icon-button', w: 36, h: 28 }],
};
/**
 * Jonas 2026-10-27 (supersedes Ruling Q): in signal a presence dot is a solid disc in the prototype's colour (online #7CFF8E,
 * idle #FFD16A, dnd #FF6E6E, invisible/offline #767D89) with no half fill, bar or ring. The status word stays for screen readers.
 */
const SOLID_DOTS: Record<string, string> = {
  'shell-userpanel--signal': 'rgb(124, 255, 142)', 'shell-userpanel--signal-idle': 'rgb(255, 209, 106)',
  'shell-userpanel--signal-dnd': 'rgb(255, 110, 110)', 'shell-userpanel--signal-invisible': 'rgb(118, 125, 137)',
  'primitives-avatar--member': 'rgb(124, 255, 142)',
};
async function checkSolidDot(page: Page, context: StoryContext, want: string): Promise<void> {
  const got = await page.evaluate(() => {
    const el = document.querySelector('#storybook-root .d-avatar__presence .d-state-dot');
    if (el === null) return null;
    const st = getComputedStyle(el);
    return { bg: st.backgroundColor, image: st.backgroundImage, border: st.borderTopWidth, after: getComputedStyle(el, '::after').content };
  });
  if (got === null) throw new Error(`${context.title} / ${context.name}: no presence dot`);
  const bad: string[] = [];
  if (got.bg !== want) bad.push(`fill ${got.bg}, want ${want}`);
  if (got.image !== 'none') bad.push(`background-image ${got.image} (a half fill)`);
  if (got.border !== '0px') bad.push(`border ${got.border} (a ring)`);
  if (got.after !== 'none' && got.after !== 'normal') bad.push(`::after ${got.after} (a bar)`);
  if (bad.length > 0) throw new Error(`${context.title} / ${context.name}: presence dot is not solid: ${bad.join('; ')}`);
}
async function checkSignalGeometry(page: Page, context: StoryContext, wants: Want[]): Promise<void> {
  await page.waitForFunction(() => document.getAnimations().every(a => a.playState !== 'running' || a.effect?.getTiming().iterations === Infinity), undefined, { timeout: 5_000 }).catch(() => {});
  const bad = await page.evaluate((list: Want[]) => {
    const out: string[] = [];
    const near = (a: number, b: number) => Math.abs(a - b) <= 0.5;
    for (const want of list) {
      const el = document.querySelector<HTMLElement>(`#storybook-root ${want.sel}`);
      if (el === null) { out.push(`no ${want.sel}`); continue; }
      const r = el.getBoundingClientRect();
      const st = getComputedStyle(el);
      if (want.w !== undefined && !near(r.width, want.w)) out.push(`${want.sel} is ${String(r.width)} wide, want ${String(want.w)}`);
      if (want.h !== undefined && !near(r.height, want.h)) out.push(`${want.sel} is ${String(r.height)} high, want ${String(want.h)}`);
      if (want.radius !== undefined && !near(Math.min(parseFloat(st.borderTopLeftRadius), 999), want.radius) && !(want.radius === 999 && parseFloat(st.borderTopLeftRadius) >= r.width / 2)) out.push(`${want.sel} radius is ${st.borderTopLeftRadius}, want ${String(want.radius)}px`);
      if (want.shadow !== undefined && !st.boxShadow.includes(want.shadow)) out.push(`${want.sel} box-shadow is ${st.boxShadow}, want ${want.shadow}`);
      if (want.transform !== undefined && st.transform !== want.transform && st.transform !== 'matrix(1, 0, 0, 1, 0, 0)') out.push(`${want.sel} transform is ${st.transform}`);
      if (want.color !== undefined && st.color !== want.color) out.push(`${want.sel} color is ${st.color}, want ${want.color}`);
      if (want.background !== undefined && st.backgroundColor !== want.background) out.push(`${want.sel} background is ${st.backgroundColor}, want ${want.background}`);
    }
    return out;
  }, wants);
  if (bad.length > 0) throw new Error(`${context.title} / ${context.name}: ${bad.join('; ')}`);
}

/**
 * Ruling Y (prototype parity, Jonas 2026-10-27): elements whose colours are exactly the prototype's and fall below 4.5:1 in the
 * signal theme. Only axe's color-contrast rule is off for them, only in signal; every other rule still runs on them, and the
 * light and high-contrast stories get no exception.
 */
const PROTOTYPE_CONTRAST: { story: (id: string) => boolean; selector: string; why: string }[] = [
  { story: id => id.startsWith('voice-voicedock--') || id.startsWith('screens-voice--'), selector: '.d-voice-dock__where',
    why: 'voice dock stats in --fg-3 #767D89 on the --live tint (4.38:1), prototype parity, Jonas 2026-10-27' },
  // Task 7 (README §5.5): the conversation's prototype colours below 4.5:1, wherever a conversation is composed.
  ...CONVERSATION_PROTOTYPE_CONTRAST.map(e => ({ story: () => true, selector: e.selector, why: `${e.why}, prototype parity, Jonas 2026-10-27` })),
];

/**
 * Review Focus 4 (task 7, Ruling C): a reaction chip draws 22px (README §6.5) and takes a 24px hit area through its ::before.
 * Measured in a real browser: the chip's box grown by its ::before's negative insets, at least 24 x 24.
 */
async function checkReactionHitAreas(page: Page, context: StoryContext): Promise<void> {
  const got = await page.evaluate(() => Array.from(document.querySelectorAll<HTMLElement>('#storybook-root :is(.d-reaction-bar__chip, .d-reaction-bar__add)')).map(el => {
    const r = el.getBoundingClientRect();
    const b = getComputedStyle(el, '::before');
    const cs = getComputedStyle(el);
    const px = (v: string) => (v.endsWith('px') ? parseFloat(v) : 0);
    const bw = px(cs.borderTopWidth);
    return { drawn: Math.round(r.height * 10) / 10, content: b.content, position: b.position,
      h: r.height - 2 * bw - px(b.top) - px(b.bottom), w: r.width - 2 * bw - px(b.left) - px(b.right) };
  }));
  if (got.length === 0 && !context.id.endsWith('--none')) throw new Error(`${context.title} / ${context.name}: no reaction chips to measure`);
  for (const g of got) {
    if (Math.abs(g.drawn - 22) > 0.5) throw new Error(`${context.title} / ${context.name}: a reaction chip draws ${String(g.drawn)}px, want 22 (§6.5)`);
    if (g.position !== 'absolute' || g.h < 23.5 || g.w < 23.5) throw new Error(`${context.title} / ${context.name}: a reaction chip's hit area is ${String(g.w)} x ${String(g.h)}, want at least 24 x 24`);
  }
}
const REACTION_HIT_STORIES = (id: string) => id.startsWith('conversation-reactionbar--') || id === 'conversation-messagerow--signal-reactions';

export async function checkStory(page: Page, context: StoryContext): Promise<void> {
    if (context.id === 'settings-settingsframe--phone-index') {
      await page.setViewportSize({ width: 640, height: 400 });
      await page.waitForFunction(() => document.querySelector('.d-settings-frame')?.getAttribute('data-layout') === 'phone');
    }
    await injectAxe(page);
    const strict = STRICT_TARGET_TITLES.some(prefix => context.title.startsWith(prefix));
    const signal = await page.evaluate(() => document.documentElement.getAttribute('data-theme') === 'signal');
    const excepted = signal ? PROTOTYPE_CONTRAST.filter(e => e.story(context.id)).map(e => e.selector) : [];
    const present = excepted.length === 0 ? [] : await page.evaluate(sels => sels.filter(s => document.querySelector(`#storybook-root ${s}`) !== null), excepted);
    const rules = { ...(strict ? { 'target-size': { enabled: true } } : {}) };
    await checkA11y(page, present.length === 0 ? '#storybook-root' : { include: [['#storybook-root']], exclude: present.map(s => [`#storybook-root ${s}`]) }, {
      detailedReport: true, detailedReportOptions: { html: true }, includedImpacts: ['critical', 'serious'],
      ...(strict ? { axeOptions: { rules } } : {}),
    });
    // The excepted elements still pass every other rule.
    if (present.length > 0) {
      await checkA11y(page, { include: present.map(s => [`#storybook-root ${s}`]) }, {
        detailedReport: true, detailedReportOptions: { html: true }, includedImpacts: ['critical', 'serious'],
        axeOptions: { rules: { ...rules, 'color-contrast': { enabled: false } } },
      });
    }
    // Control boundaries are the prototype's exact colours in signal (Jonas 2026-10-27), never --edge or a hairline.
    const boundary = boundarySelector(context.id);
    if (boundary) {
      const edge = await page.evaluate(sel => {
        const el = document.querySelector(sel);
        return el ? { theme: document.documentElement.getAttribute('data-theme'), border: getComputedStyle(el).borderTopColor } : null;
      }, boundary.selector);
      if (!edge) throw new Error(`${context.title} / ${context.name}: no ${boundary.selector} to check`);
      if (edge.theme !== 'signal') throw new Error(`${context.title} / ${context.name}: expected theme signal, got ${edge.theme}`);
      if (edge.border !== boundary.colour) {
        throw new Error(`${context.title} / ${context.name}: ${boundary.selector} border is ${edge.border}, not the prototype's ${boundary.colour}`);
      }
    }
    if (REACTION_HIT_STORIES(context.id)) await checkReactionHitAreas(page, context);
    if (context.id.startsWith('shell-topbar--')) await checkTopBar(page, context);
    if (context.id === 'search-searchpalette--messages-results') await checkPalette(page, context);
    const solidDot = SOLID_DOTS[context.id];
    if (solidDot !== undefined) await checkSolidDot(page, context, solidDot);
    const signalWants = SIGNAL_GEOMETRY[context.id];
    if (signalWants !== undefined) await checkSignalGeometry(page, context, signalWants);
    if (context.id === 'shell-appshell--aside-close-live') await checkAsideTransition(page, context);
    const asideWant = ASIDE_STORIES[context.id];
    if (asideWant !== undefined) await checkAside(page, context, asideWant);
    // A container focused only by script, an opened dialog, draws no ring: around the whole dialog it reads
    // as an error box (design ruling 23(h), A11Y-DESIGN-11). Its operable controls keep the ring (below).
    const dialogRing = await page.evaluate(() => {
      const el = document.activeElement;
      return el instanceof HTMLDialogElement ? getComputedStyle(el).outlineStyle : null;
    });
    if (dialogRing !== null && dialogRing !== 'none') {
      throw new Error(`${context.title} / ${context.name}: the opened dialog, focused by script, draws a ring (outline-style: ${dialogRing})`);
    }
    // Focus-ring guard. The visible ring is an outline, never a box-shadow,
    // so that a component's own box-shadow state cue (the active channel
    // row's inset bar, the pressed button's underline) cannot swallow it.
    // Tab once: if anything takes focus, its computed outline must be the
    // 2px solid accent ring.
    // One exception (Jonas, 2026-10-14): the composer's textarea draws no visible ring; its box shows
    // focus instead, its border turning the resolved --accent (WCAG 2.4.7 still holds). The textarea keeps
    // a transparent outline, so under Windows forced colors it is painted and focus still shows.
    await page.keyboard.press('Tab');
    const focused = await page.evaluate(() => {
      const el = document.activeElement;
      if (!el || el === document.body || el === document.documentElement) return null;
      const s = getComputedStyle(el);
      const box = el.matches('textarea.d-composer__input') ? el.closest('.d-composer__box') : null;
      let composer: { border: string; accent: string; outlineColor: string } | null = null;
      if (box) {
        // Settle the border-color transition so the value read is the focused one.
        for (const a of box.getAnimations()) a.finish();
        const probe = document.createElement('span');
        probe.style.color = 'var(--accent)';
        box.appendChild(probe);
        const accent = getComputedStyle(probe).color;
        probe.remove();
        const b = getComputedStyle(box);
        const sides = [b.borderTopColor, b.borderRightColor, b.borderBottomColor, b.borderLeftColor];
        composer = { border: sides.every(c => c === sides[0]) ? sides[0] : sides.join(' / '), accent, outlineColor: s.outlineColor };
      }
      // Ruling AK (Jonas, task 9 fix round 1): the search palette's textbox draws no ring either (the caret shows focus).
      const palette = el.matches('.d-search-palette input') ? s.outlineColor : null;
      return { tag: el.tagName.toLowerCase(), outlineStyle: s.outlineStyle, outlineWidth: s.outlineWidth, composer, palette };
    });
    if (focused?.composer) {
      const { composer } = focused;
      const bad: string[] = [];
      if (composer.outlineColor !== 'rgba(0, 0, 0, 0)') bad.push(`outline-color ${composer.outlineColor}, expected transparent rgba(0, 0, 0, 0)`);
      if (composer.border !== composer.accent) bad.push(`box border-color ${composer.border}, expected --accent ${composer.accent}`);
      // Forced colors paint the transparent outline in a system colour: the textarea must still have one.
      await page.emulateMedia({ forcedColors: 'active' });
      try {
        const forced = await page.evaluate(() => {
          const el = document.activeElement;
          if (!el) return null;
          const s = getComputedStyle(el);
          return { style: s.outlineStyle, width: s.outlineWidth };
        });
        if (!forced || forced.style !== 'solid' || !(parseFloat(forced.width) >= 1)) {
          bad.push(`under forced colors outline-style ${forced?.style} / width ${forced?.width}, expected solid / at least 1px`);
        }
      } finally {
        await page.emulateMedia({ forcedColors: 'none' });
      }
      if (bad.length > 0) {
        throw new Error(
          `${context.title} / ${context.name}: the focused composer textarea should show focus through its box, ` +
          `with only a transparent ring (${bad.join('; ')})`,
        );
      }
    } else if (focused?.palette != null) {
      if (focused.palette !== 'rgba(0, 0, 0, 0)') throw new Error(`${context.title} / ${context.name}: the palette textbox draws a ring (${focused.palette}); Ruling AK wants none`);
    } else if (focused && (focused.outlineStyle !== 'solid' || focused.outlineWidth !== '2px')) {
      throw new Error(
        `${context.title} / ${context.name}: the focused <${focused.tag}> has no visible focus ring ` +
        `(outline-style: ${focused.outlineStyle}, outline-width: ${focused.outlineWidth}; expected solid / 2px)`,
      );
    }
    // Chrome labels (design ruling (i), task 24): the channel inside the composer's label keeps the case it was given.
    const casing = await page.evaluate(() => {
      const bad: string[] = [];
      const want = (selector: string, transform: string) => {
        for (const el of Array.from(document.querySelectorAll(`#storybook-root ${selector}`))) {
          const got = getComputedStyle(el).textTransform;
          if (got !== transform) bad.push(`${selector} "${el.textContent ?? ''}" is ${got}, expected ${transform}`);
        }
      };
      want('.d-composer__target', 'none');
      return bad;
    });
    if (casing.length > 0) throw new Error(`${context.title} / ${context.name}: ${casing.join('; ')}`);
    // Print (L-UI DOM contract; F2 allows printing the recovery key). Browsers do not print
    // backgrounds and the default theme's text is near-white, so under print media the key must be
    // the system colour CanvasText under a light scheme, and the chrome around it must not print.
    if (context.id.startsWith('ceremony-recoverykey--')) {
      await page.emulateMedia({ media: 'print' });
      try {
        const r = await page.evaluate(() => {
          const root = document.querySelector('.d-recovery-key');
          const group = document.querySelector('.d-recovery-key__group');
          const actions = document.querySelector('.d-recovery-key__actions');
          if (!root || !group || !actions) return null;
          const probe = document.createElement('span');
          probe.style.color = 'CanvasText';
          root.appendChild(probe);
          const want = getComputedStyle(probe).color;
          probe.remove();
          return { want, got: getComputedStyle(group).color, scheme: getComputedStyle(root).colorScheme, actions: getComputedStyle(actions).display };
        });
        if (!r || r.got !== r.want || r.scheme !== 'light' || r.actions !== 'none') {
          throw new Error(
            `${context.title} / ${context.name}: under print media the recovery key is not CanvasText on a light scheme ` +
            `(group colour ${r?.got}, CanvasText ${r?.want}, color-scheme ${r?.scheme}, actions display ${r?.actions})`,
          );
        }
      } finally {
        await page.emulateMedia({ media: 'screen' });
      }
    }
    // A11Y-DESIGN-01: desktop settings keep the sidebar and body as independent scroll regions. At 200% zoom,
    // the effective 640 × 400 CSS viewport enters the existing phone breakpoint: the section frame stays fullscreen,
    // with the nav's chips and the footer outside the scrolling body.
    // Task 10 (README §5.9): the 240px nav, the content column at most 720px, rows at least 56px, and the 34px close
    // circle 22px from the top and 16px from the right.
    if (context.id === 'settings-settingsframe--zoomed-in') {
      const desktop = await page.evaluate(() => {
        const frame = document.querySelector('.d-settings-frame');
        const panel = document.querySelector('.d-settings-frame__panel');
        const body = document.querySelector('.d-settings-frame__body');
        const side = document.querySelector('.d-settings-frame__side');
        const section = document.querySelector('.d-settings-frame__body > section');
        const ring = document.querySelector('.d-settings-frame__close-ring');
        const key = document.querySelector('.d-settings-frame__close-key');
        const item = document.querySelector('.d-settings-nav__item');
        if (!frame || !panel || !body || !side || !section || !ring || !key || !item) return null;
        const rect = frame.getBoundingClientRect();
        const styles = getComputedStyle(panel);
        const close = ring.getBoundingClientRect();
        const rows = Array.from(document.querySelectorAll('.d-settings-frame__body :is(.d-settings-row, .d-device-row)')).map(r => Math.round(r.getBoundingClientRect().height));
        return {
          width: Math.round(rect.width), height: Math.round(rect.height), x: Math.round(rect.x), y: Math.round(rect.y),
          columns: styles.gridTemplateColumns.trim().split(/\s+/).length,
          sideWidth: Math.round(side.getBoundingClientRect().width), sectionWidth: Math.round(section.getBoundingClientRect().width),
          itemHeight: Math.round(item.getBoundingClientRect().height),
          close: { w: Math.round(close.width), h: Math.round(close.height), top: Math.round(close.top), right: Math.round(rect.right - close.right) },
          escShown: getComputedStyle(key).display !== 'none', minRow: rows.length === 0 ? 0 : Math.min(...rows),
          sideOverflow: getComputedStyle(side).overflowY, bodyOverflow: getComputedStyle(body).overflowY,
          horizontalOverflow: document.documentElement.scrollWidth > innerWidth + 1
            || document.body.scrollWidth > innerWidth + 1 || panel.scrollWidth > panel.clientWidth + 1,
        };
      });
      if (!desktop || desktop.width !== 1280 || desktop.height !== 800 || desktop.x !== 0 || desktop.y !== 0
        || desktop.columns < 2 || desktop.sideWidth !== 240 || desktop.sectionWidth > 720 || desktop.itemHeight !== 34
        || desktop.close.w !== 34 || desktop.close.h !== 34 || desktop.close.top !== 22 || desktop.close.right !== 16 || !desktop.escShown
        || desktop.minRow < 56 || desktop.sideOverflow !== 'auto' || desktop.bodyOverflow !== 'auto' || desktop.horizontalOverflow) {
        throw new Error(`${context.title} / ${context.name}: desktop settings geometry or independent scrolling is incorrect (${JSON.stringify(desktop)})`);
      }

      // A 1280 × 800 physical display at 200% browser zoom exposes a 640 × 400 CSS viewport.
      await page.setViewportSize({ width: 640, height: 400 });
      await page.waitForFunction(() => document.querySelector('.d-settings-frame')?.getAttribute('data-layout') === 'phone');
      await checkA11y(page, '#storybook-root', {
        detailedReport: true, detailedReportOptions: { html: true }, includedImpacts: ['critical', 'serious'],
        ...(strict ? { axeOptions: { rules: { 'target-size': { enabled: true } } } } : {}),
      });
      const zoomed = await page.evaluate(() => {
        const frame = document.querySelector('.d-settings-frame');
        const panel = document.querySelector('.d-settings-frame__panel');
        const body = document.querySelector('.d-settings-frame__body');
        const side = document.querySelector('.d-settings-frame__side');
        const footer = document.querySelector('.d-settings-frame__footer');
        const ring = document.querySelector('.d-settings-frame__close-ring');
        const key = document.querySelector('.d-settings-frame__close-key');
        const items = Array.from(document.querySelectorAll('.d-settings-nav__item'));
        const heading = document.querySelector('.d-settings-nav__group');
        const last = Array.from(document.querySelectorAll('.d-settings-frame__body button')).at(-1);
        if (!frame || !panel || !body || !side || !footer || !ring || !key || items.length < 2 || !last) return null;
        const size = frame.getBoundingClientRect();
        body.scrollTop = body.scrollHeight;
        const bodyRect = body.getBoundingClientRect();
        const sideRect = side.getBoundingClientRect();
        const footerRect = footer.getBoundingClientRect();
        const lastRect = last.getBoundingClientRect();
        const close = ring.getBoundingClientRect();
        const first = items[0].getBoundingClientRect();
        return {
          width: Math.round(size.width), height: Math.round(size.height), x: Math.round(size.x), y: Math.round(size.y),
          isPhoneSection: frame.getAttribute('data-layout') === 'phone' && frame.getAttribute('data-phone-page') === 'section',
          panelColumns: getComputedStyle(panel).gridTemplateColumns.trim().split(/\s+/).length,
          bodyScrolls: body.scrollHeight > body.clientHeight + 1, bodyOverflow: getComputedStyle(body).overflowY,
          chipsInARow: items.every(i => Math.abs(i.getBoundingClientRect().top - first.top) < 1),
          headingsHidden: heading === null || getComputedStyle(heading).display === 'none' || heading.getBoundingClientRect().height === 0,
          chipsAboveBody: first.bottom <= bodyRect.top + 1 && sideRect.top >= size.top - 1,
          closeTop: Math.round(close.top - size.top), closeClear: close.bottom <= first.top + 1, escHidden: getComputedStyle(key).display === 'none',
          footerInPlace: Math.abs(footerRect.bottom - size.bottom) <= 1 && footerRect.top >= bodyRect.bottom - 1,
          lastInView: lastRect.top >= bodyRect.top - 1 && lastRect.bottom <= bodyRect.bottom + 1,
          horizontalOverflow: document.documentElement.scrollWidth > innerWidth + 1
            || document.body.scrollWidth > innerWidth + 1 || panel.scrollWidth > panel.clientWidth + 1,
        };
      });
      if (!zoomed || zoomed.width !== 640 || zoomed.height !== 400 || zoomed.x !== 0 || zoomed.y !== 0
        || !zoomed.isPhoneSection || zoomed.panelColumns !== 1 || !zoomed.bodyScrolls || zoomed.bodyOverflow !== 'auto'
        || !zoomed.chipsInARow || !zoomed.headingsHidden || !zoomed.chipsAboveBody || zoomed.closeTop !== 12 || !zoomed.closeClear
        || !zoomed.escHidden || !zoomed.footerInPlace || !zoomed.lastInView || zoomed.horizontalOverflow) {
        throw new Error(`${context.title} / ${context.name}: phone section frame breaks its chips or fullscreen scroll regions at 200% zoom (${JSON.stringify(zoomed)})`);
      }
    }
    if (context.id === 'settings-settingsframe--phone-index') {
      const index = await page.evaluate(() => {
        const frame = document.querySelector('.d-settings-frame');
        const panel = document.querySelector('.d-settings-frame__panel');
        const nav = document.querySelector('.d-settings-frame__nav');
        const close = frame?.querySelector('.d-settings-frame__close');
        if (!frame || !panel || !nav || !close) return null;
        const size = frame.getBoundingClientRect();
        const navRect = nav.getBoundingClientRect();
        const closeBefore = close.getBoundingClientRect();
        nav.scrollTop = nav.scrollHeight;
        const last = Array.from(nav.querySelectorAll('.d-settings-nav__item')).at(-1);
        if (!last) return null;
        const lastRect = last.getBoundingClientRect();
        const closeAfter = close.getBoundingClientRect();
        return {
          width: Math.round(size.width), height: Math.round(size.height), x: Math.round(size.x), y: Math.round(size.y),
          isPhoneIndex: frame.getAttribute('data-layout') === 'phone' && frame.getAttribute('data-phone-page') === 'index',
          navOverflow: getComputedStyle(nav).overflowY, navScrolls: nav.scrollHeight > nav.clientHeight + 1,
          lastInView: lastRect.top >= navRect.top - 1 && lastRect.bottom <= navRect.bottom + 1,
          closeInView: closeAfter.top >= size.top && closeAfter.bottom <= size.bottom && closeAfter.bottom <= navRect.top + 1,
          closeDoesNotMove: Math.abs(closeBefore.top - closeAfter.top) < 1,
          horizontalOverflow: document.documentElement.scrollWidth > innerWidth + 1
            || document.body.scrollWidth > innerWidth + 1 || panel.scrollWidth > panel.clientWidth + 1,
        };
      });
      if (!index || index.width !== 640 || index.height !== 400 || index.x !== 0 || index.y !== 0
        || !index.isPhoneIndex || index.navOverflow !== 'auto' || !index.navScrolls || !index.lastInView
        || !index.closeInView || !index.closeDoesNotMove || index.horizontalOverflow) {
        throw new Error(`${context.title} / ${context.name}: phone index navigation scrolls with or hides Close (${JSON.stringify(index)})`);
      }
    }
    if (context.id.startsWith('ceremony-onboardingframe--')) {
      await page.emulateMedia({ media: 'print' });
      try {
        const footer = await page.evaluate(() => {
          const el = document.querySelector('.d-onboarding-frame__footer');
          return el ? getComputedStyle(el).display : null;
        });
        if (footer !== null && footer !== 'none') {
          throw new Error(`${context.title} / ${context.name}: under print media the onboarding footer is displayed (display: ${footer})`);
        }
      } finally {
        await page.emulateMedia({ media: 'screen' });
      }
    }
}
