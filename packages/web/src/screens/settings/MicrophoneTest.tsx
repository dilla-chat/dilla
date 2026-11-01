import { useCallback, useEffect, useRef, useState } from 'react';
import type { ProcessorStatus } from '@dilla/media';
import type { LocalMicrophoneTest, LoopbackSettings } from '../../call/microphone-test.ts';

/** The prototype's resting automatic line before anything was measured (README §5.9; SENSITIVITY.autoStartDb). */
export const AUTO_START_DB = -48;
const SILENT_DB = -100;

export interface MicTest {
  testing: boolean;
  failed: boolean;
  /** The level after the input volume, −100..0 dB (−100 while not testing). */
  levelDb: number;
  /** Fix round 1: the stage's gate is open (with its hold), which the pill shows. */
  open: boolean;
  /** The automatic threshold the test measured last (the call computes its own the same way), or the resting −48 dB. */
  autoDb: number;
  status: ProcessorStatus | null;
  /** Let's check / Stop testing; runs inside the press so the browser lets the audio play. */
  toggle(): void;
}

/**
 * Signal task 10b: Let's check (README §5.9). Starts the ten-second loopback on press, applies volume and sensitivity
 * changes at once while it runs, and ends it when the input, output or processing changes, when a call takes the
 * microphone (`disabled`), on pagehide and when the section goes. The module (livekit-client, the processors) loads on
 * the first press only.
 */
export function useMicTest(settings: LoopbackSettings, disabled: boolean): MicTest {
  const test = useRef<LocalMicrophoneTest | null>(null);
  const generation = useRef(0);
  const [testing, setTesting] = useState(false);
  const [failed, setFailed] = useState(false);
  const [levelDb, setLevelDb] = useState(SILENT_DB);
  const [open, setOpen] = useState(false);
  const [autoDb, setAutoDb] = useState(AUTO_START_DB);
  const [status, setStatus] = useState<ProcessorStatus | null>(null);
  const latest = useRef(settings);
  latest.current = settings;

  const close = useCallback(() => {
    generation.current++;
    void test.current?.close();
    test.current = null;
    setTesting(false);
    setLevelDb(SILENT_DB);
    setOpen(false);
    setStatus(null);
  }, []);

  const { input, output, noise } = settings;
  const { echo, agc } = settings.processing;
  useEffect(() => {
    close();
    window.addEventListener('pagehide', close);
    return () => { close(); window.removeEventListener('pagehide', close); };
  }, [input, output, noise, echo, agc, disabled, close]);

  const { inputVolume, outputVolume } = settings;
  const { auto, thresholdDb } = settings.sensitivity;
  const { mode } = settings;
  useEffect(() => { test.current?.update({ inputVolume, outputVolume, sensitivity: { auto, thresholdDb }, mode }); }, [inputVolume, outputVolume, auto, thresholdDb, mode]);

  const start = async () => {
    if (disabled) return;
    const id = ++generation.current;
    const mine = () => id === generation.current;
    setFailed(false);
    setTesting(true);
    try {
      const { LocalMicrophoneTest } = await import('../../call/microphone-test.ts');
      if (!mine()) return;
      const t = test.current = new LocalMicrophoneTest({
        level: l => { if (!mine()) return; setLevelDb(l.db); setOpen(l.open); if (latest.current.sensitivity.auto) setAutoDb(l.thresholdDb); },
        status: s => { if (mine()) setStatus(s); },
        ended: () => { if (mine()) { test.current = null; setTesting(false); setLevelDb(SILENT_DB); setOpen(false); setStatus(null); } },
      });
      await t.start(latest.current);
    } catch {
      if (mine()) { test.current = null; setTesting(false); setFailed(true); setLevelDb(SILENT_DB); }
    }
  };
  const toggle = () => { if (testing) close(); else void start(); };
  return { testing, failed, levelDb, open, autoDb, status, toggle };
}
