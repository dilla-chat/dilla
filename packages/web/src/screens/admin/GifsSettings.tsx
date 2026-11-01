// Server settings → overview: the server's Giphy key (E8, flow 16). The key is write-only. It is typed here, sent to the
// worker and forgotten: nothing keeps it, shows it again or puts it in a message, and the instance never sends it back.
import { useEffect, useId, useRef, useState } from 'react';
import { Button, Select, SettingsRow, TextField } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { errorOf } from '../../core/errors.ts';
import { t, type StringKey } from '../../strings/index.ts';

const KEY_SHAPE = /^[A-Za-z0-9]{8,64}$/;
const RATINGS = ['g', 'pg', 'pg-13', 'r'] as const;
type Rating = typeof RATINGS[number];
const isRating = (v: unknown): v is Rating => (RATINGS as readonly unknown[]).includes(v);
const GIPHY_DEVELOPERS = 'https://developers.giphy.com/';

type Focus = 'replace' | 'field' | null;

export function GifsSettings({ communityId, announce }: { communityId: string; announce: (text: string) => void }): React.JSX.Element {
  const client = useCore();
  const base = useId();
  const fieldId = `${base}-key`;
  const replaceId = `${base}-replace`;
  const [loaded, setLoaded] = useState<'loading' | 'ready' | 'error'>('loading');
  const [on, setOn] = useState(false);
  const [rating, setRating] = useState<Rating>('pg-13');
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [focus, setFocus] = useState<Focus>(null);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);

  useEffect(() => {
    let cancelled = false;
    void client.call({ m: 'gifStatus', communityId }).then(result => {
      if (cancelled) return;
      const r = result as { on?: unknown; rating?: unknown } | null;
      if (r === null || typeof r !== 'object' || typeof r.on !== 'boolean' || !isRating(r.rating)) throw new Error('E_GIF_SHAPE');
      setOn(r.on);
      setRating(r.rating);
      setLoaded('ready');
    }).catch(() => { if (!cancelled) setLoaded('error'); });
    return () => { cancelled = true; };
  }, [client, communityId]);

  // Focus follows the change: a control that disappears hands focus to the one that takes its place.
  useEffect(() => {
    if (focus === null) return;
    document.getElementById(focus === 'replace' ? replaceId : fieldId)?.focus();
    setFocus(null);
  }, [focus, replaceId, fieldId]);

  const say = (e: unknown): string => {
    const err = errorOf(e);
    return err.status === 403 || err.code === 'E_FORBIDDEN' ? t('gif.settings.forbidden') : t('gif.settings.error', { code: err.code });
  };

  const run = async (work: () => Promise<unknown>, done: () => void): Promise<boolean> => {
    if (busy) return false;
    setBusy(true);
    setError(null);
    try {
      await work();
      if (alive.current) done();
      return true;
    } catch (e) {
      // The refusal names the code, never what was sent.
      if (alive.current) setError(say(e));
      return false;
    } finally {
      if (alive.current) setBusy(false);
    }
  };

  const save = () => {
    if (!KEY_SHAPE.test(value)) { setError(t('gif.settings.invalid')); return; }
    const key = value;
    void run(() => client.call({ m: 'gifSetKey', communityId, key }), () => {
      setValue('');
      setOn(true);
      setEditing(false);
      announce(t('gif.settings.saved'));
      setFocus('replace');
    });
  };
  const remove = () => {
    void run(() => client.call({ m: 'gifClearKey', communityId }), () => {
      setOn(false);
      setEditing(false);
      setValue('');
      announce(t('gif.settings.removed'));
      setFocus('field');
    });
  };
  const pickRating = (next: string) => {
    if (!isRating(next)) return;
    const before = rating;
    setRating(next);
    // A refusal puts the previous rating back.
    void run(() => client.call({ m: 'gifSetRating', communityId, rating: next }), () => { announce(t('gif.settings.ratingSaved')); })
      .then(ok => { if (!ok && alive.current) setRating(before); });
  };

  const cancel = () => { setEditing(false); setValue(''); setError(null); setFocus('replace'); };
  // Escape in the replace field cancels it (a listener on the field, not on the form).
  const cancelRef = useRef(cancel);
  cancelRef.current = cancel;
  useEffect(() => {
    if (!(on && editing)) return;
    const el = document.getElementById(fieldId);
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); cancelRef.current(); }
    };
    el?.addEventListener('keydown', onKey);
    return () => { el?.removeEventListener('keydown', onKey); };
  }, [on, editing, fieldId]);
  const showField = loaded === 'ready' && (!on || editing);

  return (
    <SettingsRow id={`${base}-row`} label={t('gif.settings.title')} hint={t('gif.settings.hint')} layout="stacked">
      <div className="dw-gifs-settings">
        <a href={GIPHY_DEVELOPERS} target="_blank" rel="noopener noreferrer">{t('gif.settings.getKey')}</a>
        {loaded === 'error' ? <p role="alert">{t('gif.settings.loadError')}</p> : null}
        {loaded === 'ready' && on && !editing ? (
          <div className="dw-gifs-settings__set">
            <span>{t('gif.settings.maskedLabel')}</span>
            <Button id={replaceId} type="button" onClick={() => { setEditing(true); setError(null); setFocus('field'); }}>{t('gif.settings.replace')}</Button>
            <Button type="button" variant="danger" aria-disabled={busy || undefined} onClick={remove}>{t('gif.settings.remove')}</Button>
          </div>
        ) : null}
        {showField ? (
          <form className="dw-gifs-settings__form" noValidate onSubmit={e => { e.preventDefault(); save(); }}>
            <TextField id={fieldId} label={t('gif.settings.key')} type="password" value={value} autoComplete="off" spellCheck={false}
              onChange={v => { setValue(v); setError(null); }} {...(error !== null ? { error } : {})} />
            <Button type="submit" variant="accent" aria-disabled={busy || undefined}>{t('gif.settings.save')}</Button>
            {on ? <Button type="button" onClick={cancel}>{t('gif.settings.cancel')}</Button> : null}
          </form>
        ) : null}
        {loaded === 'ready' && !showField && error !== null ? <p role="alert">{error}</p> : null}
        {loaded === 'ready' ? (
          <Select label={t('gif.settings.rating')} value={rating} disabled={busy}
            options={RATINGS.map(r => ({ value: r, label: t(`gif.rating.${r}` as StringKey) }))} onChange={pickRating} />
        ) : null}
      </div>
    </SettingsRow>
  );
}
