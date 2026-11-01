import { test } from 'node:test';
import assert from 'node:assert/strict';
import { snapshotUninstall, restoreUninstall, type RegistryCommands } from '../src/update-registry.ts';
const root = 'C:\\Users\\fixture\\AppData\\Local\\Programs\\Dilla', prefix = 'HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\', key = prefix + '{dilla-fixture}';
test('Windows snapshots only the HKCU uninstall entry whose exact InstallLocation owns this program', async () => {
 const values = new Map([[key, root], [prefix + '{other}', root + '-other']]);
 const registry: RegistryCommands = { list: async () => [...values.keys()], read: async (entry, name) => name === 'InstallLocation' ? { type: 'REG_SZ', data: values.get(entry)! } : name === 'DisplayVersion' ? { type: 'REG_SZ', data: '1.0.0' } : { type: 'REG_DWORD', data: '0x12' }, write: async () => { throw new Error('snapshot mutated registry'); } };
 assert.deepEqual(await snapshotUninstall(root, registry), { key, location: root, displayVersion: { type: 'REG_SZ', data: '1.0.0' }, estimatedSize: { type: 'REG_DWORD', data: '0x12' } });
 await assert.rejects(snapshotUninstall(root + '\\nested', registry), /owned/);
});
test('rollback restores only owned metadata and refuses a moved InstallLocation, HKLM key or extra fields', async () => {
 const writes: unknown[] = [];
 let location = root;
 const registry: RegistryCommands = { list: async () => [key], read: async () => ({ type: 'REG_SZ', data: location }), write: async (...args) => { writes.push(args); } };
 const saved = { key, location: root, displayVersion: { type: 'REG_SZ' as const, data: '1.0.0' }, estimatedSize: { type: 'REG_DWORD' as const, data: '0x12' } };
 await restoreUninstall(root, saved, registry);
 assert.deepEqual(writes, [[key, 'DisplayVersion', saved.displayVersion], [key, 'EstimatedSize', saved.estimatedSize]]);
 location = root + '-other'; await assert.rejects(restoreUninstall(root, saved, registry), /owned/);
 await assert.rejects(restoreUninstall(root, { ...saved, key: key.replace('HKCU', 'HKLM') }, registry), /snapshot/);
 await assert.rejects(restoreUninstall(root, { ...saved, arbitrary: 'C:\\Windows' } as typeof saved, registry), /snapshot/);
});

test('committed Windows metadata changes only the owned version and active-program size, never shell/uninstall identity',async()=>{
 const {updateUninstallVersion}=await import('../src/update-registry.ts'),writes:unknown[]=[];
 const registry:RegistryCommands={list:async()=>[key],read:async()=>({type:'REG_SZ',data:root}),write:async(...args)=>{writes.push(args);}};
 const saved={key,location:root,displayVersion:{type:'REG_SZ' as const,data:'1.0.0'},estimatedSize:{type:'REG_DWORD' as const,data:'0x12'}};
 await updateUninstallVersion(root,saved,'1.0.1',2049,registry);
 assert.deepEqual(writes,[[key,'DisplayVersion',{type:'REG_SZ',data:'1.0.1'}],[key,'EstimatedSize',{type:'REG_DWORD',data:'3'}]]);
 await assert.rejects(updateUninstallVersion(root,saved,'../uninstall',1,registry),/version|size/);assert.equal(writes.length,2);
});
