$ErrorActionPreference = 'Stop'

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$SwiftDir = Split-Path -Parent $ScriptDir
$PackageDir = Join-Path $SwiftDir 'WendyE2ETests'
$RepoDir = Split-Path -Parent $SwiftDir
$DefaultOutputDir = if ($env:WENDY_E2E_OUTPUT_DIR) {
    $env:WENDY_E2E_OUTPUT_DIR
} else {
    Join-Path (Join-Path $RepoDir 'Build') 'e2e'
}

$OutputDir = $DefaultOutputDir
$OpenReport = $true
$Stage = 'all'
$RunPrefix = $env:WENDY_E2E_ANALYZE_RUN_ID
$Diff = $null

function Show-Usage {
    @"
Usage: E2EAnalyze.ps1 [--output-dir DIR] [--run-id ID] [--stage STAGE] [--diff RANGE] [--open|--no-open]

Analyze Swift E2E attempts found in an output directory.

Stages:
  aggregate  Aggregate attempts into matching run directories.
  review     Review existing run results.
  report     Render existing run HTML reports.
  all        Aggregate attempts, review runs, and render reports; default.
"@ | Write-Output
}

$i = 0
while ($i -lt $args.Count) {
    switch ($args[$i]) {
        '--output-dir' { $OutputDir = $args[$i + 1]; $i += 2; continue }
        '--run-id' { $RunPrefix = $args[$i + 1]; $i += 2; continue }
        '--stage' { $Stage = $args[$i + 1]; $i += 2; continue }
        '--diff' { $Diff = $args[$i + 1]; $i += 2; continue }
        '--open' { $OpenReport = $true; $i += 1; continue }
        '--no-open' { $OpenReport = $false; $i += 1; continue }
        '--help' { Show-Usage; exit 0 }
        '-h' { Show-Usage; exit 0 }
        default { throw "Unknown option: $($args[$i])" }
    }
}

$Stage = $Stage.ToLowerInvariant()
if ($Stage -notin @('aggregate', 'review', 'report', 'all')) {
    throw 'ERROR: --stage must be aggregate, review, report, or all.'
}

function Get-DefaultRunPrefix {
    if ($env:WENDY_E2E_RUN_ID) {
        $withoutAttempt = $env:WENDY_E2E_RUN_ID -replace '\.[^.]+$', ''
        return $withoutAttempt -replace '\.[^.]+$', ''
    }
    if ($env:GITHUB_RUN_ID) {
        return "swift-e2e-tests.gh$env:GITHUB_RUN_ID"
    }
    return 'swift-e2e-tests.local0000'
}

$OutputDir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($OutputDir)
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
$OutputDir = (Resolve-Path -LiteralPath $OutputDir).Path
if (-not $RunPrefix) { $RunPrefix = Get-DefaultRunPrefix }
$RunPrefix = $RunPrefix.TrimEnd('.')

function Test-AttemptDirectory([System.IO.DirectoryInfo]$Directory) {
    if (-not $Directory.Name.StartsWith("$RunPrefix.")) { return $false }
    if ($Directory.Name -notmatch '\.\d{4}$') { return $false }
    $attemptPath = Join-Path $Directory.FullName 'attempt.json'
    return Test-Path -LiteralPath $attemptPath -PathType Leaf
}

function Test-RunDirectory([System.IO.DirectoryInfo]$Directory) {
    $attemptPath = Join-Path $Directory.FullName 'attempt.json'
    return -not (Test-Path -LiteralPath $attemptPath -PathType Leaf)
}

function Get-RunDirectoryForAttempt([string]$RunID) {
    $runBase = $RunID -replace '\.[^.]+$', ''
    $runName = $runBase -replace '\.[^.]+$', ''
    return Join-Path $OutputDir $runName
}

$attemptDirs = Get-ChildItem -LiteralPath $OutputDir -Directory |
    Where-Object { Test-AttemptDirectory $_ } |
    Sort-Object Name

if ($attemptDirs) {
    $runDirs = $attemptDirs |
        ForEach-Object { Get-RunDirectoryForAttempt $_.Name } |
        Sort-Object -Unique
} else {
    $runDirs = Get-ChildItem -LiteralPath $OutputDir -Directory |
        Where-Object { $_.Name -eq $RunPrefix } |
        Where-Object { Test-RunDirectory $_ } |
        Sort-Object Name |
        ForEach-Object { $_.FullName }
}

if ($Stage -in @('aggregate', 'all') -and -not $attemptDirs) {
    throw "ERROR: no Swift E2E attempt directories found in $OutputDir."
}
if ($Stage -in @('review', 'report') -and -not $runDirs) {
    throw "ERROR: no Swift E2E run directories found in $OutputDir."
}

Write-Output '==> Analyzing Swift E2E runs'
Write-Output "    Stage:      $Stage"
Write-Output "    Run ID:     $RunPrefix"
Write-Output "    Output dir: $OutputDir"
if ($Diff) { Write-Output "    Diff:       $Diff" }
$attemptDirs | ForEach-Object { Write-Output "    Attempt:    $($_.FullName)" }
$runDirs | ForEach-Object { Write-Output "    Run:        $_" }

$status = 0
$failedReviews = @{}

if ($Stage -in @('aggregate', 'all')) {
    Push-Location $PackageDir
    try {
        & swift run swift-e2e-testing aggregate --output-dir $OutputDir --package-dir $PackageDir @($attemptDirs | ForEach-Object { $_.FullName })
        $aggregateStatus = $LASTEXITCODE
    } finally {
        Pop-Location
    }
    if ($aggregateStatus -ne 0) { $status = $aggregateStatus }
}

if ($Stage -in @('review', 'all')) {
    $reviewArgs = @()
    if ($Diff) { $reviewArgs += @('--diff', $Diff) }
    foreach ($runDir in $runDirs) {
        & (Join-Path $ScriptDir 'E2EReview.ps1') --run-dir $runDir @reviewArgs
        $reviewStatus = $LASTEXITCODE
        if ($reviewStatus -ne 0) { $failedReviews[$runDir] = $true }
        if ($status -eq 0 -and $reviewStatus -ne 0) { $status = $reviewStatus }
    }
}

if ($Stage -in @('report', 'all')) {
    foreach ($runDir in $runDirs) {
        $inheritedOutcome = $env:WENDY_E2E_REVIEW_OUTCOME
        # Otherwise the report reads a review that never finished as a clean result.
        if ($failedReviews[$runDir]) { $env:WENDY_E2E_REVIEW_OUTCOME = 'failure' }
        & (Join-Path $ScriptDir 'E2EReport.ps1') --run-dir $runDir
        $reportStatus = $LASTEXITCODE
        $env:WENDY_E2E_REVIEW_OUTCOME = $inheritedOutcome
        if ($status -eq 0 -and $reportStatus -ne 0) { $status = $reportStatus }
    }

    $latestReport = $runDirs |
        ForEach-Object { Join-Path $_ 'index.html' } |
        Where-Object { Test-Path -LiteralPath $_ -PathType Leaf } |
        Sort-Object { (Get-Item -LiteralPath $_).LastWriteTimeUtc } |
        Select-Object -Last 1

    if ($latestReport) {
        if ($OpenReport) {
            Start-Process $latestReport
        } else {
            Write-Output "HTML report: $latestReport"
        }
    } else {
        Write-Error 'HTML report not found in analyzed run directories.'
        if ($status -eq 0) { $status = 1 }
    }
}

exit $status
