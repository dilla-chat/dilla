const { writeFile } = require('node:fs/promises');
const { join } = require('node:path');

// Never infer sandbox policy from a namespace probe. Chromium/Electron must
// either start with its sandbox intact or report the actual host configuration.
const APP_RUN_SCRIPT = `#!/bin/sh
set -eu
if [ -z "\${APPDIR:-}" ]; then
  APPDIR="$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd -P)"
fi
export APPDIR
export PATH="$APPDIR:$APPDIR/usr/sbin\${PATH:+:$PATH}"
export XDG_DATA_DIRS="$APPDIR/usr/share\${XDG_DATA_DIRS:+:$XDG_DATA_DIRS}:/usr/share/gnome:/usr/local/share:/usr/share"
export LD_LIBRARY_PATH="$APPDIR/usr/lib\${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export GSETTINGS_SCHEMA_DIR="$APPDIR/usr/share/glib-2.0/schemas\${GSETTINGS_SCHEMA_DIR:+:$GSETTINGS_SCHEMA_DIR}"
exec "$APPDIR/dilla-desktop" "$@"
`;

// AppImageTarget generates a staging AppRun, then copies appOutDir over it.
// afterPack runs before that copy, including for separately compiled fixtures.
async function installLinuxAppRun(context) {
 if (context.electronPlatformName !== 'linux') return;
 if (context.packager.executableName !== 'dilla-desktop') throw new Error('Unexpected Linux packaged executable');
 await writeFile(join(context.appOutDir, 'AppRun'), APP_RUN_SCRIPT, { mode: 0o755 });
}
module.exports = installLinuxAppRun;
module.exports.APP_RUN_SCRIPT = APP_RUN_SCRIPT;
