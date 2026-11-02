// Bounded, read-only diagnostics for a COFF resource object rejected by Go.
// Symbol names are constrained to identifier characters so paths are never logged.
export function describeCoffResource(b) {
 const fail=error=>({error});
 if (!Buffer.isBuffer(b) || b.length<20 || b.length>16*1024*1024) return fail('invalid COFF header');
 const machine=b.readUInt16LE(0), count=b.readUInt16LE(2), symOffset=b.readUInt32LE(8), symCount=b.readUInt32LE(12), optional=b.readUInt16LE(16), sectionOffset=20+optional;
 if (!machine || optional || count>128 || sectionOffset+count*40>b.length) return fail('invalid COFF section table');
 if (symCount>200000 || symOffset+symCount*18+4>b.length) return fail('invalid COFF symbol table');
 const stringOffset=symOffset+symCount*18, stringSize=b.readUInt32LE(stringOffset);
 if (stringSize<4 || stringOffset+stringSize>b.length) return fail('invalid COFF string table');
 const safe=s=>/^[A-Za-z0-9_@.$?]{1,64}$/.test(s)?s:'[redacted]';
 const nameAt=(offset,length)=>safe(b.toString('ascii',offset,offset+length).split('\0',1)[0]);
 const sections=[], ranges=[];
 for(let i=0;i<count;i++) {
  const p=sectionOffset+i*40, at=b.readUInt32LE(p+24), n=b.readUInt16LE(p+32);
  if (at+n*10>b.length || n===0xffff) return fail('invalid or unsupported COFF relocations');
  sections.push({name:nameAt(p,8),relocations:n});ranges.push([at,n]);
 }
 const negativeSymbols=[], indexMap=new Map(); let truncated=false;
 for(let i=0;i<symCount;) {
  const p=symOffset+i*18, section=b.readInt16LE(p+12), aux=b[p+17];
  if(i+aux>=symCount) return fail('invalid COFF auxiliary symbols');
  if(section<0) {
   let name;
   if(b.readUInt32LE(p)===0) {
    const off=b.readUInt32LE(p+4);
    if(off<4 || off>=stringSize) return fail('invalid COFF symbol name');
    const end=b.indexOf(0,stringOffset+off);
    if(end<0 || end>=stringOffset+stringSize) return fail('invalid COFF symbol name');
    name=nameAt(stringOffset+off,Math.min(end-stringOffset-off,65));
   } else name=nameAt(p,8);
   if(negativeSymbols.length<16) { const entry={index:i,name,section,storageClass:b[p+16],relocationReferences:0};negativeSymbols.push(entry);indexMap.set(i,entry); }
   else truncated=true;
  }
  i+=aux+1;
 }
 for(const [at,n] of ranges) for(let i=0;i<n;i++) {const entry=indexMap.get(b.readUInt32LE(at+i*10+4));if(entry)entry.relocationReferences++;}
 return {machine:'0x'+machine.toString(16).padStart(4,'0'),sections,negativeSymbols,truncated};
}
