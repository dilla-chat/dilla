import { useState } from 'react';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { Segmented } from './Segmented.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

// L-COPY-02: appearance.theme.label and appearance.theme.{system,signal,light,contrast}.
const THEMES = [
  { value: 'system', label: 'system' }, { value: 'signal', label: 'signal' },
  { value: 'light', label: 'light' }, { value: 'high-contrast', label: 'high contrast' },
] as const;

function Controlled({ initial = 'system', onValue }: { initial?: string; onValue?: (v: string) => void }) {
  const [value, setValue] = useState(initial);
  return (
    <>
      <button type="button">before</button>
      <Segmented id="theme" label="theme" options={THEMES} value={value} onChange={v => { setValue(v); onValue?.(v); }} />
      <button type="button">after</button>
    </>
  );
}
const radio = (name: string) => screen.getByRole('radio', { name });

describe('Segmented', () => {
  it('is a radiogroup named by its label, one radio button per option, the value checked', () => {
    const { container } = render(<Segmented id="theme" label="theme" options={THEMES} value="signal" onChange={() => {}} />);
    const group = screen.getByRole('radiogroup', { name: 'theme' });
    expect(group).toHaveAttribute('id', 'theme');
    expect(container.firstElementChild).toHaveClass('d-segmented');
    const radios = screen.getAllByRole('radio');
    expect(radios.map(r => r.textContent)).toEqual(['system', 'signal', 'light', 'high contrast']);
    expect(radios.map(r => r.tagName)).toEqual(['BUTTON', 'BUTTON', 'BUTTON', 'BUTTON']);
    expect(radios.map(r => r.getAttribute('aria-checked'))).toEqual(['false', 'true', 'false', 'false']);
  });

  it('is one tab stop, on the checked option', async () => {
    const user = userEvent.setup();
    render(<Controlled initial="light" />);
    await user.tab();
    expect(screen.getByRole('button', { name: 'before' })).toHaveFocus();
    await user.tab();
    expect(radio('light')).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'after' })).toHaveFocus();
    await user.tab({ shift: true });
    expect(radio('light')).toHaveFocus();
  });

  it('moves the selection with the arrows, wrapping, and with Home and End', async () => {
    const user = userEvent.setup();
    const onValue = vi.fn();
    render(<Controlled onValue={onValue} />);
    await user.tab();
    await user.tab();
    expect(radio('system')).toHaveFocus();
    await user.keyboard('{ArrowRight}');
    expect(radio('signal')).toHaveFocus();
    expect(radio('signal')).toHaveAttribute('aria-checked', 'true');
    await user.keyboard('{ArrowLeft}{ArrowLeft}');
    expect(radio('high contrast')).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(radio('system')).toHaveFocus();
    await user.keyboard('{End}');
    expect(radio('high contrast')).toHaveFocus();
    await user.keyboard('{Home}');
    expect(radio('system')).toHaveAttribute('aria-checked', 'true');
    expect(onValue.mock.calls.map(c => c[0])).toEqual(['signal', 'system', 'high-contrast', 'system', 'high-contrast', 'system']);
  });

  it('selects on click and leaves a modified arrow alone', async () => {
    const user = userEvent.setup();
    const onValue = vi.fn();
    render(<Controlled onValue={onValue} />);
    await user.click(radio('light'));
    expect(radio('light')).toHaveAttribute('aria-checked', 'true');
    expect(radio('light')).toHaveFocus();
    await user.keyboard('{Alt>}{ArrowRight}{/Alt}');
    expect(radio('light')).toHaveFocus();
    expect(onValue.mock.calls.map(c => c[0])).toEqual(['light']);
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><Segmented id="theme" label="theme" options={THEMES} value="system" onChange={() => {}} /></div>);
    await expectNoAxeViolations(container);
  });
});
