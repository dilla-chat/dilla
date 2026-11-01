# Hosted Windows images normally have no sound endpoint. Keep the gate honest:
# provision a normally signed output device on the runner before audio tests.
# Do not change signing policy, clocks, Secure Boot or certificate root stores.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Start-Service AudioEndpointBuilder
Start-Service Audiosrv
$endpoints = @(Get-PnpDevice -Class AudioEndpoint -PresentOnly -ErrorAction SilentlyContinue | Where-Object { $_.Status -eq 'OK' })
Write-Output "Working Windows audio endpoints: $($endpoints.Count)"
if ($endpoints.Count -eq 0) {
  throw 'Real WASAPI acceptance requires an audio-capable runner or a normally signed virtual output device'
}
