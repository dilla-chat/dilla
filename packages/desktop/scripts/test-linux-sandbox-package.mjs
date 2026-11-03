// Required CI mutation checks for the exact trusted locally-built AppImage.
import { execFileSync } from 'node:child_process';
import { lstat, readFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { inspectAppImage, withExtractedAppImage } from './inspect-linux-sandbox-package.mjs';

if (process.platform !== 'linux' || process.arch !== 'x64' || process.argv.length !== 3) throw new Error('Usage: test-linux-sandbox-package.mjs ARTIFACT_DIRECTORY (Linux x64)');
const desktop=resolve(dirname(fileURLToPath(import.meta.url)),'..');
const version=JSON.parse(await readFile(join(desktop,'package.json'),'utf8')).version;
if (!/^(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)(?:-beta\.(?:0|[1-9]\d*))?$/.test(version)) throw new Error('Invalid desktop version');
const prefix=join(resolve(process.argv[2]),`dilla-${version}-linux-x64`);
const [image,capsuleFile,manifestFile]=[prefix+'.AppImage',prefix+'.sandbox',prefix+'.sandbox.json'];
for (const file of [image,capsuleFile,manifestFile]) {
 try {const st=await lstat(file);if(!st.isFile()||st.isSymbolicLink()||st.nlink!==1) throw new Error('unsafe');}
 catch {throw new Error('Missing canonical AppImage artifact: '+file);}
}
await inspectAppImage({image,capsuleFile,manifestFile});
await withExtractedAppImage(image,async root=>{
 const env={...process.env,DILLA_INSPECT_FIXTURE_IMAGE:image,DILLA_INSPECT_FIXTURE_CAPSULE:capsuleFile,DILLA_INSPECT_FIXTURE_MANIFEST:manifestFile,DILLA_INSPECT_FIXTURE_ROOT:root};
 delete env.NODE_TEST_CONTEXT;
 const output=execFileSync(process.execPath,['--test','--test-reporter=tap',join(desktop,'test/inspect-linux-sandbox-package.test.mjs')],{env,encoding:'utf8',timeout:180000,maxBuffer:1024*1024});
 process.stdout.write(output);
 for(const [label,count] of [['tests',2],['pass',2],['fail',0],['skipped',0]]) if(!new RegExp(`^# ${label} ${count}$`,'m').test(output)) throw new Error('Required AppImage mutation tests were not all executed and passed');
});
