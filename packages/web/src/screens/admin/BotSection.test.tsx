import { describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { PhoneLayoutProvider } from '../../../../ui/src/internal/phoneLayout.ts';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { account, ME } from '../../test/fixtures.ts';
import { en } from '../../strings/index.ts';
import { decode, encode } from '@dilla/client-core';
import { ServerSettings } from './ServerSettings.tsx';
import { AutoModerationSettingsPanel } from './AutoModerationSettingsPanel.tsx';
import { BotSection, type BotPageKind } from './BotSection.tsx';

vi.mock('./spotify-link.ts', async importOriginal => ({ ...await importOriginal<typeof import('./spotify-link.ts')>(), authorizeSpotify: vi.fn() }));
import { authorizeSpotify, spotifySession } from './spotify-link.ts';

const SERVER = 'a1'.repeat(16);
type Installation = { id: string; communityId: string; state: 0 | 1 | 2 | 3 | 4 | 5; revision: string;
  userId: string | null; deviceId: string | null; capabilities: number; updatedAt: string;
  restartRequested?: string; restartHandled?: string; version?: string | null; musicVolume?: number; musicIdleMinutes?: number };
const ROW: Installation = { id: 'b2'.repeat(16), communityId: SERVER, state: 0, revision: '1', userId: null,
  deviceId: null, capabilities: 0, updatedAt: '100' };
const ACCESS = { top: 100, owner: true, manageCommunity: true, manageChannels: true, kickMembers: true,
  banMembers: true, moderateMembers: true, createInvite: true, manageRoles: true, viewAuditLog: true, loaded: true, overwritesLoaded: true, everyoneId: 'ab'.repeat(16) };

function setup(initial: Installation | null = null, failGet = false, phone = false, manageRoles = true, fakeTimers = false,
  handler?: (command: Parameters<FakeClient['call']>[0]) => Promise<unknown>, page: BotPageKind = 'bot') {
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('factors', { owed: false, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true });
  fake.set('communities', [{ id: SERVER, name: 'Midgard', owner: ME.id }]);
  fake.set(`permissions:${SERVER}`, { ...ACCESS, manageRoles });
  fake.set(`roles:${SERVER}`, [
    { id: '22'.repeat(16), name: 'Members', position: 1, members: 0, allow: 0, deny: 0, color: 0, hoist: false, mentionable: false },
  ]);
  fake.handler = handler ?? (async command => {
    if (command.m === 'getBotInstallation') return failGet ? Promise.reject(refusal({ code: 'E_NETWORK' })) : initial;
    if (command.m === 'getBotJoinRole') return initial === null ? null : { roleId: null, installationId: initial.id };
    if (command.m === 'stepUp') return { token: 'short-lived-step-up' };
    if (command.m === 'configureBotInstallation') return { ...ROW, state: 0, revision: '1' };
    if (command.m === 'disableBotInstallation') return { ...ROW, state: 4, revision: '2' };
    return command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null;
  });
  const announce = vi.fn();
  const user = fakeTimers ? userEvent.setup({ advanceTimers: vi.advanceTimersByTime }) : userEvent.setup();
  const view = render(<PhoneLayoutProvider value={phone}><div className="d-root"><CoreProvider client={fake}>
    {phone ? <ServerSettings communityId={SERVER} tab={page} onTab={vi.fn()} onClose={vi.fn()} announce={announce} />
      : <BotSection communityId={SERVER} serverName="Midgard" announce={announce} page={page} />}
  </CoreProvider></div></PhoneLayoutProvider>);
  return { fake, user, view, announce };
}

/** Opens Glenn's row menu (Task 10c, Ruling X) with Shift+F10 and picks `item`. */
async function glennMenu(user: ReturnType<typeof userEvent.setup>, item: string) {
  const row = await waitFor(() => { const el = document.querySelector<HTMLElement>('[role="group"][aria-keyshortcuts="Shift+F10"]'); if (el === null) throw new Error('no row'); return el; });
  row.focus();
  await user.keyboard('{Shift>}{F10}{/Shift}');
  await user.click(await screen.findByRole('menuitem', { name: item }));
}

function setupPage(page: BotPageKind, initial: Installation | null = null, failGet = false, phone = false, manageRoles = true,
  fakeTimers = false, handler?: (command: Parameters<FakeClient['call']>[0]) => Promise<unknown>) {
  return setup(initial, failGet, phone, manageRoles, fakeTimers, handler, page);
}

describe('the server bot settings', () => {
  it('lets a community manager save the managed bot display name in settings', async () => {
    const botId = 'c3'.repeat(16);
    const { fake, user, announce } = setup({ ...ROW, state: 3, userId: botId });
    fake.publish(`members:${SERVER}`, [{ userId: botId, username: 'dillabot', display: 'Dilla Bot', kind: 1, roleIds: [] }]);
    fake.handler = command => command.m === 'patchBotProfile' ? Promise.resolve('Music Bot') : Promise.resolve(null);
    // Task 10c: the name is renamed from Glenn's row menu, in a dialog.
    await screen.findByText('Dilla Bot');
    await glennMenu(user, en['serverSettings.bot.menu.rename']);
    const field = await screen.findByRole('textbox', { name: 'Bot display name' });
    await waitFor(() => expect(field).toHaveValue('Dilla Bot'));
    fireEvent.change(field, { target: { value: 'Music Bot' } });
    expect(field).toHaveValue('Music Bot');
    await user.click(screen.getByRole('button', { name: 'Save bot name' }));
    await waitFor(() => expect(fake.callsOf('patchBotProfile')).toEqual([{ m: 'patchBotProfile', communityId: SERVER, display: 'Music Bot' }]));
    expect(announce).toHaveBeenCalledWith('Bot name saved.');
  });
  it('discards an op 62 upload response after the Server Settings community changes', async () => {
    const fake=new FakeClient(); let release!: (value:{requestId:string;upload:string})=>void;
    fake.handler=command=>command.m==='humanInteractionRequest'?new Promise(resolve=>{release=resolve;}):Promise.resolve(null);
    const user=userEvent.setup(); const other='ee'.repeat(16);
    const panel=(cid:string)=><CoreProvider client={fake}><AutoModerationSettingsPanel communityId={cid} botReady={true} canManage={true}
      canManageMessages={true} canModerateMembers={true} capabilities={(1<<4)|(1<<5)}/></CoreProvider>;
    const view=render(panel(SERVER));
    await user.click(screen.getByRole('button',{name:en['admin.bot.automod.load']}));
    await waitFor(()=>expect(fake.callsOf('humanInteractionRequest')).toHaveLength(1));
    const request=fake.callsOf('humanInteractionRequest')[0];
    view.rerender(panel(other));
    await act(async()=>{release({requestId:request.requestId,upload:'accepted'});await Promise.resolve();});
    expect(screen.queryByText(en['admin.bot.automod.loaded'])).not.toBeInTheDocument();
    expect(screen.getByRole('button',{name:en['admin.bot.automod.load']})).toBeInTheDocument();
  });

  it('waits for automatic settings response acknowledgement before reporting loaded', async () => {
    const ready = { ...ROW, state: 3 as const, capabilities: (1 << 4) | (1 << 5), revision: '7' };
    const { fake, user } = setupPage('automoderation', ready);
    let releaseAck!: (value: null) => void;
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(ready);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: ready.id });
      if (command.m === 'humanInteractionRequest') return Promise.resolve({ requestId: command.requestId, upload: 'accepted' });
      if (command.m === 'readBotInteractionResponses') return Promise.resolve({ through: '1', blocked: false, responses: [{
        deliveryId: '1', sourceSeq: '1', requestId: fake.callsOf('humanInteractionRequest').at(-1)?.requestId ?? '',
        communityId: SERVER, operation: 62, channelId: null, status: 0, revision: '7',
        data: encode([1n, 0n, 'log', 'log', 'log', 600000n, []]),
      }] });
      if (command.m === 'ackBotInteractionResponses') return new Promise(resolve => { releaseAck = resolve; });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await user.click(await screen.findByRole('button', { name: en['admin.bot.automod.load'] }));
    await waitFor(() => expect(fake.callsOf('ackBotInteractionResponses')).toHaveLength(1));
    expect(screen.queryByText(en['admin.bot.automod.loaded'])).not.toBeInTheDocument();
    await act(async () => { releaseAck(null); await Promise.resolve(); });
    await screen.findByText(en['admin.bot.automod.loaded']);
  });

  it('waits for the exact settings response and acknowledges its opaque lease', async () => {
    const ready = { ...ROW, state: 3 as const, capabilities: (1 << 4) | (1 << 5), revision: '7' };
    const { fake, user } = setupPage('automoderation', ready);
    let prefixOpen = false;
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(ready);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: ready.id });
      if (command.m === 'humanInteractionRequest') return Promise.resolve({ requestId: command.requestId, upload: 'accepted' });
      if (command.m === 'readBotInteractionResponses') return Promise.resolve({ through: prefixOpen ? 'a'.repeat(32) : null, blocked: !prefixOpen, responses: prefixOpen ? [{
        deliveryId: '4', sourceSeq: '8', requestId: fake.callsOf('humanInteractionRequest').at(-1)?.requestId ?? '',
        communityId: SERVER, operation: 62, channelId: null, status: 0, revision: '7',
        data: encode([1n, 0n, 'log', 'log', 'log', 600000n, []]),
      }] : [] });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await user.click(await screen.findByRole('button', { name: en['admin.bot.automod.load'] }));
    await waitFor(() => expect(fake.callsOf('readBotInteractionResponses').length).toBeGreaterThan(0));
    expect(screen.queryByText(en['admin.bot.automod.loaded'])).not.toBeInTheDocument();
    expect(fake.callsOf('ackBotInteractionResponses')).toHaveLength(0);
    prefixOpen = true;
    await screen.findByText(en['admin.bot.automod.loaded']);
    await waitFor(() => expect(fake.callsOf('ackBotInteractionResponses')).toHaveLength(1));
  });

  it('loads automatic moderation over op 62 and prepares a step-up protected op 63 save', async () => {
    const ready = { ...ROW, state: 3 as const, capabilities: (1 << 4) | (1 << 5), revision: '7' };
    const { fake, user } = setupPage('automoderation', ready);
    const latest = new Map<number, { requestId: string; operation: number }>();
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(ready);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: ready.id });
      if (command.m === 'stepUp') return Promise.resolve({ token: 'short-lived-step-up' });
      if (command.m === 'humanInteractionRequest') { latest.set(command.operation, command); return Promise.resolve({ requestId: command.requestId, upload: 'accepted' }); }
      if (command.m === 'readBotInteractionResponses') { const last=latest.get(command.operations[0]); return Promise.resolve({ through: '1', blocked: false, responses: last === undefined ? [] : [{
        deliveryId: '1', sourceSeq: '1', requestId: last.requestId, communityId: SERVER, operation: last.operation,
        channelId: null, status: 0, revision: last.operation === 62 ? '7' : '8',
        data: last.operation === 62 ? encode([1n,0n,'log','log','log',600000n,[]]) : encode([]),
      }] }); }
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await user.click(await screen.findByRole('button', { name: en['admin.bot.automod.load'] }));
    await waitFor(() => expect(fake.callsOf('humanInteractionRequest')).toMatchObject([{ operation: 62, channelId: null, revision: null, args: [] }]));
    await screen.findByText(en['admin.bot.automod.loaded']);
    const timeout=screen.getByRole('spinbutton',{name:en['admin.bot.automod.timeout']});
    await user.clear(timeout);await user.type(timeout,'0');
    await user.click(screen.getByRole('button',{name:en['admin.bot.automod.save']}));
    expect(await screen.findByRole('alert')).toHaveTextContent(en['admin.bot.automod.prepareError']);
    expect(fake.callsOf('humanInteractionRequest').filter(c=>c.operation===63)).toHaveLength(0);
    await user.clear(timeout);await user.type(timeout,'600000');
    await user.click(screen.getByRole('button', { name: en['admin.bot.automod.save'] }));
    const dialog = await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] });
    await user.type(within(dialog).getByLabelText(en['factorSettings.stepUp.password']), 'password');
    await user.click(within(dialog).getByRole('button', { name: en['factorSettings.stepUp.confirm'] }));
    await waitFor(() => expect(fake.callsOf('humanInteractionRequest').some(c => c.operation === 63)).toBe(true));
    const save = fake.callsOf('humanInteractionRequest').find(c => c.operation === 63)!;
    expect(save).toMatchObject({ communityId: SERVER, channelId: null, revision: '7', stepUp: 'short-lived-step-up', args: ['', expect.any(Uint8Array)] });
    expect(save.args[1]).toBeInstanceOf(Uint8Array);
    await waitFor(() => expect(screen.getByText(en['admin.bot.automod.loaded'])).toBeInTheDocument());
    expect(fake.callsOf('humanInteractionRequest').filter(c => c.operation === 62)).toHaveLength(2);
  });

  it.each(['E_CORE_STATE','E_FORBIDDEN'] as const)('does not poll responses after a definite %s save rejection', async code => {
    const ready={...ROW,state:3 as const,capabilities:(1<<4)|(1<<5)}; const {fake,user}=setupPage('automoderation', ready);const responseReads:number[][]=[];
    fake.handler=command=>{
      if(command.m==='getBotInstallation')return Promise.resolve(ready);
      if(command.m==='getBotJoinRole')return Promise.resolve({roleId:null,installationId:ready.id});
      if(command.m==='stepUp')return Promise.resolve({token:'short-lived-step-up'});
      if(command.m==='humanInteractionRequest'){
        if(command.operation===63)return Promise.reject(refusal({code,status:code==='E_FORBIDDEN'?403:0}));
        return Promise.resolve({requestId:command.requestId,upload:'accepted'});
      }
      if(command.m==='readBotInteractionResponses'){
        responseReads.push([...command.operations]);
        return Promise.resolve({through:'1',blocked:false,responses:[{deliveryId:'1',sourceSeq:'1',requestId:fake.callsOf('humanInteractionRequest').at(-1)?.requestId??'',
          communityId:SERVER,operation:62,channelId:null,status:0,revision:'7',data:encode([1n,0n,'log','log','log',600000n,[]])}]});
      }
      return Promise.resolve(command.m==='getAFKSettings'?{channelId:null,timeoutSeconds:300}:null);
    };
    await user.click(await screen.findByRole('button',{name:en['admin.bot.automod.load']}));
    await screen.findByText(en['admin.bot.automod.loaded']);
    await user.click(screen.getByRole('button',{name:en['admin.bot.automod.save']}));
    const dialog=await screen.findByRole('dialog',{name:en['factorSettings.stepUp.title']});
    await user.type(within(dialog).getByLabelText(en['factorSettings.stepUp.password']),'password');
    await user.click(within(dialog).getByRole('button',{name:en['factorSettings.stepUp.confirm']}));
    expect(await screen.findByRole('alert')).toHaveTextContent(en['admin.bot.automod.saveError']);
    expect(responseReads).not.toContainEqual([63]);
    expect(screen.queryByRole('button',{name:en['admin.bot.automod.retry']})).not.toBeInTheDocument();
  });

  it('does not send automatic moderation config when step-up is denied', async () => {
    const ready={...ROW,state:3 as const,capabilities:(1<<4)|(1<<5)}; const {fake,user}=setupPage('automoderation', ready);
    fake.handler=command=>{
      if(command.m==='getBotInstallation')return Promise.resolve(ready);
      if(command.m==='getBotJoinRole')return Promise.resolve({roleId:null,installationId:ready.id});
      if(command.m==='humanInteractionRequest')return Promise.resolve({requestId:command.requestId,upload:'accepted'});
      if(command.m==='readBotInteractionResponses')return Promise.resolve({through:'1',blocked:false,responses:[{deliveryId:'1',sourceSeq:'1',
        requestId:fake.callsOf('humanInteractionRequest').at(-1)?.requestId??'',communityId:SERVER,operation:62,channelId:null,status:0,revision:'7',
        data:encode([1n,0n,'log','log','log',600000n,[]])}]});
      if(command.m==='stepUp')return Promise.reject(refusal({code:'E_STEP_UP',status:403}));
      return Promise.resolve(command.m==='getAFKSettings'?{channelId:null,timeoutSeconds:300}:null);
    };
    await user.click(await screen.findByRole('button',{name:en['admin.bot.automod.load']}));
    await screen.findByText(en['admin.bot.automod.loaded']);
    await user.click(screen.getByRole('button',{name:en['admin.bot.automod.save']}));
    const dialog=await screen.findByRole('dialog',{name:en['factorSettings.stepUp.title']});
    await user.type(within(dialog).getByLabelText(en['factorSettings.stepUp.password']),'wrong');
    await user.click(within(dialog).getByRole('button',{name:en['factorSettings.stepUp.confirm']}));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(en['factorSettings.stepUp.failed']);
    expect(fake.callsOf('humanInteractionRequest').filter(c=>c.operation===63)).toHaveLength(0);
  });

  it('retries an uncertain automatic moderation upload with the same request ID and document', async () => {
    const ready={...ROW,state:3 as const,capabilities:(1<<4)|(1<<5)}; const {fake,user}=setupPage('automoderation', ready);
    const latest=new Map<number,{requestId:string;operation:number}>(); let saves=0;
    fake.handler=command=>{
      if(command.m==='getBotInstallation')return Promise.resolve(ready);
      if(command.m==='getBotJoinRole')return Promise.resolve({roleId:null,installationId:ready.id});
      if(command.m==='stepUp')return Promise.resolve({token:'short-lived-step-up'});
      if(command.m==='humanInteractionRequest'){
        latest.set(command.operation,command);
        if(command.operation===63){saves++;return Promise.resolve({requestId:command.requestId,upload:saves===1?'unknown':'accepted'});}
        return Promise.resolve({requestId:command.requestId,upload:'accepted'});
      }
      if(command.m==='readBotInteractionResponses'){
        const row=latest.get(command.operations[0]);
        if(row===undefined||row.operation===63&&saves<2)return Promise.resolve({through:null,blocked:false,responses:[]});
        return Promise.resolve({through:'1',blocked:false,responses:[{deliveryId:'1',sourceSeq:'1',requestId:row.requestId,communityId:SERVER,
          operation:row.operation,channelId:null,status:0,revision:row.operation===62?'7':'8',
          data:row.operation===62?encode([1n,0n,'log','log','log',600000n,[]]):encode([])}]});
      }
      return Promise.resolve(command.m==='getAFKSettings'?{channelId:null,timeoutSeconds:300}:null);
    };
    await user.click(await screen.findByRole('button',{name:en['admin.bot.automod.load']}));
    await screen.findByText(en['admin.bot.automod.loaded']);
    await user.click(screen.getByRole('button',{name:en['admin.bot.automod.save']}));
    const dialog=await screen.findByRole('dialog',{name:en['factorSettings.stepUp.title']});
    await user.type(within(dialog).getByLabelText(en['factorSettings.stepUp.password']),'password');
    await user.click(within(dialog).getByRole('button',{name:en['factorSettings.stepUp.confirm']}));
    await screen.findByText(en['admin.bot.automod.uncertain']);
    await user.click(screen.getByRole('button',{name:en['admin.bot.automod.retry']}));
    await waitFor(()=>expect(saves).toBe(2));
    const calls=fake.callsOf('humanInteractionRequest').filter(c=>c.operation===63);
    expect(calls).toHaveLength(2);expect(calls[1].requestId).toBe(calls[0].requestId);expect(calls[1].args[1]).toEqual(calls[0].args[1]);
    await screen.findByText(en['admin.bot.automod.loaded']);
  });

  it('loads more than 64 exemptions and preserves them on an unrelated save within the encoded size limit', async () => {
    const ready={...ROW,state:3 as const,capabilities:(1<<4)|(1<<5)};const {fake,user}=setupPage('automoderation', ready);
    const exemptions=Array.from({length:68},(_,i)=>i.toString(16).padStart(32,'0'));
    expect(encode([1n,0n,'log','log','log',601000n,exemptions]).length).toBeLessThanOrEqual(2400);
    const latest=new Map<number,{requestId:string;operation:number}>();
    fake.handler=command=>{
      if(command.m==='getBotInstallation')return Promise.resolve(ready);
      if(command.m==='getBotJoinRole')return Promise.resolve({roleId:null,installationId:ready.id});
      if(command.m==='stepUp')return Promise.resolve({token:'short-lived-step-up'});
      if(command.m==='humanInteractionRequest'){latest.set(command.operation,command);return Promise.resolve({requestId:command.requestId,upload:'accepted'});}
      if(command.m==='readBotInteractionResponses'){
        const row=latest.get(command.operations[0]);if(row===undefined)return Promise.resolve({through:null,blocked:false,responses:[]});
        return Promise.resolve({through:'1',blocked:false,responses:[{deliveryId:'1',sourceSeq:'1',requestId:row.requestId,communityId:SERVER,
          operation:row.operation,channelId:null,status:0,revision:row.operation===62?'7':'8',
          data:row.operation===62?encode([1n,0n,'log','log','log',600000n,exemptions]):encode([])}]});
      }
      return Promise.resolve(command.m==='getAFKSettings'?{channelId:null,timeoutSeconds:300}:null);
    };
    await user.click(await screen.findByRole('button',{name:en['admin.bot.automod.load']}));
    await screen.findByText(en['admin.bot.automod.loaded']);
    await user.clear(screen.getByRole('spinbutton',{name:en['admin.bot.automod.timeout']}));
    await user.type(screen.getByRole('spinbutton',{name:en['admin.bot.automod.timeout']}),'601000');
    await user.click(screen.getByRole('button',{name:en['admin.bot.automod.save']}));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    const dialog=await screen.findByRole('dialog',{name:en['factorSettings.stepUp.title']});
    await user.type(within(dialog).getByLabelText(en['factorSettings.stepUp.password']),'password');
    await user.click(within(dialog).getByRole('button',{name:en['factorSettings.stepUp.confirm']}));
    await waitFor(()=>expect(fake.callsOf('humanInteractionRequest').some(c=>c.operation===63)).toBe(true));
    const bytes=fake.callsOf('humanInteractionRequest').find(c=>c.operation===63)!.args[1] as Uint8Array;
    expect(decode(bytes)).toEqual([1n,0n,'log','log','log',601000n,exemptions]);
    expect(bytes.length).toBeLessThanOrEqual(2400);
  });

  it('reaches welcome get and put through authenticated Bot interactions and waits for correlated responses', async () => {
    const ready = { ...ROW, state: 3 as const, capabilities: 1 << 1, revision: '7' };
    const { fake, user } = setupPage('welcome', ready);
    const channelId = 'cc'.repeat(16);
    act(() => fake.set(`channels:${SERVER}`, [{ id: channelId, communityId: SERVER, kind: 0, mode: 0, name: 'general', topic: '',
      parentId: null, position: 1, slowmode: 0, announcement: false, group: 'active' }]));
    let lastInteraction: { requestId: string; operation: number } | undefined;
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(ready);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: ready.id });
      if (command.m === 'humanInteractionRequest') {
        lastInteraction = command;
        return Promise.resolve({ requestId: command.requestId, upload: 'accepted' });
      }
      if (command.m === 'readBotInteractionResponses') return Promise.resolve({ through: '1', blocked: false, responses: lastInteraction === undefined ? [] : [{
        deliveryId: '1', sourceSeq: '1', requestId: lastInteraction.requestId, communityId: SERVER, operation: lastInteraction.operation,
        channelId: null, status: 0, revision: lastInteraction.operation === 1 ? '7' : '8', data: encode([]),
      }] });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await user.click(await screen.findByRole('button', { name: en['admin.bot.welcomeSettings.load'] }));
    await waitFor(() => expect(fake.callsOf('humanInteractionRequest')).toMatchObject([
      { communityId: SERVER, channelId: null, operation: 1, args: [5], revision: null },
    ]));
    expect(await screen.findByRole('combobox', { name: en['admin.bot.welcomeSettings.channel'] })).toHaveValue(channelId);
    await user.click(screen.getByRole('button', { name: en['admin.bot.welcomeSettings.save'] }));
    await waitFor(() => expect(fake.callsOf('humanInteractionRequest')).toHaveLength(2));
    expect(fake.callsOf('humanInteractionRequest')[1]).toMatchObject({ communityId: SERVER, channelId: null,
      operation: 2, revision: '7', args: [5, expect.any(Uint8Array)] });
    expect(await screen.findByText(en['admin.bot.welcomeSettings.saved'])).toBeInTheDocument();
    expect(fake.callsOf('readBotInteractionResponses').every(call => call.communityId === SERVER && call.operations.join(',') === '1,2')).toBe(true);
    expect(fake.callsOf('ackBotInteractionResponses').every(call => call.communityId === SERVER && call.through === '1')).toBe(true);
    await waitFor(() => expect(fake.callsOf('ackBotInteractionResponses')).toHaveLength(2));
    await act(async () => { await Promise.resolve(); });
  });

  it('does not request or invent moderation details before the user asks', async () => {
    const { fake } = setupPage('moderationLog', { ...ROW, state: 3 });
    expect(await screen.findByRole('heading', { name: en['admin.bot.moderationLog.title'] })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: en['admin.bot.moderationLog.load'] })).toBeInTheDocument();
    expect(fake.calls.map(call => call.m)).not.toContain('listModerationCases');
    expect(fake.calls.map(call => call.m)).not.toContain('listManualModerationEvidence');
    expect(fake.calls.map(call => call.m)).not.toContain('readHumanInteractionResponses');
    expect(fake.calls.map(call => call.m)).not.toContain('getModerationCases');
    expect(screen.queryByText(/reason|excerpt|case #/i)).not.toBeInTheDocument();
  });

  it('keeps automatic case retrieval available alongside the manual evidence view', async () => {
    const { fake, user } = setupPage('moderationLog', { ...ROW, state: 3 });
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve({ ...ROW, state: 3 });
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: ROW.id });
      if (command.m === 'listModerationCases') return Promise.resolve({ requestId: command.requestId, status: 'accepted' });
      if (command.m === 'readHumanInteractionResponses') return Promise.resolve({ authorized: true, blocked: false, through: null, responses: [] });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await user.click(await screen.findByRole('button', { name: en['admin.bot.moderationLog.loadAutomatic'] }));
    await waitFor(() => expect(fake.callsOf('listModerationCases')).toHaveLength(1));
    expect(screen.getByRole('button', { name: en['admin.bot.moderationLog.load'] })).toBeInTheDocument();
  });

  it('renders a case once when overlapping response snapshots arrive in one journal page', async () => {
    const { fake, user } = setupPage('moderationLog', { ...ROW, state: 3 });
    let requestId = '';
    const sharedCase = { key: 'a'.repeat(32) + ':1', channelId: 'b'.repeat(32), actorId: 'c'.repeat(32),
      messageId: 'a'.repeat(32), revision: '1', reason: 'duplicate_body', action: 'warn',
      excerpt: 'one durable warning', receivedAt: '1800000000' };
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve({ ...ROW, state: 3 });
      if (command.m === 'listModerationCases') { requestId = command.requestId; return Promise.resolve({ requestId, status: 'accepted' }); }
      if (command.m === 'readHumanInteractionResponses') return Promise.resolve({ authorized: true, blocked: false, through: '3', responses: [
        { deliveryId: '2', requestId: '55'.repeat(16), status: 0, hasMore: false, nextCursor: null, cases: [sharedCase] },
        { deliveryId: '3', requestId, status: 0, hasMore: false, nextCursor: null, cases: [sharedCase,
          { ...sharedCase, key: 'd'.repeat(32) + ':1', messageId: 'd'.repeat(32), excerpt: 'a distinct warning' }] },
      ] });
      return Promise.resolve(null);
    };
    await user.click(await screen.findByRole('button', { name: en['admin.bot.moderationLog.loadAutomatic'] }));
    await waitFor(() => expect(fake.callsOf('ackHumanInteractionResponses')).toEqual([
      { m: 'ackHumanInteractionResponses', communityId: SERVER, through: '3' },
    ]));
    expect(screen.getAllByRole('article')).toHaveLength(2);
    expect(screen.getAllByText('one durable warning')).toHaveLength(1);
    expect(screen.getByText('a distinct warning')).toBeInTheDocument();
  });

  it('shows a typed response page, then acknowledges it after rendering', async () => {
    const { fake, user } = setupPage('moderationLog', { ...ROW, state: 3 });
    let requestId = '';
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve({ ...ROW, state: 3 });
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: ROW.id });
      if (command.m === 'listManualModerationEvidence') { requestId = command.requestId; return Promise.resolve({ requestId, status: 'accepted' }); }
      if (command.m === 'readHumanInteractionResponses') return Promise.resolve({ authorized: true, blocked: false, through: '3', responses: [
        { deliveryId: '3', requestId, status: 0, hasMore: true, nextCursor: '0102', cases: [{ key: 'a'.repeat(32) + ':1',
          channelId: 'b'.repeat(32), actorId: 'c'.repeat(32), messageId: 'a'.repeat(32), revision: '1',
          reason: 'duplicate', action: 'remove', excerpt: 'bounded response excerpt', receivedAt: '1800000000' }] },
      ] });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await user.click(await screen.findByRole('button', { name: en['admin.bot.moderationLog.load'] }));
    expect(await screen.findByText('bounded response excerpt')).toBeInTheDocument();
    await waitFor(() => expect(fake.callsOf('ackHumanInteractionResponses')).toEqual([
      { m: 'ackHumanInteractionResponses', communityId: SERVER, through: '3' },
    ]));
    expect(fake.callsOf('listManualModerationEvidence')).toEqual([{ m: 'listManualModerationEvidence', communityId: SERVER, requestId, cursor: null }]);
    await user.click(screen.getByRole('button', { name: en['admin.bot.moderationLog.more'] }));
    await waitFor(() => expect(fake.callsOf('listManualModerationEvidence')).toHaveLength(2));
    expect(fake.callsOf('listManualModerationEvidence')[1]).toMatchObject({ cursor: '0102' });
  });

  it('renders every response in the acknowledged prefix before acknowledging it', async () => {
    const { fake, user } = setupPage('moderationLog', { ...ROW, state: 3 });
    let requestId = '';
    const priorRequestId = '55'.repeat(16);
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve({ ...ROW, state: 3 });
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: ROW.id });
      if (command.m === 'listManualModerationEvidence') { requestId = command.requestId; return Promise.resolve({ requestId, status: 'accepted' }); }
      if (command.m === 'readHumanInteractionResponses') return Promise.resolve({ authorized: true, blocked: false, through: '3', responses: [
        { deliveryId: '2', requestId: priorRequestId, status: 0, hasMore: false, nextCursor: null, cases: [{ key: 'a'.repeat(32) + ':1',
          channelId: 'b'.repeat(32), actorId: 'c'.repeat(32), messageId: 'a'.repeat(32), revision: '1',
          reason: 'prior reason', action: 'warn', excerpt: 'prior response evidence', receivedAt: '1800000000' }] },
        { deliveryId: '3', requestId, status: 0, hasMore: false, nextCursor: null, cases: [{ key: 'd'.repeat(32) + ':1',
          channelId: 'b'.repeat(32), actorId: 'c'.repeat(32), messageId: 'd'.repeat(32), revision: '1',
          reason: 'current reason', action: 'remove', excerpt: 'current response evidence', receivedAt: '1800000000' }] },
      ] });
      if (command.m === 'ackHumanInteractionResponses') {
        expect(screen.getByText('prior response evidence')).toBeInTheDocument();
        expect(screen.getByText('current response evidence')).toBeInTheDocument();
        return Promise.resolve(null);
      }
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await user.click(await screen.findByRole('button', { name: en['admin.bot.moderationLog.load'] }));
    await waitFor(() => expect(fake.callsOf('ackHumanInteractionResponses')).toHaveLength(1));
  });

  it('renders an older response before acknowledging it while the current request is pending', async () => {
    const { fake, user } = setupPage('moderationLog', { ...ROW, state: 3 });
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve({ ...ROW, state: 3 });
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: ROW.id });
      if (command.m === 'listManualModerationEvidence') return Promise.resolve({ requestId: command.requestId, status: 'accepted' });
      if (command.m === 'readHumanInteractionResponses') return Promise.resolve({ authorized: true, blocked: false, through: '2', responses: [
        { deliveryId: '2', requestId: '55'.repeat(16), status: 0, hasMore: false, nextCursor: null, cases: [{ key: 'a'.repeat(32) + ':1',
          channelId: 'b'.repeat(32), actorId: 'c'.repeat(32), messageId: 'a'.repeat(32), revision: '1',
          reason: 'earlier reason', action: 'warn', excerpt: 'earlier evidence', receivedAt: '1800000000' }] },
      ] });
      if (command.m === 'ackHumanInteractionResponses') {
        expect(screen.getByText('earlier evidence')).toBeInTheDocument();
        return Promise.resolve(null);
      }
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await user.click(await screen.findByRole('button', { name: en['admin.bot.moderationLog.load'] }));
    await waitFor(() => expect(fake.callsOf('ackHumanInteractionResponses')).toHaveLength(1));
    expect(screen.getByText('earlier evidence')).toBeInTheDocument();
  });

  it('renders a safe date fallback for an out-of-range received timestamp', async () => {
    const { fake, user } = setupPage('moderationLog', { ...ROW, state: 3 });
    let requestId = '';
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve({ ...ROW, state: 3 });
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: ROW.id });
      if (command.m === 'listManualModerationEvidence') { requestId = command.requestId; return Promise.resolve({ requestId, status: 'accepted' }); }
      if (command.m === 'readHumanInteractionResponses') return Promise.resolve({ authorized: true, blocked: false, through: null, responses: [
        { deliveryId: '3', requestId, status: 0, hasMore: false, nextCursor: null, cases: [{ key: 'a'.repeat(32) + ':1',
          channelId: 'b'.repeat(32), actorId: 'c'.repeat(32), messageId: 'a'.repeat(32), revision: '1',
          reason: 'duplicate', action: 'remove', excerpt: 'safe date fallback case', receivedAt: '8640000000000001' }] },
      ] });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await user.click(await screen.findByRole('button', { name: en['admin.bot.moderationLog.load'] }));
    expect(await screen.findByText('safe date fallback case')).toBeInTheDocument();
    expect(screen.getByText(/Date unavailable/)).toBeInTheDocument();
  });

  it('manually refreshes installation status and capabilities from the server', async () => {
    const initial = { ...ROW, state: 2 as const, capabilities: 1 };
    const refreshed = { ...ROW, state: 3 as const, capabilities: 3 };
    const { fake, user } = setup(initial);
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(refreshed);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: initial.id });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };

    await screen.findByText(en['admin.bot.status.joining']);
    await glennMenu(user, en['serverSettings.bot.menu.refresh']);

    expect(await screen.findByText(en['admin.bot.status.ready'])).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: en['serverSettings.bot.cap.music'] })).toBeChecked();
    expect(screen.getByRole('switch', { name: en['serverSettings.bot.cap.welcome'] })).toBeChecked();
    expect(fake.callsOf('getBotInstallation')).toHaveLength(2);
  });

  it('leaves capability edit mode when a refresh finds a disabled installation', async () => {
    const initial = { ...ROW, state: 3 as const, capabilities: 1 };
    const refreshed = { ...ROW, state: 4 as const, capabilities: 3 };
    const { fake, user } = setup(initial);
    await screen.findByText(en['admin.bot.status.ready']);
    await user.click(screen.getByRole('switch', { name: en['serverSettings.bot.cap.announcements'] }));
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(refreshed);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: initial.id });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await glennMenu(user, en['serverSettings.bot.menu.refresh']);
    expect(await screen.findByText(en['admin.bot.status.disabled'])).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: en['serverSettings.bot.cap.welcome'] })).toBeChecked();
  });

  it('requires step-up, stores only attempt context, and opens the Spotify authorization URL', async () => {
    spotifySession.clear();
    vi.mocked(authorizeSpotify).mockClear();
    const { fake, user } = setupPage('music', { ...ROW, state: 3, capabilities: 1 });
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve({ ...ROW, state: 3, capabilities: 1 });
      if (command.m === 'stepUp') return Promise.resolve({ token: 'short-lived-step-up' });
      if (command.m === 'beginBotSpotifyAttempt') return Promise.resolve({
        attemptId: 'dd'.repeat(16), authorizeUrl: 'https://accounts.spotify.com/authorize?state=csrf-only', expires: `${Math.floor(Date.now() / 1000) + 600}`,
      });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await waitFor(() => expect(screen.getByRole('button', { name: en['admin.bot.music.spotifyLink'] })).not.toHaveAttribute('aria-disabled', 'true'));
    await user.type(screen.getByRole('textbox', { name: en['admin.bot.music.spotifyClientId'] }), 'client');
    await user.click(screen.getByRole('button', { name: en['admin.bot.music.spotifyLink'] }));
    expect(screen.getByRole('dialog', { name: en['factorSettings.stepUp.title'] })).toBeInTheDocument();
    await user.type(screen.getByLabelText(en['factorSettings.stepUp.password']), 'local-password');
    await user.click(screen.getByRole('button', { name: en['factorSettings.stepUp.confirm'] }));
    await waitFor(() => expect(authorizeSpotify).toHaveBeenCalledWith('https://accounts.spotify.com/authorize?state=csrf-only'));
    const stored = spotifySession.getItem('dilla.spotifyAttempt');
    expect(stored).toContain(SERVER);
    expect(stored).toContain('dd'.repeat(16));
    expect(stored).toContain('csrf-only');
    expect(stored).not.toContain('oauth-code');
    expect(fake.callsOf('beginBotSpotifyAttempt')).toEqual([expect.objectContaining({ communityId: SERVER, stepUp: 'short-lived-step-up', clientId: 'client' })]);
    expect(screen.getByRole('group', { name: en['serverSettings.music.youtube'] })).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: en['admin.bot.music.spotifyClientId'] })).toBeInTheDocument();
    expect(fake.calls.map(call => call.m)).toEqual(['getBotInstallation', 'refreshFactors', 'getBotSpotifyConfig',
      'getBotCommandStatus', 'stepUp', 'beginBotSpotifyAttempt']);
  });

  it('saves one approved join-role setting with a request ID and explains future-join assignment', async () => {
    const initial = { ...ROW, state: 3 as const, revision: '3' };
    const { fake, user, announce } = setupPage('joinRoles', initial);
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(initial);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: initial.id });
      if (command.m === 'configureBotJoinRole') return Promise.resolve({ installationId: initial.id });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    expect(await screen.findByText(en['admin.bot.joinRole.honest'])).toBeInTheDocument();
    const select = await screen.findByRole('combobox', { name: en['admin.bot.joinRole.label'] });
    await user.selectOptions(select, '22'.repeat(16));
    await user.click(screen.getByRole('button', { name: en['admin.bot.joinRole.save'] }));
    await waitFor(() => expect(fake.callsOf('configureBotJoinRole')).toHaveLength(1));
    expect(fake.callsOf('configureBotJoinRole')[0]).toMatchObject({ communityId: SERVER, roleId: '22'.repeat(16), installationId: initial.id });
    expect(fake.callsOf('configureBotJoinRole')[0].requestId).toMatch(/^[0-9a-f]{32}$/);
    expect(announce).toHaveBeenCalledWith(en['admin.bot.joinRole.saved']);
  });



  it('does not read join-role configuration on the bot page', async () => {
    const initial = { ...ROW, state: 3 as const, revision: '3' };
    const { fake } = setup(initial, false, false, false);
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(initial);
      if (command.m === 'getBotJoinRole') return Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 }));
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await screen.findByText(en['admin.bot.status.ready']);
    expect(fake.callsOf('getBotJoinRole')).toHaveLength(0);
  });

  it('keeps the join-role form available after a rejected save', async () => {
    const initial = { ...ROW, state: 3 as const, revision: '3' };
    const { fake, user } = setupPage('joinRoles', initial);
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(initial);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: initial.id });
      if (command.m === 'configureBotJoinRole') return Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 }));
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    const select = await screen.findByRole('combobox', { name: en['admin.bot.joinRole.label'] });
    await user.selectOptions(select, '22'.repeat(16));
    await user.click(screen.getByRole('button', { name: en['admin.bot.joinRole.save'] }));
    expect(await screen.findByRole('alert')).toHaveTextContent(en['admin.bot.error.forbidden']);
  });

  it('shows staged status and asks for step-up before enabling through the worker', async () => {
    const { fake, user, announce } = setup();
    expect(await screen.findByText(en['admin.bot.none'])).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: en['serverSettings.bot.enable'] }));
    expect(screen.getByRole('dialog', { name: en['factorSettings.stepUp.title'] })).toBeInTheDocument();
    await user.type(screen.getByLabelText(en['factorSettings.stepUp.password']), 'local-password');
    await user.click(screen.getByRole('button', { name: en['factorSettings.stepUp.confirm'] }));
    expect(await screen.findByText(en['admin.bot.status.pending'])).toBeInTheDocument();
    expect(fake.callsOf('configureBotInstallation')).toEqual([expect.objectContaining({ m: 'configureBotInstallation',
      communityId: SERVER, expectedRevision: null, capabilities: 256 | 512, stepUp: 'short-lived-step-up' })]);
    expect(announce).toHaveBeenCalledWith(en['admin.bot.enabledNotice']);
  });

  it('submits explicitly selected capabilities and supports toggling them by keyboard', async () => {
    const { fake, user } = setup();
    await screen.findByText(en['admin.bot.none']);
    expect(screen.getByRole('switch', { name: en['serverSettings.bot.cap.music'] })).not.toBeChecked();
    const music = screen.getByRole('switch', { name: en['serverSettings.bot.cap.music'] });
    music.focus();
    await user.keyboard(' ');
    expect(music).toBeChecked();
    const announcements = screen.getByRole('switch', { name: en['serverSettings.bot.cap.announcements'] });
    await user.click(announcements);
    await user.click(screen.getByRole('button', { name: en['serverSettings.bot.enable'] }));
    await user.type(screen.getByLabelText(en['factorSettings.stepUp.password']), 'local-password');
    await user.click(screen.getByRole('button', { name: en['factorSettings.stepUp.confirm'] }));
    await waitFor(() => expect(fake.callsOf('configureBotInstallation')).toHaveLength(1));
    // A first enable starts with Polls and Reminders on (bits 8 and 9, Task 10c fix round 1, I1).
    expect(fake.callsOf('configureBotInstallation')[0]).toMatchObject({ capabilities: 1 | 8 | 256 | 512 });
  });

  it('shows the permission ceiling and disables capabilities the current access summary cannot grant', async () => {
    const { user } = setup(null, false, false, false);
    await screen.findByText(en['admin.bot.none']);
    // Task 10c: a capability the access summary cannot grant says what it needs in its row and cannot be switched on.
    expect(screen.getByRole('switch', { name: en['serverSettings.bot.cap.roles'] })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('group', { name: en['serverSettings.bot.cap.roles'] })).toHaveAccessibleDescription(en['admin.bot.capability.requires.roles']);
    await user.keyboard('{Tab}');
  });

  it('sends the currently read revision and explains when another admin changed the installation', async () => {
    const initial = { ...ROW, state: 4 as const, revision: '11', capabilities: 0 };
    const { fake, user } = setup(initial);
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(initial);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: initial.id });
      if (command.m === 'stepUp') return Promise.resolve({ token: 'short-lived-step-up' });
      if (command.m === 'configureBotInstallation') return Promise.reject(refusal({ code: 'E_CONFLICT', status: 409 }));
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await screen.findByText(en['admin.bot.status.disabled']);
    await user.click(screen.getByRole('switch', { name: en['serverSettings.bot.cap.music'] }));
    await user.click(screen.getByRole('button', { name: en['serverSettings.bot.enable'] }));
    await user.type(screen.getByLabelText(en['factorSettings.stepUp.password']), 'local-password');
    await user.click(screen.getByRole('button', { name: en['factorSettings.stepUp.confirm'] }));
    expect(await screen.findByRole('alert')).toHaveTextContent(en['admin.bot.error.conflict']);
    expect(fake.callsOf('configureBotInstallation')[0]).toMatchObject({ expectedRevision: '11', capabilities: 1 });
  });

  it('preserves approved capabilities when re-enabling a disabled installation', async () => {
    const initial = { ...ROW, state: 4 as const, revision: '21', capabilities: 1 | 8 };
    const { fake, user } = setup(initial);
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(initial);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: initial.id });
      if (command.m === 'stepUp') return Promise.resolve({ token: 'short-lived-step-up' });
      if (command.m === 'configureBotInstallation') return Promise.resolve({ ...initial, state: 0, revision: '22' });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await screen.findByText(en['admin.bot.status.disabled']);
    expect(screen.getByRole('switch', { name: en['serverSettings.bot.cap.music'] })).toBeChecked();
    expect(screen.getByRole('switch', { name: en['serverSettings.bot.cap.announcements'] })).toBeChecked();
    await user.click(screen.getByRole('button', { name: en['serverSettings.bot.enable'] }));
    await user.type(screen.getByLabelText(en['factorSettings.stepUp.password']), 'local-password');
    await user.click(screen.getByRole('button', { name: en['factorSettings.stepUp.confirm'] }));
    await waitFor(() => expect(fake.callsOf('configureBotInstallation')).toHaveLength(1));
    expect(fake.callsOf('configureBotInstallation')[0]).toMatchObject({ expectedRevision: '21', capabilities: 1 | 8 });
  });

  it('allows editing capabilities on a ready installation using its current revision', async () => {
    const initial = { ...ROW, state: 3 as const, revision: '30', capabilities: 1 | 8, userId: '77'.repeat(16) };
    const { fake, user } = setup(initial);
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(initial);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: initial.id });
      if (command.m === 'stepUp') return Promise.resolve({ token: 'short-lived-step-up' });
      if (command.m === 'configureBotInstallation') return Promise.resolve({ ...initial, state: 2, revision: '31', capabilities: 1 | 8 | 16 });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await screen.findByText(en['admin.bot.status.ready']);
    expect(screen.getByRole('switch', { name: en['serverSettings.bot.cap.music'] })).toBeChecked();
    expect(screen.getByRole('switch', { name: en['serverSettings.bot.cap.announcements'] })).toBeChecked();
    await user.click(screen.getByRole('switch', { name: en['serverSettings.bot.cap.warnings'] }));
    await user.click(screen.getByRole('button', { name: en['admin.bot.capabilities.save'] }));
    await user.type(screen.getByLabelText(en['factorSettings.stepUp.password']), 'local-password');
    await user.click(screen.getByRole('button', { name: en['factorSettings.stepUp.confirm'] }));
    await waitFor(() => expect(fake.callsOf('configureBotInstallation')).toHaveLength(1));
    expect(fake.callsOf('configureBotInstallation')[0]).toMatchObject({ expectedRevision: '30', capabilities: 1 | 8 | 16 });
    expect(await screen.findByText(en['admin.bot.status.joining'])).toBeInTheDocument();
  });

  it('does not save an edit against a stale ready installation revision', async () => {
    const initial = { ...ROW, state: 3 as const, revision: '40', capabilities: 1, userId: '77'.repeat(16) };
    const { fake, user } = setup(initial);
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(initial);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: initial.id });
      if (command.m === 'stepUp') return Promise.resolve({ token: 'short-lived-step-up' });
      if (command.m === 'configureBotInstallation') return Promise.reject(refusal({ code: 'E_CONFLICT', status: 409 }));
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await screen.findByText(en['admin.bot.status.ready']);
    await user.click(screen.getByRole('switch', { name: en['serverSettings.bot.cap.welcome'] }));
    await user.click(screen.getByRole('button', { name: en['admin.bot.capabilities.save'] }));
    await user.type(screen.getByLabelText(en['factorSettings.stepUp.password']), 'local-password');
    await user.click(screen.getByRole('button', { name: en['factorSettings.stepUp.confirm'] }));
    expect(await screen.findByRole('alert')).toHaveTextContent(en['admin.bot.error.conflict']);
    expect(fake.callsOf('configureBotInstallation')[0]).toMatchObject({ expectedRevision: '40', capabilities: 3 });
  });

  it('lets an owner remove an approved capability after losing its permission, but cannot add it again', async () => {
    const initial = { ...ROW, state: 3 as const, revision: '50', capabilities: 4, userId: '77'.repeat(16) };
    const { fake, user } = setup(initial, false, false, false);
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(initial);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: initial.id });
      if (command.m === 'stepUp') return Promise.resolve({ token: 'short-lived-step-up' });
      if (command.m === 'configureBotInstallation') return Promise.resolve({ ...initial, revision: '51', capabilities: 0, state: 2 });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await screen.findByText(en['admin.bot.status.ready']);
    const roles = screen.getByRole('switch', { name: en['serverSettings.bot.cap.roles'] });
    expect(roles).toBeChecked();
    expect(roles).not.toHaveAttribute('aria-disabled');
    await user.click(roles);
    expect(roles).not.toBeChecked();
    expect(roles).toHaveAttribute('aria-disabled', 'true');
    await user.click(screen.getByRole('button', { name: en['admin.bot.capabilities.save'] }));
    await user.type(screen.getByLabelText(en['factorSettings.stepUp.password']), 'local-password');
    await user.click(screen.getByRole('button', { name: en['factorSettings.stepUp.confirm'] }));
    await waitFor(() => expect(fake.callsOf('configureBotInstallation')).toHaveLength(1));
    expect(fake.callsOf('configureBotInstallation')[0]).toMatchObject({ expectedRevision: '50', capabilities: 0 });
  });

  it('allows editing a pending installation with its current revision', async () => {
    const initial = { ...ROW, state: 0 as const, revision: '60', capabilities: 4 };
    const { fake, user } = setup(initial);
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return Promise.resolve(initial);
      if (command.m === 'getBotJoinRole') return Promise.resolve({ roleId: null, installationId: initial.id });
      if (command.m === 'stepUp') return Promise.resolve({ token: 'short-lived-step-up' });
      if (command.m === 'configureBotInstallation') return Promise.resolve({ ...initial, revision: '61', capabilities: 0 });
      return Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 300 } : null);
    };
    await screen.findByText(en['admin.bot.status.pending']);
    const roles = screen.getByRole('switch', { name: en['serverSettings.bot.cap.roles'] });
    expect(roles).toBeChecked();
    await user.click(roles);
    await user.click(screen.getByRole('button', { name: en['admin.bot.capabilities.save'] }));
    await user.type(screen.getByLabelText(en['factorSettings.stepUp.password']), 'local-password');
    await user.click(screen.getByRole('button', { name: en['factorSettings.stepUp.confirm'] }));
    await waitFor(() => expect(fake.callsOf('configureBotInstallation')).toHaveLength(1));
    expect(fake.callsOf('configureBotInstallation')[0]).toMatchObject({ expectedRevision: '60', capabilities: 0 });
    expect(await screen.findByRole('status')).toHaveTextContent(en['admin.bot.status.pending']);
  });

  it.each([
    [0, 'admin.bot.status.pending'], [1, 'admin.bot.status.pending'], [2, 'admin.bot.status.joining'],
    [3, 'admin.bot.status.ready'], [4, 'admin.bot.status.disabled'], [5, 'admin.bot.status.failed'],
  ] as const)('labels installation state %i plainly', async (state, key) => {
    const { user } = setup({ ...ROW, state });
    expect(await screen.findByRole('status')).toHaveTextContent(en[key]);
    await user.keyboard('{Tab}');
    expect(document.activeElement).toBeInstanceOf(HTMLElement);
  });

  it('refreshes pending and joining status until Ready, then stops polling', async () => {
    vi.useFakeTimers();
    try {
      const initial = { ...ROW, state: 0 as const };
      const { fake } = setup(initial);
      await act(async () => { await Promise.resolve(); await Promise.resolve(); });
      expect(screen.getByRole('status')).toHaveTextContent(en['admin.bot.status.pending']);
      let polls = 0;
      fake.handler = command => {
        if (command.m === 'getBotInstallation') {
          polls += 1;
          return Promise.resolve(polls === 1 ? { ...initial, state: 2 } : { ...initial, state: 3, revision: '3', userId: '77'.repeat(16) });
        }
        return Promise.resolve(null);
      };
      await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
      expect(screen.getByRole('status')).toHaveTextContent(en['admin.bot.status.joining']);
      await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
      expect(screen.getByRole('status')).toHaveTextContent(en['admin.bot.status.ready']);
      await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
      expect(fake.callsOf('getBotInstallation')).toHaveLength(3);
    } finally {
      vi.useRealTimers();
    }
  });

  it('starts polling when a newly enabled bot moves from absent to Pending', async () => {
    vi.useFakeTimers();
    try {
      const { fake } = setupPage('bot', null, false, false, true, true);
      await act(async () => { await Promise.resolve(); await Promise.resolve(); });
      expect(screen.getByText(en['admin.bot.none'])).toBeInTheDocument();
      let polls = 0;
      fake.handler = command => {
        if (command.m === 'getBotInstallation') {
          polls += 1;
          return Promise.resolve({ ...ROW, state: 3, revision: `${polls + 1}`, userId: '77'.repeat(16) });
        }
        if (command.m === 'stepUp') return Promise.resolve({ token: 'short-lived-step-up' });
        if (command.m === 'configureBotInstallation') return Promise.resolve({ ...ROW, state: 0, revision: '1' });
        return Promise.resolve(null);
      };
      act(() => { fireEvent.click(screen.getByRole('button', { name: en['serverSettings.bot.enable'] })); });
      await act(async () => {
        fireEvent.change(screen.getByLabelText(en['factorSettings.stepUp.password']), { target: { value: 'local-password' } });
        fireEvent.click(screen.getByRole('button', { name: en['factorSettings.stepUp.confirm'] }));
        await Promise.resolve(); await Promise.resolve();
      });
      expect(screen.getByText(en['admin.bot.status.pending'])).toBeInTheDocument();
      await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
      expect(screen.getByText(en['admin.bot.status.ready'])).toBeInTheDocument();
      expect(fake.callsOf('getBotInstallation')).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('cancels a pending community poll after switching settings to another community', async () => {
    vi.useFakeTimers();
    try {
      const communityB = 'c3'.repeat(16);
      const rowA = { ...ROW, state: 0 as const };
      const rowB = { ...ROW, communityId: communityB, state: 3 as const, revision: '9', userId: '99'.repeat(16) };
      const fake = new FakeClient();
      fake.set('account', account());
      fake.set('factors', { owed: false, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true });
      fake.set('communities', [{ id: SERVER, name: 'A', owner: ME.id }, { id: communityB, name: 'B', owner: ME.id }]);
      fake.set(`permissions:${SERVER}`, ACCESS);
      fake.set(`permissions:${communityB}`, ACCESS);
      fake.handler = command => {
        if (command.m === 'getBotInstallation') return Promise.resolve(command.communityId === SERVER ? rowA : rowB);
        return Promise.resolve(null);
      };
      const announce = vi.fn();
      const view = render(<div className="d-root"><CoreProvider client={fake}>
        <ServerSettings communityId={SERVER} tab="bot" onTab={vi.fn()} onClose={vi.fn()} announce={announce} />
      </CoreProvider></div>);
      await act(async () => { await Promise.resolve(); await Promise.resolve(); });
      expect(screen.getByText(en['admin.bot.status.pending'])).toBeInTheDocument();
      view.rerender(<div className="d-root"><CoreProvider client={fake}>
        <ServerSettings communityId={communityB} tab="bot" onTab={vi.fn()} onClose={vi.fn()} announce={announce} />
      </CoreProvider></div>);
      await act(async () => { await Promise.resolve(); await Promise.resolve(); });
      expect(screen.getByText(en['admin.bot.status.ready'])).toBeInTheDocument();
      await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
      expect(fake.callsOf('getBotInstallation')).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('disables the bot using the current revision and announces the returned state', async () => {
    const { fake, user, announce } = setup({ ...ROW, state: 3, revision: '8', userId: '77'.repeat(16) });
    expect(await screen.findByText(en['admin.bot.status.ready'])).toBeInTheDocument();
    await glennMenu(user, en['serverSettings.bot.menu.disable'].replace('{bot}', en['serverSettings.bot.fallbackName']));
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent(en['admin.bot.status.disabled']));
    expect(fake.callsOf('disableBotInstallation')).toEqual([expect.objectContaining({ m: 'disableBotInstallation',
      communityId: SERVER, expectedRevision: '8' })]);
    expect(announce).toHaveBeenCalledWith(en['admin.bot.disabledNotice']);
  });

  it('shows an honest error after a forbidden read', async () => {
    const { fake, user } = setup({ ...ROW, state: 3 });
    fake.handler = command => command.m === 'disableBotInstallation'
      ? Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 })) : Promise.resolve(null);
    await screen.findByText(en['admin.bot.status.ready']);
    await glennMenu(user, en['serverSettings.bot.menu.disable'].replace('{bot}', en['serverSettings.bot.fallbackName']));
    expect(await screen.findByRole('alert')).toHaveTextContent(en['admin.bot.error.forbidden']);
    expect(screen.getByRole('status')).toHaveTextContent(en['admin.bot.status.ready']);
  });

  it('keeps the enable control reachable by keyboard', async () => {
    const { user } = setup();
    await screen.findByText(en['admin.bot.none']);
    const button = screen.getByRole('button', { name: en['serverSettings.bot.enable'] });
    button.focus();
    await user.keyboard('{Enter}');
    expect(screen.getByRole('dialog', { name: en['factorSettings.stepUp.title'] })).toBeInTheDocument();
  });

  it('does not offer a new install when the current installation could not be read', async () => {
    const { fake } = setup(null, true);
    expect(await screen.findByRole('alert')).toHaveTextContent(en['admin.bot.error.other'].replace('{code}', 'E_NETWORK'));
    expect(screen.getByRole('button', { name: en['serverSettings.bot.enable'] })).toHaveAttribute('aria-disabled', 'true');
    expect(fake.callsOf('configureBotInstallation')).toHaveLength(0);
  });

  it('keeps the bot install control in the phone settings frame', async () => {
    setup(null, false, true);
    await screen.findByText(en['admin.bot.none']);
    expect(document.querySelector('.d-settings-frame')).toHaveAttribute('data-layout', 'phone');
    expect(screen.getByRole('button', { name: en['serverSettings.bot.enable'] })).toBeVisible();
  });

  it('ignores a late A read after an in-place switch to B and only disables B after B loads', async () => {
    let resolveA!: (value: Installation) => void;
    let resolveB!: (value: Installation) => void;
    const readA = new Promise<Installation>(resolve => { resolveA = resolve; });
    const readB = new Promise<Installation>(resolve => { resolveB = resolve; });
    const communityB = 'c3'.repeat(16);
    const rowA: Installation = { ...ROW, communityId: SERVER, state: 3, revision: '4', userId: '44'.repeat(16) };
    const rowB: Installation = { ...ROW, communityId: communityB, state: 3, revision: '9', userId: '99'.repeat(16) };
    const fake = new FakeClient();
    fake.set('account', account());
    fake.set('factors', { owed: false, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true });
    fake.set('communities', [{ id: SERVER, name: 'A', owner: ME.id }, { id: communityB, name: 'B', owner: ME.id }]);
    fake.set(`permissions:${SERVER}`, ACCESS);
    fake.set(`permissions:${communityB}`, ACCESS);
    fake.handler = command => {
      if (command.m === 'getBotInstallation') return command.communityId === SERVER ? readA : readB;
      if (command.m === 'disableBotInstallation') return Promise.resolve({ ...rowB, state: 4, revision: '10' });
      return Promise.resolve(null);
    };
    const announce = vi.fn();
    const user = userEvent.setup();
    const view = render(<div className="d-root"><CoreProvider client={fake}>
      <ServerSettings communityId={SERVER} tab="bot" onTab={vi.fn()} onClose={vi.fn()} announce={announce} />
    </CoreProvider></div>);
    await waitFor(() => expect(fake.callsOf('getBotInstallation')).toHaveLength(1));
    view.rerender(<div className="d-root"><CoreProvider client={fake}>
      <ServerSettings communityId={communityB} tab="bot" onTab={vi.fn()} onClose={vi.fn()} announce={announce} />
    </CoreProvider></div>);
    await waitFor(() => expect(fake.callsOf('getBotInstallation')).toHaveLength(2));
    await act(async () => { resolveA(rowA); await readA; });
    // Task 10c: Disable sits in Glenn's row menu; a stale A row offers none before B's read arrives.
    expect(document.querySelector('[role="group"][aria-keyshortcuts="Shift+F10"]')).toBeNull();
    expect(fake.callsOf('disableBotInstallation')).toHaveLength(0);
    expect(screen.queryByText(en['admin.bot.status.ready'])).toBeNull();

    await act(async () => { resolveB(rowB); await readB; });
    expect(await screen.findByText(en['admin.bot.status.ready'])).toBeInTheDocument();
    await glennMenu(user, en['serverSettings.bot.menu.disable'].replace('{bot}', en['serverSettings.bot.fallbackName']));
    await waitFor(() => expect(fake.callsOf('disableBotInstallation')).toHaveLength(1));
    expect(fake.callsOf('disableBotInstallation')[0]).toMatchObject({ communityId: communityB, expectedRevision: '9' });
    expect(screen.getByText(en['admin.bot.status.disabled'])).toBeInTheDocument();
  });

  // Task 10c: Glenn's card, Restart, the ten capability rows and the Music page's defaults.
  describe('Signal task 10c', () => {
    const GLENN = '77'.repeat(16);
    const ready = { ...ROW, state: 3 as const, revision: '5', userId: GLENN, capabilities: 1 | 1 << 8 | 1 << 9,
      restartRequested: '0', restartHandled: '0', version: '0.4.2', musicVolume: 100, musicIdleMinutes: 30 };
    const glenn = (fake: FakeClient) => act(() => fake.publish(`members:${SERVER}`, [{ userId: GLENN, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] }]));

    it('shows Glenn as a managed bot with presence and version, and ten capability rows', async () => {
      const { fake } = setup(ready);
      glenn(fake);
      expect(await screen.findByText('Managed bot · online · v0.4.2')).toBeInTheDocument();
      expect(screen.getAllByRole('switch').map(sw => sw.getAttribute('aria-label'))).toEqual(['Music', 'Polls', 'Reminders', 'Welcome messages', 'Roles',
        'Announcements', 'Warnings and timeouts', 'Message removal', 'Kick', 'Ban']);
      expect(screen.getByRole('switch', { name: en['serverSettings.bot.cap.polls'] })).toBeChecked();
      expect(screen.getByRole('switch', { name: en['serverSettings.bot.cap.reminders'] })).toBeChecked();
    });
    it('omits the version for a runtime that reports none', async () => {
      const { fake } = setup({ ...ready, version: null });
      glenn(fake);
      expect(await screen.findByText('Managed bot · online')).toBeInTheDocument();
    });
    it('switching Polls off saves bit 8 off through the step-up configure', async () => {
      const { fake, user } = setup(ready);
      fake.handler = command => {
        if (command.m === 'getBotInstallation') return Promise.resolve(ready);
        if (command.m === 'stepUp') return Promise.resolve({ token: 'short-lived-step-up' });
        if (command.m === 'configureBotInstallation') return Promise.resolve({ ...ready, revision: '6', capabilities: 1 | 1 << 9 });
        return Promise.resolve(null);
      };
      await user.click(await screen.findByRole('switch', { name: en['serverSettings.bot.cap.polls'] }));
      await user.click(screen.getByRole('button', { name: en['admin.bot.capabilities.save'] }));
      await user.type(screen.getByLabelText(en['factorSettings.stepUp.password']), 'local-password');
      await user.click(screen.getByRole('button', { name: en['factorSettings.stepUp.confirm'] }));
      await waitFor(() => expect(fake.callsOf('configureBotInstallation')[0]).toMatchObject({ expectedRevision: '5', capabilities: 1 | 1 << 9 }));
    });
    it('Restart asks once, says Restarting… until the runtime reports it, then announces', async () => {
      vi.useFakeTimers();
      try {
        let handled = '0';
        const { fake, announce } = setup(ready, false, false, true, true, command => {
          if (command.m === 'getBotInstallation') return Promise.resolve({ ...ready, restartRequested: '1', restartHandled: handled });
          if (command.m === 'restartBot') return Promise.resolve({ requested: '1', handled: '0' });
          return Promise.resolve(null);
        });
        glenn(fake);
        await act(async () => { await Promise.resolve(); await Promise.resolve(); });
        act(() => { fireEvent.click(screen.getByRole('button', { name: en['serverSettings.bot.restart'] })); });
        await act(async () => { await Promise.resolve(); await Promise.resolve(); });
        expect(fake.callsOf('restartBot')).toEqual([{ m: 'restartBot', communityId: SERVER, installationId: ready.id }]);
        const button = screen.getByRole('button', { name: en['serverSettings.bot.restarting'] });
        expect(button).toHaveAttribute('aria-disabled', 'true');
        act(() => { fireEvent.click(button); });
        expect(fake.callsOf('restartBot')).toHaveLength(1);
        handled = '1';
        await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
        expect(announce).toHaveBeenCalledWith(en['serverSettings.bot.restarted'].replace('{bot}', 'Glenn'));
        expect(screen.getByRole('button', { name: en['serverSettings.bot.restart'] })).toBeInTheDocument();
      } finally {
        vi.useRealTimers();
      }
    });
    it('Restart comes back with a note when the runtime has not picked it up by the end of polling', async () => {
      vi.useFakeTimers();
      try {
        const { fake } = setup(ready, false, false, true, true, command => {
          if (command.m === 'getBotInstallation') return Promise.resolve({ ...ready, restartRequested: '1', restartHandled: '0' });
          if (command.m === 'restartBot') return Promise.resolve({ requested: '1', handled: '0' });
          return Promise.resolve(null);
        });
        glenn(fake);
        await act(async () => { await Promise.resolve(); await Promise.resolve(); });
        act(() => { fireEvent.click(screen.getByRole('button', { name: en['serverSettings.bot.restart'] })); });
        await act(async () => { await Promise.resolve(); await Promise.resolve(); });
        expect(screen.getByRole('button', { name: en['serverSettings.bot.restarting'] })).toHaveAttribute('aria-disabled', 'true');
        await act(async () => { await vi.advanceTimersByTimeAsync(2_000 * 46); });
        const again = screen.getByRole('button', { name: en['serverSettings.bot.restart'] });
        expect(again).not.toHaveAttribute('aria-disabled');
        expect(screen.getByText(en['serverSettings.bot.restartPending'].replace('{bot}', 'Glenn'))).toBeInTheDocument();
        act(() => { fireEvent.click(again); });
        await act(async () => { await Promise.resolve(); await Promise.resolve(); });
        expect(fake.callsOf('restartBot')).toHaveLength(2);
      } finally {
        vi.useRealTimers();
      }
    });
    it('a refused Restart says why', async () => {
      const { fake, user } = setup(ready);
      fake.handler = command => command.m === 'restartBot' ? Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 }))
        : Promise.resolve(command.m === 'getBotInstallation' ? ready : null);
      await user.click(await screen.findByRole('button', { name: en['serverSettings.bot.restart'] }));
      expect(await screen.findByRole('alert')).toHaveTextContent(en['admin.bot.error.forbidden']);
    });
    it('the Music page lists the roles with Control music and saves the defaults as they are picked', async () => {
      const { fake, user, announce } = setupPage('music', ready);
      act(() => fake.set(`roles:${SERVER}`, [
        { id: '22'.repeat(16), name: 'Members', position: 1, members: 0, allow: 0, deny: 0, color: 0, hoist: false, mentionable: false },
        { id: '33'.repeat(16), name: 'DJ', position: 2, members: 2, allow: 1 << 25, deny: 0, color: 0, hoist: false, mentionable: false },
      ]));
      fake.handler = command => {
        if (command.m === 'getBotInstallation') return Promise.resolve(ready);
        if (command.m === 'setBotMusicSettings') return Promise.resolve({ ...ready, musicVolume: command.volume, musicIdleMinutes: command.idleMinutes });
        return Promise.resolve(null);
      };
      expect(await screen.findByText(en['serverSettings.music.djRoles'].replace('{roles}', 'DJ'))).toBeInTheDocument();
      await user.click(within(screen.getByRole('radiogroup', { name: en['serverSettings.music.volume'] })).getByRole('radio', { name: '50' }));
      await waitFor(() => expect(fake.callsOf('setBotMusicSettings')).toEqual([{ m: 'setBotMusicSettings', communityId: SERVER,
        installationId: ready.id, volume: 50, idleMinutes: 30 }]));
      await user.click(within(screen.getByRole('radiogroup', { name: en['serverSettings.music.idle'] })).getByRole('radio', { name: '15m' }));
      await waitFor(() => expect(fake.callsOf('setBotMusicSettings').at(-1)).toMatchObject({ volume: 50, idleMinutes: 15 }));
      expect(announce).toHaveBeenCalledWith(en['serverSettings.music.saved']);
      expect(screen.getByText(en['serverSettings.music.idleHint'].replace('{bot}', 'Glenn'))).toBeInTheDocument();
    });
    it('the Music page says no role has Control music yet, and a member without Manage server cannot change the defaults', async () => {
      const { fake, user } = setupPage('music', ready);
      act(() => fake.set(`permissions:${SERVER}`, { ...ACCESS, owner: false, manageCommunity: false }));
      expect(await screen.findByText(en['serverSettings.music.djNone'])).toBeInTheDocument();
      const fifty = within(screen.getByRole('radiogroup', { name: en['serverSettings.music.volume'] })).getByRole('radio', { name: '50' });
      expect(fifty).toHaveAttribute('aria-disabled', 'true');
      await user.click(fifty);
      expect(fake.callsOf('setBotMusicSettings')).toHaveLength(0);
    });
  });
});
