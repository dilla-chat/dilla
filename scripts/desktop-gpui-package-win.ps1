# The Windows installer of the native Dilla desktop app (GPUI release plan, Task 13, decision D10), from one release binary:
#   dilla-<v>-win-x64-setup.exe   Inno Setup 6 (clients/desktop/packaging/windows/dilla.iss): per user, silent-capable
# and nothing else: the out-dir must be empty, and the script fails if the setup is missing at the end.
#
# Order: check the binary is a release build with no test-only code, that its CRT is linked statically (no
# VCRUNTIME140.dll in `dumpbin /dependents`; build with RUSTFLAGS="-C target-feature=+crt-static") and that --version
# names the release, stage the binary, the icon and the licence bundle, then run iscc.
#
# Usage: desktop-gpui-package-win.ps1 -Version <version> -Binary <dilla-desktop.exe> -OutDir <dir>
# Environment:
#   DILLA_WEBRTC_LICENSE      libwebrtc's LICENSE.md (default: $env:LK_CUSTOM_WEBRTC\LICENSE.md, from fetch-libwebrtc.sh)
#   DILLA_ISCC                the Inno Setup 6 compiler (default: iscc on PATH, then Inno Setup 6's install folder)
#   DILLA_DUMPBIN             dumpbin.exe (default: found through vswhere)
#   DILLA_SIGNTOOL_COMMAND    the disabled signtool hook: when set, iscc signs setup and uninstaller with this command
#                             (`signtool sign /fd sha256 ... $f`); nothing sets it for the alpha (D10, signing is decided before beta)
[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$Version,
  [Parameter(Mandatory = $true)][string]$Binary,
  [Parameter(Mandatory = $true)][string]$OutDir
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Fail([string]$Message) { throw "desktop-gpui-package-win: $Message" }
function Invoke-Native([string]$What, [scriptblock]$Command) {
  & $Command
  if ($LASTEXITCODE -ne 0) { Fail "$What failed (exit $LASTEXITCODE)" }
}

$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$iss = Join-Path $root 'clients/desktop/packaging/windows/dilla.iss'

# D1: only X.Y.Z-alpha.N releases. NumericVersion is the x.x.x.x form file properties need: 0.2.0-alpha.1 -> 0.2.0.1.
if ($Version -notmatch '^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)-alpha\.(0|[1-9]\d*)$') { Fail "unsupported release version: $Version" }
$numericVersion = "$($Matches[1]).$($Matches[2]).$($Matches[3]).$($Matches[4])"
if (-not (Test-Path -LiteralPath $Binary -PathType Leaf)) { Fail "$Binary does not exist" }
$Binary = (Resolve-Path -LiteralPath $Binary).Path

$webrtcLicense = $env:DILLA_WEBRTC_LICENSE
if (-not $webrtcLicense -and $env:LK_CUSTOM_WEBRTC) { $webrtcLicense = Join-Path $env:LK_CUSTOM_WEBRTC 'LICENSE.md' }
if (-not $webrtcLicense -or -not (Test-Path -LiteralPath $webrtcLicense -PathType Leaf)) {
  Fail "libwebrtc's LICENSE.md not found (set DILLA_WEBRTC_LICENSE or LK_CUSTOM_WEBRTC)"
}

# iscc: Inno Setup 6 (preinstalled on windows-latest). The install folder comes first: the `iscc` on PATH there is a
# Chocolatey shim, whose own file version is the shim's (0.0.0.0), not Inno Setup's.
$iscc = $env:DILLA_ISCC
if (-not $iscc) {
  foreach ($base in @(${env:ProgramFiles(x86)}, $env:ProgramFiles, "$env:LOCALAPPDATA\Programs")) {
    if (-not $base) { continue }
    $candidate = Join-Path $base 'Inno Setup 6/ISCC.exe'
    if (Test-Path -LiteralPath $candidate) { $iscc = $candidate; break }
  }
}
if (-not $iscc) {
  $found = Get-Command iscc -ErrorAction SilentlyContinue
  if ($found) { $iscc = $found.Source }
}
if (-not $iscc) { Fail 'iscc (Inno Setup 6) not found (set DILLA_ISCC)' }
# The version from ISCC's own banner ("Inno Setup 6.x.y Command-Line Compiler"): on windows-latest the file's version
# resource reads 0.0.0.0, both for the Chocolatey shim and for ISCC.exe in its install folder.
$banner = (& $iscc '/?' 2>&1 | Out-String)
if ($banner -match 'Inno Setup (\d+(\.\d+)*)') { $isccVersion = $Matches[1] } else { $isccVersion = 'unknown' }
if ($isccVersion -notmatch '^6(\.|$)') { Fail "Inno Setup 6 is required, found $isccVersion ($iscc); its banner: $($banner.Trim())" }

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$OutDir = (Resolve-Path -LiteralPath $OutDir).Path
if (@(Get-ChildItem -Force -LiteralPath $OutDir).Count -ne 0) { Fail "$OutDir is not empty" }
$setupName = "dilla-$Version-win-x64-setup.exe"

# 1: no test-only code (the same byte check the Linux packages run), through the bash Git for Windows ships.
$bash = Get-Command bash -ErrorAction SilentlyContinue
if (-not $bash) { Fail 'bash (Git for Windows) not found; scripts/check-desktop-release-binary.sh needs it' }
Invoke-Native 'check-desktop-release-binary.sh' { & $bash.Source (Join-Path $root 'scripts/check-desktop-release-binary.sh') ($Binary -replace '\\', '/') }

# 2: the static CRT. LiveKit's libwebrtc links /MT, so a dynamic-CRT build would not link at all; the check is that the
# result needs no Visual C++ redistributable on a clean machine.
$dumpbin = $env:DILLA_DUMPBIN
if (-not $dumpbin) {
  $vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio/Installer/vswhere.exe'
  if (-not (Test-Path -LiteralPath $vswhere)) { Fail 'vswhere not found; set DILLA_DUMPBIN to dumpbin.exe' }
  $dumpbin = & $vswhere -latest -products '*' -find 'VC/Tools/MSVC/**/bin/Hostx64/x64/dumpbin.exe' | Select-Object -First 1
}
if (-not $dumpbin -or -not (Test-Path -LiteralPath $dumpbin)) { Fail 'dumpbin.exe not found' }
$dependents = & $dumpbin /dependents $Binary
if ($LASTEXITCODE -ne 0) { Fail "dumpbin /dependents failed (exit $LASTEXITCODE)" }
$dependents | ForEach-Object { Write-Host $_ }
if ($dependents -match '(?i)VCRUNTIME|MSVCP|api-ms-win-crt') { Fail "$Binary depends on the dynamic C runtime; build with RUSTFLAGS='-C target-feature=+crt-static'" }

# 3: --version names this release ("Dilla <version> (<commit>)").
$reported = (& $Binary --version) -join "`n"
if ($LASTEXITCODE -ne 0) { Fail "$Binary --version failed (exit $LASTEXITCODE)" }
if ($reported -notmatch "^Dilla $([regex]::Escape($Version)) ") { Fail "--version says '$reported', not Dilla $Version" }

# 4: the stage folder iscc packs: the binary, the icon, the licence bundle.
$stage = Join-Path ([System.IO.Path]::GetTempPath()) ("dilla-win-stage-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $stage | Out-Null
try {
  Copy-Item -LiteralPath $Binary -Destination (Join-Path $stage 'dilla-desktop.exe')
  Copy-Item -LiteralPath (Join-Path $root 'clients/desktop/assets/brand/dilla.ico') -Destination (Join-Path $stage 'dilla.ico')
  $licenses = Join-Path $stage 'licenses'
  Invoke-Native 'desktop-third-party.mjs' { node (Join-Path $root 'scripts/desktop-third-party.mjs') $licenses --webrtc-license $webrtcLicense --target x86_64-pc-windows-msvc }

  # 5: the installer.
  $isccArgs = @("/DAppVersion=$Version", "/DNumericVersion=$numericVersion", "/DStageDir=$stage", "/DOutDir=$OutDir", '/Qp')
  if ($env:DILLA_SIGNTOOL_COMMAND) { $isccArgs += @('/DWithSigning', "/Sdilla_sign=$($env:DILLA_SIGNTOOL_COMMAND)") }
  Invoke-Native 'iscc' { & $iscc @isccArgs $iss }
} finally {
  Remove-Item -Recurse -Force -LiteralPath $stage -ErrorAction SilentlyContinue
}

$setup = Join-Path $OutDir $setupName
if (-not (Test-Path -LiteralPath $setup -PathType Leaf)) { Fail "$setupName was not built" }
$extra = @(Get-ChildItem -Force -LiteralPath $OutDir | Where-Object { $_.Name -ne $setupName })
if ($extra.Count -ne 0) { Fail "unexpected files in ${OutDir}: $($extra.Name -join ', ')" }
Write-Host "desktop-gpui-package-win: $setup ($((Get-Item -LiteralPath $setup).Length) bytes)"
