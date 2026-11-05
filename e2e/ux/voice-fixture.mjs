/** Observe the shipping voice states without racing an automatic join against a fallback CTA. */
export async function waitForVoiceJoin(page, { timeout = 60_000, joinLabel = 'Join voice' } = {}) {
  const deadline = Date.now() + timeout;
  const stage = page.locator('.dw-voice-stage').first();
  const join = stage.getByRole('button', { name: joinLabel, exact: true });
  let clicked = false;
  let phase = 'not mounted';
  while (Date.now() < deadline) {
    if (await stage.isVisible().catch(() => false)) {
      phase = await stage.getAttribute('data-call-phase', { timeout: Math.max(1_000, deadline - Date.now()) });
      // The dock is intentionally hidden/collapsed while the full stage is open.
      // Its connected state is authoritative even when it has no painted box.
      if (phase === 'in' && await page.locator('.d-voice-dock[data-phase="connected"]').count() > 0) return;
      // The row already starts joining on a desktop. Only a real idle preview
      // needs its own CTA; never await a button that disappeared while joining.
      if (!clicked && ['idle', 'failed', 'removed'].includes(phase) && await join.isVisible().catch(() => false) && await join.isEnabled().catch(() => false)) {
        try {
          await join.click({ timeout: Math.max(1, Math.min(1000, deadline - Date.now())) });
          clicked = true;
        } catch {
          // Automatic joining can remove the visible CTA between observation
          // and hit testing. Re-read the phase instead of awaiting that old CTA.
        }
      }
    }
    const remaining = deadline - Date.now();
    if (remaining > 0) await page.waitForTimeout(Math.min(100, remaining));
  }
  const stageText = await stage.innerText({ timeout: 1000 }).catch(() => '(no stage)');
  const body = await page.locator('body').innerText({ timeout: 1000 }).catch(() => '(no body)');
  throw new Error(`voice did not connect within ${timeout}ms: phase=${phase}; stage=${stageText.slice(0, 2500)}; body=${body.slice(0, 5000)}`);
}
