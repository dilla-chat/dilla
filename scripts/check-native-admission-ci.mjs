const command = 'xvfb-run -a bash e2e/web/support/installed-keyring.sh bash packages/desktop/scripts/with-window-manager.sh npm exec -w @dilla/e2e -- playwright test --config playwright.native-admission.config.ts --project=native-admission --workers=1 --max-failures=1';
const condition = "runner.os == 'Linux' && matrix.os == 'ubuntu-24.04' && matrix.arch == 'x64' && matrix.shard == 1";
export const run = `      - name: Full-client native admission\n        if: ${condition}\n        env:\n          CHROME_DEVEL_SANDBOX: \${{ github.workspace }}/node_modules/electron/dist/chrome-sandbox\n        run: ${command}`;
export const upload = `      - name: Retain full-client native admission outcome\n        if: (success() || failure()) && ${condition}\n        uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7\n        with:\n          name: desktop-native-admission-outcome-linux-x64\n          path: e2e/native-admission-results/**/attachments/native-admission-*.json\n          if-no-files-found: error`;
export function checkNativeAdmissionWiring(text, pkg) {
  const client = text.split('\n  client:\n')[1]?.split(/\n  [a-z][a-z0-9-]*:\n/)[0] ?? '';
  const count = (s, needle) => s.split(needle).length - 1;
  if (count(text, 'playwright.native-admission.config.ts') !== 1 || count(client, `${run}\n`) !== 1 || count(client, `${upload}\n`) !== 1) return false;
  const at = client.indexOf(run), proof = client.indexOf(upload);
  const before = ['name: dilla-e2e-tools-', 'node scripts/e2e-test-tools.mjs verify ', 'name: dilla-web-dist', 'name: dilla-desktop-core', 'name: dilla-core-wasi', 'playwright install --with-deps chromium', 'node packages/desktop/scripts/prepare-browser-sandbox.mjs', 'npm run build -w @dilla/desktop -- --existing-client', 'sudo chmod 4755 node_modules/electron/dist/chrome-sandbox'];
  return before.every(s => client.indexOf(s) >= 0 && client.indexOf(s) < at) && proof > at
    && typeof pkg.scripts?.test === 'string' && ['actor', 'fixture', 'process', 'ci'].every(name => pkg.scripts.test.split(/\s+/).filter(s => s === `unit/native-admission-${name}.test.mjs`).length === 1);
}
