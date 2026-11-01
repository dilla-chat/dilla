import { arr, decode, encode, u53 } from '../cbor';
import { HttpClient } from './client';

export interface SessionCapabilities {
  wire: number;
  e2ee: number;
  media: number;
  botAvailable: boolean;
}

/** Negotiate this authenticated bearer session's protocol capabilities. Server refusals, including an
 * unsupported endpoint on an older instance, are passed through as DillaHttpError. */
export async function negotiateSession(http: HttpClient): Promise<SessionCapabilities> {
  const response = await http.request({ method: 'POST', path: '/v1/sessions/negotiate', bucket: 'write', idempotent: false,
    body: encode([[5, 4, 3, 2], [1], [1]]), ok: [200] });
  const accepted = arr(decode(response.body), 3).map((value) => {
    const version = u53(value);
    if (version < 1) throw new RangeError('E_CBOR_RANGE: negotiated versions must be positive');
    return version;
  });
  const [wire, e2ee, media] = accepted;
  if (wire === undefined || e2ee === undefined || media === undefined) throw new RangeError('E_CBOR_SHAPE');
  if ((wire !== 5 && wire !== 4 && wire !== 3 && wire !== 2) || e2ee !== 1 || media !== 1) {
    throw new RangeError('E_CBOR_RANGE: server accepted a version the client did not offer');
  }
  return { wire, e2ee, media, botAvailable: wire >= 3 };
}
