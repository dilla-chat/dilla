import { chromium, expect, type Page } from '@playwright/test';
import { desktopApplication } from './electron.ts';
import { canonicalInstance } from '../../../packages/desktop/src/policy.ts';
import type { VirtualAuthenticator } from './webauthn.ts';
import { createServer } from 'node:http';
import { randomBytes } from 'node:crypto';

/** Only the OS browser launch is substituted. The installed renderer, main
 * process, PKCE request/consume flow, server cookie, real browser ceremony and
 * WebAuthn verification all run unchanged. The virtual authenticator lives in
 * this separate browser, never in the Electron renderer. */
export async function systemBrowserAuthenticator(desktopPage: Page): Promise<VirtualAuthenticator> {
 const context = desktopPage.context(), application = desktopApplication(context);
 const instance = await desktopPage.evaluate(async () => (await (window as any).dillaDesktop.info()).instance as string);
 const base = canonicalInstance(instance, true);
 const browser = await chromium.launch({ channel: 'chromium', headless: true, chromiumSandbox: true });
 const secret = '/' + randomBytes(32).toString('hex');
 const server = createServer();
 const stop = async () => { server.closeAllConnections(); await new Promise<void>(done => server.close(() => done())); await browser.close(); };
 try {
  const external = await browser.newContext();
  const page = await external.newPage();
  const cdp = await external.newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', { options: {
   protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true,
   isUserVerified: true, automaticPresenceSimulation: true,
  } });
  const open = async (href: string) => {
   const url = new URL(href), prefix = new URL('desktop-auth/', base);
   if (url.origin !== prefix.origin || !url.pathname.startsWith(prefix.pathname)
     || !/^[A-Za-z0-9_-]{43}$/.test(url.pathname.slice(prefix.pathname.length))
     || url.search || url.hash || url.username || url.password) throw new Error('Unexpected system-browser handoff');
   await page.goto(url.href);
   await page.locator('#continue').click();
   await expect(page.locator('#status')).toHaveText('Return to your Dilla desktop app.');
  };
  server.on('request', (request, response) => {
   if (request.method !== 'POST' || request.url !== secret) { response.writeHead(404).end(); return; }
   let href = '';
   request.on('data', data => { href += String(data); if (href.length > 4096) request.destroy(); });
   request.on('end', () => { void open(href).then(() => response.writeHead(204).end(), error => { response.writeHead(500).end(String(error)); }); });
  });
  await new Promise<void>((done, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', done); });
  const address = server.address(); if (!address || typeof address === 'string') throw new Error('System-browser fixture did not start');
  await application.evaluate(({ shell }, endpoint) => {
   shell.openExternal = async href => { const response = await fetch(endpoint, { method: 'POST', body: href }); if (!response.ok) throw new Error(await response.text()); };
  }, `http://127.0.0.1:${address.port}${secret}`);
  const close = context.close.bind(context);
  context.close = async options => { try { await close(options); } finally { await stop(); } };
  return { cdp, authenticatorId };
 } catch (error) { await stop(); throw error; }
}
