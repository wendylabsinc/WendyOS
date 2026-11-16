param([Parameter(Mandatory=$true)][string]$Python)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'fixture_process.ps1')

function Invoke-FixtureSupervisorCheck([string]$Name, [string]$Source, [string]$Expected, [int]$TimeoutMilliseconds = 3000) {
    $dir = Join-Path ([IO.Path]::GetTempPath()) ("launcher supervisor " + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $dir | Out-Null
    $case = @{ T = $dir; Fix = (Join-Path $dir 'fixtures with spaces') }
    $script = Join-Path $dir 'child.py'
    [IO.File]::WriteAllText($script, $Source)
    $timer = [Diagnostics.Stopwatch]::StartNew()
    try {
        $failure = $null
        try { $null = Start-FixtureProcess $case $Python $script $TimeoutMilliseconds }
        catch { $failure = $_.Exception.Message }
        if (-not $failure -or -not $failure.Contains($Expected)) { throw "$Name expected [$Expected], got [$failure]" }
        if ($case.Server -or $case.Keep) { throw "$Name did not confirm owned child cleanup" }
        if ($timer.Elapsed.TotalSeconds -gt 10) { throw "$Name exceeded startup and cleanup bounds" }
        if (-not (Test-Path (Join-Path $dir 'fixture-Stderr.log'))) { throw "$Name lost stderr capture" }
        Write-Output "ok   fixture supervisor $Name"
    } finally {
        Stop-FixtureProcess $case
        if (-not $case.Keep) { Remove-Item -LiteralPath $dir -Recurse -Force }
    }
}

Invoke-FixtureSupervisorCheck 'early exit reports stderr and status' @'
import sys
print('fixture-stdout', flush=True)
print('fixture-specific-error', file=sys.stderr, flush=True)
sys.exit(23)
'@ 'Python exited with code 23'
# Exercise the same failure again only as an independent diagnostic assertion;
# this does not retry any launcher case or accept a failed fresh installation.
Invoke-FixtureSupervisorCheck 'early exit retains stderr' @'
import sys
print('fixture-specific-error', file=sys.stderr, flush=True)
sys.exit(23)
'@ 'fixture-specific-error'
Invoke-FixtureSupervisorCheck 'live child never publishes readiness' @'
import time
print('fixture waiting', flush=True)
time.sleep(60)
'@ 'Fixture readiness exceeded 500ms' 500
Invoke-FixtureSupervisorCheck 'invalid port is refused' @'
import pathlib, sys, time
pathlib.Path(sys.argv[2]).write_text('65536')
time.sleep(60)
'@ 'Invalid fixture port [65536]'

Invoke-FixtureSupervisorCheck 'wrong HTTP identity never becomes ready' @'
import http.server, pathlib, sys
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header('Content-Length', '32')
        self.end_headers()
        self.wfile.write(b'x' * 32)
    def log_message(self, *args): pass
server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
pathlib.Path(sys.argv[2]).write_text(str(server.server_port))
server.serve_forever()
'@ 'readiness nonce mismatch' 3000
# A wrong-identity response is terminal, even if a subsequent request would
# return the expected nonce. This also prevents retries from masking the cause.
Invoke-FixtureSupervisorCheck 'wrong identity cannot recover on a later response' @'
import http.server, pathlib, sys
class Handler(http.server.BaseHTTPRequestHandler):
    attempts = 0
    def do_GET(self):
        Handler.attempts += 1
        nonce = self.path.rsplit('/', 1)[-1][len('ready-'):-len('.txt')]
        body = b'x' * 32 if Handler.attempts == 1 else nonce.encode('ascii')
        self.send_response(200)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args): pass
server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
pathlib.Path(sys.argv[2]).write_text(str(server.server_port))
server.serve_forever()
'@ 'readiness nonce mismatch' 3000

Invoke-FixtureSupervisorCheck 'stalled HTTP body remains bounded' @'
import http.server, pathlib, sys, time
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header('Content-Length', '32')
        self.end_headers()
        self.wfile.flush()
        time.sleep(60)
    def log_message(self, *args): pass
server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
pathlib.Path(sys.argv[2]).write_text(str(server.server_port))
server.serve_forever()
'@ 'readiness body read timed out' 3000

$dir = Join-Path ([IO.Path]::GetTempPath()) ("launcher actual server " + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $dir | Out-Null
$case = @{ T = $dir; Fix = (Join-Path $dir 'fixtures with spaces') }
try {
    $url = Start-FixtureProcess $case $Python (Join-Path $PSScriptRoot 'fixture_server.py')
    if (-not $url.StartsWith('http://127.0.0.1:') -or $case.Server.HasExited) { throw 'real fixture not ready' }
    # Match Test-TruncatedDownloadCanRetry: become ready, then inject loss.
    [IO.File]::WriteAllText((Join-Path $case.Fix 'asset.zip'), '0123456789abcdef')
    [IO.File]::WriteAllText((Join-Path $case.Fix 'truncate-downloads'), 'truncate every asset')
    $check = Join-Path $dir 'check-truncation.py'
    [IO.File]::WriteAllText($check, @'
import http.client, sys, urllib.request
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
with opener.open(sys.argv[1] + '/asset.zip', timeout=2) as response:
    assert response.headers['Content-Length'] == '16'
    try:
        response.read()
    except http.client.IncompleteRead as error:
        assert error.partial == b'01234567', error.partial
    else:
        raise AssertionError('fault-injected asset unexpectedly complete')
print('ACTUAL_ASSET_TRUNCATION_VERIFIED')
'@)
    $result = & $Python $check $url
    if ($LASTEXITCODE -ne 0 -or $result -ne 'ACTUAL_ASSET_TRUNCATION_VERIFIED') { throw "actual truncation check failed: $result" }
    Write-Output 'ok   fixture supervisor readiness precedes actual asset truncation'
    $owned = $case.Server
    $ownedId = $owned.Id
    Stop-FixtureProcess $case
    if ($case.Server -or $case.Keep) { throw 'actual fixture cleanup was not confirmed' }
    Write-Output "ok   fixture supervisor real HTTP server and owned cleanup ($ownedId)"
} finally {
    Stop-FixtureProcess $case
    if (-not $case.Keep) { Remove-Item -LiteralPath $dir -Recurse -Force }
}
