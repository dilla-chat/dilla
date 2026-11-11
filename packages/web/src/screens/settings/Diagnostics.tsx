import { useEffect } from 'react';
import type { DiagnosticsState } from '@dilla/client-core';
import { Button, SettingsRow } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { useSlice } from '../../core/use-slice.ts';
import { devicesChunk } from '../shell-model.ts';
import { formatClock, t } from '../../strings/index.ts';

/**
 * Device-local diagnostics: stable codes only (L-TS-43); Copy writes those codes, one per line, and nothing else. The
 * instance shows by its name (W-05); its hex id stays the BroadcastChannel's. Bot v2: the slowest socket requests follow
 * the facts, by route pattern (never a path with its ids).
 */
export function Diagnostics({ data, instanceName, writeText }: {
  data: DiagnosticsState; instanceName: string | null; writeText: (text: string) => Promise<void>;
}): React.JSX.Element {
  const client = useCore();
  const devices = useSlice('devices');
  // The device list, once per mount, for the devices fact; a refusal leaves the count as it was.
  useEffect(() => {
    client.call({ m: 'refreshDevices' }).catch(() => undefined);
  }, [client]);
  const errors = data.errors.slice(-20);
  // Bot v2: the socket routes with the slowest p95 round trip, as the worker last published them; at most five.
  const slowRoutes = (data.slowRoutes ?? []).slice(0, 5);
  const iso = (at: number) => new Date(at).toISOString();
  const facts: readonly [string, string, string | number | null][] = [
    ['connection', t('diagnostics.connection'), data.connection], ['generation', t('diagnostics.generation'), data.generation],
    ['instance', t('diagnostics.instance'), instanceName ?? t('shell.status.unknown')], ['devices', t('diagnostics.devices'), devicesChunk(devices)],
  ];
  const kp = data.keyPackages;
  // The count, then when a publish was last tried and the code it failed with; nothing is shown for what is not known.
  const keyPackages = <span data-fact="keyPackages">
    {[kp.count === null ? '–' : String(kp.count),
      kp.lastAttempt === null ? null : <time key="at" dateTime={iso(kp.lastAttempt)}>{formatClock(kp.lastAttempt)}</time>,
      kp.lastError === null ? null : <code key="code">{kp.lastError}</code>]
      .filter((part) => part !== null)
      .flatMap((part, index) => (index === 0 ? [part] : [' · ', part]))}</span>;
  // Each fact is a settings row (I2): its name on the left, its value in the control column. The last 20 stable codes follow.
  return <section className="dw-settings-section" data-doc-state="settings-diagnostics" aria-labelledby="diagnostics-title">
    <h2 id="diagnostics-title" tabIndex={-1}>{t('diagnostics.title')}</h2>
    {facts.map(([id, label, value]) => <SettingsRow key={id} id={`diag-${id}`} label={label}><span data-fact={id}>{value}</span></SettingsRow>)}
    <SettingsRow id="diag-keyPackages" label={t('diagnostics.keyPackages')}>{keyPackages}</SettingsRow>
    {slowRoutes.length > 0
      ? <section className="dw-diagnostics-routes" aria-labelledby="diagnostics-slow-routes">
        <h3 id="diagnostics-slow-routes">{t('diagnostics.slowRoutes')}</h3>
        <table className="dw-diagnostics-routes__table" aria-labelledby="diagnostics-slow-routes">
          <thead><tr>
            <th scope="col">{t('diagnostics.slowRoutes.route')}</th><th scope="col">{t('diagnostics.slowRoutes.p50')}</th>
            <th scope="col">{t('diagnostics.slowRoutes.p95')}</th><th scope="col">{t('diagnostics.slowRoutes.count')}</th>
          </tr></thead>
          <tbody>{slowRoutes.map((route) => <tr key={route.pattern} data-route={route.pattern}>
            <th scope="row"><code>{route.pattern}</code></th><td>{t('diagnostics.slowRoutes.ms', { ms: route.p50 })}</td>
            <td>{t('diagnostics.slowRoutes.ms', { ms: route.p95 })}</td><td>{route.count}</td>
          </tr>)}</tbody>
        </table>
      </section>
      : null}
    {errors.length > 0
      ? <ul className="dw-diagnostics-errors">{errors.map((error, index) => <li data-error-code={error.code} key={`${error.at}-${index}`}>
        <code>{error.code}</code><time dateTime={iso(error.at)}>{formatClock(error.at)}</time></li>)}</ul>
      : <p>{t('diagnostics.empty')}</p>}
    <SettingsRow id="diag-copy" label={t('diagnostics.copy')}>
      <Button onClick={() => { writeText(errors.map(error => error.code).join('\n')).catch(() => undefined); }}>{t('diagnostics.copy')}</Button>
    </SettingsRow>
  </section>;
}
