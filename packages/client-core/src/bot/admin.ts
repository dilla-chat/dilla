// Bot v2 Task 22: what the Bot page and its admin surfaces read: the Bot's status (sealed read 94: the Bot channels it
// needs re-adding to, the last music starts), the webhook list, and the "via webhook" label line of a webhook post.
import { arr, bin, decode, encode, str, u53, type CborValue } from '../cbor';
import { toHex, fromHex } from '../hex';

/** The Bot page's status read, a sealed read with no v1 operation (internal/bot/protocol OpReadBotStatus). */
export const BOT_STATUS_READ = 94;
/** The music starts the Bot keeps and the status read carries. */
export const MUSIC_STARTS_KEPT = 20;

export interface MusicStart { firstFrameMs: number; cold: boolean }
export interface BotPageStatus {
  /** Bot channels (hex) whose group the Bot cannot join on its own: an admin's client must add it again. */
  needsReAdd: string[];
  /** The last music starts, oldest first. */
  musicStarts: MusicStart[];
}

const flag = (v: CborValue): boolean => {
  const n = u53(v);
  if (n !== 0 && n !== 1) throw new TypeError('invalid flag');
  return n === 1;
};

/** The status read's data: [1, needs_re_add [bstr 16…], starts [[first_frame_ms, cold]…] (at most 20)]. */
export function decodeBotStatus(data: Uint8Array): BotPageStatus {
  if (data.length > 8192) throw new TypeError('Bot status too large');
  const [version, channels, starts] = arr(decode(data), 3);
  if (u53(version) !== 1) throw new TypeError('unsupported Bot status version');
  const startRows = arr(starts);
  if (startRows.length > MUSIC_STARTS_KEPT) throw new TypeError('too many music starts');
  return {
    needsReAdd: arr(channels).map(c => toHex(bin(c, 16))),
    musicStarts: startRows.map(row => { const [ms, cold] = arr(row, 2); return { firstFrameMs: u53(ms), cold: flag(cold) }; }),
  };
}

/** The status read's data, for tests and the test Bot. */
export function encodeBotStatus(status: BotPageStatus): Uint8Array {
  return encode([1n, status.needsReAdd.map(fromHex), status.musicStarts.map(s => [BigInt(s.firstFrameMs), s.cold ? 1n : 0n])]);
}

export interface BotWebhook {
  webhookId: string; channelId: string; name: string; avatar: string | null; allowEveryone: boolean;
  createdAt: number; rotatedAt: number | null;
}

/** GET …/bot/webhooks → [[webhook_id, channel_id, name, avatar|null, allow_everyone, created_at, rotated_at|null]…]. */
export function decodeBotWebhooks(body: Uint8Array): BotWebhook[] {
  return arr(decode(body)).map(row => {
    const [wid, channel, name, avatar, allow, created, rotated] = arr(row, 7);
    return { webhookId: toHex(bin(wid, 16)), channelId: toHex(bin(channel, 16)), name: str(name),
      avatar: avatar === null ? null : toHex(bin(avatar)), allowEveryone: flag(allow), createdAt: u53(created),
      rotatedAt: rotated === null ? null : u53(rotated) };
  });
}

export interface WebhookLabel { text: string; webhookId: string; name: string; avatarId: string | null }

const LABEL = /\n\[dilla-webhook:v1:([0-9a-f]{32}):([^:\]\n]*)(?::([0-9a-f]{64}))?\]$/u;

/** The label line a webhook post ends in (protocol/10 § Webhooks), and the text before it; null for any other body. The
 *  caller shows "via webhook <name>" only for a post the community's pinned Bot user authored: the Bot defangs `[dilla-`
 *  in every free text it posts, so only its own label line can match. */
export function decodeWebhookLabel(body: string): WebhookLabel | null {
  const m = LABEL.exec(body);
  if (m === null) return null;
  let name: string;
  try { name = decodeURIComponent(m[2]); } catch { return null; }
  return { text: body.slice(0, m.index), webhookId: m[1], name, avatarId: m[3] ?? null };
}
