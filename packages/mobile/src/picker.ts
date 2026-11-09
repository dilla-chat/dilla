// The instance picker: a port of packages/desktop/src/manager.ts (list, add, connect) over @capacitor/preferences.
import { Preferences } from '@capacitor/preferences';
import { addInstance, CONNECT_ERROR, currentInstance, listInstances, reachInstance, selectInstance } from './instances.ts';

const form = document.querySelector('form')!;
const input = document.querySelector<HTMLInputElement>('#instance')!;
const error = document.querySelector('#error')!;

async function connect(choose: () => Promise<unknown>): Promise<void> {
  error.textContent = '';
  for (const button of document.querySelectorAll('button')) button.disabled = true;
  try { await choose(); location.replace('/'); }
  catch { error.textContent = CONNECT_ERROR; for (const button of document.querySelectorAll('button')) button.disabled = false; }
}

async function render(): Promise<void> {
  const [instances, current] = await Promise.all([listInstances(Preferences), currentInstance(Preferences)]);
  const list = document.querySelector('#instances')!;
  list.replaceChildren();
  document.querySelector<HTMLElement>('#empty')!.hidden = instances.length > 0;
  for (const instance of instances) {
    const item = document.createElement('li');
    const button = document.createElement('button');
    button.type = 'button';
    button.textContent = instance;
    if (instance === current) button.setAttribute('aria-current', 'true');
    button.addEventListener('click', () => { void connect(() => selectInstance(Preferences, instance)); });
    item.append(button); list.append(item);
  }
}
form.addEventListener('submit', event => { event.preventDefault(); void connect(() => addInstance(Preferences, input.value, reachInstance(window.fetch.bind(window)))); });
void render().catch(() => { error.textContent = 'Could not read your instances. Restart Dilla to try again.'; });
