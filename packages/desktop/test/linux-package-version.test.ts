import test from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
// @ts-ignore Build script module is also consumed directly by release tooling.
import { linuxPackageVersion } from '../scripts/linux-package-version.mjs';

const versions = ['0.1.0-beta.1', '0.1.0-beta.2', '0.1.0-beta.10', '0.1.0', '0.1.1'];
test('maps supported semantic versions to exact native versions and architectures', () => {
 assert.deepEqual(linuxPackageVersion('0.1.0-beta.10', 'x64'), {
  upstreamVersion: '0.1.0-beta.10', channel: 'beta', arch: 'x64',
  deb: { version: '0.1.0~beta.10-1', arch: 'amd64' },
  rpm: { version: '0.1.0', release: '0.beta.10.1', arch: 'x86_64' },
  aur: { pkgver: '0.1.0beta10', pkgrel: '1', arch: 'x86_64' },
 });
 assert.equal(linuxPackageVersion('0.1.0', 'x64').deb.version, '0.1.0-1');
 assert.equal(linuxPackageVersion('0.1.0', 'x64').rpm.release, '1');
 assert.equal(linuxPackageVersion('0.1.0', 'x64').aur.pkgver, '0.1.0');
 assert.equal(linuxPackageVersion('0.1.0-beta.0', 'x64').deb.version, '0.1.0~beta.0-1');
});
test('rejects unsupported semver, unsafe numbers and architecture', () => {
 for (const version of ['v0.1.0', '0.1', '01.1.0', '0.1.0-beta.01', '0.1.0-rc.1', '0.1.0+build', '0.1.0-beta', '0.1.0\n', '9007199254740992.0.0', '0.1.0-beta.9007199254740992', '', null]) {
  assert.throws(() => linuxPackageVersion(version, 'x64'), /version/);
 }
 for (const arch of ['arm64', 'amd64', '', null]) assert.throws(() => linuxPackageVersion('0.1.0', arch), /architecture/);
});
for (const manager of ['deb', 'rpm', 'aur']) {
 const command = manager === 'deb' ? 'dpkg' : manager === 'rpm' ? 'rpm' : 'vercmp';
 const available = spawnSync(command, ['--version']).error === undefined;
 test(`real ${command} orders beta.1 < beta.2 < beta.10 < final < next patch`, { skip: !available && `${command} unavailable; native comparator proof remains required in disposable VM` }, () => {
  const mapped = versions.map(version => linuxPackageVersion(version, 'x64'));
  for (let index = 1; index < mapped.length; index++) {
   const previous = mapped[index - 1], current = mapped[index];
   const args = manager === 'deb' ? ['--compare-versions', previous.deb.version, 'lt', current.deb.version]
    : manager === 'aur' ? [`${previous.aur.pkgver}-${previous.aur.pkgrel}`, `${current.aur.pkgver}-${current.aur.pkgrel}`]
    : ['--eval', `%{lua:local result=rpm.vercmp('${previous.rpm.version}', '${current.rpm.version}'); if result == 0 then result=rpm.vercmp('${previous.rpm.release}', '${current.rpm.release}') end; print(result)}`];
   const result = spawnSync(command, args, { encoding: 'utf8' });
   assert.equal(result.status, 0, result.stderr);
   if (manager !== 'deb') assert.equal(Number(result.stdout.trim()), -1, `${command}: ${versions[index - 1]} < ${versions[index]}`);
  }
 });
}
