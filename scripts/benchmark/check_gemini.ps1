$ErrorActionPreference = 'Stop'
# Run from the Windows account that can access Docker Desktop.
# Never prints API keys or complete container environment variables.
foreach ($scopeName in @('Process','User','Machine')) {
  $value = [Environment]::GetEnvironmentVariable('GEMINI_API_KEY',$scopeName)
  Write-Output ("GEMINI_API_KEY in {0}: configured={1}" -f $scopeName,(-not [string]::IsNullOrWhiteSpace($value)))
}
$containerIds = & docker ps -a --filter 'label=com.docker.compose.service=orchestrator' --format '{{.ID}}'
if ($LASTEXITCODE -ne 0) { throw 'Docker access failed. Run this from your own PowerShell after Docker starts.' }
if (-not $containerIds) { Write-Output 'No orchestrator containers found.'; exit }
foreach ($containerId in $containerIds) {
  $rawEnvironment = & docker inspect --format '{{json .Config.Env}}' $containerId
  if ($LASTEXITCODE -ne 0) { throw 'Could not inspect container environment.' }
  $environmentLines = $rawEnvironment | ConvertFrom-Json
  $configured = $false
  $provider = 'unset'
  foreach ($entry in $environmentLines) {
    if ($entry.StartsWith('GEMINI_API_KEY=')) {
      $configured = -not [string]::IsNullOrWhiteSpace($entry.Substring('GEMINI_API_KEY='.Length))
    }
    if ($entry.StartsWith('LLM_MODE=')) { $provider = $entry.Substring('LLM_MODE='.Length) }
  }
  Write-Output ("Container {0}: provider={1}; GEMINI_API_KEY configured={2}" -f $containerId,$provider,$configured)
}
