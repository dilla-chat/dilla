// The one place for the TypeSafe parity judge's wording, thresholds, model and price (design: the judge reads them from
// here; this file is what gets audited and edited). Question text is identical for both images of a pair: the image field
// is always `screen`, and "prototype", "app" or "story" never appear in what the model reads.

/** Bump when a template, a view phrase, a tone option or a threshold changes; it is written into every verdict. */
export const QUESTIONS_VERSION = 'judge-questions@3';

/** Pinned, not jev-latest: aliases move, and images and Box are "Dreamer-ready" (format and accuracy can change). */
export const MODEL = 'jev-1.14.0';
export const API_URL = 'https://api.typesafe.ai/v1/systemone';
/** USD per million input tokens for jev-1.14.0 (models page, 2026-10-30). Output tokens are free. */
export const USD_PER_MTOK = 0.042;
export const DEFAULT_BUDGET_USD = 0.5;

/** Image limits from the images page: each side 280-1568 px, total pixels 280^2..1568^2. Out-of-range images are rejected. */
export const LIMITS = { minSide: 280, maxSide: 1568, minPixels: 78_400, maxPixels: 2_458_624 };

/** Noul bands: >= HI reads yes, <= LO reads no, between is "not settled". Starting values from the skill. */
export const HI = 0.7;
export const LO = 0.3;

/**
 * Geometry tolerances in CSS px, set from the probe's repeatability spread (2026-10-30: two runs of 16 byte-identical
 * requests, 440 Box edges). Box edges come back in steps of BOX_STEP (0.01) of the sent image; 70 % of edges repeated
 * exactly, and the 90th percentile moved by one step. A position or size compares two independent Boxes (prototype, app),
 * so the tolerance is BOX_TOL_STEPS = 2 steps of that axis, never below the 6 px floor. On a 1568x980 desktop frame that
 * is 25.6 px across and 16 px down; on a 725x1568 phone frame 7.8 px across and 16.9 px down. G2 needs twice that.
 * Not fitted to any probe case: the median spread (0) times 3 would give the 6 px floor, which is below one step.
 */
export const BOX_STEP = 0.01;
export const BOX_TOL_STEPS = 2;
export const POS_TOL = 6;
export const SIZE_TOL_MIN = 6;
export const SIZE_TOL_FRAC = 0.08;
export const posTol = (quantum = 0) => Math.max(POS_TOL, BOX_TOL_STEPS * quantum);
export const sizeTol = (protoDim, quantum = 0) => Math.max(SIZE_TOL_MIN, BOX_TOL_STEPS * quantum, SIZE_TOL_FRAC * protoDim);

/** Pairs narrower than this are phone pairs (390 px); the prototype draws a phone frame around them. */
export const PHONE_MAX_WIDTH = 600;

/**
 * Element-list rules (checked by validateJudge; probe of 2026-10-30):
 * - `data: true` for an element whose size or place follows sample data (a code block with more lines, a message list):
 *   presence-only, no Box comparison.
 * - Phone pairs: every positioned element has an `anchor` or an `edge` ("top" or "bottom"). The prototype's bezel insets the
 *   top and the bottom by different amounts, so top and bottom elements are aligned separately.
 * - `small: true` for icons, dots and badges under about 16 CSS px: they need a `region` (a close-up crop). At 0.6 sent px per
 *   CSS px a small check mark was reported missing; with a crop the 22 px avatar badge was found.
 * - Names describe UI roles, never sample data. Name a text line by what it says ("the line saying that nobody is in the
 *   channel"), not by its colour or rank ("the grey line under ...": unsettled in the probe's wording round).
 *   When the app can show the thing more than once (a second code block), say which one: a hold-out false alarm.
 */

/** One short sentence of context the model may lack, the same on both sides. No sample data, no task wording. */
export const VIEW = {
  desktop: 'A 1280 by 800 desktop screen of a dark-themed chat app.',
  phone: 'A phone screen of a dark-themed chat app.',
  clip: 'A cut-out part of the screen of a dark-themed chat app.',
  region: 'A close-up of part of the screen of a dark-themed chat app.',
};

/** Tone (text class) options, in plain words. Coarse classes, not colour checks: exact colours stay with pixel diff. */
export const TONE_OPTIONS = {
  accent_blue_text: 'Text drawn in the accent blue colour.',
  bright_white_text: 'Text in bright white or near-white, the main text colour.',
  muted_grey_text: 'Text in a muted mid grey, used for secondary lines.',
  danger_red_text: 'Text in red, used for warnings and destructive actions.',
  dimmed_disabled_text: 'Very dim dark-grey text, as on a disabled control.',
};

// Question templates. `name` comes from the spec and names one thing on the screen; it never names sample data.
export const presentQuestion = el => ({
  type: 'noul',
  instructions: `Is there ${el.name} in \`screen\`?`,
  ...(el.criteria ? { criteria: { true: el.criteria.true, false: el.criteria.false } } : {}),
});
export const boxQuestion = el => ({ type: 'box', instructions: `${el.name} in \`screen\`` });
export const stateQuestion = (el, s) => ({
  type: 'noul',
  instructions: `Is ${el.name} in \`screen\` ${s.phrase}?`,
  ...(s.true || s.false ? { criteria: { true: s.true, false: s.false } } : {}),
});
export const copyQuestion = el => ({ type: 'noul', instructions: `Does ${el.name} in \`screen\` read exactly "${el.copy}"?` });
export const toneQuestion = el => ({ type: 'choice', instructions: `What colour is the text of ${el.name} in \`screen\`?`, criteria: { ...TONE_OPTIONS } });
