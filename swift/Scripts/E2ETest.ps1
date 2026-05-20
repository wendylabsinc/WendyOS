$ErrorActionPreference = 'Stop'

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$SwiftDir = Split-Path -Parent $ScriptDir
$PackageDir = Join-Path $SwiftDir 'WendyE2ETests'

function Show-Usage {
    @"
Usage: E2ETest.ps1 [OPTIONS]

Run the WendyAgent Swift E2E tests and write generated files to an E2E run directory.

Options match Scripts/E2ETest.sh.
"@ | Write-Output
}

function Get-ValueOption([string]$Name, [int]$Index) {
    if ($Index + 1 -ge $script:RemainingArgs.Count) {
        throw "ERROR: $Name requires a value."
    }
    return $script:RemainingArgs[$Index + 1]
}

function ConvertTo-Bool([string]$Name, [string]$Value) {
    switch ($Value.ToLowerInvariant()) {
        { $_ -in @('true', '1', 'yes', 'on', 'enabled') } { return $true }
        { $_ -in @('false', '0', 'no', 'off', 'disabled') } { return $false }
        default { throw "ERROR: $Name must be true or false." }
    }
}

function ConvertTo-Isolation([string]$Value) {
    $normalized = $Value.ToLowerInvariant()
    if ($normalized -in @('none', 'per-run', 'per-test')) { return $normalized }
    throw 'ERROR: WENDY_E2E_ISOLATION must be none, per-run, or per-test.'
}

function New-DefaultRunID([string]$OutputDirectory, [string]$RunName) {
    if (-not $RunName) { $RunName = 'local' }
    if ($env:GITHUB_RUN_ID) {
        $attemptValue = if ($env:GITHUB_RUN_ATTEMPT) { [int]$env:GITHUB_RUN_ATTEMPT } else { 1 }
        return 'swift-e2e-tests.gh{0}.{1}.{2:D4}' -f $env:GITHUB_RUN_ID, $RunName, $attemptValue
    }

    $evaluationID = 'local' + (Get-Date).ToUniversalTime().ToString('yyMMdd')
    $base = "swift-e2e-tests.$evaluationID.$RunName"
    $maxAttempt = 0
    if ($OutputDirectory -and (Test-Path -LiteralPath $OutputDirectory -PathType Container)) {
        Get-ChildItem -LiteralPath $OutputDirectory -Directory -Filter "$base.????" | ForEach-Object {
            $suffix = $_.Name.Substring($_.Name.Length - 4)
            $attempt = 0
            if ([int]::TryParse($suffix, [ref]$attempt) -and $attempt -gt $maxAttempt) {
                $maxAttempt = $attempt
            }
        }
    }
    return '{0}.{1:D4}' -f $base, ($maxAttempt + 1)
}

function ConvertTo-SafeRunID([string]$Value) {
    $safe = $Value -replace '[^A-Za-z0-9._-]', '-'
    while ($safe.Contains('--')) { $safe = $safe.Replace('--', '-') }
    return $safe.Trim('-')
}

function Resolve-E2EPath([string]$Path, [switch]$Create, [switch]$Existing) {
    if ([string]::IsNullOrWhiteSpace($Path)) { return $null }
    $expanded = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path)
    if ($Create) { New-Item -ItemType Directory -Force -Path $expanded | Out-Null }
    if ($Existing -and -not (Test-Path -LiteralPath $expanded -PathType Container)) {
        throw "Directory does not exist: $expanded"
    }
    return (Resolve-Path -LiteralPath $expanded).Path
}

function ConvertTo-RemotePath([string]$Path) {
    if ([string]::IsNullOrWhiteSpace($Path)) { return $Path }
    if ($Path -eq '~') { return '$HOME' }
    if ($Path.StartsWith('~/')) { return '$HOME/' + $Path.Substring(2) }
    return $Path
}

function Get-SSHHost([string]$User, [string]$Address) {
    $hostName = $Address
    if ($hostName.Contains(':')) { $hostName = "[$hostName]" }
    if ($User) { return "$User@$hostName" }
    return $hostName
}

function Invoke-CLICommand([string]$Command) {
    if ($script:CLIAddress) {
        $target = Get-SSHHost $script:CLIUser $script:CLIAddress
        $quoted = $Command.Replace("'", "'\''")
        return & ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -T $target "bash -lc '$quoted'"
    }

    return & powershell -NoProfile -NonInteractive -Command $Command
}

function Build-CLI {
    $exeName = if ($env:OS -eq 'Windows_NT') { 'wendy.exe' } else { 'wendy' }
    $wendyPath = Join-Path $script:CLIBinDir $exeName
    $script:WendyCLIPath = $wendyPath

    Write-Output '==> Building wendy CLI'
    Write-Output "    Target: $(if ($script:CLIAddress) { "${script:CLIUser}@${script:CLIAddress}" } else { '<local>' })"
    Write-Output "    Output: $wendyPath"

    if ($script:CLIAddress) {
        throw 'ERROR: Remote CLI builds from Windows are not supported yet. Run the Windows E2E harness with a local CLI and remote agent.'
    }

    New-Item -ItemType Directory -Force -Path $script:CLIBinDir | Out-Null

    Push-Location (Join-Path $script:CLIRepoDir 'go')
    try {
        & go build -o $wendyPath ./cmd/wendy
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    } finally {
        Pop-Location
    }

    $oldPath = $env:PATH
    try {
        $env:PATH = "$script:CLIBinDir;$env:PATH"
        $resolved = (Get-Command wendy -ErrorAction SilentlyContinue).Source
        if ($resolved -ne $wendyPath) {
            throw "ERROR: managed wendy CLI was not first on PATH.`nExpected: $wendyPath`nResolved: $(if ($resolved) { $resolved } else { '<not found>' })"
        }
        $script:WendyCLIVersion = (& $wendyPath --version) -join "`n"
    } finally {
        $env:PATH = $oldPath
    }

    Write-Output "    Version: $script:WendyCLIVersion"
}

function Write-RunInfo([int]$Status) {
    New-Item -ItemType Directory -Force -Path $script:RunDir | Out-Null
    $gitCommit = (& git -C $script:RepoDir rev-parse HEAD 2>$null) -join ''
    $gitBranch = (& git -C $script:RepoDir branch --show-current 2>$null) -join ''
    if (-not $gitBranch) { $gitBranch = $env:GITHUB_REF_NAME }
    $gitRemote = (& git -C $script:RepoDir remote get-url origin 2>$null) -join ''
    $gitDirty = [bool]((& git -C $script:RepoDir status --porcelain 2>$null) -join '')
    $swiftVersion = (& swift --version 2>$null | Select-Object -First 1) -join ''
    $goVersion = (& go version 2>$null) -join ''

    $info = [ordered]@{
        runID = $script:RunID
        createdAt = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
        exitStatus = $Status
        git = [ordered]@{
            commit = if ($gitCommit) { $gitCommit } else { $null }
            branch = if ($gitBranch) { $gitBranch } else { $null }
            ref = if ($env:GITHUB_REF) { $env:GITHUB_REF } else { $null }
            remote = if ($gitRemote) { $gitRemote } else { $null }
            dirty = $gitDirty
        }
        github = [ordered]@{
            repository = if ($env:GITHUB_REPOSITORY) { $env:GITHUB_REPOSITORY } else { $null }
            workflow = if ($env:GITHUB_WORKFLOW) { $env:GITHUB_WORKFLOW } else { $null }
            runID = if ($env:GITHUB_RUN_ID) { $env:GITHUB_RUN_ID } else { $null }
            runAttempt = if ($env:GITHUB_RUN_ATTEMPT) { $env:GITHUB_RUN_ATTEMPT } else { $null }
            job = if ($env:GITHUB_JOB) { $env:GITHUB_JOB } else { $null }
            actor = if ($env:GITHUB_ACTOR) { $env:GITHUB_ACTOR } else { $null }
            sha = if ($env:GITHUB_SHA) { $env:GITHUB_SHA } else { $null }
        }
        target = [ordered]@{
            cliOS = if ($script:CLIOS) { $script:CLIOS } else { $null }
            cliAddress = if ($script:CLIAddress) { $script:CLIAddress } else { $null }
            cliUser = if ($script:CLIUser) { $script:CLIUser } else { $null }
            agentOS = if ($script:AgentOS) { $script:AgentOS } else { $null }
            agentAddress = if ($script:AgentAddress) { $script:AgentAddress } else { $null }
            agentUser = if ($script:AgentUser) { $script:AgentUser } else { $null }
            transport = if ($script:Transport) { $script:Transport } else { $null }
        }
        paths = [ordered]@{
            runDirectory = $script:RunDir
            outputDirectory = $script:OutputDir
            cliRunDirectory = $script:CLIRunDir
            cliBinDirectory = $script:CLIBinDir
            agentRunDirectory = $script:AgentRunDir
            agentBinDirectory = if ($script:AgentBinDir) { $script:AgentBinDir } else { $null }
            testsDirectory = $script:TestsDir
        }
        test = [ordered]@{
            filters = $script:TestFilters
            isolation = $script:Isolation
            parallel = $script:Parallel
        }
        tools = [ordered]@{
            swift = if ($swiftVersion) { $swiftVersion } else { $null }
            go = if ($goVersion) { $goVersion } else { $null }
            wendy = if ($script:WendyCLIVersion) { $script:WendyCLIVersion } else { $null }
            wendyPath = if ($script:WendyCLIPath) { $script:WendyCLIPath } else { $null }
        }
    }

    $path = Join-Path $script:RunDir 'info.json'
    $info | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $path -Encoding UTF8
    Write-Output "==> Wrote Swift E2E run info: $path"
}

$RunID = $env:WENDY_E2E_RUN_ID
$RunName = if ($env:WENDY_E2E_RUN_NAME) { $env:WENDY_E2E_RUN_NAME } else { 'local' }
$DefaultRunID = $null
$OutputDir = $env:WENDY_E2E_OUTPUT_DIR
$CLIRootDir = $env:WENDY_E2E_CLI_ROOT_DIR
$CLIRepoDir = $env:WENDY_E2E_CLI_REPO_DIR
$CLIBinDir = $env:WENDY_E2E_CLI_BIN_DIR
$CLIUser = $env:WENDY_E2E_CLI_USER
$CLIAddress = $env:WENDY_E2E_CLI_ADDRESS
$CLIOS = if ($env:WENDY_E2E_CLI_OS) { $env:WENDY_E2E_CLI_OS } else { 'windows' }
$AgentRootDir = $env:WENDY_E2E_AGENT_ROOT_DIR
$AgentRepoDir = $env:WENDY_E2E_AGENT_REPO_DIR
$AgentBinDir = $env:WENDY_E2E_AGENT_BIN_DIR
$AgentUser = $env:WENDY_E2E_AGENT_USER
$AgentAddress = $env:WENDY_E2E_AGENT_ADDRESS
$AgentOS = $env:WENDY_E2E_AGENT_OS
$Transport = $env:WENDY_E2E_TRANSPORT
$Isolation = if ($env:WENDY_E2E_ISOLATION) { $env:WENDY_E2E_ISOLATION } else { 'per-test' }
$Verbose = if ($env:WENDY_E2E_VERBOSE) { ConvertTo-Bool 'WENDY_E2E_VERBOSE' $env:WENDY_E2E_VERBOSE } else { $false }
$Parallel = if ($env:WENDY_E2E_PARALLEL) { ConvertTo-Bool 'WENDY_E2E_PARALLEL' $env:WENDY_E2E_PARALLEL } else { $false }
$TestFilters = @()

$script:RemainingArgs = [System.Collections.Generic.List[string]]::new()
$script:RemainingArgs.AddRange([string[]]$args)
$i = 0
while ($i -lt $script:RemainingArgs.Count) {
    switch ($script:RemainingArgs[$i]) {
        '--filter' { $TestFilters += Get-ValueOption '--filter' $i; $i += 2; continue }
        '--run-id' { $RunID = Get-ValueOption '--run-id' $i; $i += 2; continue }
        '--run-name' { $RunName = Get-ValueOption '--run-name' $i; $i += 2; continue }
        '--default-run-id' { $DefaultRunID = Get-ValueOption '--default-run-id' $i; $i += 2; continue }
        '--output-dir' { $OutputDir = Get-ValueOption '--output-dir' $i; $i += 2; continue }
        '--cli-root-dir' { $CLIRootDir = Get-ValueOption '--cli-root-dir' $i; $i += 2; continue }
        '--cli-repo-dir' { $CLIRepoDir = Get-ValueOption '--cli-repo-dir' $i; $i += 2; continue }
        '--cli-bin-dir' { $CLIBinDir = Get-ValueOption '--cli-bin-dir' $i; $i += 2; continue }
        '--cli-user' { $CLIUser = Get-ValueOption '--cli-user' $i; $i += 2; continue }
        '--cli-address' { $CLIAddress = Get-ValueOption '--cli-address' $i; $i += 2; continue }
        '--cli-os' { $CLIOS = Get-ValueOption '--cli-os' $i; $i += 2; continue }
        '--agent-root-dir' { $AgentRootDir = Get-ValueOption '--agent-root-dir' $i; $i += 2; continue }
        '--agent-repo-dir' { $AgentRepoDir = Get-ValueOption '--agent-repo-dir' $i; $i += 2; continue }
        '--agent-bin-dir' { $AgentBinDir = Get-ValueOption '--agent-bin-dir' $i; $i += 2; continue }
        '--agent-user' { $AgentUser = Get-ValueOption '--agent-user' $i; $i += 2; continue }
        '--agent-address' { $AgentAddress = Get-ValueOption '--agent-address' $i; $i += 2; continue }
        '--agent-os' { $AgentOS = Get-ValueOption '--agent-os' $i; $i += 2; continue }
        '--isolation' { $Isolation = Get-ValueOption '--isolation' $i; $i += 2; continue }
        '--parallel' { $Parallel = $true; $i += 1; continue }
        '--no-parallel' { $Parallel = $false; $i += 1; continue }
        '--verbose' { $Verbose = $true; $i += 1; continue }
        '--no-verbose' { $Verbose = $false; $i += 1; continue }
        '--help' { Show-Usage; exit 0 }
        '-h' { Show-Usage; exit 0 }
        default { throw "Unknown option: $($script:RemainingArgs[$i])" }
    }
}

if ($TestFilters.Count -eq 0 -and $env:WENDY_E2E_TEST_FILTERS) {
    $TestFilters = $env:WENDY_E2E_TEST_FILTERS.Split(',') | ForEach-Object { $_.Trim() } | Where-Object { $_ }
}
if ($TestFilters.Count -eq 0) { $TestFilters = @('WendyE2ETests') }

if (-not $OutputDir) { throw 'ERROR: --output-dir or WENDY_E2E_OUTPUT_DIR is required.' }

if (-not $CLIRootDir) { $CLIRootDir = if ($CLIAddress) { '$HOME/.wendy/e2e' } else { Join-Path $HOME '.wendy/e2e' } }
if (-not $AgentRootDir) { $AgentRootDir = if ($AgentAddress) { '$HOME/.wendy/e2e' } else { Join-Path $HOME '.wendy/e2e' } }

$Isolation = ConvertTo-Isolation $Isolation
if ($Parallel -and $Isolation -ne 'per-test') { throw 'ERROR: --parallel requires --isolation per-test.' }
if ($Parallel -and ($CLIAddress -or $AgentAddress)) { throw 'ERROR: --parallel is only valid when CLI and agent machines are local.' }
if ($CLIAddress -and -not $CLIRepoDir) { throw 'ERROR: --cli-repo-dir is required when --cli-address is set.' }

$script:RepoDir = Resolve-E2EPath (Join-Path $SwiftDir '..') -Existing
$script:OutputDir = Resolve-E2EPath $OutputDir -Create
$RunName = ConvertTo-SafeRunID $RunName
$RunID = ConvertTo-SafeRunID $(if ($RunID) { $RunID } elseif ($DefaultRunID) { $DefaultRunID } else { New-DefaultRunID $script:OutputDir $RunName })
if (-not $RunID) { $RunID = ConvertTo-SafeRunID (New-DefaultRunID $script:OutputDir $RunName) }
if (-not $CLIAddress) {
    $script:CLIRootDir = Resolve-E2EPath $CLIRootDir -Create
    $script:CLIRepoDir = Resolve-E2EPath $(if ($CLIRepoDir) { $CLIRepoDir } else { $script:RepoDir }) -Existing
} else {
    $script:CLIRootDir = ConvertTo-RemotePath $CLIRootDir
    $script:CLIRepoDir = ConvertTo-RemotePath $CLIRepoDir
}
if (-not $AgentAddress) {
    $script:AgentRootDir = Resolve-E2EPath $AgentRootDir -Create
    $script:AgentRepoDir = Resolve-E2EPath $(if ($AgentRepoDir) { $AgentRepoDir } else { $script:RepoDir }) -Existing
} else {
    $script:AgentRootDir = ConvertTo-RemotePath $AgentRootDir
    $script:AgentRepoDir = ConvertTo-RemotePath $AgentRepoDir
}

$script:RunID = $RunID
$script:CLIUser = $CLIUser
$script:CLIAddress = $CLIAddress
$script:CLIOS = $CLIOS
$script:AgentUser = $AgentUser
$script:AgentAddress = $AgentAddress
$script:AgentOS = $AgentOS
$script:Transport = $Transport
$script:Isolation = $Isolation
$script:Verbose = $Verbose
$script:Parallel = $Parallel
$script:TestFilters = @($TestFilters)
$script:RunDir = Join-Path $script:OutputDir $RunID
$script:CLIRunDir = if ($CLIAddress) { "$script:CLIRootDir/$RunID/cli" } else { Join-Path (Join-Path $script:CLIRootDir $RunID) 'cli' }
$script:AgentRunDir = if ($AgentAddress) { "$script:AgentRootDir/$RunID/agent" } else { Join-Path (Join-Path $script:AgentRootDir $RunID) 'agent' }
if ($CLIBinDir) {
    $script:CLIBinDir = if ($CLIAddress) { ConvertTo-RemotePath $CLIBinDir } else { Resolve-E2EPath $CLIBinDir -Create }
} else {
    $script:CLIBinDir = if ($CLIAddress) { "$script:CLIRepoDir/go/bin" } else { Resolve-E2EPath (Join-Path (Join-Path $script:CLIRepoDir 'go') 'bin') -Create }
}
if ($AgentBinDir) {
    $script:AgentBinDir = if ($AgentAddress) { ConvertTo-RemotePath $AgentBinDir } else { Resolve-E2EPath $AgentBinDir -Create }
} else {
    $script:AgentBinDir = $null
}
$script:TestsDir = Join-Path $script:RunDir 'tests'
$TestResultsOutputPath = Join-Path $script:RunDir 'test-results.xml'
$ExpandedTestResultsOutputPath = Join-Path $script:RunDir 'test-results-swift-testing.xml'

if (Test-Path -LiteralPath $script:RunDir) { Remove-Item -LiteralPath $script:RunDir -Recurse -Force }
if (-not $AgentAddress -and (Test-Path -LiteralPath $script:AgentRunDir)) { Remove-Item -LiteralPath $script:AgentRunDir -Recurse -Force }
New-Item -ItemType Directory -Force -Path $script:RunDir, $script:TestsDir | Out-Null

Build-CLI

$swiftArgs = @('test')
if (-not $Parallel) { $swiftArgs += '--no-parallel' }
if ($TestFilters.Count -eq 1) {
    $swiftArgs += @('--filter', $TestFilters[0])
} else {
    $swiftArgs += @('--filter', ($TestFilters -join '|'))
}
$swiftArgs += @('--xunit-output', $TestResultsOutputPath)

$env:WENDY_E2E_RUN_ID = $RunID
$env:WENDY_E2E_RUN_DIR = $script:RunDir
$env:WENDY_E2E_CLI_RUN_DIR = $script:CLIRunDir
$env:WENDY_E2E_CLI_REPO_DIR = if ($script:CLIRepoDir) { $script:CLIRepoDir } else { '' }
$env:WENDY_E2E_CLI_BIN_DIR = if ($script:CLIBinDir) { $script:CLIBinDir } else { '' }
$env:WENDY_E2E_CLI_USER = if ($CLIUser) { $CLIUser } else { '' }
$env:WENDY_E2E_CLI_ADDRESS = if ($CLIAddress) { $CLIAddress } else { '' }
$env:WENDY_E2E_AGENT_RUN_DIR = $script:AgentRunDir
$env:WENDY_E2E_AGENT_REPO_DIR = if ($script:AgentRepoDir) { $script:AgentRepoDir } else { '' }
$env:WENDY_E2E_AGENT_BIN_DIR = if ($script:AgentBinDir) { $script:AgentBinDir } else { '' }
$env:WENDY_E2E_AGENT_USER = if ($AgentUser) { $AgentUser } else { '' }
$env:WENDY_E2E_AGENT_ADDRESS = if ($AgentAddress) { $AgentAddress } else { '' }
$env:WENDY_E2E_CLI_OS = if ($CLIOS) { $CLIOS } else { '' }
$env:WENDY_E2E_AGENT_OS = if ($AgentOS) { $AgentOS } else { '' }
$env:WENDY_E2E_ISOLATION = $Isolation
$env:WENDY_E2E_PARALLEL = $Parallel.ToString().ToLowerInvariant()
$env:WENDY_E2E_VERBOSE = $Verbose.ToString().ToLowerInvariant()

Write-Output '==> Running Swift E2E tests'
Write-Output "    Package:  $PackageDir"
Write-Output "    Run ID:   $RunID"
Write-Output "    Run dir:  $script:RunDir"
Write-Output "    CLI run:  $script:CLIRunDir"
Write-Output "    Agent run: $script:AgentRunDir"
Write-Output "    CLI bin:  $script:CLIBinDir"
Write-Output "    CLI:      $script:WendyCLIPath"
Write-Output "    Tests:    $script:TestsDir"
Write-Output "    Filters:  $($TestFilters -join ' ')"
Write-Output "    Isolation: $Isolation"
Write-Output "    Verbose:  $Verbose"
Write-Output "    Parallel: $Parallel"
Write-Output '    HTML:     <deferred to Scripts/E2EReport.ps1>'
Write-Output "    CLI target: $(if ($CLIAddress) { "${CLIUser}@${CLIAddress}" } else { '<local>' }):$(if ($script:CLIRepoDir) { $script:CLIRepoDir } else { '<no-repo>' })"
Write-Output "    CLI OS:   $(if ($CLIOS) { $CLIOS } else { '<current>' })"
Write-Output "    Agent:    $(if ($AgentAddress) { "$(Get-SSHHost $AgentUser $AgentAddress):$(if ($script:AgentRepoDir) { $script:AgentRepoDir } else { '<no-repo>' })" } else { "<local>:$(if ($script:AgentRepoDir) { $script:AgentRepoDir } else { '<no-repo>' })" })"
Write-Output "    Agent OS: $(if ($AgentOS) { $AgentOS } else { '<current>' })"
Write-Output "    Transport: $(if ($Transport) { $Transport } else { '<none>' })"

Push-Location $PackageDir
try {
    & swift @swiftArgs
    $testStatus = $LASTEXITCODE
} finally {
    Pop-Location
}

if (Test-Path -LiteralPath $ExpandedTestResultsOutputPath -PathType Leaf) {
    Move-Item -LiteralPath $ExpandedTestResultsOutputPath -Destination $TestResultsOutputPath -Force
}

& (Join-Path $ScriptDir 'E2ESanitizeXUnit.ps1') --run-dir $script:RunDir
$sanitizeStatus = $LASTEXITCODE
if ($testStatus -eq 0 -and $sanitizeStatus -ne 0) { $testStatus = $sanitizeStatus }

Write-RunInfo $testStatus
exit $testStatus
