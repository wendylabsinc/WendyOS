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
$i = 0
while ($i -lt $args.Count) {
    switch ($args[$i]) {
        '--run-dir' { $RunDir = $args[$i + 1]; $i += 2; continue }
        '--package-dir' { $PackageDir = $args[$i + 1]; $i += 2; continue }
        '--help' { 'Usage: E2EReport.ps1 --run-dir DIR [--package-dir DIR]'; exit 0 }
        '-h' { 'Usage: E2EReport.ps1 --run-dir DIR [--package-dir DIR]'; exit 0 }
        default { throw "Unknown option: $($args[$i])" }
    }
}

if (-not $RunDir) { throw 'ERROR: --run-dir is required.' }

$script:RunDir = Resolve-E2EPath $RunDir -Existing
$PackageDir = Resolve-E2EPath $PackageDir -Existing
$ReportPath = Join-Path $script:RunDir 'index.html'

Write-Output '==> Rendering Swift E2E aggregate HTML report'
Write-Output "    Package: $PackageDir"
Write-Output "    Run dir: $script:RunDir"
Write-Output "    Output:  $ReportPath"

$resultPaths = @()
Get-ChildItem -LiteralPath $script:RunDir -Directory | ForEach-Object {
    $suiteDir = $_.FullName
    Get-ChildItem -LiteralPath $suiteDir -Directory | ForEach-Object {
        $testDir = $_.FullName
        Get-ChildItem -LiteralPath $testDir -Directory | ForEach-Object {
            $targetDir = $_.FullName
            Get-ChildItem -LiteralPath $targetDir -Directory | ForEach-Object {
                $candidate = Join-Path $_.FullName 'test-results.xml'
                if (Test-Path -LiteralPath $candidate -PathType Leaf) {
                    $resultPaths += $candidate
                }
            }
        }
    }
}
$resultPaths | Sort-Object | ForEach-Object {
    & (Join-Path $ScriptDir 'E2ESanitizeXUnit.ps1') --file $_
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

Push-Location $PackageDir
try {
    & swift run swift-e2e-testing report --run-dir $script:RunDir
    $reportStatus = $LASTEXITCODE
} finally {
    Pop-Location
}

if ($reportStatus -eq 0 -and (Test-Path -LiteralPath $ReportPath)) {
    Write-Output "==> Wrote Swift E2E aggregate HTML report: $ReportPath"
    exit 0
}

$failureStatus = if ($reportStatus -eq 0) { 1 } else { $reportStatus }
Write-Error 'ERROR: Swift E2E aggregate HTML report generation failed.'
exit $failureStatus
