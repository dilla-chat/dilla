type Manager = { list(): Promise<string[]>; connect(instance: string): Promise<void> };
const manager = (window as unknown as { dillaManager: Manager }).dillaManager;
const form = document.querySelector('form')!;
const input = document.querySelector<HTMLInputElement>('#instance')!;
const error = document.querySelector('#error')!;

async function connect(instance: string): Promise<void> {
  error.textContent = '';
  for (const button of document.querySelectorAll('button')) button.disabled = true;
  try { await manager.connect(instance); await render(); }
  catch { error.textContent = 'Could not connect. Enter a complete HTTPS instance address, then try again.'; }
  finally { for (const button of document.querySelectorAll('button')) button.disabled = false; }
}

async function render(): Promise<void> {
  const instances = await manager.list();
  const list = document.querySelector('#instances')!;
  list.replaceChildren();
  document.querySelector<HTMLElement>('#empty')!.hidden = instances.length > 0;
  for (const instance of instances) {
    const item = document.createElement('li');
    const button = document.createElement('button');
    button.textContent = instance;
    button.addEventListener('click', () => { void connect(instance); });
    item.append(button); list.append(item);
  }
}
form.addEventListener('submit', event => { event.preventDefault(); void connect(input.value); });
void render().catch(() => { error.textContent = 'Could not read your instances. Restart Dilla to try again.'; });
