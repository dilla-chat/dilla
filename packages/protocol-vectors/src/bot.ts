import { encode } from './cbor.ts';
import { hex } from './bytes.ts';
import { createHash } from 'node:crypto';
import { keyFromSeed, sign } from './ed25519.ts';
import { concat } from './bytes.ts';

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
  [3,[],false,false,'capabilities'],
  [70,[feffQuestionPoll],false,true,'poll question accepts FEFF, not Unicode White_Space'],
  [70,[feffAnswerPoll],false,true,'poll answer accepts FEFF, not Unicode White_Space'],
  [70,[pollPayload],false,true,'poll.create'],[71,[id(0x91)],false,false,'poll.get'],
  [72,[id(0x44),null,null],false,false,'poll.list'],[73,[id(0x91),encode([0n,1n])],false,true,'poll.vote'],
  [74,[id(0x91)],false,true,'poll.close'],[75,[id(0x91),1n,null],false,false,'poll.voters'],
  [80,[reminderPayload],false,true,'reminder.create'],[81,[null],false,false,'reminder.list'],
  [82,[id(0x92)],false,true,'reminder.cancel'],[83,[id(0x92)],false,true,'reminder.dismiss'],[84,[null],false,false,'reminder.due'],
  [0,[],false,false,'overview.get'],[1,[2n],false,false,'settings.get'],[2,[2n,encode([1n,'x'])],false,true,'settings.put'],
  [10,[],true,false,'music.state'],[11,['search'],true,false,'music.search'],[12,['enqueue'],true,true,'music.enqueue'],
  [13,[],true,true,'music.pause'],[14,[],true,true,'music.resume'],[15,[],true,true,'music.skip'],[16,[],true,true,'music.stop'],
  [17,[18446744073709551615n],true,true,'music.seek uint64'],[18,[80n],true,true,'music.volume'],[19,[],true,true,'music.shuffle'],
  [20,[2n],true,true,'music.repeat'],[21,[id(0x55)],true,true,'music.remove'],[22,[id(0x55),null],true,true,'music.move'],[23,[],true,true,'music.clear'],[24,[],false,false,'music.providers.status'],
  [30,['hello'],false,false,'welcome.preview'],[31,[id(0x44),encode([['👍',id(0x55),'Readers']])],false,true,'roles.panel.publish'],[40,[0n],false,false,'roles.list'],[50,[0n],false,false,'announcements.list'],
  [51,[null,encode([1n])],false,true,'announcements.put'],[52,[id(0x66),1n],false,true,'announcements.delete'],
  [60,[null],false,false,'moderation.cases'],[64,[null],false,false,'moderation.evidence.list'],[61,[id(0x77),'reason','excerpt'],false,true,'moderation.evidence'],
  [62,[],false,false,'automod.config.get'],[63,['grant-token',encode([1n,0n,'log','log','log',600000n,[]])],false,true,'automod.config.put'],
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
 for(const [kind,name,v] of [
  ['helper_request','helper request',[1n,id(10),1n,encode([])]],['helper_request','helper group_transfer operation 16',[1n,id(10),16n,encode([])]],
  ['helper_request','helper ingest removals operation 17',[1n,id(15),17n,encode([])]],['helper_request','helper removal actor refresh operation 18',[1n,id(16),18n,encode([])]],
  ['helper_request','call join check helper operation',[1n,id(11),19n,encode([id(12)])]],['helper_request','call group forget helper operation',[1n,id(13),20n,encode([id(14)])]],
  ['helper_response','helper response',[1n,id(10),0n,encode([])]],
 ] as const)cases.push({name,kind,hex:cbor(v)});
 cases.push({name:'generic helper op5 payload remains opaque',kind:'helper_request',hex:cbor([1n,id(0x95),5n,encode([32n,1n])])});
 const rejects:any[]=[];
 const rej=(name:string,kind:string,v:unknown,extra:any={})=>rejects.push({name,kind,hex:cbor(v),...extra});
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
 rej('automod config get has arguments','request',[1n,id(1),comm,null,62n,null,null,[0n]]);
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
 const automodDoc=encode([1n,0n,'log','log','log',600000n,[]]);
 rej('automod config put empty grant','request',[1n,id(64),comm,null,63n,1n,120n,['',automodDoc]]);
 rej('automod config put oversized grant','request',[1n,id(64),comm,null,63n,1n,120n,['g'.repeat(513),automodDoc]]);
 rej('automod config put oversized document','request',[1n,id(64),comm,null,63n,1n,120n,['grant-token',encode([new Uint8Array(2400)])]]);
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
 rej('helper operation 21 is unknown','helper_request',[1n,id(1),21n,encode([])]);
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
 // Cross-language server-signed interaction admission proof. All IDs are fixed
 // repeated-byte values and the seed is public test material, so Go and Rust
 // can independently reproduce the exact canonical payload and signature.
 const admissionSeed = new Uint8Array(32).fill(0x41);
 const admissionKey = await keyFromSeed(admissionSeed);
 const admissionPayload = encode([
  1n, id(1), id(2), id(3), id(4), id(5), id(6), id(7), id(8), id(9), 11n, 130n,
 ]);
 const admissionSignature = await sign(admissionKey.privateKey,
  concat(new TextEncoder().encode('dilla bot interaction admission v1\0'), admissionPayload));
 const admissionProof = encode([admissionPayload, admissionSignature]);
 const snapshotSeed=new Uint8Array(32);snapshotSeed[0]=0x41;
 const snapshotKey=await keyFromSeed(snapshotSeed);
 const snapshotAdmissionPayload=encode([
  1n,id(1),id(2),id(3),id(4),id(5),id(6),id(7),id(8),id(9),7n,130n,
 ]);
 const snapshotAdmissionSignature=await sign(snapshotKey.privateKey,
  concat(new TextEncoder().encode('dilla bot interaction admission v1\0'),snapshotAdmissionPayload));
 const list = (user:number, umk:number, ssk:number, bindingSig:number, blobValue:number,
  listSig:number, prevHash:number) => {
  const blob=new Uint8Array([0x82,blobValue,0x01]);
  const digest=createHash('sha256').update(blob).digest();
  return [id(user),new Uint8Array(32).fill(umk),new Uint8Array(32).fill(ssk),
   new Uint8Array(64).fill(bindingSig),3n,blob,new Uint8Array(64).fill(listSig),
   new Uint8Array(32).fill(prevHash),new Uint8Array(digest)];
 };
 const snapshotPayload=encode([1n,id(10),id(1),snapshotAdmissionPayload,snapshotAdmissionSignature,
  100n,7n,9n,1n,0n,id(2),id(11),snapshotKey.publicKey,
  [id(2),id(3),id(4),id(5),id(6),id(7),id(8),id(9)],
  list(5,0x10,0x11,0x12,0x10,0x13,0x14),
  list(7,0x20,0x21,0x22,0x20,0x23,0x24)]);
 const snapshotSignature=await sign(snapshotKey.privateKey,
  concat(new TextEncoder().encode('dilla bot interaction context snapshot v1\0'),snapshotPayload));
 // Human current-group context proof: this matches currentInteractionContextWire and its
 // nested Go `,toarray` structs exactly. The embedded device lists are real signed lists so
 // the Rust verifier can validate the complete proof rather than only its outer signature.
 const currentSeed=new Uint8Array(32).fill(0x61);
 const currentKey=await keyFromSeed(currentSeed);
 const signedDeviceList=async (user:number,device:number,base:number) => {
  const umk=await keyFromSeed(new Uint8Array(32).fill(base));
  const ssk=await keyFromSeed(new Uint8Array(32).fill(base+1));
  const dsk=await keyFromSeed(new Uint8Array(32).fill(base+2));
  const userId=id(user),deviceId=id(device),zero32=new Uint8Array(32);
  const bindingSignature=await sign(umk.privateKey,
   concat(new TextEncoder().encode('dilla ssk v1'),ssk.publicKey));
  const unsigned=encode([1n,userId,1n,zero32,[[deviceId,dsk.publicKey,0n,1n,null]]]);
  const listSignature=await sign(ssk.privateKey,
   concat(new TextEncoder().encode('dilla devices v1'),unsigned));
  const blob=encode([1n,userId,1n,zero32,[[deviceId,dsk.publicKey,0n,1n,null]],listSignature]);
  return {tuple:[userId,umk.publicKey,ssk.publicKey,bindingSignature,1n,blob,listSignature,zero32,
   createHash('sha256').update(Buffer.from(blob)).digest()] as const,dskPub:dsk.publicKey};
 };
 const currentBotList=await signedDeviceList(5,6,0x71);
 const currentParticipantList=await signedDeviceList(7,8,0x74);
 const currentRegistration=[id(3),id(10),id(5),id(6),id(4),id(9),
  new Uint8Array(32).fill(0x81),2n,4n,1n,90n];
 const currentBinding=[id(2),id(3),id(4),id(5),id(7),id(8),9n,1n,0n];
 const currentPayload=encode([1n,id(2),id(11),currentKey.publicKey,100n,130n,
  currentRegistration,currentBinding,7n,2n,new Uint8Array(32).fill(0x82),
  [[0n,id(5),id(6),currentBotList.dskPub],[1n,id(7),id(8),currentParticipantList.dskPub]],
  12n,9n,1n,0n,currentBotList.tuple,currentParticipantList.tuple]);
 const currentSignature=await sign(currentKey.privateKey,
  concat(new TextEncoder().encode('dilla bot current interaction context v1\0'),currentPayload));
 const currentWire=encode([currentPayload,currentSignature]);
 const invalidSignature=currentSignature.slice();invalidSignature[0]^=1;
 const invalidSignatureWire=encode([currentPayload,invalidSignature]);
 const malformedPayload=new Uint8Array([...currentPayload,0x00]);
 const malformedSignature=await sign(currentKey.privateKey,
  concat(new TextEncoder().encode('dilla bot current interaction context v1\0'),malformedPayload));
 const malformedWire=encode([malformedPayload,malformedSignature]);
 return {version:1,cases,rejects,admission_proof:{
  seed:hex(admissionSeed),public_key:hex(admissionKey.publicKey),
  payload_hex:hex(admissionPayload),proof_hex:hex(admissionProof),
  server_now:100,minimum_eligibility_revision:11,not_after:130,
  ids:{admission:hex(id(1)),instance:hex(id(2)),installation:hex(id(3)),community:hex(id(4)),
   bot_user:hex(id(5)),bot_device:hex(id(6)),participant_user:hex(id(7)),
   participant_device:hex(id(8)),group:hex(id(9))},
 },interaction_context_snapshot:{seed:hex(snapshotSeed),public_key:hex(snapshotKey.publicKey),
  payload_hex:hex(snapshotPayload),signature_hex:hex(snapshotSignature)},
 current_human_interaction_context:{seed:hex(currentSeed),public_key:hex(currentKey.publicKey),
  payload_hex:hex(currentPayload),signature_hex:hex(currentSignature),wire_hex:hex(currentWire),
  expected:{instance:hex(id(2)),group:hex(id(9)),participant_user:hex(id(7)),
   participant_device:hex(id(8)),now:100},
  rejects:[{name:'current interaction context signature bit flipped',wire_hex:hex(invalidSignatureWire),error:'E_BOT_CONTEXT'},
   {name:'current interaction context signed payload has trailing CBOR',wire_hex:hex(malformedWire),error:'E_BOT_FORMAT'}]}};
}
function bodyOf(v:unknown):string {const raw=encode(v as never);let s='';for(const b of raw)s+=String.fromCharCode(b);return 'dilla.bot/1:'+btoa(s).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/u,'');}
