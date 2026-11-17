#!/usr/bin/env node
// The shared libraries the Linux dilla-desktop binary links (its DT_NEEDED entries), each mapped to the package that
// provides it on Debian/Ubuntu, Fedora and Arch. desktop-gpui-linux-deps.test.mjs checks that nfpm.yaml's deb and rpm
// depends and the AUR recipe's DEPENDS name every one of these packages, and the release build runs this script on
// the binary it just built, so a library linked without its package fails the build, not a user's install.
//
// alpha.1's first smoke run failed this way: libgio-2.0.so.0 (GLib, through the PipeWire client) had no deb
// dependency, and slim Ubuntu and Debian images do not carry it. Vulkan, Wayland, X11, libdrm and libgbm are opened
// at runtime (dlopen), so they are not DT_NEEDED; nfpm.yaml depends on them by hand.
//
// Usage: desktop-gpui-linux-deps.mjs <binary>
// Exit: 0 every linked library is known, 1 one is not, 2 usage or tooling error.
import { execFileSync } from 'node:child_process';
import { pathToFileURL } from 'node:url';
import { resolve } from 'node:path';

const GLIBC = { deb: 'libc6 (>= 2.39)', rpm: 'glibc >= 2.39', aur: 'glibc' };
const GLIB = { deb: 'libglib2.0-0t64 | libglib2.0-0', rpm: 'glib2', aur: 'glib2' };

/** soname -> the package that provides it, per format, spelled exactly as nfpm.yaml and DEPENDS spell it. */
export const LINKED_LIBRARIES = {
  'ld-linux-x86-64.so.2': GLIBC,
  'libc.so.6': GLIBC,
  'libm.so.6': GLIBC,
  'libgcc_s.so.1': { deb: 'libgcc-s1', rpm: 'libgcc', aur: 'gcc-libs' },
  'libxcb.so.1': { deb: 'libxcb1', rpm: 'libxcb', aur: 'libxcb' },
  'libxkbcommon.so.0': { deb: 'libxkbcommon0', rpm: 'libxkbcommon', aur: 'libxkbcommon' },
  'libxkbcommon-x11.so.0': { deb: 'libxkbcommon-x11-0', rpm: 'libxkbcommon-x11', aur: 'libxkbcommon-x11' },
  'libasound.so.2': { deb: 'libasound2t64 | libasound2', rpm: 'alsa-lib', aur: 'alsa-lib' },
  'libpipewire-0.3.so.0': { deb: 'libpipewire-0.3-0t64 | libpipewire-0.3-0', rpm: 'pipewire-libs', aur: 'libpipewire' },
  'libglib-2.0.so.0': GLIB,
  'libgobject-2.0.so.0': GLIB,
  'libgio-2.0.so.0': GLIB,
};

/** The DT_NEEDED sonames in `readelf -d` output. */
export function neededLibraries(readelfOutput) {
  return [...readelfOutput.matchAll(/\(NEEDED\)\s+Shared library: \[([^\]]+)\]/g)].map((m) => m[1]);
}

/** The sonames in `needed` that LINKED_LIBRARIES does not map to a package. */
export function unknownLibraries(needed) {
  return needed.filter((soname) => !Object.hasOwn(LINKED_LIBRARIES, soname));
}

// Run directly, on every OS: a Windows path (D:\a\...) or one with a space never equals `file://` + argv[1].
if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  const args = process.argv.slice(2);
  if (args.length !== 1) {
    console.error('desktop-gpui-linux-deps: usage: desktop-gpui-linux-deps.mjs <binary>');
    process.exit(2);
  }
  let output;
  try {
    output = execFileSync('readelf', ['-d', args[0]], { encoding: 'utf8' });
  } catch (error) {
    console.error(`desktop-gpui-linux-deps: readelf -d ${args[0]} failed: ${error.message}`);
    process.exit(2);
  }
  const needed = neededLibraries(output);
  if (needed.length === 0) {
    console.error(`desktop-gpui-linux-deps: ${args[0]} links no shared library; not a dynamic ELF binary?`);
    process.exit(2);
  }
  const unknown = unknownLibraries(needed);
  for (const soname of unknown) {
    console.error(`desktop-gpui-linux-deps: ${args[0]} links ${soname}, which no package dependency covers; add it to LINKED_LIBRARIES and to nfpm.yaml and the AUR DEPENDS`);
  }
  if (unknown.length > 0) process.exit(1);
  console.log(`desktop-gpui-linux-deps: every library ${args[0]} links has its package (${needed.join(', ')})`);
}
