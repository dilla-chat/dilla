import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { readFile, mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { fixtureRuntimePath, fixtureSandbox } from './support/fixture-update-runtime.ts';
import { fixtureCacheSource } from './support/fixture-cache-portability.ts';

const desktop = resolve(import.meta.dirname, '..'), cache = join(desktop, 'src/linux-sandbox-cache.ts');
test('only the Windows test graph adapts unsupported POSIX cache ownership checks', async t => {
 const raw = await readFile(cache,'utf8'), windows = fixtureCacheSource(raw,'win32');
 assert.equal(fixtureCacheSource(raw,'linux'),raw); assert.equal(fixtureCacheSource(raw,'darwin'),raw);
 const compiled=await readFile(fixtureRuntimePath,'utf8'), boundary=compiled.indexOf('Unsafe sandbox cache directory'); assert.ok(boundary>0);
 const check=compiled.slice(boundary-180,boundary);
 if (process.platform === 'win32') assert.doesNotMatch(check,/s\.uid !== process\.getuid/); else assert.match(check,/s\.uid !== process\.getuid/);
 assert.notEqual(windows,raw); assert.throws(()=>fixtureCacheSource(raw.replace('s.uid !== process.getuid?.()', 'unexpected'), 'win32'),/boundary/i);
 assert.match(windows,/!s\.isDirectory\(\) \|\| s\.isSymbolicLink\(\)/); assert.match(windows,/!s\.isFile\(\) \|\| s\.nlink !== 1/);
 const root = await mkdtemp(join(tmpdir(),'dilla-cache-portability-')); t.after(()=>rm(root,{recursive:true,force:true}));
 async function compile(name:string, source:string) {
  const out=join(root,name+'.mjs'); await build({entryPoints:[cache],outfile:out,bundle:true,format:'esm',platform:'node',target:'node24',define:{'process.getuid':'undefined'},plugins:[{name:'fixture-cache',setup(plugin){plugin.onLoad({filter:/[\\/]linux-sandbox-cache\.ts$/},()=>({contents:source,loader:'ts',resolveDir:join(desktop,'src')}));}}]});
  return await import(pathToFileURL(out).href) as typeof import('../src/linux-sandbox-cache.ts');
 }
 const original=await compile('original',raw), adapted=await compile('adapted',windows), data=fixtureSandbox('1.0.1');
 const unchanged=join(root,'original-profile'), profile=join(root,'fixture-profile'); await mkdir(unchanged); await mkdir(profile);
 await assert.rejects(original.saveSandboxSidecars(unchanged,data),/Unsafe sandbox cache directory/);
 const nonDirectory=join(root,'not-directory'); await writeFile(nonDirectory,'x'); await assert.rejects(adapted.readSandboxSidecars(nonDirectory,data),/Unsafe sandbox cache directory/);
 const directory=await adapted.saveSandboxSidecars(profile,data); assert.equal((await adapted.readSandboxSidecars(directory,data)).generation,1);
 await writeFile(join(directory,'signature'),Buffer.from('A'.repeat(88))); await assert.rejects(adapted.readSandboxSidecars(directory,data),/sandbox/i);
});
