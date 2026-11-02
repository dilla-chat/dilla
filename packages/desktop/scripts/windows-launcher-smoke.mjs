import { generateKeyPairSync } from 'node:crypto';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { convertIcon } from 'app-builder-lib/out/util/iconConverter.js';
import { compileNativeLauncher } from './windows-package.mjs';

const desktop=resolve(import.meta.dirname,'..');
// The same pinned converter and source used by WinPackager.getIconPath().
export function parseSmokeArguments(args) {
 if(args.length===0) return false;
 if(args.length===1 && args[0]==='--capture-coff') return true;
 throw new Error('Usage: windows-launcher-smoke.mjs [--capture-coff]');
}
const captureFile=join(desktop,'test-results','windows-launcher-smoke-failure.syso');
async function writeCapture(file,bytes) { await mkdir(join(desktop,'test-results'),{recursive:true}); await writeFile(file,bytes); }
export async function runWindowsLauncherSmoke({platform=process.platform,compileLauncher=compileNativeLauncher,captureCoff=false,writeCapture:saveCapture=writeCapture}={}) {
 if(platform!=='win32') throw new Error('Windows launcher smoke requires native Windows');
 const root=await mkdtemp(join(tmpdir(),'dilla-launcher-smoke-'));
 let primaryError;
 try {
  const icon=await convertIcon({sources:[join(desktop,'assets/icon.png')],fallbackSources:[],roots:[join(desktop,'build'),desktop],format:'ico',outDir:join(root,'icon')});
  if(icon.isFallback || icon.icons.length!==1 || resolve(icon.icons[0].file ?? '')!==join(root,'icon','icon.ico')) throw new Error('Production Windows icon conversion failed');
  const bytes=await readFile(icon.icons[0].file);
  if(bytes.length<6 || !bytes.subarray(0,4).equals(Buffer.from([0,0,1,0])) || bytes.readUInt16LE(4)===0) throw new Error('Production Windows icon conversion failed');
  const publicKey=generateKeyPairSync('ed25519').publicKey.export({type:'spki',format:'pem'}).toString();
  const config={format:1,appId:'dev.dilla.desktop.smoke',productName:'DillaSmoke',channel:'stable',arch:'x64',initialVersion:'1.0.0',initialManifestSha512:Buffer.alloc(64).toString('base64'),publicKeys:[publicKey]};
  const onCoffLinkFailure=captureCoff?bytes=>saveCapture(captureFile,bytes):undefined;
  await compileLauncher({file:join(root,'Dilla.exe'),config,iconFile:icon.icons[0].file,onCoffLinkFailure});
 } catch(error) { primaryError=error; throw error; }
 finally {
  try { await rm(root,{recursive:true,force:true,maxRetries:10,retryDelay:100}); }
  catch(error) { if(!primaryError) throw error; console.error('Windows launcher smoke cleanup failed'); }
 }
}
if(process.argv[1] && import.meta.url===pathToFileURL(resolve(process.argv[1])).href) await runWindowsLauncherSmoke({captureCoff:parseSmokeArguments(process.argv.slice(2))});
