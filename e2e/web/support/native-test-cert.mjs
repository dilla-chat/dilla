import { execFileSync } from 'node:child_process';
import { mkdir, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';

// Caller supplies fresh, disposable paths. Never overwrites an NSS database.
export async function createTestCertificate(directory, nssdb) {
 directory = resolve(directory); nssdb = resolve(nssdb);
 await mkdir(nssdb, { mode: 0o700 });
 await mkdir(directory, { mode: 0o700 });
 const ca = join(directory, 'ca.pem'), caKey = join(directory, 'ca-key.pem'), key = join(directory, 'server-key.pem'), cert = join(directory, 'server.pem');
 const run = (command, args) => execFileSync(command, args, { stdio: 'ignore' });
 run('openssl', ['req','-x509','-newkey','rsa:2048','-nodes','-keyout',caKey,'-out',ca,'-days','1','-subj','/CN=Dilla disposable CI CA','-addext','basicConstraints=critical,CA:TRUE','-addext','keyUsage=critical,keyCertSign,cRLSign']);
 run('openssl', ['req','-new','-newkey','rsa:2048','-nodes','-keyout',key,'-out',join(directory,'server.csr'),'-subj','/CN=127.0.0.1']);
 await writeFile(join(directory,'server.ext'),'subjectAltName=IP:127.0.0.1\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n',{ mode:0o600, flag:'wx' });
 run('openssl', ['x509','-req','-in',join(directory,'server.csr'),'-CA',ca,'-CAkey',caKey,'-CAcreateserial','-out',cert,'-days','1','-extfile',join(directory,'server.ext')]);
 run('certutil', ['-N','-d',`sql:${nssdb}`,'--empty-password']);
 run('certutil', ['-A','-d',`sql:${nssdb}`,'-n','dilla-test-ca','-t','C,,','-i',ca]);
 return { ca, cert, key };
}
