import { test } from 'node:test';
import { strict as assert } from 'node:assert';
import { spawnSync } from 'node:child_process';
import { resolve } from 'node:path';
const config = resolve(import.meta.dirname, '../electron-builder.native-linux.cjs');
function load(version) {
 const env = { ...process.env }; if (version === undefined) delete env.DILLA_NATIVE_BUILD_VERSION; else env.DILLA_NATIVE_BUILD_VERSION = version;
 return spawnSync(process.execPath, ['-e', `const c=require(${JSON.stringify(config)});process.stdout.write(JSON.stringify({metadata:c.extraMetadata,deb:c.deb.fpm,rpm:c.rpm.fpm}))`], { env, encoding: 'utf8' });
}
test('native acceptance build version binds ASAR metadata and package-manager versions', () => {
 const result = load('1.2.3-beta.4'); assert.equal(result.status, 0, result.stderr);
 const c = JSON.parse(result.stdout); assert.equal(c.metadata.version, '1.2.3-beta.4'); assert.ok(c.deb.includes('1.2.3~beta.4')); assert.ok(c.rpm.includes('1.2.3')); assert.ok(c.rpm.includes('0.beta.4.1'));
});
test('unsupported explicit native version fails closed', () => { assert.notEqual(load('1.2.3;evil').status, 0); assert.notEqual(load('').status, 0); });
test('ordinary native build retains package version', () => { const r = load(); assert.equal(r.status, 0, r.stderr); assert.equal(JSON.parse(r.stdout).metadata.version, undefined); });
