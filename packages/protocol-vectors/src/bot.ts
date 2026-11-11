import { encode } from './cbor.ts';
import { hex } from './bytes.ts';
import { createHash } from 'node:crypto';
import { keyFromSeed, sign } from './ed25519.ts';
import { concat } from './bytes.ts';
import { deriveKeyPair, sealBase } from './hpke.ts';

const id = (n: number) => new Uint8Array(16).fill(n);
const comm = id(0x22), group = id(0x33), now = '100';
const context = { community: hex(comm), now };
const app = (op:number,args:unknown[],channel:boolean,mutation:boolean,name:string) => {
  const val=[1,id(1+op%200),comm,channel?id(0x44):null,op,mutation?([31,70,80].includes(op)?0n:120n):null,mutation?120n:null,args];
  const raw=encode(val as never); let bin=''; for(const b of raw)bin+=String.fromCharCode(b);
  const body='dilla.bot/1:'+btoa(bin).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/u,'');
  return {name,kind:'request',hex:hex(raw),body,context};
};
const cbor = (v:unknown) => hex(encode(v as never));
export async function botVectors() {
 const pollPayload=encode([1n,id(0x91),null,'Lunch?', ['Pizza','Salad'],0n,86400n]);
 const reminderPayload=encode([1n,0n,3600n,'Take a break','Europe/Stockholm',null,null]);
 const feffQuestionPoll=encode([1n,id(0x44),null,'\uFEFF',['One','Two'],0n,3600n]);
 const feffAnswerPoll=encode([1n,id(0x44),null,'Question?',['\uFEFF','Two'],0n,3600n]);
 const inputs: Array<[number,unknown[],boolean,boolean,string]> = [
  [70,[feffQuestionPoll],false,true,'poll question accepts FEFF, not Unicode White_Space'],
  [70,[feffAnswerPoll],false,true,'poll answer accepts FEFF, not Unicode White_Space'],
  [70,[pollPayload],false,true,'poll.create'],[71,[id(0x91)],false,false,'poll.get'],
  [72,[id(0x44),null,null],false,false,'poll.list'],[73,[id(0x91),encode([0n,1n])],false,true,'poll.vote'],
  [74,[id(0x91)],false,true,'poll.close'],[75,[id(0x91),1n,null],false,false,'poll.voters'],
  [80,[reminderPayload],false,true,'reminder.create'],[81,[null],false,false,'reminder.list'],
  [82,[id(0x92)],false,true,'reminder.cancel'],[83,[id(0x92)],false,true,'reminder.dismiss'],
  [0,[],false,false,'overview.get'],[1,[2n],false,false,'settings.get'],[2,[2n,encode([1n,'x'])],false,true,'settings.put'],
  [10,[],true,false,'music.state'],[11,['search'],true,false,'music.search'],[12,['enqueue'],true,true,'music.enqueue'],
  [13,[],true,true,'music.pause'],[14,[],true,true,'music.resume'],[15,[],true,true,'music.skip'],[16,[],true,true,'music.stop'],
  [17,[18446744073709551615n],true,true,'music.seek uint64'],[18,[80n],true,true,'music.volume'],[19,[],true,true,'music.shuffle'],
  [20,[2n],true,true,'music.repeat'],[21,[id(0x55)],true,true,'music.remove'],[22,[id(0x55),null],true,true,'music.move'],[23,[],true,true,'music.clear'],[24,[],false,false,'music.providers.status'],
  [30,['hello'],false,false,'welcome.preview'],[31,[id(0x44),encode([['👍',id(0x55),'Readers']])],false,true,'roles.panel.publish'],[40,[0n],false,false,'roles.list'],[50,[0n],false,false,'announcements.list'],
  [51,[null,encode([1n])],false,true,'announcements.put'],[52,[id(0x66),1n],false,true,'announcements.delete'],
  [60,[null],false,false,'moderation.cases'],[64,[null],false,false,'moderation.evidence.list'],[61,[id(0x77),'reason','excerpt'],false,true,'moderation.evidence'],
 ];
 const cases:any[]=inputs.map(([op,a,ch,mu,n])=>app(op,a,ch,mu,n));
 const response=[1n,id(2),comm,0n,null,new Uint8Array()];
 cases.push({name:'response ok',kind:'response',hex:cbor(response),body:bodyOf(response),context});
 const proof=[1n,id(3),id(4),id(5),comm,group,7n,120n];
 const pre=Uint8Array.from([...new TextEncoder().encode('dilla moderation removal v1'),...encode(proof as never)]);
 cases.push({name:'signed moderation removal proof',kind:'removal',hex:cbor(proof),preimage_hex:hex(pre),context:{...context,group:hex(group),actor_user:hex(id(4)),actor_device:hex(id(5))}});
 const eventData:any[]=[
  [id(1),id(2)],[id(1),id(2)],[id(1),3n,7n],[0n,id(2),7n],[id(2),1n,7n],
  [id(1),id(2),id(3),id(4),null,group,9n,4n,null],[id(1),7n,1n],[id(1),2n],
 ];
 eventData.forEach((d,i)=>{const v=[1n,id(0x80+i),BigInt(i+1),comm,1700000000n,BigInt(i+1),d];cases.push({name:`event kind ${i+1}`,kind:'event',hex:cbor(v),context});});
 const sendResponse=(seq:bigint,tag:Uint8Array,at:bigint)=>encode([seq,tag,at]);
 const receipt=(op:Uint8Array,attempt:Uint8Array,predecessor:Uint8Array|null,generation:bigint,hash:Uint8Array,cls:bigint,outcome:bigint,epoch:bigint,body:Uint8Array|null)=>[
  1n,op,attempt,predecessor,comm,group,generation,hash,cls,outcome,epoch,body,
 ];
 const receiptContext={...context,group:hex(group)};
 const receipts:[string,unknown][]=[
  ['send receipt closed not accepted zero boundary',receipt(id(9),id(10),null,0n,new Uint8Array(32),0n,0n,0n,null)],
  ['send receipt accepted successor maximum boundary',receipt(id(9),id(11),id(10),(1n<<63n)-1n,new Uint8Array(32).fill(0x88),1n,1n,(1n<<63n)-1n,sendResponse((1n<<63n)-1n,new Uint8Array(32),(1n<<63n)-1n))],
  ['send receipt unknown after restore successor zero epoch',receipt(id(9),id(12),id(11),0n,new Uint8Array(32).fill(0x99),0n,2n,0n,null)],
  ['send receipt accepted zero receive time and opaque zero tag',receipt(id(9),id(13),null,1n,new Uint8Array(32),0n,1n,1n,sendResponse(1n,new Uint8Array(32),0n))],
 ];
 for(const [name,value] of receipts)cases.push({name,kind:'receipt',hex:cbor(value),context:receiptContext});
 const pushKey=(b:number)=>new Uint8Array(32).fill(b);
 const pushRow=(v:bigint):unknown[]=>[v,encode([1n,id(0x60),v,new Uint8Array(32),[],new Uint8Array(64)]),new Uint8Array(64).fill(0x44),new Uint8Array(32).fill(Number(v))];
 const devicePush=(rows:unknown[]):unknown[]=>[pushKey(0x11),pushKey(0x22),new Uint8Array(64).fill(0x33),rows];
 for(const [kind,name,v] of [
  ['helper_request','helper request',[1n,id(10),1n,encode([])]],['helper_request','helper group_transfer operation 16',[1n,id(10),16n,encode([])]],
  ['helper_request','helper ingest removals operation 17',[1n,id(15),17n,encode([])]],['helper_request','helper removal actor refresh operation 18',[1n,id(16),18n,encode([])]],
  ['helper_request','call join check helper operation',[1n,id(11),19n,encode([id(12)])]],['helper_request','call group forget helper operation',[1n,id(13),20n,encode([id(14)])]],
  ['helper_request','command key helper operation 21',[1n,id(17),21n,encode([id(18),1n])]],['helper_request','command open helper operation 22',[1n,id(19),22n,encode([new Uint8Array([0x80]),id(20),id(21),1n])]],
  ['helper_request','reply seal helper operation 23',[1n,id(22),23n,encode([new Uint8Array(32),id(23),0n,new Uint8Array([1])])]],['helper_request','device list ingest helper operation 24',[1n,id(24),24n,encode([id(25),encode(devicePush([pushRow(1n)])),1n])]],
  // Bot v2 task 17 fix round 2: the health probe's seal and the opening of its answer.
  ['helper_request','probe seal helper operation 25',[1n,id(26),25n,encode([id(27),id(28),1n])]],['helper_request','probe open helper operation 26',[1n,id(29),26n,encode([1n,0n,new Uint8Array([0x83]),1n])]],
  // Bot v2 task 23 (R1 d): the Bot forgets a text group whose channel is not a Bot channel, or that it was removed from.
  ['helper_request','group forget helper operation 27',[1n,id(30),27n,encode([id(31)])]],
  ['helper_response','helper response',[1n,id(10),0n,encode([])]],
 ] as const)cases.push({name,kind,hex:cbor(v)});
 cases.push({name:'generic helper op5 payload remains opaque',kind:'helper_request',hex:cbor([1n,id(0x95),5n,encode([32n,1n])])});
 // The device-list push (helper op 24 body): shape only; the trust rules are the core's.
 cases.push({name:'device list push first sight from version 1',kind:'device_list_push',hex:cbor(devicePush([pushRow(1n)]))});
 cases.push({name:'device list push contiguous history',kind:'device_list_push',hex:cbor(devicePush([pushRow(2n),pushRow(3n),pushRow(4n)]))});
 const rejects:any[]=[];
 const rej=(name:string,kind:string,v:unknown,extra:any={})=>rejects.push({name,kind,hex:cbor(v),...extra});
 rej('device list push without history','device_list_push',devicePush([]));
 rej('device list push zero umk','device_list_push',[new Uint8Array(32),pushKey(0x22),new Uint8Array(64).fill(0x33),[pushRow(1n)]]);
 rej('device list push short ssk','device_list_push',[pushKey(0x11),new Uint8Array(31).fill(0x22),new Uint8Array(64).fill(0x33),[pushRow(1n)]]);
 rej('device list push version zero','device_list_push',devicePush([pushRow(0n)]));
 rej('device list push empty blob','device_list_push',devicePush([[1n,new Uint8Array(),new Uint8Array(64).fill(0x44),new Uint8Array(32)]]));
 rej('device list push short row signature','device_list_push',devicePush([[1n,encode([1n]),new Uint8Array(63).fill(0x44),new Uint8Array(32)]]));
 rej('device list push extra field','device_list_push',[...devicePush([pushRow(1n)]),null]);
 const base=[1n,id(1),comm,null,0n,null,null,[]];
 rej('application wrong version','request',[2n,...base.slice(1)]);
 rej('application wrong shape','request',[1n,id(1)]);
 rej('identifier wrong type','request',[1n,1n,comm,null,0n,null,null,[]]);
 rej('identifier wrong length','request',[1n,new Uint8Array(15).fill(1),comm,null,0n,null,null,[]]);
 rej('identifier zero','request',[1n,id(0),comm,null,0n,null,null,[]]);
 rej('unsupported application operation','request',[1n,id(1),comm,null,999n,null,null,[]]);
 rej('missing mutation revision','request',[1n,id(1),comm,null,2n,null,120n,[0n,encode([1n])]]);
 rej('missing required channel','request',[1n,id(1),comm,null,10n,null,null,[]]);
 rej('wrong argument type','request',[1n,id(1),comm,null,1n,null,null,['bad']]);
 rej('wrong argument count','request',[1n,id(1),comm,null,0n,null,null,[1n]]);
 // Bot v2 Task 23: the v1-only operations are gone (capabilities 3, AutoModeration 62 and 63, due reminders 84).
 rej('retired capabilities operation 3','request',[1n,id(1),comm,null,3n,null,null,[]]);
 rej('retired AutoModeration get operation 62','request',[1n,id(1),comm,null,62n,null,null,[]]);
 rej('retired AutoModeration put operation 63','request',[1n,id(64),comm,null,63n,1n,120n,['grant-token',encode([1n,0n,'log','log','log',600000n,[]])]]);
 rej('retired due reminders operation 84','request',[1n,id(1),comm,null,84n,null,null,[null]]);
 const badPoll=encode([1n,id(0x91),null,'Question?',['Same','Same'],0n,86400n]);
 rej('poll duplicate answers','request',[1n,id(70),comm,null,70n,0n,120n,[badPoll]]);
 const nelQuestionPoll=encode([1n,id(0x44),null,'\u0085',['One','Two'],0n,3600n]);
 rej('poll question rejects Unicode White_Space NEL','request',[1n,id(70),comm,null,70n,0n,120n,[nelQuestionPoll]]);
 const nelAnswerPoll=encode([1n,id(0x44),null,'Question?',['\u0085','Two'],0n,3600n]);
 rej('poll answer rejects Unicode White_Space NEL','request',[1n,id(70),comm,null,70n,0n,120n,[nelAnswerPoll]]);
 const badDuration=encode([1n,id(0x91),null,'Question?',['One','Two'],0n,1800n]);
 rej('poll unsupported duration','request',[1n,id(70),comm,null,70n,0n,120n,[badDuration]]);
 rej('poll create nonzero revision','request',[1n,id(70),comm,null,70n,1n,120n,[pollPayload]]);
 rej('poll read has mutation context','request',[1n,id(71),comm,null,71n,1n,120n,[id(0x91)]]);
 rej('poll list has outer channel','request',[1n,id(72),comm,id(0x44),72n,null,null,[id(0x44),null,null]]);
 rej('poll reused selections','request',[1n,id(73),comm,null,73n,1n,120n,[id(0x91),encode([0n,0n])]]);
 rej('poll out of range selection','request',[1n,id(73),comm,null,73n,1n,120n,[id(0x91),encode([10n])]]);
 const oversizedPoll=encode([1n,id(0x91),null,'😀'.repeat(300),Array.from({length:10},(_,i)=>String(i)+'😀'.repeat(99)),0n,86400n]);
 rej('poll payload exceeds text aggregate','request',[1n,id(70),comm,null,70n,0n,120n,[oversizedPoll]]);
 const threadOnly=encode([1n,0n,3600n,'Take a break','UTC',null,id(0x93)]);
 rej('reminder thread without source channel','request',[1n,id(80),comm,null,80n,0n,120n,[threadOnly]]);
 const badWhen=encode([1n,1n,253402300800n,'Take a break','UTC',null,null]);
 rej('reminder timestamp outside numeric range','request',[1n,id(80),comm,null,80n,0n,120n,[badWhen]]);
 rej('query over UTF8 byte limit','request',[1n,id(1),comm,id(0x44),11n,null,null,['é'.repeat(101)]]);
 rej('cross community context','request',base,{body:bodyOf(base),context:{community:hex(id(0x23)),now}});
 const mut=[1n,id(1),comm,null,2n,100n,131n,[0n,encode([1n])]];
 rej('expired mutation context','request',mut,{body:bodyOf(mut),context:{community:hex(comm),now}});
 rej('far future mutation context','request',[...mut.slice(0,6),131n+30n,...mut.slice(7)],{context:{community:hex(comm),now}});
 rejects.push({name:'noncanonical base64url',kind:'request',body:'dilla.bot/1:gA=',context});
 rej('nonminimal CBOR','request',null as never); rejects.pop();
 rejects.push({name:'nonminimal CBOR',kind:'request',hex:'9800'});
 rejects.push({name:'trailing CBOR',kind:'request',hex:'8000'});
 rejects.push({name:'CBOR map',kind:'request',hex:'a0'});
 rejects.push({name:'CBOR nesting over limit',kind:'request',hex:'8181818181818181818100'});
 rej('removal sequence zero','removal',[1n,id(3),id(4),id(5),comm,group,0n,120n]);
 const validProof=[1n,id(3),id(4),id(5),comm,group,7n,120n];
 rejects.push({name:'removal actor user mismatch',kind:'removal',hex:cbor(validProof),
  context:{...context,group:hex(group),actor_user:hex(id(9)),actor_device:hex(id(5))}});
 rejects.push({name:'removal actor device mismatch',kind:'removal',hex:cbor(validProof),
  context:{...context,group:hex(group),actor_user:hex(id(4)),actor_device:hex(id(9))}});
 rej('event metadata invalid','event',[1n,id(1),1n,comm,1n,3n,[id(2),6n,7n]]);
 rej('timeout event missing expiry','event',[1n,id(1),1n,comm,10n,6n,[id(2),id(3),id(4),id(5),id(6),null,null,1n,null]]);
 const receiptBase=receipt(id(1),id(2),null,0n,new Uint8Array(32),0n,0n,0n,null);
 rej('send receipt obsolete eight field tuple','receipt',[1n,id(1),comm,group,new Uint8Array(32),0n,1n,null]);
 rej('send receipt wrong version','receipt',[2n,...receiptBase.slice(1)]);
 rej('send receipt wrong twelve field shape','receipt',receiptBase.slice(0,11));
 for(const [name,index,value] of [
  ['zero operation id',1,id(0)],['wrong type operation id',1,1n],['wrong length operation id',1,new Uint8Array(15).fill(1)],
  ['zero attempt id',2,id(0)],['wrong type attempt id',2,1n],['wrong length attempt id',2,new Uint8Array(17).fill(1)],
  ['zero predecessor id',3,id(0)],['wrong type predecessor id',3,1n],['wrong length predecessor id',3,new Uint8Array(15).fill(1)],
  ['zero community id',4,id(0)],['wrong type community id',4,1n],['wrong length community id',4,new Uint8Array(15).fill(1)],
  ['zero group id',5,id(0)],['wrong type group id',5,1n],['wrong length group id',5,new Uint8Array(17).fill(1)],
  ['wrong hash type',7,Array(32).fill(0)],['wrong hash length',7,new Uint8Array(31)],
  ['invalid class',8,2n],['invalid outcome',9,3n],
 ] as const){const v=[...receiptBase];v[index]=value;rej(`send receipt ${name}`,'receipt',v);}
 for(const [name,index] of [['generation exceeds int64',6],['epoch exceeds int64',10]] as const){const v=[...receiptBase];v[index]=1n<<63n;rej(`send receipt ${name}`,'receipt',v);}
 for(const [name,seq,time,body] of [
  ['accepted sequence zero',0n,0n,null],
  ['accepted sequence exceeds int64',(1n<<63n),0n,null],
  ['accepted time exceeds int64',1n,(1n<<63n),null],
  ['accepted null response',1n,0n,null],
 ] as const){const v=[...receiptBase];v[9]=1n;v[11]=name==='accepted null response'?null:body??sendResponse(seq,new Uint8Array(32),time);rej(`send receipt ${name}`,'receipt',v);}
 const malformedNested:[string,Uint8Array][]=[
  ['nested wrong shape',encode([1n,new Uint8Array(32)])],
  ['nested noncanonical',Uint8Array.from([0x98,0x03,0x01,0x58,0x20,...new Uint8Array(32),0x00])],
  ['nested trailing item',Uint8Array.from([...sendResponse(1n,new Uint8Array(32),0n),0x00])],
  ['nested map',Uint8Array.from([0x83,0x01,0xa0,0x00])],
  ['nested tagged item',Uint8Array.from([0x83,0x01,0xd8,0x2a,0x58,0x20,...new Uint8Array(32),0x00])],
  ['nested boolean timestamp',Uint8Array.from([0x83,0x01,0x58,0x20,...new Uint8Array(32),0xf5])],
  ['nested integer array tag',encode([1n,Array(32).fill(0),0n])],
 ];
 for(const [name,body] of malformedNested){const v=receipt(id(1),id(2),null,0n,new Uint8Array(32),0n,1n,0n,body);rej(`send receipt ${name}`,'receipt',v);}
 for(const [name,outcome,body] of [['closed not accepted with response',0n,sendResponse(1n,new Uint8Array(32),0n)],['unknown after restore with response',2n,sendResponse(1n,new Uint8Array(32),0n)]] as const)
  rej(`send receipt ${name}`,'receipt',receipt(id(1),id(2),null,0n,new Uint8Array(32),0n,outcome,0n,body));
 rej('send receipt response byte coercion','receipt',receipt(id(1),id(2),null,0n,new Uint8Array(32),0n,1n,0n,Array.from(sendResponse(1n,new Uint8Array(32),0n))));
 rej('send receipt exceeds 512 bytes','receipt',receipt(id(1),id(2),null,0n,new Uint8Array(32),0n,0n,0n,new Uint8Array(500)));
 rej('send receipt cross community context','receipt',receiptBase,{context:{...receiptContext,community:hex(id(0x23))}});
 rej('send receipt cross group context','receipt',receiptBase,{context:{...receiptContext,group:hex(id(0x34))}});
 rej('response unsupported status','response',[1n,id(1),comm,10n,null,new Uint8Array()]);
 rej('helper operation 28 is unknown','helper_request',[1n,id(1),28n,encode([])]);
 const oversizedPayload=encode([new Uint8Array(1_048_475)]);
 if(oversizedPayload.length!==1_048_481)throw new Error('helper payload fixture must exceed the payload bound by one byte');
 rejects.push({name:'helper payload one byte over bound',kind:'helper_request',hex:cbor([1n,id(1),1n,oversizedPayload])});
 rejects.push({name:'helper malformed length prefix',kind:'helper_request',hex:'0000000580'});
 // Typed KeyPackage payloads are distinct from generic helper envelopes.
 const upload=encode([[new Uint8Array([0x51])],null]);
 const refs=[new Uint8Array(32).fill(0xa1)];
 const typedCases:[string,unknown][]=[
  ['keypackage_publication',[id(0x91),(1n<<63n)-1n,upload]],
  ['keypackage_publication_receipt',[0n,new Uint8Array(32).fill(0x92),33n]],
  ['keypackage_intent',[id(0x93),32n,1n,(1n<<63n)-1n]],
  ['keypackage_intent_result',[upload,refs,(1n<<63n)-1n]],
  ['keypackage_resolution',[id(0x94),refs,1n,(1n<<63n)-1n,(1n<<63n)-1n]],
 ];
 for(const [kind,value] of typedCases)cases.push({name:`${kind} maximum valid boundary`,kind,hex:cbor(value)});
 const zeroBoundaries:[string,unknown][]=[
  ['keypackage_publication',[id(0x91),0n,upload]],
  ['keypackage_publication_receipt',[0n,new Uint8Array(32).fill(0x92),1n]],
  ['keypackage_intent',[id(0x93),1n,0n,0n]],
  ['keypackage_intent_result',[upload,refs,0n]],
  ['keypackage_resolution',[id(0x94),refs,0n,0n,0n]],
 ];
 for(const [kind,value] of zeroBoundaries)cases.push({name:`${kind} zero boundary`,kind,hex:cbor(value)});
 cases.push({name:'keypackage_resolution permits opaque zero-valued ref',kind:'keypackage_resolution',hex:cbor([id(0x94),[new Uint8Array(32)],0n,0n,0n])});
 const typedRejects:[string,string,unknown][]=[
  ['keypackage_publication','zero operation id',[id(0),1n,upload]],
  ['keypackage_publication','generation exceeds int64',[id(0x91),1n<<63n,upload]],
  ['keypackage_publication','malformed nested upload',[id(0x91),1n,encode(['legacy',null])]],
  ['keypackage_publication_receipt','wrong upload hash length',[0n,new Uint8Array(31),1n]],
  ['keypackage_publication_receipt','zero receipt count',[0n,new Uint8Array(32),0n]],
  ['keypackage_intent','count exceeds maximum',[id(0x93),33n,0n,0n]],
  ['keypackage_intent','legacy op5 request shape',[32n,1n]],
  ['keypackage_intent_result','duplicate package refs',[upload,[refs[0],refs[0]],1n]],
  ['keypackage_intent_result','missing package ref',[upload,[],1n]],
  ['keypackage_intent_result','legacy op5 result shape',[upload,1n]],
  ['keypackage_resolution','outcome out of range',[id(0x94),refs,2n,0n,0n]],
  ['keypackage_resolution','duplicate refs',[id(0x94),[refs[0],refs[0]],0n,0n,0n]],
  ['keypackage_resolution','legacy op14 request shape',[id(0x94),refs]],
 ];
 for(const [kind,name,value] of typedRejects)rej(name,kind,value);
 // Managed Bot first-registration receipt contract. The bstr is the exact
 // canonical four-field body sent to POST /groups; hash it without rebuilding.
 const registrationBody=encode([id(0x33),new Uint8Array([0x01]),new Uint8Array([0x02]),new Uint8Array([0x03])]);
 const registrationHash=createHash('sha256').update(Buffer.from('dilla bot group registration v1\0','ascii')).update(registrationBody).digest();
 const registrationOperation=id(0xa1), registrationGroup=id(0x33);
 const registrationRequest=[registrationOperation,7n,registrationBody];
 const registrationReceipt=[registrationOperation,registrationGroup,new Uint8Array(registrationHash),7n,4n,1n];
 cases.push({name:'group registration request carries exact canonical four-field body',kind:'group_registration_request',hex:cbor(registrationRequest),registration_body_hex:hex(registrationBody),registration_sha256:registrationHash.toString('hex')});
 cases.push({name:'group registration acceptance receipt preserves original generation epoch and sequence',kind:'group_registration_receipt',hex:cbor(registrationReceipt)});
 const registrationRejects:[string,unknown][]=[
  ['zero operation id',[id(0),7n,registrationBody]],
  ['generation exceeds int64',[registrationOperation,1n<<63n,registrationBody]],
  ['registration body is a noncanonical four-field array',[registrationOperation,7n,new Uint8Array([0x98,0x04,...registrationBody.slice(1)])]],
  ['registration body has wrong tuple length',[registrationOperation,7n,encode([registrationGroup,new Uint8Array([1]),new Uint8Array([2])])]],
  ['registration body has trailing bytes',[registrationOperation,7n,new Uint8Array([...registrationBody,0x00])]],
 ];
 for(const [name,value] of registrationRejects)rej(name,'group_registration_request',value);
 for(const [name,index,value] of [
  ['receipt zero operation id',0,id(0)],['receipt zero group id',1,id(0)],['receipt wrong hash length',2,new Uint8Array(31)],
  ['receipt generation exceeds int64',3,1n<<63n],['receipt epoch exceeds int64',4,1n<<63n],
  ['receipt next sequence zero',5,0n],
  ['receipt initial next sequence is not one',5,2n],
 ] as const){const v=[...registrationReceipt];v[index]=value;rej(name,'group_registration_receipt',v);}
 const malformedOuter=[['nonminimal CBOR array head','9800'],['trailing CBOR item','8000'],['CBOR map root','a0'],['indefinite CBOR array','9fff']] as const;
 for(const kind of ['keypackage_publication','keypackage_publication_receipt','keypackage_intent','keypackage_intent_result','keypackage_resolution'])
  for(const [name,hexValue] of malformedOuter)rejects.push({name:`${kind}: ${name}`,kind,hex:hexValue});
 const sealed_command=await sealedCommandVectors();
 // The native helper's Bot event page (group sub-op 18), body [events, next, high_water]. The core reports a high
 // water never below its durable ack, so a journal whose newest rows were deleted (the v1 upgrade) still pages; Go keeps
 // next <= high_water as an invariant (dilla-core tests/client_bot_events.rs, internal/bot/native groups_test.go).
 const helper_event_pages={
  accepts:[
   {name:'acked journal whose newest rows were deleted reports the ack as high water',hex:cbor([[],5n,5n]),next:5,high_water:5},
   {name:'journal with no rows left under ack 5 reports the ack as next and high water',hex:cbor([[],5n,5n]),next:5,high_water:5},
   {name:'empty journal',hex:cbor([[],0n,0n]),next:0,high_water:0},
  ],
  rejects:[
   {name:'next above high water',hex:cbor([[],5n,3n]),error:'event cursors'},
  ],
 };
 // The helper's command-key timing, which dillad checks too (internal/api TestCommandKeyTimingMatchesTheVector).
 return {version:1,command_key_timing:{max_lifetime_secs:691200,publication_slack_secs:600},cases,rejects,sealed_command,helper_event_pages};
}
function bodyOf(v:unknown):string {const raw=encode(v as never);let s='';for(const b of raw)s+=String.fromCharCode(b);return 'dilla.bot/1:'+btoa(s).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/u,'');}

// Sealed Bot commands (Bot v2 task 7). Every key comes from a fixed seed or ikm, so Rust reproduces each byte:
// the DSK signatures are deterministic Ed25519, and the HPKE ephemeral and reply keys are DeriveKeyPair(ikm).
async function sealedCommandVectors() {
 const te=(s:string)=>new TextEncoder().encode(s);
 const now=1790000000n;
 const signedList=async (user:Uint8Array,device:Uint8Array,dskSeed:Uint8Array,sskSeed:Uint8Array) => {
  const dsk=await keyFromSeed(dskSeed),ssk=await keyFromSeed(sskSeed);
  const entries=[[device,dsk.publicKey,0n,100n,null]];
  const unsigned=encode([1n,user,1n,new Uint8Array(32),entries]);
  const sig=await sign(ssk.privateKey,concat(te('dilla devices v1'),unsigned));
  return {dsk,sskPub:ssk.publicKey,blob:encode([1n,user,1n,new Uint8Array(32),entries,sig])};
 };
 const botUser=id(0xb0),botDevice=id(0xb1),botDskSeed=new Uint8Array(32).fill(0xb2),botSskSeed=new Uint8Array(32).fill(0xb3);
 const user=id(0x10),device=id(0x11),userDskSeed=new Uint8Array(32).fill(0x12),userSskSeed=new Uint8Array(32).fill(0x13);
 const bot=await signedList(botUser,botDevice,botDskSeed,botSskSeed);
 const usr=await signedList(user,device,userDskSeed,userSskSeed);
 const community=id(0x22),keyId=id(0x33),notAfter=now+604800n;
 const commandIkm=new Uint8Array(32).fill(0x42);
 const commandKey=await deriveKeyPair(commandIkm);
 const pubFields=[1n,community,keyId,commandKey.pk,notAfter,botDevice];
 const pubSig=await sign(bot.dsk.privateKey,concat(te('dilla bot command key v1'),encode(pubFields)));
 const publication=encode([...pubFields,pubSig]);
 const requestId=id(0x44),channel=id(0x55);
 const args=['never gonna give you up',3n,new Uint8Array([1,2,3]),null];
 const replyIkm=new Uint8Array(32).fill(0x02),ephemeralIkm=new Uint8Array(32).fill(0x01);
 const replyKey=await deriveKeyPair(replyIkm);
 // The signed form names the command key (key_id) it is sealed to, between reply_key and user_id.
 const unsignedCmd=[1n,requestId,community,channel,11n,args,now,replyKey.pk,keyId,user,device];
 const cmdSig=await sign(usr.dsk.privateKey,concat(te('dilla bot command v1'),encode(unsignedCmd as never)));
 const signedCmd=encode([...unsignedCmd,cmdSig] as never);
 const sealedInner=await sealBase(commandKey.pk,te('dilla bot command v1'),requestId,signedCmd,ephemeralIkm);
 const sealed=encode([1n,keyId,requestId,sealedInner.enc,sealedInner.ct]);
 const replies=[];
 for(const [seq,body,fill] of [[0,'Searching…',0x03],[1,'Now playing: never gonna give you up',0x04]] as const){
  const ikm=new Uint8Array(32).fill(fill);
  const aad=concat(requestId,new Uint8Array([seq>>>24,(seq>>>16)&0xff,(seq>>>8)&0xff,seq&0xff]));
  const r=await sealBase(replyKey.pk,te('dilla bot reply v1'),aad,te(body),ikm);
  replies.push({seq,body_hex:hex(te(body)),ephemeral_ikm:hex(ikm),sealed_hex:hex(encode([1n,r.enc,r.ct]))});
 }
 // [0x85, 0x01, 0x50, key_id(16), 0x50, request_id(16), ...]: byte 20 is the first byte of the outer request id.
 // A modified client's commands: signed for another community than the key's, or for another key_id than the outer.
 const sealRaw=async (fields:unknown[],outerKeyId:Uint8Array,ikm:number) => {
  const sig=await sign(usr.dsk.privateKey,concat(te('dilla bot command v1'),encode(fields as never)));
  const inner=await sealBase(commandKey.pk,te('dilla bot command v1'),requestId,encode([...fields,sig] as never),new Uint8Array(32).fill(ikm));
  return encode([1n,outerKeyId,requestId,inner.enc,inner.ct]);
 };
 const otherCommunity=await sealRaw([1n,requestId,id(0x23),channel,11n,args,now,replyKey.pk,keyId,user,device],keyId,0x06);
 const otherKeyId=await sealRaw([1n,requestId,community,channel,11n,args,now,replyKey.pk,id(0x34),user,device],keyId,0x07);
 const tampered=sealed.slice();tampered[tampered.length-1]!^=1;
 const movedId=sealed.slice();movedId[20]!^=1;
 return {
  now:Number(now),
  bot:{user:hex(botUser),device:hex(botDevice),dsk_seed:hex(botDskSeed),ssk_pub:hex(bot.sskPub),list_hex:hex(bot.blob)},
  user:{user:hex(user),device:hex(device),dsk_seed:hex(userDskSeed),ssk_pub:hex(usr.sskPub),list_hex:hex(usr.blob)},
  command_key:{ikm:hex(commandIkm),sk:hex(commandKey.sk),pk:hex(commandKey.pk)},
  publication_hex:hex(publication),
  request:{request_id:hex(requestId),community_id:hex(community),channel_id:hex(channel),operation:11,
   args:[{text:args[0]},{uint:3},{bytes:'010203'},{null:true}],sent_at:Number(now)},
  ephemeral_ikm:hex(ephemeralIkm),reply_ikm:hex(replyIkm),reply_key:hex(replyKey.pk),
  signed_hex:hex(signedCmd),sealed_hex:hex(sealed),replies,
  rejects:[
   {name:'attached to another device',sealed_hex:hex(sealed),attached_device:hex(id(0x14)),now:Number(now),refusal:'WrongDevice'},
   {name:'sent_at 121 s in the past',sealed_hex:hex(sealed),attached_device:hex(device),now:Number(now+121n),refusal:'Stale'},
   {name:'ciphertext bit flipped',sealed_hex:hex(tampered),attached_device:hex(device),now:Number(now),refusal:'Malformed'},
   {name:'outer request id changed',sealed_hex:hex(movedId),attached_device:hex(device),now:Number(now),refusal:'Malformed'},
   {name:'signed for another community than the key',sealed_hex:hex(otherCommunity),attached_device:hex(device),now:Number(now),refusal:'Malformed'},
   {name:'signed for another key_id than the outer',sealed_hex:hex(otherKeyId),attached_device:hex(device),now:Number(now),refusal:'Malformed'},
  ],
 };
}
