// Microsoft cvtres emits an unreferenced absolute @comp.id compiler marker.
// Go's PE linker rejects that marker. Remove only this known marker, preserving
// all sections and all remaining symbols and relocation targets. Fail closed on
// COFF forms whose symbol references this small normalizer cannot account for.
export function normalizeWindowsCoffResource(input) {
 const fail=()=>{throw new Error('Unsupported Windows launcher COFF resource');};
 if(!Buffer.isBuffer(input) || input.length<20 || input.length>16*1024*1024) fail();
 const sections=input.readUInt16LE(2), sym=input.readUInt32LE(8), count=input.readUInt32LE(12), sectionEnd=20+sections*40;
 if(input.readUInt16LE(0)!==0x8664 || input.readUInt16LE(16)!==0 || sections<1 || sections>128 || sectionEnd>input.length || !sym || count>200000 || sym<sectionEnd || sym+count*18+4>input.length) fail();
 const strings=sym+count*18, stringSize=input.readUInt32LE(strings);
 if(strings+Math.max(4,stringSize)!==input.length) fail();
 const ranges=[], relocs=[];
 const addRange=(start,length)=>{if(!length)return;if(start<sectionEnd || start+length>sym)fail();ranges.push([start,start+length]);};
 for(let i=0;i<sections;i++) {
  const p=20+i*40, size=input.readUInt32LE(p+16), raw=input.readUInt32LE(p+20), rel=input.readUInt32LE(p+24), n=input.readUInt16LE(p+32);
  if(n===0xffff || (input.readUInt32LE(p+36)&0x01000000)!==0 || input.readUInt16LE(p+34)!==0 || input.readUInt32LE(p+28)!==0) fail();
  addRange(raw,size);addRange(rel,n*10);
  for(let j=0;j<n;j++) {const at=rel+j*10;if(input.readUInt32LE(at)>=size)fail();relocs.push(at+4);}
 }
 ranges.sort((a,b)=>a[0]-b[0]);for(let i=1;i<ranges.length;i++)if(ranges[i][0]<ranges[i-1][1])fail();
 let marker=-1;const auxiliary=new Set();
 for(let i=0;i<count;) {
  const p=sym+i*18, section=input.readInt16LE(p+12), type=input.readUInt16LE(p+14), storage=input[p+16], aux=input[p+17], name=input.subarray(p,p+8);
  if(i+aux>=count || section>sections) fail();
  if(section<0) {
   if(section!==-1 || storage!==3 || type!==0 || aux!==0) fail();
   if(name.equals(Buffer.from('@comp.id','ascii'))) {if(marker!==-1)fail();marker=i;}
   else if(!name.equals(Buffer.from('@feat.00','ascii'))) fail();
  }
  if(aux) {
   if(aux!==1 || section<1 || storage!==3 || type!==0 || !input.subarray(p,p+8).equals(input.subarray(20+(section-1)*40,28+(section-1)*40))) fail();
   auxiliary.add(i+1);
  }
  i+=aux+1;
 }
 for(const at of relocs) {const index=input.readUInt32LE(at);if(index>=count || index===marker || auxiliary.has(index))fail();}
 if(marker<0) return Buffer.from(input);
 const position=sym+marker*18, result=Buffer.concat([input.subarray(0,position),input.subarray(position+18)]);
 result.writeUInt32LE(count-1,12);
 for(const at of relocs) {const index=input.readUInt32LE(at);if(index>marker)result.writeUInt32LE(index-1,at);}
 return result;
}
