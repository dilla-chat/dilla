import { useEffect, useLayoutEffect } from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { Dialog } from './Dialog.tsx';
import { Button } from '../Button/Button.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

describe('Dialog', () => {
  it('opens as a modal dialog named by its title and closes on Escape', async () => {
    const onClose = vi.fn();
    render(<Dialog open title="Leave voice?" onClose={onClose}><p>You can rejoin any time.</p></Dialog>);
    const dialog = screen.getByRole('dialog', { name: 'Leave voice?' });
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    await userEvent.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalledTimes(1);
  });
  it('closes only the inner dialog when Escape is pressed inside a nested one', async () => {
    const outer = vi.fn();
    const inner = vi.fn();
    render(<Dialog open title="Outer" onClose={outer}><Dialog open title="Inner" onClose={inner}><Button>inside</Button></Dialog></Dialog>);
    screen.getByRole('button', { name: 'inside' }).focus();
    await userEvent.keyboard('{Escape}');
    expect(inner).toHaveBeenCalledTimes(1);
    expect(outer).not.toHaveBeenCalled();
  });
  it('is described by the element whose id describedBy names', () => {
    render(<Dialog open title="Ban Astrid?" describedBy="why" onClose={() => {}}><p id="why">They cannot join again.</p></Dialog>);
    expect(screen.getByRole('dialog', { name: 'Ban Astrid?' })).toHaveAccessibleDescription('They cannot join again.');
  });
  it('has no description when describedBy is not given', () => {
    render(<Dialog open title="Ban Astrid?" onClose={() => {}}><p>They cannot join again.</p></Dialog>);
    expect(screen.getByRole('dialog', { name: 'Ban Astrid?' })).not.toHaveAttribute('aria-describedby');
  });
  it('renders nothing visible when closed', () => {
    render(<Dialog open={false} title="Hidden" onClose={() => {}}><p>x</p></Dialog>);
    expect(screen.queryByRole('dialog')).toBeNull();
  });
  it('has a close button in the footer by default', async () => {
    const onClose = vi.fn();
    render(<Dialog open title="Verify device" onClose={onClose}><p>Compare the code.</p></Dialog>);
    await userEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(onClose).toHaveBeenCalled();
  });
  it('closes the native dialog when open flips to false, so the browser returns focus', () => {
    const close = vi.spyOn(HTMLDialogElement.prototype, 'close');
    const { rerender } = render(<Dialog open title="Leave voice?" onClose={() => {}}><p>You can rejoin any time.</p></Dialog>);
    expect(close).not.toHaveBeenCalled();
    rerender(<Dialog open={false} title="Leave voice?" onClose={() => {}}><p>You can rejoin any time.</p></Dialog>);
    // close() is what hands focus back to whatever opened the dialog:
    // unmounting the element instead drops it on the document body.
    expect(close).toHaveBeenCalledTimes(1);
    close.mockRestore();
  });
  it('closes the native dialog in the commit that empties it, before any passive effect runs', () => {
    // A later sibling's layout effect runs after Dialog's own and before any passive effect: what it sees is what a key
    // event between the commit and React's next task meets. An open, empty dialog there took that Escape as its own
    // cancel, and the settings frame under it stayed open (WebKit CI run 39668959443).
    const seen: string[] = [];
    function Probe({ open }: { open: boolean }) {
      useLayoutEffect(() => {
        const el = document.querySelector('dialog');
        seen.push(`${String(open)}:${String(el?.open)}:${String(el?.childElementCount)}`);
      }, [open]);
      return null;
    }
    const tree = (open: boolean) => <><Dialog open={open} title="New category" onClose={() => {}}><p>Name it.</p></Dialog><Probe open={open} /></>;
    const { rerender } = render(tree(true));
    rerender(tree(false));
    expect(seen.at(-1)).toBe('false:false:0');
  });
  it('gives every dialog its own title id', () => {
    render(
      <>
        <Dialog open title="Leave voice?" onClose={() => {}}><p>You can rejoin any time.</p></Dialog>
        <Dialog open title="Compare the code" onClose={() => {}}><p>Read the six digits aloud.</p></Dialog>
      </>,
    );
    const [first, second] = screen.getAllByRole('dialog');
    const firstId = first.getAttribute('aria-labelledby');
    expect(firstId).toBeTruthy();
    expect(firstId).not.toBe(second.getAttribute('aria-labelledby'));
    expect(screen.getByRole('dialog', { name: 'Leave voice?' })).toBe(first);
    expect(screen.getByRole('dialog', { name: 'Compare the code' })).toBe(second);
  });
  it('names its close button Close when no closeLabel is given', () => {
    render(<Dialog open title="Leave voice?" onClose={() => {}}><p>You can rejoin any time.</p></Dialog>);
    expect(screen.getByRole('button', { name: 'Close' })).toBeInTheDocument();
  });
  it('names its close button by closeLabel, and then has no button named Close', async () => {
    const onClose = vi.fn();
    render(<Dialog open title="Lämna röst?" onClose={onClose} closeLabel="Stäng"><p>Du kan gå med igen när som helst.</p></Dialog>);
    expect(screen.queryByRole('button', { name: 'Close' })).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'Stäng' }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><Dialog open title="Leave voice?" onClose={() => {}} footer={<Button variant="danger">Leave</Button>}><p>You can rejoin any time.</p></Dialog></div>);
    await expectNoAxeViolations(container);
  });
});

describe('Dialog focus', () => {
  function Opener({ open }: { open: boolean }) {
    return <><button>{'opener'}</button>{open ? <Dialog open title="Edit" onClose={() => {}}><p>x</p></Dialog> : null}</>;
  }
  it('opens on the element inside marked data-autofocus', () => {
    render(<Dialog open title="Edit role" onClose={() => {}}><input aria-label="name" data-autofocus="" /></Dialog>);
    expect(screen.getByRole('textbox', { name: 'name' })).toHaveFocus();
  });
  it('skips a disabled data-autofocus element and opens on the dialog', () => {
    render(<Dialog open title="Edit role" onClose={() => {}}><input aria-label="name" disabled data-autofocus="" /></Dialog>);
    expect(screen.getByRole('dialog', { name: 'Edit role' })).toHaveFocus();
  });
  it('opens on its close button when initialFocus is close and nothing is marked', () => {
    render(<Dialog open title="Roles" closeLabel="Done" initialFocus="close" onClose={() => {}}><p>Nothing to give.</p></Dialog>);
    expect(screen.getByRole('button', { name: 'Done' })).toHaveFocus();
  });
  it('keeps its real opener when a child focuses something inside before the dialog opens', () => {
    // A child's effect runs before Dialog's: the opener must be read before it can move focus into the dialog.
    function Grabby() { useEffect(() => { document.getElementById('grab')?.focus(); }, []); return <input id="grab" aria-label="grab" />; }
    function Host({ open }: { open: boolean }) {
      return <><button>{'opener'}</button>{open ? <Dialog open title="Edit" onClose={() => {}}><Grabby /></Dialog> : null}</>;
    }
    const { rerender } = render(<Host open={false} />);
    screen.getByRole('button', { name: 'opener' }).focus();
    rerender(<Host open />);
    rerender(<Host open={false} />);
    expect(screen.getByRole('button', { name: 'opener' })).toHaveFocus();
  });
  it('returns focus to its opener when it is unmounted while open', () => {
    const { rerender } = render(<Opener open={false} />);
    screen.getByRole('button', { name: 'opener' }).focus();
    rerender(<Opener open />);
    expect(screen.getByRole('dialog', { name: 'Edit' })).toHaveFocus();
    rerender(<Opener open={false} />);
    expect(screen.getByRole('button', { name: 'opener' })).toHaveFocus();
  });
  it('returns focus to the element carrying the opener\'s focus key when the opener was re-mounted', () => {
    // A keyed wrapper re-mounts the opener (a member row moving into another group) while the dialog is open.
    function Moving({ open, at }: { open: boolean; at: 'a' | 'b' }) {
      return <>
        <div key={at}><button data-focus-key="member:u2">{'actions for Bjorn'}</button></div>
        {open ? <Dialog open title="Roles" onClose={() => {}}><p>x</p></Dialog> : null}
      </>;
    }
    const { rerender } = render(<Moving open={false} at="a" />);
    const first = screen.getByRole('button', { name: 'actions for Bjorn' });
    first.focus();
    rerender(<Moving open at="a" />);
    rerender(<Moving open at="b" />);
    const second = screen.getByRole('button', { name: 'actions for Bjorn' });
    expect(second).not.toBe(first);
    rerender(<Moving open={false} at="b" />);
    expect(second).toHaveFocus();
  });
  it('falls back to returnFocus when its opener is gone, never to the body', () => {
    function Gone({ open, opener }: { open: boolean; opener: boolean }) {
      return <>
        <button>{'new role'}</button>
        {opener ? <button>{'delete'}</button> : null}
        {open ? <Dialog open title="Delete" onClose={() => {}} returnFocus={() => screen.getByRole('button', { name: 'new role' })}><p>x</p></Dialog> : null}
      </>;
    }
    const { rerender } = render(<Gone open={false} opener />);
    screen.getByRole('button', { name: 'delete' }).focus();
    rerender(<Gone open opener />);
    rerender(<Gone open opener={false} />);
    rerender(<Gone open={false} opener={false} />);
    expect(screen.getByRole('button', { name: 'new role' })).toHaveFocus();
  });
  it('returns focus the same way when open flips to false while it stays mounted', () => {
    function Stay({ open, at }: { open: boolean; at: 'a' | 'b' }) {
      return <>
        <div key={at}><button data-focus-key="row:1">{'edit'}</button></div>
        <Dialog open={open} title="Edit" onClose={() => {}}><p>x</p></Dialog>
      </>;
    }
    const { rerender } = render(<Stay open={false} at="a" />);
    screen.getByRole('button', { name: 'edit' }).focus();
    rerender(<Stay open at="a" />);
    rerender(<Stay open at="b" />);
    rerender(<Stay open={false} at="b" />);
    expect(screen.getByRole('button', { name: 'edit' })).toHaveFocus();
  });
});

describe('Dialog late initial focus', () => {
  function Late({ ready, extra }: { ready: boolean; extra?: boolean }) {
    return <><button>{'opener'}</button>
      <Dialog open title="Roles" closeLabel="Done" initialFocus="close" onClose={() => {}}>
        {ready ? <input type="checkbox" aria-label="crew" data-autofocus="" /> : <p>Loading…</p>}
        {extra ? <button>{'other'}</button> : null}
      </Dialog></>;
  }
  it('moves to a marked element that arrives later while focus is still where the dialog put it', () => {
    const { rerender } = render(<Late ready={false} />);
    expect(screen.getByRole('button', { name: 'Done' })).toHaveFocus();
    rerender(<Late ready />);
    expect(screen.getByRole('checkbox', { name: 'crew' })).toHaveFocus();
  });
  it('leaves focus alone when the user moved it before the marked element arrived', () => {
    const { rerender } = render(<Late ready={false} extra />);
    screen.getByRole('button', { name: 'other' }).focus();
    rerender(<Late ready extra />);
    expect(screen.getByRole('button', { name: 'other' })).toHaveFocus();
  });
});
describe('Dialog focus, review minors', () => {
  it('skips a hidden data-autofocus element for the next shown one', () => {
    render(<Dialog open title="Edit" onClose={() => {}}>
      <div hidden><input aria-label="gone" data-autofocus="" /></div>
      <input aria-label="styled away" style={{ display: 'none' }} data-autofocus="" />
      <input aria-label="name" data-autofocus="" />
    </Dialog>);
    expect(screen.getByRole('textbox', { name: 'name' })).toHaveFocus();
  });
  it('falls back to the dialog when every marked element is hidden', () => {
    render(<Dialog open title="Edit" onClose={() => {}}><div hidden><input aria-label="gone" data-autofocus="" /></div></Dialog>);
    expect(screen.getByRole('dialog', { name: 'Edit' })).toHaveFocus();
  });
  it('leaves focus where a flow already moved it outside the dialog when it closes', () => {
    function Moved({ open }: { open: boolean }) {
      return <><button>{'opener'}</button><button>{'elsewhere'}</button>
        {open ? <Dialog open title="Edit" onClose={() => {}}><p>x</p></Dialog> : null}</>;
    }
    const { rerender } = render(<Moved open={false} />);
    screen.getByRole('button', { name: 'opener' }).focus();
    rerender(<Moved open />);
    screen.getByRole('button', { name: 'elsewhere' }).focus();
    rerender(<Moved open={false} />);
    expect(screen.getByRole('button', { name: 'elsewhere' })).toHaveFocus();
  });
  it('a nested dialog closing returns focus to its opener inside the outer dialog, which stays open', () => {
    function Nested({ inner }: { inner: boolean }) {
      return <Dialog open title="Settings" onClose={() => {}}>
        <button>{'new role'}</button>
        {inner ? <Dialog open title="New role" onClose={() => {}}><input aria-label="name" data-autofocus="" /></Dialog> : null}
      </Dialog>;
    }
    const { rerender } = render(<Nested inner={false} />);
    screen.getByRole('button', { name: 'new role' }).focus();
    rerender(<Nested inner />);
    expect(screen.getByRole('textbox', { name: 'name' })).toHaveFocus();
    rerender(<Nested inner={false} />);
    expect(screen.getByRole('button', { name: 'new role' })).toHaveFocus();
    expect(screen.getByRole('dialog', { name: 'Settings' })).toBeInTheDocument();
  });
});

it('distinguishes a backdrop dismissal from an explicit close choice', async () => {
  const close = vi.fn(); const backdrop = vi.fn();
  render(<Dialog open title="Confirm" onClose={close} onBackdrop={backdrop}><p>Details</p></Dialog>);
  fireEvent.click(screen.getByRole('dialog'));
  expect(backdrop).toHaveBeenCalledOnce();
  expect(close).not.toHaveBeenCalled();
  await userEvent.click(screen.getByRole('button', { name: 'Close' }));
  expect(close).toHaveBeenCalledOnce();
});
