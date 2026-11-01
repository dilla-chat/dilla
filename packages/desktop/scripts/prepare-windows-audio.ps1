# CI only: a pinned, signed USB/IP transport plus a loopback USB Audio Class
# device. Windows supplies the audio class driver. No test signing, certificate
# installation, clock changes, reboot, or OS protection changes are permitted.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Start-Service AudioEndpointBuilder
Start-Service Audiosrv

function Get-AudioEndpoints {
  @(Get-PnpDevice -Class AudioEndpoint -PresentOnly -ErrorAction SilentlyContinue | Where-Object { $_.Status -eq 'OK' })
}
function Get-TrustedRoots {
  @(Get-ChildItem Cert:\LocalMachine\Root, Cert:\CurrentUser\Root | ForEach-Object { $_.Thumbprint } | Sort-Object -Unique)
}
function Get-PinnedFile([string]$Url, [string]$Path, [string]$Sha256) {
  Write-Output "Downloading pinned audio fixture: $Url"
  Invoke-WebRequest -Uri $Url -OutFile $Path
  if ((Get-FileHash -Algorithm SHA256 $Path).Hash -ne $Sha256) { throw "Audio fixture download hash mismatch: $Path" }
}

$endpoints = @(Get-AudioEndpoints)
if ($endpoints.Count -eq 0) {
  if ($env:GITHUB_ACTIONS -ne 'true' -or $env:RUNNER_OS -ne 'Windows' -or -not $env:RUNNER_TEMP) {
    throw 'Audio-device provisioning is restricted to the disposable Windows GitHub runner'
  }
  $work = Join-Path $env:RUNNER_TEMP 'dilla-windows-audio'
  New-Item -ItemType Directory -Path $work -Force | Out-Null
  function Invoke-AudioDeviceSetup([string]$File, [string[]]$Arguments, [string]$Label) {
    Write-Output "USB/IP device setup: $Label"
    $owned = Start-Process -FilePath $File -ArgumentList $Arguments -PassThru -NoNewWindow -RedirectStandardOutput (Join-Path $work "$Label.out") -RedirectStandardError (Join-Path $work "$Label.err")
    if (-not $owned.WaitForExit(60000)) {
      & taskkill /PID $owned.Id /T /F
      throw "USB/IP device setup timed out: $Label"
    }
    if ($owned.ExitCode -ne 0) { throw "USB/IP device setup failed: $Label ($($owned.ExitCode))" }
  }
  $installer = Join-Path $work 'USBip-0.9.7.5-x64-preview.exe'
  # This corrected release removes the development-root-certificate installation
  # present in earlier installers. It contains Microsoft-attested driver catalogs.
  # https://github.com/OSSign/vadimgrn--usbip-win2-old/releases/tag/0.9.7.5-preview
  Get-PinnedFile 'https://github.com/OSSign/vadimgrn--usbip-win2-old/releases/download/0.9.7.5-preview/USBip-0.9.7.5-x64-preview.exe' $installer '32d2c909740b2cd2e8ec3c72e8b9415c0d27a2c69b74697c82d6b5e3cd6dd664'
  $signature = Get-AuthenticodeSignature $installer
  Write-Output "USB/IP publisher verification: $($signature.Status) - $($signature.StatusMessage)"
  if ($signature.Status -ne 'Valid' -or -not $signature.TimeStamperCertificate -or
      $signature.SignerCertificate.Subject -notlike '*CN=Cloudyne Systems (Scheibling Consulting AB)*') {
    throw 'USB/IP installer does not have its expected valid, timestamped publisher signature'
  }
  $roots = Get-TrustedRoots
  # The installer's client component is FIXED: even /COMPONENTS=main runs
  # pnputil /install against unrelated USB root hubs and hangs the hosted VM.
  # Never execute it. Extract only the authenticated user-space files instead.
  # This pinned innoextract build supports the installer's Inno Setup 6.4.3.
  # https://github.com/UserUnknownFactor/innoextract_win/tree/643
  $extractorArchive = Join-Path $work 'innoextract643.zip'
  Get-PinnedFile 'https://github.com/UserUnknownFactor/innoextract_win/releases/download/643/innoextract643.zip' $extractorArchive 'a61219f066cd240f7dba9043e894087397dfbda0539fb1ec2e59c9e66938d019'
  $extractor = Join-Path $work 'innoextract'
  Expand-Archive -Path $extractorArchive -DestinationPath $extractor
  $unpacked = Join-Path $work 'USBip'
  Invoke-AudioDeviceSetup (Join-Path $extractor 'innoextract.exe') @('--extract', '--no-extract-unknown', '--include', 'app\usbip.exe', '--include', 'app\devnode.exe', '--include', 'app\libusbip.dll', '--include', 'app\resources.dll', '--output-dir', "`"$unpacked`"", "`"$installer`"") 'extract-tools'
  $transport = Join-Path $unpacked 'app'
  $toolHashes = @{
    'usbip.exe' = '27b2e198d20965f5ec4eaa06df37257dfa36a0965c5b87682de18333fca34b40'
    'devnode.exe' = 'a10dafab0e07ba8074213e48857a2db9a506032d8ef2dba4a4c74c9b0c1f922d'
    'libusbip.dll' = '59f91eebdf1585092f96ee79f044b78eff4a1d139fad3906f742a8b7f3cf34ee'
    'resources.dll' = 'af2960d0bcae2e090e1b2512526ad07148884b3752b07de1feb9d3306f2441a0'
  }
  foreach ($file in $toolHashes.Keys) {
    if ((Get-FileHash -Algorithm SHA256 (Join-Path $transport $file)).Hash -ne $toolHashes[$file]) {
      throw "Extracted USB/IP tool hash mismatch: $file"
    }
  }
  if (@(Compare-Object $roots (Get-TrustedRoots)).Count -ne 0) { throw 'USB/IP tool extraction changed a trusted certificate root store' }
  $drivers = Join-Path $work 'drivers'
  New-Item -ItemType Directory -Path $drivers -Force | Out-Null
  $files = @{
    'usbip2_filter.cat' = 'efdb52949e258b79fc60cf522898b67f390856210421fa9b293d89c3ab93eacf'
    'usbip2_filter.inf' = 'a3bd59c2d564dbf388ffd3c7a73fbb2fe2235a477fb1061d83a3fc69c25b5911'
    'usbip2_filter.sys' = 'b4e794626a067af8ba92ef608c049905ac21e312b18ed1da4338d4f155a3fb49'
    'usbip2_ude.cat' = '3587f744766f3a55845e3dc9d990730ce6e779c0d67594b748f351d51eb501b3'
    'usbip2_ude.inf' = 'ce84dbdb634e620bdd9b1c2117155b4b04efa1a357b2247b98049989f98ee9b2'
    'usbip2_ude.sys' = '434ab4426dd877f632b0afd4d5a365f2c41bdeb8fea1308fd0fe5042cf1de9ea'
  }
  foreach ($file in $files.Keys) {
    Get-PinnedFile "https://github.com/OSSign/vadimgrn--usbip-win2-old/releases/download/0.9.7.5-preview/$file" (Join-Path $drivers $file) $files[$file]
  }
  foreach ($name in @('usbip2_ude', 'usbip2_filter')) {
    $signed = Get-AuthenticodeSignature (Join-Path $drivers "$name.cat")
    if ($signed.Status -ne 'Valid' -or $signed.SignerCertificate.Subject -notlike '*CN=Microsoft Windows Hardware Compatibility Publisher*') {
      throw "Downloaded USB/IP kernel catalog lacks valid Microsoft attestation: $name"
    }
  }
  # Pre-stage only: no /install and no restart of unrelated root hubs. Windows
  # applies this signed extension when the new USB/IP controller enumerates.
  Invoke-AudioDeviceSetup "$env:windir\System32\pnputil.exe" @('/add-driver', "`"$drivers\usbip2_filter.inf`"") 'stage-filter'
  Invoke-AudioDeviceSetup (Join-Path $transport 'devnode.exe') @('install', "`"$drivers\usbip2_ude.inf`"", 'ROOT\USBIP_WIN2\UDE') 'create-controller'
  if (@(Compare-Object $roots (Get-TrustedRoots)).Count -ne 0) { throw 'USB/IP device setup changed a trusted certificate root store' }
  # Verify the installed kernel packages, including the Microsoft attestation.
  foreach ($name in @('usbip2_ude', 'usbip2_filter')) {
    $catalog = Get-ChildItem "$env:windir\System32\DriverStore\FileRepository\$name.inf_amd64_*\$name.cat" | Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if (-not $catalog) { throw "Missing installed USB/IP catalog: $name" }
    $signed = Get-AuthenticodeSignature $catalog.FullName
    if ($signed.Status -ne 'Valid' -or $signed.SignerCertificate.Subject -notlike '*CN=Microsoft Windows Hardware Compatibility Publisher*') {
      throw "USB/IP kernel catalog lacks valid Microsoft attestation: $name"
    }
    Write-Output "Microsoft-attested kernel catalog: $name"
  }

  $archive = Join-Path $work 'virtual-cables.zip'
  Get-PinnedFile 'https://codeload.github.com/tarekwasfy01/Virtual-Cables/zip/8a819c29971d30a73955178ac8a3aac00b1a5230' $archive 'e1b42e5fc523cdf907c3f969f93085f2554a195a793ffd0f554d845410dd1391'
  Expand-Archive -Path $archive -DestinationPath $work
  $source = Join-Path $work 'Virtual-Cables-8a819c29971d30a73955178ac8a3aac00b1a5230\src'
  & go -C $source test ./...
  if ($LASTEXITCODE -ne 0) { throw 'Pinned virtual USB audio implementation failed its tests' }
  $entry = Join-Path $source 'cmd\dilla-ci-audio'
  New-Item -ItemType Directory -Path $entry | Out-Null
  Copy-Item (Join-Path $PSScriptRoot '..\test\native\windows-usb-audio.go.txt') (Join-Path $entry 'main.go')
  $server = Join-Path $work 'dilla-ci-audio.exe'
  & go -C $source build -buildvcs=false -trimpath -o $server ./cmd/dilla-ci-audio
  if ($LASTEXITCODE -ne 0) { throw 'Virtual USB audio server build failed' }
  $process = Start-Process -FilePath $server -PassThru -WindowStyle Hidden -RedirectStandardOutput (Join-Path $work 'server.out') -RedirectStandardError (Join-Path $work 'server.err')
  $usbip = Join-Path $transport 'usbip.exe'
  $deadline = (Get-Date).AddSeconds(30)
  do {
    if ($process.HasExited) { throw 'Virtual USB audio server exited before attaching its device' }
    $devices = & $usbip list -r 127.0.0.1 2>&1
    $listed = $LASTEXITCODE -eq 0
    if (-not $listed) { Start-Sleep -Milliseconds 200 }
  } until ($listed -or (Get-Date) -ge $deadline)
  if (-not $listed) { throw 'Loopback USB audio server did not become ready' }
  Write-Output $devices
  & $usbip attach -r 127.0.0.1 -b '1-1'
  if ($LASTEXITCODE -ne 0) { throw 'Windows could not attach the virtual USB audio device' }
  $deadline = (Get-Date).AddSeconds(30)
  do {
    $endpoints = @(Get-AudioEndpoints)
    if ($endpoints.Count -eq 0) { Start-Sleep -Milliseconds 200 }
  } until ($endpoints.Count -gt 0 -or (Get-Date) -ge $deadline)
}
Write-Output "Working Windows audio endpoints: $($endpoints.Count)"
if ($endpoints.Count -eq 0) {
  throw 'The signed virtual USB audio device did not expose a working Windows audio endpoint'
}
$endpoints | Format-Table FriendlyName, Status
