import { render, screen, fireEvent } from '@testing-library/react';
import { describe, it, expect, vi } from 'vitest';
import { BotResult } from './BotResult.tsx';
const labels = { status: { pending: 'Working', success: 'Done', failed: 'Failed', cancelled: 'Not confirmed' }, onlyYou: 'Only you can see this', dismiss: 'Dismiss', dismissName: 'Dismiss: Result', tag: 'bot' };
const author = { name: 'dilla', avatarName: 'dilla', shape: 'circle' as const };
const mount = (onDismiss: () => void, at?: number) => render(<div>
  <BotResult author={author} time="10:24" status="success" body="Result" onDismiss={onDismiss} {...(at !== undefined ? { autoDismissAt: at } : {})} labels={labels} />
  <div className="d-composer"><textarea aria-label="Message" /></div></div>);
const article = () => document.querySelector('article')!;
describe('BotResult auto-dismiss', () => {
  it('dismisses at its deadline and not before, without moving focus', () => {
    vi.useFakeTimers(); vi.setSystemTime(0);
    try {
      const dismiss = vi.fn(); mount(dismiss, 60_000);
      const other = document.createElement('button'); document.body.append(other); other.focus();
      vi.advanceTimersByTime(59_999); expect(dismiss).not.toHaveBeenCalled();
      vi.advanceTimersByTime(1); expect(dismiss).toHaveBeenCalledOnce();
      expect(other).toHaveFocus(); other.remove();
    } finally { vi.useRealTimers(); }
  });
  it('pauses while hovered and resumes with the time remaining', () => {
    vi.useFakeTimers(); vi.setSystemTime(0);
    try {
      const dismiss = vi.fn(); mount(dismiss, 60_000);
      vi.advanceTimersByTime(50_000); fireEvent.pointerEnter(article());
      vi.advanceTimersByTime(30_000); expect(dismiss).not.toHaveBeenCalled();
      fireEvent.pointerLeave(article());
      vi.advanceTimersByTime(9_999); expect(dismiss).not.toHaveBeenCalled();
      vi.advanceTimersByTime(1); expect(dismiss).toHaveBeenCalledOnce();
    } finally { vi.useRealTimers(); }
  });
  it('pauses while focus is within and resumes on blur', () => {
    vi.useFakeTimers(); vi.setSystemTime(0);
    try {
      const dismiss = vi.fn(); mount(dismiss, 60_000);
      vi.advanceTimersByTime(50_000); screen.getByRole('button', { name: 'Dismiss: Result' }).focus();
      vi.advanceTimersByTime(30_000); expect(dismiss).not.toHaveBeenCalled();
      screen.getByRole('textbox', { name: 'Message' }).focus();
      vi.advanceTimersByTime(10_000); expect(dismiss).toHaveBeenCalledOnce();
    } finally { vi.useRealTimers(); }
  });
  it('a row already past its deadline goes at once, a row with no deadline stays', () => {
    vi.useFakeTimers(); vi.setSystemTime(100_000);
    try {
      const dismiss = vi.fn(); const { unmount } = mount(dismiss, 60_000);
      vi.advanceTimersByTime(0); expect(dismiss).toHaveBeenCalledOnce(); unmount();
      const stay = vi.fn(); mount(stay);
      vi.advanceTimersByTime(600_000); expect(stay).not.toHaveBeenCalled();
    } finally { vi.useRealTimers(); }
  });
});
