import { useState } from 'react';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { SettingsEditProvider, useSettingsEdit, useSettingsPendingOperation, useSettingsWorkspace, type SettingsEditState } from './edit-state.tsx';
import { settingsEditing as copy } from '../strings/settingsEditing.ts';
import { pushHistoryState, replaceHistoryState, requestNavigation, useRoute } from '../router.ts';

function Draft({ busy = false, canSave = true, saveResult = true }: { busy?: boolean; canSave?: boolean; saveResult?: boolean }) {
  const [dirty, setDirty] = useState(true);
  const [busyState, setBusy] = useState(busy);
  const [saved, setSaved] = useState(false);
  const save = vi.fn(() => {
    if (saveResult) { setDirty(false); setSaved(true); }
    return Promise.resolve(saveResult);
  });
  const reset = vi.fn(() => { setDirty(false); setSaved(false); });
  const state: SettingsEditState = { dirty, busy: busyState, canSave, save, reset };
  const managed = useSettingsEdit(state);
  const workspace = useSettingsWorkspace();
  return <>
    <output data-testid="managed">{String(managed)}</output>
    <output data-testid="saved">{String(saved)}</output>
    {workspace?.footer}
    {workspace?.confirmation}
    <button type="button" onClick={() => setBusy(false)}>finish request</button>
    <button type="button" onClick={() => setDirty(false)}>clear dirty</button>
  </>;
}

function ManagedDraft(props: React.ComponentProps<typeof Draft>) {
  return <SettingsEditProvider><Draft {...props} /></SettingsEditProvider>;
}

function InlineDraft() {
  const [revision, setRevision] = useState(0);
  const [dirty, setDirty] = useState(true);
  const [saved, setSaved] = useState(-1);
  const managed = useSettingsEdit({
    dirty, busy: false, canSave: dirty,
    save: () => { setSaved(revision); setDirty(false); return Promise.resolve(true); },
    reset: () => setDirty(false),
  });
  const workspace = useSettingsWorkspace();
  return <>
    <output data-testid="inline-managed">{String(managed)}</output>
    <output data-testid="inline-saved">{saved}</output>
    <button type="button" onClick={() => setRevision(value => value + 1)}>change revision</button>
    {workspace?.footer}
    {workspace?.confirmation}
  </>;
}

function NestedNavigationDraft() {
  const [, navigate] = useRoute();
  const [accepted, setAccepted] = useState(false);
  useSettingsEdit({ dirty: true, busy: false, canSave: false, save: () => Promise.resolve(false), reset: () => undefined });
  const workspace = useSettingsWorkspace();
  return <>
    <button type="button" onClick={() => { void requestNavigation(() => { void navigate({ name: 'activity' }); }).then(setAccepted); }}>approved outer navigation</button>
    <output data-testid="nested-accepted">{String(accepted)}</output>
    {workspace?.confirmation}
  </>;
}

function PendingSection() {
  useSettingsEdit({ dirty: true, busy: false, canSave: true, save: () => Promise.resolve(true), reset: () => undefined });
  return null;
}

function PendingHarness() {
  const [pending, setPending] = useState(false);
  const [secondPending, setSecondPending] = useState(false);
  const [sectionMounted, setSectionMounted] = useState(true);
  useSettingsPendingOperation(pending);
  useSettingsPendingOperation(secondPending);
  const workspace = useSettingsWorkspace();
  return <>
    <button type="button" onClick={() => setPending(true)}>start pending operation</button>
    <button type="button" onClick={() => setPending(false)}>finish pending operation</button>
    <button type="button" onClick={() => setSecondPending(true)}>start second pending operation</button>
    <button type="button" onClick={() => setSecondPending(false)}>finish second pending operation</button>
    <button type="button" onClick={() => setSectionMounted(false)}>unmount edit section</button>
    {sectionMounted ? <PendingSection /> : null}
    {workspace?.footer}
    {workspace?.confirmation}
  </>;
}

function PopPromptDraft() {
  useRoute();
  useSettingsEdit({ dirty: true, busy: false, canSave: false, save: () => Promise.resolve(false), reset: () => undefined });
  return <>{useSettingsWorkspace()?.confirmation}</>;
}

function CleanRouteProbe() {
  const [, navigate] = useRoute();
  const [accepted, setAccepted] = useState(false);
  return <>
    <button type="button" onClick={() => { void navigate({ name: 'you' }, true).then(setAccepted); }}>navigate after prompt unmount</button>
    <output data-testid="after-unmount-accepted">{String(accepted)}</output>
  </>;
}

function AutosaveDraft({ flush }: { flush: () => Promise<boolean> }) {
  useSettingsEdit({ dirty: false, busy: false, canSave: true, save: flush, reset: () => undefined, flush });
  return <>{useSettingsWorkspace()?.confirmation}</>;
}

describe('useSettingsEdit', () => {
  // Fix round 2: a section that saves as you type (Profile) is let go only once its last change has landed.
  it('lets a saving-as-you-type section go once its flush lands, and prompts when the flush fails', async () => {
    const action = vi.fn();
    let land!: (ok: boolean) => void;
    const flush = vi.fn(() => new Promise<boolean>(resolve => { land = resolve; }));
    const view = render(<SettingsEditProvider><AutosaveDraft flush={flush} /></SettingsEditProvider>);
    let request!: Promise<boolean>;
    act(() => { request = requestNavigation(action); });
    await waitFor(() => expect(flush).toHaveBeenCalledTimes(1));
    expect(action).not.toHaveBeenCalled();
    await act(async () => { land(true); await request; });
    await expect(request).resolves.toBe(true);
    expect(action).toHaveBeenCalledTimes(1);
    act(() => { request = requestNavigation(action); });
    await waitFor(() => expect(flush).toHaveBeenCalledTimes(2));
    act(() => { land(false); });
    expect(await screen.findByRole('dialog', { name: copy.leaveTitle })).toBeVisible();
    expect(action).toHaveBeenCalledTimes(1);
    view.unmount();
    await expect(request).resolves.toBe(false);
  });

  it('keeps standalone forms unmanaged', () => {
    render(<Draft />);
    expect(screen.getByTestId('managed')).toHaveTextContent('false');
    expect(screen.queryByRole('button', { name: copy.save })).not.toBeInTheDocument();
  });

  it('accepts inline state objects and uses the latest operation closures without rerendering forever', async () => {
    const user = userEvent.setup();
    render(<SettingsEditProvider><InlineDraft /></SettingsEditProvider>);
    expect(await screen.findByRole('button', { name: copy.save })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'change revision' }));
    await user.click(screen.getByRole('button', { name: 'change revision' }));
    await user.click(screen.getByRole('button', { name: copy.save }));
    expect(await screen.findByTestId('inline-saved')).toHaveTextContent('2');
    await waitFor(() => expect(screen.queryByRole('button', { name: copy.save })).not.toBeInTheDocument());
  });

  it('shows shared Save and Reset controls, retaining dirty state after a refused save', async () => {
    const user = userEvent.setup();
    render(<ManagedDraft saveResult={false} />);
    await screen.findByRole('button', { name: copy.save });
    await user.click(screen.getByRole('button', { name: copy.save }));
    expect(await screen.findByText(copy.saveFailed)).toHaveAttribute('role', 'status');
    expect(screen.getByRole('button', { name: copy.reset })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: copy.reset }));
    await waitFor(() => expect(screen.queryByRole('button', { name: copy.save })).not.toBeInTheDocument());
  });

  it('runs a successful save and hides the footer after the section updates its baseline', async () => {
    const user = userEvent.setup();
    render(<ManagedDraft />);
    await user.click(await screen.findByRole('button', { name: copy.save }));
    await waitFor(() => expect(screen.getByTestId('saved')).toHaveTextContent('true'));
    await waitFor(() => expect(screen.queryByRole('button', { name: copy.save })).not.toBeInTheDocument());
  });

  it('prompts once for router navigation, keeps editing on cancel, and permits after discard', async () => {
    const user = userEvent.setup();
    const action = vi.fn();
    render(<ManagedDraft />);
    await screen.findByRole('button', { name: copy.save });
    let request!: Promise<boolean>;
    act(() => { request = requestNavigation(action); });
    const dialog = await screen.findByRole('dialog', { name: copy.leaveTitle });
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    await expect(requestNavigation(action)).resolves.toBe(false);
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    await user.click(within(dialog).getByRole('button', { name: copy.keepEditing }));
    await expect(request).resolves.toBe(false);
    expect(action).not.toHaveBeenCalled();
    act(() => { request = requestNavigation(action); });
    const nextDialog = await screen.findByRole('dialog', { name: copy.leaveTitle });
    await user.click(within(nextDialog).getByRole('button', { name: copy.discard }));
    await expect(request).resolves.toBe(true);
    expect(action).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(screen.queryByRole('button', { name: copy.save })).not.toBeInTheDocument());
  });

  it('allows a guarded action to call navigate once after its confirmation', async () => {
    const user = userEvent.setup();
    window.history.replaceState(null, '', '/you');
    render(<SettingsEditProvider><NestedNavigationDraft /></SettingsEditProvider>);
    await user.click(screen.getByRole('button', { name: 'approved outer navigation' }));
    const dialog = await screen.findByRole('dialog', { name: copy.leaveTitle });
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    await user.click(within(dialog).getByRole('button', { name: copy.discard }));
    await waitFor(() => expect(window.location.pathname).toBe('/activity'));
    expect(screen.getByTestId('nested-accepted')).toHaveTextContent('true');
  });

  it('cancels an open guard on provider unmount and permits navigation after a later provider mounts', async () => {
    const action = vi.fn();
    const view = render(<SettingsEditProvider><Draft /></SettingsEditProvider>);
    await screen.findByRole('button', { name: copy.save });
    let request!: Promise<boolean>;
    act(() => { request = requestNavigation(action); });
    await screen.findByRole('dialog', { name: copy.leaveTitle });

    view.unmount();
    await expect(request).resolves.toBe(false);
    expect(action).not.toHaveBeenCalled();
    await expect(requestNavigation(() => undefined)).resolves.toBe(true);

    render(<SettingsEditProvider><button type="button">later provider</button></SettingsEditProvider>);
    await screen.findByRole('button', { name: 'later provider' });
    await expect(requestNavigation(action)).resolves.toBe(true);
    expect(action).toHaveBeenCalledTimes(1);
  });

  it('clears a guarded browser traversal when its prompt provider unmounts', async () => {
    const user = userEvent.setup();
    replaceHistoryState(null, '', '/you');
    pushHistoryState(null, '', '/activity');
    const view = render(<SettingsEditProvider><PopPromptDraft /></SettingsEditProvider>);
    act(() => { window.history.back(); });
    await screen.findByRole('dialog', { name: copy.leaveTitle });
    expect(window.location.pathname).toBe('/activity');

    view.unmount();
    await expect(requestNavigation(() => undefined)).resolves.toBe(true);
    render(<SettingsEditProvider><CleanRouteProbe /></SettingsEditProvider>);
    await user.click(screen.getByRole('button', { name: 'navigate after prompt unmount' }));
    await waitFor(() => expect(window.location.pathname).toBe('/you'));
    expect(screen.getByTestId('after-unmount-accepted')).toHaveTextContent('true');
  });

  it('keeps independent pending work guarded after a section unmounts, without disabling its form actions', async () => {
    const user = userEvent.setup();
    render(<SettingsEditProvider><PendingHarness /></SettingsEditProvider>);
    expect(await screen.findByRole('button', { name: copy.save })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: 'start pending operation' }));
    await user.click(screen.getByRole('button', { name: 'start second pending operation' }));
    expect(screen.getByRole('button', { name: copy.save })).toBeEnabled();
    expect(screen.getByRole('button', { name: copy.reset })).toBeEnabled();
    const activeEvent = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(activeEvent);
    expect(activeEvent.defaultPrevented).toBe(true);

    await user.click(screen.getByRole('button', { name: 'unmount edit section' }));
    await waitFor(() => expect(screen.queryByRole('button', { name: copy.save })).not.toBeInTheDocument());
    let accepted!: Promise<boolean>;
    const action = vi.fn();
    act(() => { accepted = requestNavigation(action); });
    const dialog = await screen.findByRole('dialog', { name: copy.leaveTitle });
    expect(within(dialog).getByText(copy.leaveBusyBody)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: copy.keepEditing }));
    await expect(accepted).resolves.toBe(false);
    expect(action).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: 'finish pending operation' }));
    const stillBusyEvent = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(stillBusyEvent);
    expect(stillBusyEvent.defaultPrevented).toBe(true);
    await user.click(screen.getByRole('button', { name: 'finish second pending operation' }));
    await waitFor(() => {
      const cleanEvent = new Event('beforeunload', { cancelable: true });
      window.dispatchEvent(cleanEvent);
      expect(cleanEvent.defaultPrevented).toBe(false);
    });
    await expect(requestNavigation(action)).resolves.toBe(true);
    expect(action).toHaveBeenCalledTimes(1);
  });

  it('guards a pending accepted request and explains that it continues after leaving', async () => {
    const user = userEvent.setup();
    render(<ManagedDraft busy />);
    let request!: Promise<boolean>;
    act(() => { request = requestNavigation(() => undefined); });
    const dialog = await screen.findByRole('dialog', { name: copy.leaveTitle });
    expect(within(dialog).getByText(copy.leaveBusyBody)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: copy.save })).toBeDisabled();
    await user.click(within(dialog).getByRole('button', { name: copy.discard }));
    await expect(request).resolves.toBe(true);
  });

  it('sets beforeunload protection only while dirty or busy', async () => {
    const user = userEvent.setup();
    render(<ManagedDraft />);
    await screen.findByRole('button', { name: copy.save });
    const dirtyEvent = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(dirtyEvent);
    expect(dirtyEvent.defaultPrevented).toBe(true);
    await user.click(screen.getByRole('button', { name: 'clear dirty' }));
    await waitFor(() => expect(screen.queryByRole('button', { name: copy.save })).not.toBeInTheDocument());
    const cleanEvent = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(cleanEvent);
    expect(cleanEvent.defaultPrevented).toBe(false);
  });
});
