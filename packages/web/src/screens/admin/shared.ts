// Shared by the Server settings sections (flow 08): one action at a time with ConfirmDialog's busy/error contract, and the
// small rules every section reads the worker's refusals by.
import { createContext, useCallback, useRef, useState } from 'react';
import { errorOf, type UiError } from '../../core/errors.ts';

/** Says a success to the person: the shell's polite announcer when ServerSettings was given one, else its own status region. */
export type Announce = (message: string) => void;

/** What a section is given: the server it manages and how to announce a success. */
export interface SectionProps { communityId: string; serverName: string; announce: Announce }

/** True for a refusal the instance answered with this HTTP status (the page error carries it). */
export const statusIs = (e: UiError, status: number): boolean => e.status === status;
/** True for a 403. */
export const forbidden = (e: UiError): boolean => e.code === 'E_FORBIDDEN' || e.status === 403;
/** True for a rename or edit the instance refused as invalid (the worker's own check says E_BAD_INPUT). */
export const refused = (e: UiError): boolean => e.code === 'E_INVALID_REQUEST' || e.code === 'E_BAD_INPUT';

/** A typed name and a server name compare under NFC, trimmed (flow 08: "Björn" typed as NFD matches). */
export const nfc = (s: string): string => s.normalize('NFC').trim();

/** A member's shown name. */
export const shownName = (m: { display: string; username: string }): string => (m.display !== '' ? m.display : m.username);

export interface RunState { busy: boolean; error: string | null; attempt: number }
const IDLE: RunState = { busy: false, error: null, attempt: 0 };

/**
 * One action at a time. A failure clears `busy` and sets `error` in the SAME state update, which is what ConfirmDialog's
 * contract needs to move focus to Cancel; `attempt` counts the refusals shown so the same text is announced again.
 * `run` resolves true when `work` resolved, false when it failed (and `say` named why) or another action was running.
 */
export function useRun(): RunState & { run(this: void, work: () => Promise<unknown>, say: (e: UiError) => string): Promise<boolean>; reset(this: void): void } {
  const [state, setState] = useState<RunState>(IDLE);
  const running = useRef(false);
  const run = useCallback(async (work: () => Promise<unknown>, say: (e: UiError) => string): Promise<boolean> => {
    if (running.current) return false;
    running.current = true;
    setState(s => ({ busy: true, error: null, attempt: s.attempt }));
    try {
      await work();
      setState(s => ({ busy: false, error: null, attempt: s.attempt }));
      return true;
    } catch (e) {
      const message = say(errorOf(e));
      setState(s => ({ busy: false, error: message, attempt: s.attempt + 1 }));
      return false;
    } finally {
      running.current = false;
    }
  }, []);
  const reset = useCallback(() => { if (!running.current) setState(s => ({ busy: false, error: null, attempt: s.attempt })); }, []);
  return { ...state, run, reset };
}

/** The element of the SettingsFrame footer a section draws its one save bar into (R17); null while the footer is not mounted. */
export const FooterSlot = createContext<HTMLElement | null>(null);
