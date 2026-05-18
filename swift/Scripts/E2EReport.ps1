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

function Update-ReadmeBlock([string]$ReadmePath, [string]$Status, [string]$ReportPath) {
    $start = '<!-- swift-e2e-report:start -->'
    $end = '<!-- swift-e2e-report:end -->'
    $kept = @()
    $skipping = $false

    if (Test-Path -LiteralPath $ReadmePath) {
        foreach ($line in Get-Content -LiteralPath $ReadmePath) {
            if ($line -eq $start) { $skipping = $true; continue }
            if ($line -eq $end) { $skipping = $false; continue }
            if (-not $skipping) { $kept += $line }
        }
    }

    $files = Get-ChildItem -LiteralPath $script:RunDir -Recurse -File | Sort-Object FullName | ForEach-Object { '- ' + $_.FullName.Substring($script:RunDir.Length).TrimStart('\', '/') }
    $block = @(
        $start,
        '## Report Rendering',
        '',
        "- Status: ``$Status``",
        "- HTML report: ``$ReportPath``"
    )
    $reviewPath = Join-Path $script:RunDir 'ai-review.md'
    if (Test-Path -LiteralPath $reviewPath) { $block += "- AI review: ``$reviewPath``" }
    $block += @('', '### Files after report rendering') + $files + $end

    $content = @($kept)
    if ($content.Count -gt 0 -and $content[-1] -ne '') { $content += '' }
    $content += $block
    $content | Set-Content -LiteralPath $ReadmePath -Encoding UTF8
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
$ReadmePath = Join-Path $script:RunDir 'README.md'
$ReportPath = Join-Path $script:RunDir 'report.html'

Write-Output '==> Rendering Swift E2E HTML report'
Write-Output "    Package: $PackageDir"
Write-Output "    Run dir: $script:RunDir"
Write-Output "    Output:  $ReportPath"

Push-Location $PackageDir
try {
    & swift run swift-e2e-testing report --run-dir $script:RunDir
    $reportStatus = $LASTEXITCODE
} finally {
    Pop-Location
}

if ($reportStatus -eq 0 -and (Test-Path -LiteralPath $ReportPath)) {
    Update-ReadmeBlock $ReadmePath 'generated' $ReportPath
    Write-Output "==> Wrote Swift E2E HTML report: $ReportPath"
    exit 0
}

$failureStatus = if ($reportStatus -eq 0) { 1 } else { $reportStatus }
Update-ReadmeBlock $ReadmePath "failed (exit $failureStatus)" $ReportPath
Write-Error 'ERROR: Swift E2E HTML report generation failed.'
exit $failureStatus
