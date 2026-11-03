// Executed only by trace-retention.spec.ts in a nested runner: its deliberate
// failures reproduce callers that close Electron in the test body's finally.
import { test } from '@playwright/test';
import { createServer } from 'node:http';
import { launchDesktopContext } from '../../../../e2e/web/support/electron.ts';

const server = createServer((_request, response) => { response.writeHead(404); response.end(); });
test.beforeAll(async () => { await new Promise<void>(done => server.listen(0, '127.0.0.1', done)); });
test.afterAll(async () => { await new Promise<void>(done => server.close(() => done())); });

test('failure before explicit peer cleanup', async ({}, info) => {
 const contexts = [];
 try {
  const baseURL = `http://127.0.0.1:${(server.address() as { port: number }).port}/`;
  for (const peer of ['alice', 'bob']) {
   const context = await launchDesktopContext(null, { baseURL }, false);
   contexts.push(context);
   // A real tracing.start would reject if the trace:off fixture had already
   // started tracing. This guards benchmark overhead, not just ZIP retention.
   if (info.project.use.trace === 'off') {
    await context.tracing.start();
    await context.tracing.stop();
   }
   const page = await context.newPage();
   await page.evaluate(peer => {
    console.warn(`electron-trace-${peer}`);
    const button = document.createElement('button');
    button.textContent = `Trace peer ${peer}`;
    button.style.cssText = 'position:fixed;top:0;left:0;z-index:2147483647';
    button.onclick = () => { button.dataset.clicked = 'yes'; };
    document.body.appendChild(button);
   }, peer);
   await page.getByRole('button', { name: `Trace peer ${peer}`, exact: true }).click();
  }
  throw new Error('intentional failure before Electron finally cleanup');
 } finally {
  for (const context of contexts) { await context.close(); await context.close(); }
 }
});
