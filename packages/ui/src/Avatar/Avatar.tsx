import './Avatar.css';
import { StateDot } from '../StateDot/StateDot.tsx';
export type Presence = 'online' | 'idle' | 'dnd' | 'offline';

/**
 * Task 11 (Jonas, Q1): the Signal prototype's own avatar colours (its `C` table: hue, saturation and lightness per person),
 * picked per name by a hash. Exported so the contrast guard in Avatar.test.tsx reads this palette, not a copy.
 */
export const AVATAR_PALETTE = [
  { h: 211, s: 45, l: 42 }, { h: 298, s: 45, l: 42 }, { h: 12, s: 55, l: 42 }, { h: 48, s: 45, l: 34 },
  { h: 28, s: 50, l: 40 }, { h: 234, s: 45, l: 42 }, { h: 160, s: 40, l: 34 },
] as const;
/** The palette's hues, for a caller that pins one (`hue`). */
export const AVATAR_HUES = AVATAR_PALETTE.map(p => p.h);
export type AvatarColour = (typeof AVATAR_PALETTE)[number];

export type AvatarProps = { name: string; initials?: string; hue?: number; presence?: Presence; size?: 'mini' | 'voice' | 'sug' | 'xs' | 'sm' | 'member' | 'msg' | 'row' | 'md' | 'lg' | 'panel' | 'confirm' | 'card'; shape?: 'circle' | 'bot' };

function deriveInitials(name: string): string {
  const parts = name.trim().split(/\s+/);
  const s = parts.length > 1 ? parts[0][0] + parts[1][0] : name.slice(0, 2);
  return s.toUpperCase();
}
/**
 * The colour an avatar of `name` takes (the profile card's band uses it too, task 9): FNV-1a over a salted name, modulo the
 * palette. The salt ("avatar58:") is the one under which the prototype's sample people (Ada, Björn, Mira, Sven, Liv, Kari,
 * Ole) each land on the colour the prototype gives them; anyone else gets one of the same seven.
 */
export function avatarColour(name: string): AvatarColour {
  let h = 0x811c9dc5;
  for (const c of `avatar58:${name}`) { h ^= c.codePointAt(0) ?? 0; h = Math.imul(h, 0x01000193) >>> 0; }
  return AVATAR_PALETTE[h % AVATAR_PALETTE.length];
}
export function avatarHue(name: string): number { return avatarColour(name).h; }
/** The custom properties an element carries to draw `name`'s colour (`hsl(var(--avatar-hue) var(--avatar-s) var(--avatar-l))`). */
export function avatarStyle(name: string, hue?: number): Record<string, string> {
  const c = avatarColour(name);
  return hue === undefined ? { '--avatar-hue': String(c.h), '--avatar-s': `${String(c.s)}%`, '--avatar-l': `${String(c.l)}%` }
    : { '--avatar-hue': String(hue) };
}

const PRESENCE_TONE = { online: 'ok', idle: 'idle', dnd: 'dnd', offline: 'off' } as const;

export function Avatar({ name, initials, hue, presence, size = 'md', shape = 'circle' }: AvatarProps) {
  const label = presence ? `${name}, ${presence}` : name;
  return (
    <span className="d-avatar" data-size={size} data-shape={shape} role="img" aria-label={label} style={avatarStyle(name, hue)}>
      <span className="d-avatar__initials" aria-hidden="true">{initials ?? deriveInitials(name)}</span>
      {presence ? <span className="d-avatar__presence" data-presence={presence} aria-hidden="true"><StateDot tone={PRESENCE_TONE[presence]} size="md" /></span> : null}
    </span>
  );
}
