'use strict';
const {ipcRenderer}=require('electron');
const values=process.argv.filter(arg=>arg.startsWith('--dilla-sandbox-nonce='));
if(values.length===1){const nonce=values[0].slice('--dilla-sandbox-nonce='.length);if(/^[a-f0-9]{64}$/.test(nonce))ipcRenderer.send('dilla-sandbox-ack',{nonce,sandboxed:process.sandboxed,contextIsolated:process.contextIsolated});}
