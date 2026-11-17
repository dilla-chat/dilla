// desktop-gpui-package-mac.sh runs on macOS only; these check its text where a regression would only show on a runner.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const script = readFileSync(fileURLToPath(new URL('./desktop-gpui-package-mac.sh', import.meta.url)), 'utf8');
const live = script.split('\n').filter((l) => !/^\s*#/.test(l));

test('the DMG is created at an explicit size, from the folder it holds plus headroom', () => {
  // hdiutil's own -srcfolder estimate fell short of the app on a macOS runner (PR #179: "No space left on device" on
  // /Volumes/Dilla with the disk 100 GB free).
  const creates = live.filter((l) => /\bhdiutil create\b/.test(l));
  assert.equal(creates.length, 1, creates.join('\n'));
  assert.match(creates[0], /-srcfolder "\$dmgroot"/);
  assert.match(creates[0], /-size "\$\{dmg_mb\}m"/);
  assert.ok(live.some((l) => l.includes('dmg_kb=$(du -sk "$dmgroot" | cut -f 1)')), 'the size comes from the folder');
  assert.ok(live.some((l) => l.includes('dmg_mb=$(( dmg_kb * 5 / 4 / 1024 + 32 ))')), 'a quarter and 32 MB of headroom');
});
