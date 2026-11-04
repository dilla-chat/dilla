// Wave B task 3b (Jonas): the recovery key's "Save as file". Its name carries a slug of the instance's name (security review
// LOW-1: the name is any string the instance serves; a bidi override, a path or hundreds of characters would show or be cut
// differently in each browser's download list). Its body keeps the name as written.
import { t } from '../strings/index.ts';

const SLUG_MAX = 64;
const FALLBACK = 'dilla';

/** NFKC, lower case, every run of characters outside a-z 0-9 . - as one hyphen, no leading or trailing dot or hyphen, at most
 *  64 characters; "dilla" when nothing is left. Pure. */
export function instanceSlug(name: string): string {
  const trim = (s: string) => s.replace(/^[.-]+|[.-]+$/g, '');
  const slug = trim(name.normalize('NFKC').toLowerCase().replace(/[^a-z0-9.-]+/g, '-').replace(/-{2,}/g, '-'));
  const capped = trim(slug.slice(0, SLUG_MAX));
  return capped === '' ? FALLBACK : capped;
}

/** The file Save as file hands to the browser: its suggested name, and its text with the key once (its groups joined by
 *  hyphens). Pure. */
export function recoveryKeyFile(instanceName: string, groups: readonly string[]): { name: string; text: string } {
  return {
    name: t('onboarding.keys.fileName', { instance: instanceSlug(instanceName) }),
    text: `${t('onboarding.keys.fileBody', { instance: instanceName })}\n\n${groups.join('-')}\n`,
  };
}
