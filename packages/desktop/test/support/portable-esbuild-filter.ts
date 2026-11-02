import assert from 'node:assert/strict';
import vm from 'node:vm';
import { build } from 'esbuild';

/** Exercise esbuild's Go-regexp dispatch on both operating-system path forms. */
export async function assertPortableEsbuildFilter(filter: RegExp, name: string): Promise<void> {
 for (const path of [`/repo/src/${name}.ts`, `D:\\repo\\src\\${name}.ts`]) {
  let matches = 0;
  const result = await build({ stdin: { contents: "import value from 'fixture'; globalThis.result = value;", resolveDir: process.cwd() }, bundle: true, write: false, format: 'cjs', platform: 'node', plugins: [{ name: 'portable-filter-probe', setup(plugin) {
   plugin.onResolve({filter: /^fixture$/}, () => ({path, namespace:'fixture'}));
   plugin.onLoad({filter, namespace:'fixture'}, () => { matches++; return {contents:'export default 42',loader:'js'}; });
  }}] });
  const context = vm.createContext({ result: 0 });vm.runInContext(result.outputFiles[0].text,context);
  assert.equal(context.result,42);assert.equal(matches,1);
 }
}
