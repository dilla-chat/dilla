'use strict';
// The reserved role can only enter the isolated, content-free probe. A direct
// invoker with its own pipes can use it but cannot request setup or app boot.
function parseProbeArguments(args) {
 const reserved=args.filter(x=>x.startsWith('--dilla-sandbox-'));
 if (!reserved.length) return null;
 if (args.length!==2 || reserved.length!==2) throw new Error('Invalid sandbox probe role');
 const probe=reserved.filter(x=>x.startsWith('--dilla-sandbox-probe='));
 const profile=reserved.filter(x=>x.startsWith('--dilla-sandbox-profile='));
 if(probe.length!==1||profile.length!==1)throw new Error('Invalid sandbox probe role');
 const nonce=probe[0].slice('--dilla-sandbox-probe='.length), dir=profile[0].slice('--dilla-sandbox-profile='.length);
 if(!/^[a-f0-9]{64}$/.test(nonce)||!/^\/tmp\/dilla-sandbox-probe-[a-zA-Z0-9]+$/.test(dir))throw new Error('Invalid sandbox probe role');
 return {nonce,profile:dir};
}
module.exports={parseProbeArguments};
