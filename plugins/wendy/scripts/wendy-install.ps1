# Wendy CLI launcher helper for Windows, called by wendy.cmd.
#
# Prints the full path of a usable wendy.exe on stdout (wendy.cmd captures it
# and runs it), installing the pinned release into <root>\cli\<version>\ first
# when needed. The CLI is chosen in the same order as the POSIX launcher `wendy`
# next to this file, which also holds the pinned version and checksums
# (scripts/pin-cli.sh writes them there, so they are pinned in one place).
# Messages go to stderr; a failure is also written to <root>\cli\last-error.txt.
param([switch]$InstallWorker)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$pinned = New-Object System.Collections.Hashtable ([StringComparer]::Ordinal)
$inBlock = $false
foreach ($line in Get-Content -LiteralPath (Join-Path $PSScriptRoot 'wendy')) {
    if ($line -cmatch '^# >>> pinned by scripts/pin-cli\.sh') { $inBlock = $true; continue }
    if ($line -ceq '# <<< pinned') { break }
    if ($inBlock -and $line -cmatch '^([A-Za-z0-9_]+)="([^"]*)"$') { $pinned[$Matches[1]] = $Matches[2] }
}
$CliVersion = $pinned['CLI_VERSION']
$MinVersion = $pinned['MIN_VERSION']
$Base = if ($env:WENDY_CLI_DOWNLOAD_BASE) { $env:WENDY_CLI_DOWNLOAD_BASE } else { $pinned['DOWNLOAD_BASE'] }
$Root = if ($env:WENDY_CONFIG_DIR) { $env:WENDY_CONFIG_DIR } else { Join-Path $env:USERPROFILE '.wendy' }
$CliRoot = Join-Path $Root 'cli'
$ErrorFile = Join-Path $CliRoot 'last-error.txt'
$LogFile = Join-Path $CliRoot 'install.log'
$Managed = Join-Path $CliRoot "$CliVersion\wendy.exe"
$Arch = switch ($env:PROCESSOR_ARCHITECTURE) { 'AMD64' { 'amd64' } 'ARM64' { 'arm64' } default { '' } }
# x64 PowerShell emulated on an ARM64 machine: install the native build.
if ($env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { $Arch = 'arm64' }
$WellKnown = if ($env:WENDY_LAUNCHER_WELL_KNOWN_DIRS) {
    $env:WENDY_LAUNCHER_WELL_KNOWN_DIRS -split ';'
} else {
    @((Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Links'), (Join-Path $env:ProgramFiles 'Wendy'))
}
$UpgradeHint = 'winget upgrade WendyLabs.Wendy'

function Say([string]$Message) {
    [Console]::Error.WriteLine("wendy launcher: $Message")
    if ($InstallWorker) { Add-Content -LiteralPath $LogFile -Value "wendy launcher: $Message" -ErrorAction SilentlyContinue }
}

function Fail([string]$Message, [string]$Next) {
    New-Item -ItemType Directory -Force -Path $CliRoot | Out-Null
    $stamp = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    Set-Content -LiteralPath $ErrorFile -Encoding UTF8 -Value @("time: $stamp", "error: $Message", "next step: $Next")
    Say $Message
    Say "next step: $Next"
    exit 1
}

function Get-CliVersion([string]$Exe) {
    try { $out = & $Exe --version 2>$null | Select-Object -First 1 } catch { return '' }
    if (-not $out) { return '' }
    return ("$out".Trim() -split '\s+')[-1]
}

# 0 = new enough, 2 = a dev build (accepted), 1 = too old or unrecognised.
function Get-VersionStatus([string]$Version) {
    if ($Version -eq 'dev' -or $Version -like '*-dev') { return 2 }
    if ($Version -match '^\d{4}\.\d{2}\.\d{2}-\d{6}$' -and [string]::CompareOrdinal($Version, $MinVersion) -ge 0) { return 0 }
    return 1
}

function Remove-OldVersions {
    $versions = @(Get-ChildItem -LiteralPath $CliRoot -Directory | Where-Object { $_.Name -match '^\d{4}\.\d{2}\.\d{2}-\d{6}$' } | Sort-Object Name)
    $previous = ($versions | Where-Object { $_.Name -ne $CliVersion } | Select-Object -Last 1).Name
    foreach ($v in $versions) {
        if ($v.Name -ne $CliVersion -and $v.Name -ne $previous) { Remove-Item -Recurse -Force -LiteralPath $v.FullName -ErrorAction SilentlyContinue }
    }
}

# Install-Managed runs in its own hidden PowerShell process (-InstallWorker) so
# it finishes even if the MCP client kills the launcher during a slow start.
function Install-Managed {
    New-Item -ItemType Directory -Force -Path $CliRoot | Out-Null
    $mutex = New-Object System.Threading.Mutex($false, 'Local\WendyCliInstall')
    try {
        try { [void]$mutex.WaitOne([TimeSpan]::FromMinutes(10)) } catch [System.Threading.AbandonedMutexException] { }
        if (Test-Path -LiteralPath $Managed) { return }
        if (-not $Arch) { Fail "the Wendy CLI has no build for $env:PROCESSOR_ARCHITECTURE" 'Wendy runs on Windows x64 and arm64' }
        $asset = "wendy-cli-windows-$Arch-$CliVersion.zip"
        $want = $pinned["SHA256_windows_$Arch"]
        if (-not $want) { Fail "this launcher has no pinned checksum for windows-$Arch" 'update the Wendy plugin' }
        $tmp = Join-Path $CliRoot ".tmp.$PID"
        Remove-Item -Recurse -Force -LiteralPath $tmp -ErrorAction SilentlyContinue
        New-Item -ItemType Directory -Force -Path $tmp | Out-Null
        try {
            $zip = Join-Path $tmp $asset
            [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
            try { Invoke-WebRequest -UseBasicParsing -Uri "$Base/$CliVersion/$asset" -OutFile $zip }
            catch { Fail "could not download $Base/$CliVersion/$asset ($($_.Exception.Message))" "check the network connection (details in $LogFile), then restart the Wendy MCP server" }
            $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $zip).Hash.ToLowerInvariant()
            if ($got -ne $want) { Fail "checksum mismatch for ${asset}: expected $want, got $got; the download was deleted" "if you are behind a captive portal or a proxy that rewrites downloads, fix that and restart the Wendy MCP server; otherwise don't install it by hand and report this at https://github.com/wendylabsinc/wendy-agentic-coding/issues" }
            Expand-Archive -LiteralPath $zip -DestinationPath $tmp -Force
            $pkg = Join-Path $tmp "wendy-cli-windows-$Arch"
            if (-not (Test-Path -LiteralPath (Join-Path $pkg 'wendy.exe'))) { Fail "$asset has no wendy.exe inside" 'report this at https://github.com/wendylabsinc/wendy-agentic-coding/issues' }
            $dest = Join-Path $CliRoot $CliVersion
            if (-not (Test-Path -LiteralPath $Managed)) {
                Remove-Item -Recurse -Force -LiteralPath $dest -ErrorAction SilentlyContinue
                Move-Item -LiteralPath $pkg -Destination $dest
            }
        } finally {
            Remove-Item -Recurse -Force -LiteralPath $tmp -ErrorAction SilentlyContinue
        }
        $bin = Join-Path $Root 'bin'
        New-Item -ItemType Directory -Force -Path $bin | Out-Null
        try { Copy-Item -Force -LiteralPath $Managed -Destination (Join-Path $bin 'wendy.exe') }
        catch { Say "could not update $bin\wendy.exe (in use?)" }
        Remove-OldVersions
        Remove-Item -Force -LiteralPath $ErrorFile -ErrorAction SilentlyContinue
    } finally {
        try { $mutex.ReleaseMutex() } catch { }
        $mutex.Dispose()
    }
}

function Select-Cli([string]$Path) {
    Remove-Item -Force -LiteralPath $ErrorFile -ErrorAction SilentlyContinue
    Write-Output $Path
    exit 0
}

foreach ($name in 'CLI_VERSION', 'MIN_VERSION', 'DOWNLOAD_BASE') {
    if (-not $pinned[$name]) { Fail "the launcher's pinned block has no $name" 'update the Wendy plugin' }
}

if ($InstallWorker) {
    try { Install-Managed }
    catch { Fail "the Wendy CLI install failed: $($_.Exception.Message)" "see $LogFile, then restart the Wendy MCP server" }
    exit 0
}

if ($env:WENDY_CLI) {
    if (-not (Test-Path -LiteralPath $env:WENDY_CLI -PathType Leaf)) { Fail "WENDY_CLI is set to $env:WENDY_CLI, which is not a file" 'unset WENDY_CLI, or point it at wendy.exe' }
    Select-Cli $env:WENDY_CLI
}

$oldNotice = ''
$candidates = @()
$onPath = Get-Command -Name 'wendy.exe' -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
if ($onPath) { $candidates += $onPath.Source }
foreach ($dir in $WellKnown) { if ($dir) { $candidates += (Join-Path $dir 'wendy.exe') } }
foreach ($candidate in $candidates) {
    if (-not (Test-Path -LiteralPath $candidate -PathType Leaf)) { continue }
    $version = Get-CliVersion $candidate
    $status = Get-VersionStatus $version
    if ($status -eq 0) { Select-Cli $candidate }
    if ($status -eq 2) { Say "using development build $candidate ($version)"; Select-Cli $candidate }
    if ($version -and -not $oldNotice) {
        $oldNotice = "$candidate is version $version, older than $MinVersion, so the plugin uses its own copy of $CliVersion (to upgrade yours: $UpgradeHint)"
    }
}

if (-not (Test-Path -LiteralPath $Managed)) {
    if ($oldNotice) { Say $oldNotice; $oldNotice = '' }
    New-Item -ItemType Directory -Force -Path $CliRoot | Out-Null
    Remove-Item -Force -LiteralPath $ErrorFile -ErrorAction SilentlyContinue
    Say "installing the Wendy CLI $CliVersion into $CliRoot (first run, about 15 MB)"
    $worker = Start-Process -FilePath (Join-Path $PSHOME 'powershell.exe') -WindowStyle Hidden -PassThru -ArgumentList @(
        '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', "`"$PSCommandPath`"", '-InstallWorker')
    $worker.WaitForExit()
    if (-not (Test-Path -LiteralPath $Managed)) {
        if (Test-Path -LiteralPath $ErrorFile) { [Console]::Error.WriteLine((Get-Content -Raw -LiteralPath $ErrorFile)) }
        else { Fail 'the Wendy CLI install did not finish' "see $LogFile, then restart the Wendy MCP server" }
        exit 1
    }
    Say "installed the Wendy CLI $CliVersion"
}
if ($oldNotice) { Say $oldNotice }
Select-Cli $Managed
