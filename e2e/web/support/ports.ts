/**
 * Every port the core, web and voice suites bind (host, control and in-process SFU) is a CI default plus
 * DILLA_E2E_PORT_OFFSET, so two runs on one machine never share one: scripts/ci-local.mjs gives each browser job a block
 * of its own (block k is offset k*1000; the suites span 7884..8615). Unset, the offset is 0 and the ports are the ones
 * CI and the specs' comments name. A per-spec env var (DILLA_E2E_PORT, DILLA_E2E_PHONE_PORT, …) still wins over both.
 */
const raw = process.env.DILLA_E2E_PORT_OFFSET ?? '0';
if (!/^\d+$/.test(raw) || Number(raw) + 8615 > 65_535) throw new Error(`DILLA_E2E_PORT_OFFSET=${raw} is not a port offset (0..${String(65_535 - 8615)})`);
export const PORT_OFFSET = Number(raw);

/** A suite's CI port moved by the run's offset. */
export function e2ePort(port: number): number {
  return port + PORT_OFFSET;
}
