import assert from 'node:assert/strict';
import { test } from 'node:test';
import { generate, parseColor, stale, toHsl } from './gen-gpui-tokens.mjs';

test('the committed tokens.rs, icon_names.rs and icon SVGs are what the generator writes', async () => {
  assert.deepEqual(await stale(), [], 'stale generated files: run node scripts/gen-gpui-tokens.mjs');
});

test('the generator emits 100 icons and tokens.rs', async () => {
  const files = [...(await generate()).keys()];
  assert.equal(files.filter((p) => p.endsWith('.svg')).length, 100);
  assert.ok(files.some((p) => p.endsWith('src/tokens.rs')));
});

test('colour parsing and the HSL conversion', () => {
  assert.deepEqual(parseColor('#08090B'), [8, 9, 11, 1]);
  assert.deepEqual(parseColor('rgba(90,168,230,.1)'), [90, 168, 230, 0.1]);
  assert.deepEqual(toHsl([255, 0, 0]), [0, 1, 0.5]);
  assert.deepEqual(toHsl([255, 255, 255]), [0, 0, 1]);
});
