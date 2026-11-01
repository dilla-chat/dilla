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
  $installer = Join-Path $work 'USBip-0.9.7.5-x64-preview.exe'
  # This corrected release removes the development-root-certificate installation
  # present in earlier installers. It contains Microsoft-attested driver catalogs.
  # https://github.com/OSSign/vadimgrn--usbip-win2-old/releases/tag/0.9.7.5-preview
  Get-PinnedFile 'https://github.com/OSSign/vadimgrn--usbip-win2-old/releases/download/0.9.7.5-preview/USBip-0.9.7.5-x64-preview.exe' $installer '32d2c909740b2cd2e8ec3c72e8b9415c0d27a2c69b74697c82d6b5e3cd6dd664'
  $signature = Get-AuthenticodeSignature $installer
  if ($signature.Status -ne 'Valid' -or -not $signature.TimeStamperCertificate -or
      $signature.SignerCertificate.Subject -notlike '*CN=Cloudyne Systems (Scheibling Consulting AB)*') {
    throw 'USB/IP installer does not have its expected valid, timestamped publisher signature'
  }
  $roots = Get-TrustedRoots
  $transport = Join-Path $work 'USBip'
  $setup = Start-Process -FilePath $installer -ArgumentList @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/SP-', '/COMPONENTS=main,client', "/DIR=`"$transport`"", "/LOG=`"$work\install.log`"") -PassThru -Wait
  if (@(Compare-Object $roots (Get-TrustedRoots)).Count -ne 0) { throw 'The USB/IP installer changed a trusted certificate root store' }
  if ($setup.ExitCode -ne 0) { throw "Signed USB/IP installation failed or requires a reboot: $($setup.ExitCode)" }
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
