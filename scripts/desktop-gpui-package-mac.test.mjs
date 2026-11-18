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

test('arrays that may be empty expand with a guard: bash 3.2 calls an empty "${a[@]}" unbound under set -u', () => {
  // alpha.1's first signed build died on "${metadata_args[@]}" (sign run 44241908836). sign_flags and notary are
  // never empty where they expand; every other array must use ${a[@]+"${a[@]}"}.
  const bare = live
    .map((l) => l.replace(/\$\{(\w+)\[@\]\+"\$\{\1\[@\]\}"\}/g, ''))
    .filter((l) => /"\$\{\w+\[@\]\}"/.test(l) && !/"\$\{(sign_flags|notary)\[@\]\}"/.test(l));
  assert.deepEqual(bare, []);
  assert.ok(live.some((l) => l.includes('${metadata_args[@]+"${metadata_args[@]}"}')));
  assert.ok(live.some((l) => l.includes('${existing[@]+"${existing[@]}"}')));
});

test('a fatal error never exits 0: bash 3.2 enters the EXIT trap with $? = 0 after one', () => {
  assert.ok(live.includes('finished=0'));
  assert.ok(live.some((l) => l.includes('if [ "$finished" != 1 ] && [ "$status" = 0 ]; then status=1; fi')));
  assert.equal(live.filter((l) => l.trim()).at(-1), 'finished=1', 'the flag is the last line');
});
