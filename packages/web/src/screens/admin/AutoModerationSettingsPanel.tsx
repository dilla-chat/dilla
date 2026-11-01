import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { Button } from '@dilla/ui';
import { arr, decode, encode } from '@dilla/client-core';
import { useCore } from '../../core/context.tsx';
import { useSlice } from '../../core/use-slice.ts';
import { t } from '../../strings/index.ts';
import { StepUpDialog } from '../settings/StepUpDialog.tsx';
import { useBotDraftState, useBotInteractionOperation } from './bot-settings-context.tsx';
import { useSettingsEdit, type SettingsEditState } from '../../settings/edit-state.tsx';

type Config = { enabled: boolean; rate: string; duplicate: string; mention: string; timeout: number; exempt: string[] };
type Row = { deliveryId: string; requestId: string; communityId: string; operation: number; status: number; revision: string | null; data: Uint8Array };
type SaveRetry = { requestId: string; revision: string; document: Uint8Array; stepUp: string };
const id = (): string => [...crypto.getRandomValues(new Uint8Array(16))].map(x => x.toString(16).padStart(2, '0')).join('');
const defaults: Config = { enabled: false, rate: 'log', duplicate: 'log', mention: 'log', timeout: 600_000, exempt: [] };
const decodeConfig = (raw: Uint8Array): Config => {
  const a = arr(decode(raw), 7); if (a[0] !== 1n || (a[1] !== 0n && a[1] !== 1n) || typeof a[2] !== 'string'
    || typeof a[3] !== 'string' || typeof a[4] !== 'string' || typeof a[5] !== 'bigint' || !Array.isArray(a[6])) throw new Error('invalid config');
  const actions=['log','remove','warn','timeout'];
  if (![a[2],a[3],a[4]].every(v=>typeof v==='string'&&actions.includes(v))||a[5]<1n||a[5]>2419200000n
    ||a[6].length>256||a[6].some(v=>typeof v!=='string'||!/^[0-9a-f]{32}$/u.test(v))||new Set(a[6] as string[]).size!==a[6].length) throw new Error('invalid config');
  return { enabled: a[1] === 1n, rate: a[2], duplicate: a[3], mention: a[4], timeout: Number(a[5]), exempt: a[6] as string[] };
};

export function AutoModerationSettingsPanel({ communityId, botReady, canManage, canManageMessages, canModerateMembers, capabilities }: {
  communityId: string; botReady: boolean; canManage: boolean; canManageMessages: boolean; canModerateMembers: boolean; capabilities: number;
}): React.JSX.Element {
  const client = useCore(); const base = useId(); const dirtyRef = useRef(false); const generation = useRef(0);
  const readOperations = useBotInteractionOperation('automod-read');
  const saveOperations = useBotInteractionOperation('automod-save');
  const sharedRead = readOperations?.operation('automod-read') ?? null;
  const sharedSave = saveOperations?.operation('automod-save') ?? null;
  const factors = useSlice('factors');
  const [config, setConfig] = useBotDraftState<Config>(communityId, 'automod.config', defaults);
  const [confirmed, setConfirmed] = useBotDraftState<Config | null>(communityId, 'automod.confirmed', null);
  const [revision, setRevision] = useBotDraftState<string | null>(communityId, 'automod.revision', null);
  const [loaded, setLoaded] = useBotDraftState(communityId, 'automod.loaded', false);
  const [phase, setPhase] = useState<'idle'|'stepup'|'sending'|'waiting'>('idle');
  const [request, setRequest] = useState<string | null>(null); const [error, setError] = useState<string | null>(null); const [notice, setNotice] = useState<string | null>(null);
  const [requestOp, setRequestOp] = useState<62|63>(62);
  const [exemptText, setExemptText] = useBotDraftState(communityId, 'automod.exemptText', '');
  const [pendingConfig, setPendingConfig] = useBotDraftState<Config | null>(communityId, 'automod.pendingConfig', null);
  const [saveRetry, setSaveRetry] = useState<SaveRetry | null>(null);
  const saveResolver = useRef<((saved: boolean) => void) | null>(null);
  const current = useRef(communityId); current.current = communityId;
  const previousCommunity = useRef(communityId);
  useEffect(() => {
    if (previousCommunity.current === communityId) return;
    previousCommunity.current = communityId;
    generation.current++; setConfig(defaults); setConfirmed(null); setRevision(null); setLoaded(false); setPhase('idle');
    setRequest(null); setError(null); setNotice(null); setPendingConfig(null); setSaveRetry(null);
  }, [communityId, setConfig, setConfirmed, setRevision, setLoaded, setPendingConfig]);
  const read = useCallback(async (): Promise<void> => {
    if (!botReady || !canManage) return;
    if (readOperations !== null) {
      const requestId = readOperations.startOperation('automod-read', { communityId, operation: 62, args: [], revision: null });
      if (requestId === null) setError(t('admin.bot.automod.loadError'));
      else { setError(null); setNotice(null); }
      return;
    }
    const rid=id(), g=++generation.current; setError(null); setNotice(null); setRequest(rid); setRequestOp(62); setPhase('sending');
    try { const result = await client.call({m:'humanInteractionRequest',communityId,channelId:null,requestId:rid,operation:62,args:[],revision:null}) as {upload:string};
      if (g !== generation.current || current.current !== communityId) return;
      if (result.upload === 'rejected') throw new Error('upload'); setPhase('waiting');
    } catch { if(g===generation.current) { setPhase('idle'); setError(t('admin.bot.automod.loadError')); } }
  },[botReady,canManage,client,communityId,readOperations]);

  useEffect(() => {
    if (readOperations === null || sharedRead === null) return;
    if (sharedRead.phase === 'failed') { setError(t('admin.bot.automod.loadError')); return; }
    if (sharedRead.error === 'read') setError(t('admin.bot.automod.readError'));
    const row = sharedRead.response;
    if (row === null || !sharedRead.acked) return;
    try {
      if (row.status !== 0) { setError(t('admin.bot.automod.readDenied')); }
      else {
        const loadedConfig = decodeConfig(row.data);
        if (!dirtyRef.current) { setConfig(loadedConfig); setExemptText(loadedConfig.exempt.join('\n')); }
        setConfirmed(loadedConfig); setRevision(row.revision); setLoaded(true);
        setNotice(t('admin.bot.automod.loaded')); setError(null);
      }
    } catch { setError(t('admin.bot.automod.readError')); }
    finally { readOperations.clearOperation('automod-read'); }
  }, [readOperations, sharedRead, setConfig, setConfirmed, setExemptText, setRevision, setLoaded]);

  useEffect(() => {
    if (saveOperations === null || sharedSave === null) return;
    if (sharedSave.phase === 'failed') { setPhase('idle'); setError(t('admin.bot.automod.saveError')); return; }
    if (sharedSave.error === 'read') setError(t('admin.bot.automod.readError'));
    const row = sharedSave.response;
    if (row === null || !sharedSave.acked) return;
    setPhase('idle');
    if (row.status === 0) { setRevision(row.revision); setLoaded(false); setNotice(t('admin.bot.automod.saved')); setError(null); }
    else if (row.status === 4) { setRevision(row.revision); setLoaded(false); setNotice(t('admin.bot.automod.conflictCurrent',{revision:row.revision??'unknown'})); setError(t('admin.bot.automod.conflict')); }
    else setError(t('admin.bot.automod.saveDenied'));
    saveOperations.clearOperation('automod-save');
    if (row.status === 0) void read();
  }, [read, saveOperations, sharedSave, setLoaded, setRevision]);
  useEffect(() => {
    if (phase !== 'waiting' || request === null || requestOp !== 62) return; const g=generation.current; let canceled=false; let timer:number|undefined;
    const poll=async ():Promise<void> => { try {
      const page=await client.call({m:'readBotInteractionResponses',communityId,operations:[62],requestIds:[request]}) as {responses:Row[];through:string|null;blocked:boolean};
      if(canceled||g!==generation.current||current.current!==communityId)return;
      const row=page.responses.find(r=>r.requestId===request&&r.communityId===communityId&&r.operation===62);
      if(row===undefined){timer=window.setTimeout(()=>void poll(),1000);return;}
      if(page.through) await client.call({m:'ackBotInteractionResponses',communityId,through:page.through});
      if(canceled||g!==generation.current||current.current!==communityId)return;
      if(row){ if(row.status===0){ const x=decodeConfig(row.data); if (!dirtyRef.current) { setConfig(x); setExemptText(x.exempt.join('\n')); } setConfirmed(x); setRevision(row.revision); setLoaded(true); setPhase('idle'); setNotice(t('admin.bot.automod.loaded')); }
        else {setPhase('idle');setError(t('admin.bot.automod.readDenied'));} }
      else timer=window.setTimeout(()=>void poll(),1000);
    } catch { if(!canceled){setPhase('idle');setError(t('admin.bot.automod.readError'));} } };
    void poll(); return ()=>{canceled=true;if(timer!==undefined)window.clearTimeout(timer);};
  },[client,communityId,phase,request,requestOp,setConfig,setConfirmed,setExemptText,setLoaded,setRevision]);
  const beginSave=useCallback(():boolean=>{
    if(!loaded||revision===null||!botReady||!canManage)return false;
    const exemptions=exemptText.split(/[\s,]+/u).filter(Boolean);
    if(exemptions.length>256||exemptions.some(v=>!/^[0-9a-f]{32}$/u.test(v))||new Set(exemptions).size!==exemptions.length){setError(t('admin.bot.automod.invalidExemptions'));return false;}
    if(!Number.isSafeInteger(config.timeout)||config.timeout<1||config.timeout>2419200000){setError(t('admin.bot.automod.prepareError'));return false;}
    try { const raw=encode([1n,config.enabled?1n:0n,config.rate,config.duplicate,config.mention,BigInt(config.timeout),exemptions]);
      if(raw.length>2400)throw new Error('oversize'); setPendingConfig({...config,exempt:exemptions}); setPhase('stepup'); setError(null);
      return true;
    } catch {setError(t('admin.bot.automod.prepareError'));return false;}
  },[loaded,revision,botReady,canManage,exemptText,config,setError,setPendingConfig,setPhase]);
  const dispatchSave=(retry:SaveRetry):string|null=>{
    if (saveOperations !== null) {
      const requestId = saveOperations.startOperation('automod-save', { communityId, operation: 63, args: ['', retry.document],
        revision: retry.revision, stepUp: retry.stepUp, metadata: retry });
      if (requestId === null) return null;
      setPhase('waiting'); setNotice(t('admin.bot.automod.waiting')); setError(null);
      return requestId;
    }
    setRequest(retry.requestId);setRequestOp(63);setPhase('sending');setNotice(null);setError(null);
    void client.call({m:'humanInteractionRequest',communityId,channelId:null,requestId:retry.requestId,operation:63,args:['',retry.document],revision:retry.revision,stepUp:retry.stepUp}).then(result=>{
      if(current.current!==communityId)return; const status=(result as {upload:string}).upload;
      if(status==='accepted'||status==='pending'||status==='unknown'){
        setPhase('waiting');setNotice(t(status==='accepted'?'admin.bot.automod.waiting':'admin.bot.automod.uncertain'));
      } else {setPhase('idle');setError(t('admin.bot.automod.saveError'));}
    }).catch(()=>{if(current.current===communityId){setPhase('idle');setError(t('admin.bot.automod.saveError'));}});
    return retry.requestId;
  };
  const save=(stepUp:string):Promise<boolean>=>{
    const doc=pendingConfig; if(doc===null||revision===null)return Promise.resolve(false);
    try { const raw=encode([1n,doc.enabled?1n:0n,doc.rate,doc.duplicate,doc.mention,BigInt(doc.timeout),doc.exempt]); if(raw.length>2400)throw new Error('size');
      const retry={requestId:id(),revision,document:raw,stepUp};setSaveRetry(retry);
      const requestId = dispatchSave(retry);
      if (requestId === null || saveOperations === null) return Promise.resolve(false);
      return saveOperations.waitOperation('automod-save', requestId).then(result => result?.response?.status === 0);
    }catch{setPhase('idle');setError(t('admin.bot.automod.prepareError'));return Promise.resolve(false);}
  };
  useEffect(()=>{if(phase!=='waiting'||request===null||requestOp!==63)return;const g=generation.current;let canceled=false;let timer:number|undefined;
    const poll=async():Promise<void>=>{try{const page=await client.call({m:'readBotInteractionResponses',communityId,operations:[63],requestIds:[request]}) as {responses:Row[];through:string|null};
      if(canceled||g!==generation.current||current.current!==communityId)return;
      const row=page.responses.find(r=>r.requestId===request&&r.communityId===communityId&&r.operation===63);
      if(row===undefined){timer=window.setTimeout(()=>void poll(),1000);return;}
      if(page.through) await client.call({m:'ackBotInteractionResponses',communityId,through:page.through});
      if(canceled||g!==generation.current||current.current!==communityId)return;
      if(row){setPhase('idle');if(row.status===0){setRevision(row.revision);setLoaded(false);setNotice(t('admin.bot.automod.saved'));void read();}
        else if(row.status===4){setRevision(row.revision);setLoaded(false);setNotice(t('admin.bot.automod.conflictCurrent',{revision:row.revision??'unknown'}));setError(t('admin.bot.automod.conflict'));}
        else setError(t('admin.bot.automod.saveDenied'));}
      else timer=window.setTimeout(()=>void poll(),1000);
    }catch{if(!canceled){setPhase('idle');setError(t('admin.bot.automod.readError'));}}};void poll();return()=>{canceled=true;if(timer!==undefined)window.clearTimeout(timer);};
  },[client,communityId,phase,request,requestOp,read,setLoaded,setRevision]);
  const sharedBusy = sharedRead !== null && (sharedRead.phase === 'sending' || sharedRead.phase === 'waiting')
    || sharedSave !== null && (sharedSave.phase === 'sending' || sharedSave.phase === 'waiting');
  const busy=phase==='sending'||phase==='waiting'||sharedBusy;
  const exemptions = exemptText.split(/[\s,]+/u).filter(Boolean);
  const dirty = loaded && (confirmed === null || config.enabled !== confirmed.enabled || config.rate !== confirmed.rate
    || config.duplicate !== confirmed.duplicate || config.mention !== confirmed.mention || config.timeout !== confirmed.timeout
    || exemptions.join(',') !== confirmed.exempt.join(','));
  dirtyRef.current = dirty;
  const validDraft = exemptions.length <= 256 && exemptions.every(value => /^[0-9a-f]{32}$/u.test(value))
    && new Set(exemptions).size === exemptions.length && Number.isSafeInteger(config.timeout)
    && config.timeout >= 1 && config.timeout <= 2419200000;
  const submitSave = useCallback((): Promise<boolean> => new Promise(resolve => {
    saveResolver.current = resolve;
    if (!beginSave()) { saveResolver.current = null; resolve(false); }
  }), [beginSave]);
  const editState = useMemo<SettingsEditState>(() => ({ dirty, busy, canSave: dirty && validDraft && botReady && canManage && factors !== undefined && !busy,
    save: submitSave, reset: () => {
      if (confirmed === null) return;
      setConfig(confirmed); setExemptText(confirmed.exempt.join('\n')); setError(null);
    },
  }), [dirty, busy, validDraft, botReady, canManage, factors, confirmed, setConfig, setExemptText, submitSave]);
  const managed = useSettingsEdit(editState);
  const actionDisabled=(value:string):boolean=>value==='remove'?(!canManageMessages||(capabilities&(1<<5))===0)
    :(value==='warn'||value==='timeout')&&(!canModerateMembers||(capabilities&(1<<4))===0);
  const action=(key:'rate'|'duplicate'|'mention',label:string)=> <label>{label}<select value={config[key]} disabled={!canManage||!loaded} onChange={e=>setConfig({...config,[key]:e.target.value})}>
    {['log','remove','warn','timeout'].map(v=><option key={v} value={v} disabled={actionDisabled(v)}>
      {t(`admin.bot.automod.action.${v}` as 'admin.bot.automod.action.log'|'admin.bot.automod.action.remove'|'admin.bot.automod.action.warn'|'admin.bot.automod.action.timeout')}</option>)}</select></label>;
  return <section aria-labelledby={`${base}-title`}><h3 id={`${base}-title`}>{t('admin.bot.automod.title')}</h3><p>{t('admin.bot.automod.help')}</p>
    <p>{t('admin.bot.automod.defaults')}</p><p>{t('admin.bot.automod.capabilityHelp')}</p>{!botReady?<p>{t('admin.bot.automod.requiresBot')}</p>:null}
    {!canManage?<p>{t('admin.bot.automod.requiresManager')}</p>:null}
    <Button aria-disabled={!botReady||!canManage||busy||undefined} onClick={()=>void read()}>{busy?t('admin.bot.automod.waiting'):t('admin.bot.automod.load')}</Button>
    {loaded?<fieldset disabled={!canManage || busy}><label><input type="checkbox" checked={config.enabled} onChange={e=>setConfig({...config,enabled:e.target.checked})}/>{t('admin.bot.automod.enabled')}</label>
      {action('rate',t('admin.bot.automod.rate'))}{action('duplicate',t('admin.bot.automod.duplicate'))}{action('mention',t('admin.bot.automod.mention'))}
      <label>{t('admin.bot.automod.timeout')}<input type="number" min={1} max={2419200000} step={1000} value={config.timeout} onChange={e=>setConfig({...config,timeout:Number(e.target.value)})}/></label>
      <label>{t('admin.bot.automod.exemptions')}<textarea value={exemptText} onChange={e=>setExemptText(e.target.value)}/></label></fieldset>:null}
    {loaded&&!managed?<Button aria-disabled={!botReady||!canManage||busy||factors===undefined||undefined} onClick={beginSave}>{t('admin.bot.automod.save')}</Button>:null}
    {sharedSave !== null && sharedSave.phase === 'waiting' && sharedSave.uploadStatus !== 'accepted'
      && sharedSave.uploadStatus !== 'pending' && sharedSave.response === null
      ? <Button onClick={() => saveOperations?.retryOperation('automod-save')}>{t('admin.bot.automod.retry')}</Button>
      : requestOp===63&&phase==='waiting'&&saveRetry!==null?<Button onClick={()=>dispatchSave(saveRetry)}>{t('admin.bot.automod.retry')}</Button>:null}
    {notice?<p role="status">{notice}</p>:null}{error?<p role="alert">{error}</p>:sharedRead?.error==='read'||sharedSave?.error==='read'?<p role="alert">{t('admin.bot.automod.readError')}</p>:null}
    <StepUpDialog open={phase==='stepup'} hasPassword={factors?.hasPassword ?? true} notice={null} onToken={token=>{
      setPhase('idle'); void save(token).then(result => { saveResolver.current?.(result); saveResolver.current = null; });
    }} onClose={()=>{setPhase('idle');saveResolver.current?.(false);saveResolver.current=null;}}/>
  </section>;
}
