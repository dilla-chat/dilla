/** Exact release packaging versions; no lossy reverse mapping is supported. */
export function linuxPackageVersion(version, arch) {
 const match = typeof version === 'string' && /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/.exec(version);
 if (!match || match[0] !== version || !match.slice(1).filter(value => value !== undefined).every(value => Number.isSafeInteger(Number(value)))) throw new Error('Unsupported native package version');
 if (arch !== 'x64') throw new Error('Unsupported native package architecture');
 const base = `${match[1]}.${match[2]}.${match[3]}`, beta = match[4];
 return {
  upstreamVersion: version, channel: beta === undefined ? 'stable' : 'beta', arch,
  deb: { version: `${base}${beta === undefined ? '' : `~beta.${beta}`}-1`, arch: 'amd64' },
  rpm: { version: base, release: beta === undefined ? '1' : `0.beta.${beta}.1`, arch: 'x86_64' },
  aur: { pkgver: `${base}${beta === undefined ? '' : `beta${beta}`}`, pkgrel: '1', arch: 'x86_64' },
 };
}
