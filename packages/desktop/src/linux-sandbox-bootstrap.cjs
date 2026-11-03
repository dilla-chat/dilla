'use strict';
// Future AppImage packaging selects this as the entry point. The ordinary
// bundle is loaded only after rejecting malformed reserved probe arguments.
const { parseProbeArguments } = require('./linux-sandbox-probe-arguments.cjs');
try {
 const role=parseProbeArguments(process.argv.slice(1));
 if(role) require('./linux-sandbox-probe-main.cjs').start(role);
 else require('./main-app.cjs');
} catch {
 require('electron').app.exit(1);
}
