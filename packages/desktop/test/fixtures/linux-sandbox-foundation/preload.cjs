const { ipcRenderer } = require('electron');
const value = name => process.argv.find(arg => arg.startsWith(name + '='))?.slice(name.length + 1);
const nonce = value('--foundation-nonce');
const behavior = value('--foundation-ack');
if (behavior !== 'silent') {
  ipcRenderer.send('foundation-ack', {
    nonce: behavior === 'wrong' ? 'wrong' : nonce,
    sandboxed: process.sandboxed,
    contextIsolated: process.contextIsolated,
  });
}
