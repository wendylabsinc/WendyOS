# Tests for the Wendy plugin's Windows launcher (wendy.cmd + wendy-install.ps1).
# Run with Windows PowerShell 5.1 from the repo root:
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\test\plugin-launcher\run.ps1
# Needs Git for Windows (sh, for scripts/pin-cli.sh) and python.
$ErrorActionPreference = 'Stop'
$Repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
$Script:Failures = 0
$Script:Passes = 0
$V = '2026.10.01-000000'

function New-StubExe([string]$Path, [string]$Version) {
    New-Item -ItemType Directory -Force -Path (Split-Path $Path) | Out-Null
    $src = @"
using System;
public static class Program {
    public static int Main(string[] args) {
        if (args.Length == 1 && args[0] == "--version") { Console.WriteLine("wendy version $Version"); return 0; }
        Console.Write("stub-wendy $Version args:");
        foreach (var a in args) Console.Write("[" + a + "]");
        Console.WriteLine();
        if (Environment.GetEnvironmentVariable("STUB_READ_STDIN") == "1") Console.WriteLine("stdin:" + Console.In.ReadLine());
        return 0;
    }
}
"@
    Add-Type -TypeDefinition $src -OutputAssembly $Path -OutputType ConsoleApplication
}

function New-Case {
    $t = Join-Path ([IO.Path]::GetTempPath()) ("wendy-launcher-" + [Guid]::NewGuid())
    New-Item -ItemType Directory -Force -Path $t | Out-Null
    $scripts = Join-Path $t 'plugin\scripts'
    New-Item -ItemType Directory -Force -Path $scripts | Out-Null
    foreach ($f in 'wendy', 'wendy.cmd', 'wendy-install.ps1') { Copy-Item (Join-Path $Repo "plugins\wendy\scripts\$f") $scripts }
    $env:WENDY_CONFIG_DIR = Join-Path $t 'home\.wendy'
    $env:WENDY_LAUNCHER_WELL_KNOWN_DIRS = Join-Path $t 'wellknown'
    Remove-Item Env:WENDY_CLI, Env:WENDY_CLI_DOWNLOAD_BASE, Env:STUB_READ_STDIN -ErrorAction SilentlyContinue
    $env:PATH = "$(Join-Path $t 'stubbin');$env:SystemRoot\System32;$env:SystemRoot;$env:SystemRoot\System32\WindowsPowerShell\v1.0"
    return @{ T = $t; Cmd = (Join-Path $scripts 'wendy.cmd'); Launcher = (Join-Path $scripts 'wendy'); Fix = (Join-Path $t 'fixtures') }
}

function New-Release($c, [string]$Version) {
    $d = Join-Path $c.Fix $Version
    New-Item -ItemType Directory -Force -Path $d | Out-Null
    foreach ($arch in 'amd64', 'arm64') {
        $pkg = Join-Path $c.T "pkg\wendy-cli-windows-$arch"
        New-StubExe (Join-Path $pkg 'wendy.exe') $Version
        Compress-Archive -Path $pkg -DestinationPath (Join-Path $d "wendy-cli-windows-$arch-$Version.zip") -Force
        Remove-Item -Recurse -Force (Join-Path $c.T 'pkg')
    }
    foreach ($p in 'darwin-arm64', 'linux-amd64', 'linux-arm64') { Set-Content (Join-Path $d "wendy-cli-$p-$Version.tar.gz") "placeholder $p" }
    $casePath = $env:PATH
    $env:PATH = "$(Split-Path $Script:Sh);$casePath"
    try { & $Script:Sh "$Repo/scripts/pin-cli.sh" --launcher ($c.Launcher -replace '\\', '/') --local ($d -replace '\\', '/') --min $Version $Version | Out-Null }
    finally { $env:PATH = $casePath }
    if ($LASTEXITCODE -ne 0) { throw "pin-cli.sh failed" }
}

function Start-Fixtures($c) {
    $port = Join-Path $c.T 'port'
    $c.Server = Start-Process -FilePath $Script:Python -ArgumentList @("`"$Repo\scripts\test\plugin-launcher\fixture_server.py`"", "`"$($c.Fix)`"", "`"$port`"") -PassThru -WindowStyle Hidden
    for ($i = 0; $i -lt 100 -and -not (Test-Path $port); $i++) { Start-Sleep -Milliseconds 100 }
    $env:WENDY_CLI_DOWNLOAD_BASE = "http://127.0.0.1:$(Get-Content $port)"
}

function Stop-Case($c) {
    if ($c.Server) { Stop-Process -Id $c.Server.Id -Force -ErrorAction SilentlyContinue }
    Remove-Item -Recurse -Force $c.T -ErrorAction SilentlyContinue
}

function Invoke-Cmd($c, [string[]]$CliArgs, [string]$Stdin = '') {
    $out = Join-Path $c.T 'out.txt'
    $err = Join-Path $c.T 'err.txt'
    $in = Join-Path $c.T 'in.txt'
    Set-Content -LiteralPath $in -Value $Stdin -NoNewline
    $p = Start-Process -FilePath $c.Cmd -ArgumentList $CliArgs -RedirectStandardOutput $out -RedirectStandardError $err -RedirectStandardInput $in -PassThru -Wait -NoNewWindow
    return @{ Code = $p.ExitCode; Out = (Get-Content -Raw -LiteralPath $out); Err = (Get-Content -Raw -LiteralPath $err) }
}

function Assert-Equal($got, $want, [string]$what) { if ("$got".TrimEnd() -ne "$want".TrimEnd()) { throw "${what}: expected [$want], got [$got]" } }
function Assert-Contains($got, $want, [string]$what) { if (-not "$got".Contains($want)) { throw "${what}: [$got] does not contain [$want]" } }

function Test-WendyCliEnvRunsItAsIs($c) {
    New-StubExe (Join-Path $c.T 'custom\wendy.exe') '2000.01.01-000000'
    $env:WENDY_CLI = Join-Path $c.T 'custom\wendy.exe'
    $r = Invoke-Cmd $c @('mcp', 'serve')
    Assert-Equal $r.Out 'stub-wendy 2000.01.01-000000 args:[mcp][serve]' 'stdout'
}

function Test-FreshInstallThroughCmd($c) {
    New-Release $c $V
    Start-Fixtures $c
    $r = Invoke-Cmd $c @('mcp', 'serve')
    Assert-Equal $r.Code 0 'exit code'
    Assert-Equal $r.Out "stub-wendy $V args:[mcp][serve]" 'stdout is only the CLI'
    if (-not (Test-Path (Join-Path $env:WENDY_CONFIG_DIR "cli\$V\wendy.exe"))) { throw 'managed exe missing' }
    if (-not (Test-Path (Join-Path $env:WENDY_CONFIG_DIR 'bin\wendy.exe'))) { throw 'bin\wendy.exe missing' }
}

function Test-SecondRunNeedsNoNetwork($c) {
    New-Release $c $V
    Start-Fixtures $c
    Invoke-Cmd $c @('--version') | Out-Null
    $env:WENDY_CLI_DOWNLOAD_BASE = 'http://127.0.0.1:9'
    $r = Invoke-Cmd $c @('mcp', 'serve')
    Assert-Equal $r.Out "stub-wendy $V args:[mcp][serve]" 'managed exe reused'
}

function Test-ChecksumMismatchInstallsNothing($c) {
    New-Release $c $V
    $arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
    Add-Content (Join-Path $c.Fix "$V\wendy-cli-windows-$arch-$V.zip") 'tampered'
    Start-Fixtures $c
    $r = Invoke-Cmd $c @('mcp', 'serve')
    Assert-Equal $r.Code 1 'exit code'
    Assert-Equal $r.Out '' 'stdout empty'
    Assert-Contains (Get-Content -Raw (Join-Path $env:WENDY_CONFIG_DIR 'cli\last-error.txt')) 'checksum mismatch' 'error file'
    if (Test-Path (Join-Path $env:WENDY_CONFIG_DIR "cli\$V")) { throw 'nothing may be installed' }
}

function Test-PathWendyNewEnoughIsUsed($c) {
    New-Release $c $V
    New-StubExe (Join-Path $c.T 'stubbin\wendy.exe') '2026.11.01-000000'
    $env:WENDY_CLI_DOWNLOAD_BASE = 'http://127.0.0.1:9'
    $r = Invoke-Cmd $c @('run')
    Assert-Equal $r.Out 'stub-wendy 2026.11.01-000000 args:[run]' 'PATH wendy runs'
}

function Test-StdinReachesCli($c) {
    New-Release $c $V
    Start-Fixtures $c
    $env:STUB_READ_STDIN = '1'
    $r = Invoke-Cmd $c @('mcp', 'serve') '{"jsonrpc":"2.0","id":1,"method":"initialize"}'
    Assert-Contains $r.Out 'stdin:{"jsonrpc":"2.0","id":1,"method":"initialize"}' 'first stdin line reaches the CLI'
}

$Script:Sh = (Get-Command sh.exe -ErrorAction SilentlyContinue).Source
if (-not $Script:Sh) { $Script:Sh = Join-Path $env:ProgramFiles 'Git\usr\bin\sh.exe' }
$Script:Python = (Get-Command python.exe -ErrorAction Stop).Source
$savedPath = $env:PATH
$cases = Get-ChildItem function:Test-* | Select-Object -ExpandProperty Name
if ($args.Count -gt 0) { $cases = $args }
foreach ($name in $cases) {
    $env:PATH = $savedPath
    $c = New-Case
    try { & $name $c; $Script:Passes++; Write-Output "ok   $name" }
    catch { $Script:Failures++; Write-Output "FAIL $name`n    $($_.Exception.Message)" }
    finally { Stop-Case $c; $env:PATH = $savedPath }
}
Write-Output "$Script:Passes passed, $Script:Failures failed"
if ($Script:Failures -gt 0) { exit 1 }
