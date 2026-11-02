import { execFileSync } from 'node:child_process';
import { mkdir } from 'node:fs/promises';
import { resolve } from 'node:path';
const root = resolve(import.meta.dirname, '..');
await mkdir(resolve(root, 'native-bin'), { recursive: true });
if (process.platform === 'linux') {
  const flags = execFileSync('pkg-config', ['--cflags', '--libs', 'libpipewire-0.3'], { encoding: 'utf8' }).trim().split(/\s+/);
  execFileSync(process.env.CXX ?? 'c++', ['-std=c++17', '-O2', '-Wall', '-Wextra', '-Werror', resolve(root, 'native/linux.cpp'), '-o', resolve(root, 'native-bin/dilla-audio'), ...flags], { stdio: 'inherit' });
} else if (process.platform === 'win32') {
  const env = { ...process.env };
  if (!env.VCINSTALLDIR) {
    const vswhere = resolve(process.env['ProgramFiles(x86)'] ?? 'C:/Program Files (x86)', 'Microsoft Visual Studio/Installer/vswhere.exe');
    const installation = execFileSync(vswhere, ['-latest', '-products', '*', '-requires', 'Microsoft.VisualStudio.Component.VC.Tools.x86.x64', '-property', 'installationPath'], { encoding: 'utf8' }).trim();
    if (!installation) throw new Error('Visual Studio C++ build tools are required');
    const bat = resolve(installation, 'VC/Auxiliary/Build/vcvars64.bat');
    const vars = execFileSync(process.env.ComSpec ?? 'cmd.exe', ['/d', '/s', '/c', `call "${bat}" >nul && set`], { windowsVerbatimArguments: true, encoding: 'utf8' });
    for (const line of vars.split(/\r?\n/)) { const split = line.indexOf('='); if (split > 0) env[line.slice(0, split)] = line.slice(split + 1); }
  }
  execFileSync('cl.exe', ['/nologo', '/std:c++17', '/EHsc', '/O2', '/W4', resolve(root, 'native/windows.cpp'), '/Fe:' + resolve(root, 'native-bin/dilla-audio.exe'), 'ole32.lib', 'mmdevapi.lib', 'runtimeobject.lib'], { env, cwd: resolve(root, 'native-bin'), stdio: 'inherit' });
} else if (process.platform === 'darwin') {
  execFileSync('swiftc', ['-O', '-parse-as-library', '-target', `${process.arch === 'arm64' ? 'arm64' : 'x86_64'}-apple-macos13.0`, resolve(root, 'native/AudioCapture.swift'), '-o', resolve(root, 'native-bin/dilla-audio')], { stdio: 'inherit' });
  execFileSync(process.execPath, [resolve(root, 'scripts/build-sparkle.mjs')], { stdio: 'inherit' });
} else throw new Error('Unsupported desktop OS');
