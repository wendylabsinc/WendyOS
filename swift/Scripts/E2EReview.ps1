$ErrorActionPreference = 'Stop'

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$SwiftDir = Split-Path -Parent $ScriptDir
$DefaultPackageDir = Join-Path $SwiftDir 'WendyE2ETests'

function Resolve-E2EPath([string]$Path, [switch]$Existing) {
    $expanded = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path)
    if ($Existing -and -not (Test-Path -LiteralPath $expanded -PathType Container)) {
        throw "Directory does not exist: $expanded"
    }
    return (Resolve-Path -LiteralPath $expanded).Path
}

$RunDir = $null
$PackageDir = $DefaultPackageDir
$Provider = if ($env:WENDY_E2E_AI_PROVIDER) { $env:WENDY_E2E_AI_PROVIDER } else { 'auto' }
$Model = $env:WENDY_E2E_AI_MODEL
$Overwrite = $false
$ExtraArgs = @()

$i = 0
while ($i -lt $args.Count) {
    switch ($args[$i]) {
        '--run-dir' { $RunDir = $args[$i + 1]; $i += 2; continue }
        '--package-dir' { $PackageDir = $args[$i + 1]; $i += 2; continue }
        '--provider' { $Provider = $args[$i + 1]; $i += 2; continue }
        '--model' { $Model = $args[$i + 1]; $i += 2; continue }
        '--overwrite' { $Overwrite = $true; $i += 1; continue }
        '--help' { 'Usage: E2EReview.ps1 --run-dir DIR [OPTIONS]'; exit 0 }
        '-h' { 'Usage: E2EReview.ps1 --run-dir DIR [OPTIONS]'; exit 0 }
        default { $ExtraArgs += $args[$i]; $i += 1; continue }
    }
}

if (-not $RunDir) { throw 'ERROR: --run-dir is required.' }

$RunDir = Resolve-E2EPath $RunDir -Existing
$PackageDir = Resolve-E2EPath $PackageDir -Existing

$commandArgs = @('run', 'swift-e2e-testing', 'review', '--run-dir', $RunDir, '--provider', $Provider)
if ($Model) { $commandArgs += @('--model', $Model) }
if ($Overwrite) { $commandArgs += '--overwrite' }
$commandArgs += $ExtraArgs

Write-Output '==> Reviewing Swift E2E results'
Write-Output "    Package:  $PackageDir"
Write-Output "    Run dir:  $RunDir"
Write-Output "    Provider: $Provider"
if ($Model) { Write-Output "    Model:    $Model" }

Push-Location $PackageDir
try {
    & swift @commandArgs
    exit $LASTEXITCODE
} finally {
    Pop-Location
}
