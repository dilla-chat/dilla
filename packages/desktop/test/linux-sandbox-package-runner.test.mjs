import {test} from 'node:test';
import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {mkdtemp,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join,resolve} from 'node:path';
const script=resolve(import.meta.dirname,'../scripts/test-linux-sandbox-package.mjs');
test('mutation runner fails if the canonical artifacts do not exist',async()=>{
 const dir=await mkdtemp(join(tmpdir(),'dilla-mutation-runner-'));
 try {assert.throws(()=>execFileSync(process.execPath,[script,dir],{stdio:'pipe'}), error=>/missing canonical AppImage artifact/i.test(String(error.stderr)));} finally {await rm(dir,{recursive:true,force:true});}
});
test('mutation runner executes both real artifact tests without skips',{skip:!process.env.DILLA_INSPECT_FIXTURE_DIRECTORY},()=>{
 const output=execFileSync(process.execPath,[script,process.env.DILLA_INSPECT_FIXTURE_DIRECTORY],{encoding:'utf8',timeout:180000});
 assert.match(output,/# tests 2\b/);assert.match(output,/# pass 2\b/);assert.match(output,/# skipped 0\b/);
});
