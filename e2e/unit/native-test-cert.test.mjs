import { test } from 'node:test';
import { strict as assert } from 'node:assert';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { execFileSync } from 'node:child_process';
import { createTestCertificate } from '../web/support/native-test-cert.mjs';

test('test CA is generated in a new private NSS database and cannot replace it', async () => {
 const root = await mkdtemp(join(tmpdir(),'dilla-cert-'));
 try {
  const result = await createTestCertificate(join(root,'certs'), join(root,'nssdb'));
  assert.match(await readFile(result.ca,'utf8'), /BEGIN CERTIFICATE/);
  assert.match(execFileSync('certutil',['-L','-d',`sql:${join(root,'nssdb')}`],{encoding:'utf8'}), /dilla-test-ca/);
  await assert.rejects(createTestCertificate(join(root,'other'), join(root,'nssdb')));
 } finally { await rm(root,{recursive:true,force:true}); }
});
