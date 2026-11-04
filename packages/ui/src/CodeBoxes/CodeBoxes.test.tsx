import { useState } from 'react';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { CodeBoxes, COMPLETE_DELAY_MS } from './CodeBoxes.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

function Controlled({ onComplete, onValue, initial = '' }: { onComplete?: (v: string) => void; onValue?: (v: string) => void; initial?: string }) {
  const [value, setValue] = useState(initial);
  return <CodeBoxes id="code" label="Code" value={value} onChange={v => { setValue(v); onValue?.(v); }} onComplete={onComplete} />;
}
const input = () => screen.getByRole('textbox', { name: 'Code' });
const boxes = (container: HTMLElement) => [...container.querySelectorAll('.d-code-boxes__box')];

describe('CodeBoxes', () => {
  it('is one labelled numeric one-time-code input over six aria-hidden boxes', async () => {
    const { container } = render(<Controlled />);
    const field = input();
    expect(field).toHaveAttribute('inputmode', 'numeric');
    expect(field).toHaveAttribute('autocomplete', 'one-time-code');
    expect(field).toHaveAttribute('maxlength', '6');
    expect(boxes(container)).toHaveLength(6);
    expect(container.querySelector('.d-code-boxes__row')).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getAllByRole('textbox')).toHaveLength(1);
    await expectNoAxeViolations(container);
  });

  it('keeps digits only: typing "12a3" keeps "123" and draws them in the first three boxes', async () => {
    const user = userEvent.setup();
    const { container } = render(<Controlled />);
    await user.type(input(), '12a3');
    expect(input()).toHaveValue('123');
    expect(boxes(container).map(b => b.textContent)).toEqual(['1', '2', '3', '', '', '']);
    expect(boxes(container)[3]).toHaveAttribute('data-caret', 'true');
    expect(boxes(container)[0]).toHaveAttribute('data-filled', 'true');
    expect(boxes(container)[0]).not.toHaveAttribute('data-caret');
  });

  it('a pasted grouped code "123 456" fills all six and completes once', async () => {
    const user = userEvent.setup();
    const onComplete = vi.fn();
    const { container } = render(<Controlled onComplete={onComplete} />);
    await user.click(input());
    await user.paste('123 456');
    expect(input()).toHaveValue('123456');
    expect(boxes(container).map(b => b.textContent)).toEqual(['1', '2', '3', '4', '5', '6']);
    // All six typed: the last box keeps the caret, so focus still shows.
    expect(boxes(container)[5]).toHaveAttribute('data-caret', 'true');
    await waitFor(() => { expect(onComplete).toHaveBeenCalledTimes(1); });
    expect(onComplete).toHaveBeenCalledWith('123456');
  });

  it('a pasted code with a trailing newline, or an autofilled one, completes once', async () => {
    const user = userEvent.setup();
    const onComplete = vi.fn();
    render(<Controlled onComplete={onComplete} />);
    await user.click(input());
    await user.paste('654321\n');
    expect(input()).toHaveValue('654321');
    await waitFor(() => { expect(onComplete).toHaveBeenCalledTimes(1); });
    // A further keystroke on a full code changes nothing and completes nothing again.
    await user.type(input(), '7');
    expect(input()).toHaveValue('654321');
    expect(onComplete).toHaveBeenCalledTimes(1);
  });

  it('typing the sixth digit completes once; deleting and retyping it completes again', async () => {
    const user = userEvent.setup();
    const onComplete = vi.fn();
    render(<Controlled onComplete={onComplete} />);
    await user.type(input(), '12345');
    expect(onComplete).not.toHaveBeenCalled();
    await user.type(input(), '6');
    await waitFor(() => { expect(onComplete).toHaveBeenCalledTimes(1); });
    await user.type(input(), '{Backspace}9');
    await waitFor(() => { expect(onComplete).toHaveBeenCalledTimes(2); });
    expect(onComplete).toHaveBeenLastCalledWith('123459');
  });

  it('a partial paste goes in at the caret', async () => {
    const user = userEvent.setup();
    const onValue = vi.fn();
    render(<Controlled onValue={onValue} initial="12" />);
    await user.click(input());
    await user.paste('3-4');
    expect(input()).toHaveValue('1234');
  });

  it('waits the prototype\'s 350 ms before completing; a change or an unmount in that time cancels it', () => {
    vi.useFakeTimers();
    try {
      const onComplete = vi.fn();
      const { unmount } = render(<Controlled onComplete={onComplete} />);
      fireEvent.change(input(), { target: { value: '123456' } });
      act(() => { vi.advanceTimersByTime(COMPLETE_DELAY_MS - 1); });
      expect(onComplete).not.toHaveBeenCalled();
      fireEvent.change(input(), { target: { value: '12345' } });
      act(() => { vi.advanceTimersByTime(COMPLETE_DELAY_MS); });
      expect(onComplete).not.toHaveBeenCalled();
      fireEvent.change(input(), { target: { value: '123457' } });
      act(() => { vi.advanceTimersByTime(COMPLETE_DELAY_MS); });
      expect(onComplete).toHaveBeenCalledTimes(1);
      expect(onComplete).toHaveBeenCalledWith('123457');
      fireEvent.change(input(), { target: { value: '12345' } });
      fireEvent.change(input(), { target: { value: '123458' } });
      unmount();
      act(() => { vi.advanceTimersByTime(COMPLETE_DELAY_MS); });
      expect(onComplete).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a value given from outside (a prefilled six) never completes', () => {
    vi.useFakeTimers();
    try {
      const onComplete = vi.fn();
      render(<Controlled onComplete={onComplete} initial="123456" />);
      act(() => { vi.advanceTimersByTime(COMPLETE_DELAY_MS * 2); });
      expect(onComplete).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it('lights the box at the input\'s caret, not only at the end', async () => {
    const user = userEvent.setup();
    const { container } = render(<Controlled initial="1234" />);
    await user.click(input());
    (input() as HTMLInputElement).setSelectionRange(1, 1);
    fireEvent.select(input());
    expect(boxes(container)[1]).toHaveAttribute('data-caret', 'true');
    expect(boxes(container)[4]).not.toHaveAttribute('data-caret');
    await user.keyboard('{End}');
    expect(boxes(container)[4]).toHaveAttribute('data-caret', 'true');
  });

  it('marks the input invalid and keeps its description', () => {
    const { container } = render(
      <><CodeBoxes id="code" label="Code" value="000000" onChange={() => {}} invalid describedBy="err" /><p id="err">Wrong code</p></>,
    );
    expect(input()).toHaveAttribute('aria-invalid', 'true');
    expect(input()).toHaveAccessibleDescription('Wrong code');
    expect(container.querySelector('.d-code-boxes')).toHaveAttribute('data-invalid', 'true');
  });
});
