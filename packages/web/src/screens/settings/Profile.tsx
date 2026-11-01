import { useEffect, useRef, useState } from 'react';
import { BIO_MAX_CHARS, DISPLAY_MAX_CHARS, STATUS_MAX_CHARS, profileTextProblem, profileTextValue, type ProfileTextProblem } from '@dilla/client-core';
import { Avatar, Banner, TextField, avatarStyle } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { errorOf } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { t, type StringKey } from '../../strings/index.ts';
import { useSettingsEdit } from '../../settings/edit-state.tsx';

const TITLE_ID = 'profile-title';
const SAVE_AFTER_MS = 700;
const PROBLEM: Record<ProfileTextProblem, StringKey> = { chars: 'profile.error.chars', long: 'profile.error.long', blank: 'profile.error.blank' };

interface Fields { display: string; statusMsg: string; bio: string }
const same = (a: Fields, b: Fields) => a.display === b.display && a.statusMsg === b.statusMsg && a.bio === b.bio;

/**
 * Settings → Profile (EOD E6): the display name, a one-line status message and a bio. The server stores them readable, and
 * the section says so. Save sends only the fields that changed ('' clears one), trimmed; a text the server would refuse is
 * named next to its field as it is typed, so a refusal never reads "try again". The worker puts the saved text on the account and
 * the member rows. A field changed on another device replaces the form's copy while nothing here has been edited.
 */
export function Profile(): React.JSX.Element {
  const client = useCore();
  const user = useSlice('account')?.user ?? null;
  const saved: Fields = { display: user?.display || user?.username || '', statusMsg: user?.statusMsg ?? '', bio: user?.bio ?? '' };
  const [fields, setFields] = useState<Fields>(saved);
  // What the last successful save wrote: the baseline until the account slice says the same.
  const [sent, setSent] = useState<Fields | null>(null);
  const [pending, setPending] = useState(false);
  const [failed, setFailed] = useState<'save' | 'text' | 'wait' | null>(null);
  const [done, setDone] = useState(false);
  const [displayError, setDisplayError] = useState(false);
  const baseline = sent ?? saved;

  // The account moved under the form (another device, or this save landing): follow it unless it is being edited.
  const previous = useRef(saved);
  useEffect(() => {
    const before = previous.current;
    previous.current = saved;
    if (same(before, saved)) return;
    setSent(null);
    // Each field follows the account unless it is being edited: a field still equal to the old stored value is replaced.
    setFields(current => ({
      display: current.display === before.display ? saved.display : current.display,
      statusMsg: current.statusMsg === before.statusMsg ? saved.statusMsg : current.statusMsg,
      bio: current.bio === before.bio ? saved.bio : current.bio,
    }));
  }, [saved.display, saved.statusMsg, saved.bio]); // eslint-disable-line react-hooks/exhaustive-deps

  const edit = (patch: Partial<Fields>) => { setFields(current => ({ ...current, ...patch })); setDone(false); setFailed(null); setDisplayError(false); };
  // A field is changed when its text differs from the stored one: only a changed field is checked and sent, so a stored
  // value that fails today's rule (kept from before) or an empty display name never blocks the others.
  const next: Fields = { display: profileTextValue(fields.display), statusMsg: profileTextValue(fields.statusMsg), bio: profileTextValue(fields.bio, true) };
  const rule = {
    display: profileTextProblem(fields.display, DISPLAY_MAX_CHARS),
    statusMsg: profileTextProblem(fields.statusMsg, STATUS_MAX_CHARS),
    bio: profileTextProblem(fields.bio, BIO_MAX_CHARS, true),
  };
  // Typing spaces at the end of the stored text is not a change; any other difference, or text the rule refuses, is.
  const edited = {
    display: fields.display !== baseline.display && (next.display !== baseline.display || rule.display !== null),
    statusMsg: fields.statusMsg !== baseline.statusMsg && (next.statusMsg !== baseline.statusMsg || rule.statusMsg !== null),
    bio: fields.bio !== baseline.bio && (next.bio !== baseline.bio || rule.bio !== null),
  };
  const changed = {
    ...(edited.display ? { display: next.display } : {}),
    ...(edited.statusMsg ? { statusMsg: next.statusMsg } : {}),
    ...(edited.bio ? { bio: next.bio } : {}),
  };
  const problem = {
    display: edited.display ? rule.display : null,
    statusMsg: edited.statusMsg ? rule.statusMsg : null,
    bio: edited.bio ? rule.bio : null,
  };
  const invalid = problem.display !== null || problem.statusMsg !== null || problem.bio !== null;
  const text = (p: ProfileTextProblem | null) => (p === null ? undefined : t(PROBLEM[p]));
  const dirty = Object.keys(changed).length > 0;

  // Fix round 2: one save at a time, and a save asked for while one is in flight waits for it and then sends the form as it is
  // by then, so keystrokes typed during a save (or before leaving) are never dropped. The latest render's view of the form is
  // read through a ref, because the wait outlives the render that started it (and, on leaving, the section itself).
  // What the stored profile is once `changed` lands: the baseline with each edited field as typed.
  const written: Fields = { ...baseline, ...Object.fromEntries(Object.entries(edited).filter(([, on]) => on).map(([k]) => [k, fields[k as keyof Fields]])) };
  const view = { fields, dirty, invalid, blank: edited.display && next.display === '', changed, written };
  const latest = useRef(view);
  latest.current = view;
  const inflight = useRef<Promise<boolean> | null>(null);
  // The fields the last successful save wrote, so a save that waited for it does not send the same text twice.
  const lastWritten = useRef<Fields | null>(null);
  const save = async (): Promise<boolean> => {
    while (inflight.current !== null) await inflight.current;
    const now = latest.current;
    if (lastWritten.current !== null && same(now.fields, lastWritten.current)) return true;
    if (!now.dirty) return true;
    if (now.invalid) return false;
    if (now.blank) { setDisplayError(true); return false; }
    const run = (async () => {
      setPending(true);
      setFailed(null);
      try {
        await client.call({ m: 'updateProfile', ...now.changed });
        lastWritten.current = now.fields;
        setSent(now.written);
        setDone(true);
        return true;
      } catch (e: unknown) {
        const code = errorOf(e).code;
        setFailed(code === 'E_INVALID_REQUEST' || code === 'E_BAD_INPUT' ? 'text' : code === 'E_RATE_LIMITED' ? 'wait' : 'save');
        return false;
      } finally { setPending(false); }
    })();
    inflight.current = run;
    try { return await run; } finally { if (inflight.current === run) inflight.current = null; }
  };
  const reset = () => { setFields(baseline); setFailed(null); setDone(false); setDisplayError(false); };
  // Task 11 (Jonas, Q7): the prototype saves as you type: a valid change is saved 700 ms after the last keystroke, and the
  // workspace only holds the section (its unsaved-changes prompt) while a change cannot be saved (invalid, or the save failed).
  const fieldsKey = `${fields.display}\n${fields.statusMsg}\n${fields.bio}`;
  const unsaveable = dirty && (invalid || failed !== null || (edited.display && next.display === ''));
  useEffect(() => {
    if (!dirty || invalid || failed !== null) return;
    const handle = setTimeout(() => { void save(); }, SAVE_AFTER_MS);
    return () => clearTimeout(handle);
  }, [fieldsKey, dirty, invalid, failed]); // eslint-disable-line react-hooks/exhaustive-deps
  // Leaving the section: the workspace's guard calls `flush` first and only lets the section go once the change has landed (a
  // failure keeps it, with the unsaved-changes prompt and the error). Unmounting some other way still sends the latest
  // valid change, after any save in flight.
  const flush = useRef(save);
  flush.current = save;
  useEffect(() => () => { if (latest.current.dirty && !latest.current.invalid && !latest.current.blank) void flush.current(); }, []);
  useSettingsEdit({ dirty: unsaveable, busy: pending, canSave: dirty && !pending && !invalid, save, reset, flush: save });
  const status = pending ? t('profile.saving') : done && !dirty ? t('profile.saved') : '';
  const preview = { display: next.display || user?.username || '', statusMsg: next.statusMsg, bio: next.bio };

  return (
    <section className="dw-settings-section" aria-labelledby={TITLE_ID}>
      <h2 id={TITLE_ID} tabIndex={-1}>{t('profile.title')}</h2>
      <p>{t('profile.body')}</p>
      {failed === null ? null : <Banner tone="danger">{t(failed === 'text' ? 'profile.error.chars' : failed === 'wait' ? 'profile.error.wait' : 'profile.error.save')}</Banner>}
      <div className="dw-profile">
        {/* Enter in a one-line field saves at once (the form has no Save button to submit it); the keys are the fields'. */}
        {/* eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions */}
        <form className="dw-profile-form" onSubmit={e => { e.preventDefault(); void save(); }} noValidate
          onKeyDown={e => { if (e.key === 'Enter' && e.target instanceof HTMLInputElement && !e.nativeEvent.isComposing) { e.preventDefault(); void save(); } }}>
          <TextField id="profile-display" label={t('profile.display.label')} value={fields.display} onChange={display => { edit({ display }); }}
            maxLength={DISPLAY_MAX_CHARS} required autoComplete="nickname" error={text(problem.display) ?? (displayError ? t('profile.error.display') : undefined)} />
          <TextField id="profile-status" label={t('profile.status.label')} value={fields.statusMsg} onChange={statusMsg => { edit({ statusMsg: statusMsg.replace(/[\r\n]+/g, ' ') }); }}
            maxLength={STATUS_MAX_CHARS} error={text(problem.statusMsg)} />
          <TextField id="profile-bio" label={t('profile.bio.label')} value={fields.bio} onChange={bio => { edit({ bio }); }}
            maxLength={BIO_MAX_CHARS} multiline rows={3} error={text(problem.bio)} />
          <p className="dw-profile-form__note">{t('profile.footnote')}</p>
          <p role="status" className="dw-profile-form__saved">{status}</p>
        </form>
        {/* The prototype's preview: how the card shows you, as you type. */}
        <div className="dw-profile-preview" aria-label={t('profile.preview')} role="group">
          <div className="dw-profile-preview__band" style={avatarStyle(preview.display)} />
          <div className="dw-profile-preview__body">
            <Avatar name={preview.display} size="card" />
            <div className="dw-profile-preview__name">{preview.display}</div>
            <div className="dw-profile-preview__line">{user === null ? '' : preview.statusMsg === '' ? `@${user.username}` : t('you.line', { handle: user.username, status: preview.statusMsg })}</div>
            {preview.bio === '' ? null : <p className="dw-profile-preview__bio">{preview.bio}</p>}
          </div>
        </div>
      </div>
    </section>
  );
}
