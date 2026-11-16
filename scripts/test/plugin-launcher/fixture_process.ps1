# Direct children only: fixture_server.py does not launch descendants. Retain the
# Process and its Windows handle through the final signal and confirmed exit.
function Stop-FixtureProcess($c) {
    if (-not $c.Server) { return }
    if (-not $c.Server.HasExited) { $c.Server.Kill() }
    if (-not $c.Server.WaitForExit(5000)) {
        $c.Keep = $true
        throw "Fixture did not exit; retaining $($c.T)"
    }
    foreach ($stream in 'Stdout', 'Stderr') {
        $task = $c["Server$stream"]
        if (-not $task.Wait(1000)) {
            $c.Keep = $true
            throw "Fixture $stream did not close; retaining $($c.T)"
        }
        [IO.File]::WriteAllText((Join-Path $c.T "fixture-$stream.log"), $task.Result)
    }
    $c.Server.Dispose()
    $c.Server = $null
}

function Start-FixtureProcess($c, [string]$Python, [string]$ServerScript, [int]$TimeoutMilliseconds = 10000) {
    $portFile = Join-Path $c.T 'port'
    $nonce = [Guid]::NewGuid().ToString('N')
    $probeName = "ready-$nonce.txt"
    New-Item -ItemType Directory -Force -Path $c.Fix | Out-Null
    [IO.File]::WriteAllText((Join-Path $c.Fix $probeName), $nonce)
    $start = New-Object Diagnostics.ProcessStartInfo
    $start.FileName = $Python
    $start.Arguments = "-u `"$ServerScript`" `"$($c.Fix)`" `"$portFile`""
    $start.UseShellExecute = $false
    $start.CreateNoWindow = $true
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    $process = New-Object Diagnostics.Process
    $process.StartInfo = $start
    $timer = [Diagnostics.Stopwatch]::StartNew()
    try {
        if (-not $process.Start()) { throw 'Python process did not start' }
        $c.Server = $process
        $null = $process.Handle
        $c.ServerStdout = $process.StandardOutput.ReadToEndAsync()
        $c.ServerStderr = $process.StandardError.ReadToEndAsync()
        $lastProbe = 'port file not published'
        while ($timer.ElapsedMilliseconds -lt $TimeoutMilliseconds) {
            if ($process.HasExited) { throw "Python exited with code $($process.ExitCode) before readiness" }
            if (Test-Path -LiteralPath $portFile) {
                $value = [IO.File]::ReadAllText($portFile).Trim()
                if ($value -notmatch '^[0-9]{1,5}$' -or [int]$value -lt 1 -or [int]$value -gt 65535) {
                    throw "Invalid fixture port [$value]"
                }
                $remaining = $TimeoutMilliseconds - $timer.ElapsedMilliseconds
                if ($remaining -le 0) { break }
                $request = [Net.HttpWebRequest]::Create("http://127.0.0.1:$value/$probeName")
                $request.Proxy = $null
                $request.AllowAutoRedirect = $false
                $request.Timeout = [int][Math]::Min(500, $remaining)
                $request.ReadWriteTimeout = $request.Timeout
                $response = $null
                try {
                    $response = $request.GetResponse()
                    # This exact owned static file is 32 ASCII bytes. Reject any
                    # wrong/partial response without an unbounded body read.
                    if ($response.StatusCode -ne 200 -or $response.ContentLength -ne 32) { throw 'unexpected readiness response' }
                    $buffer = New-Object byte[] 32
                    $stream = $response.GetResponseStream()
                    $offset = 0
                    while ($offset -lt 32) {
                        $left = $TimeoutMilliseconds - $timer.ElapsedMilliseconds
                        if ($left -le 0) { throw 'readiness body exceeded deadline' }
                        $read = $stream.ReadAsync($buffer, $offset, 32 - $offset)
                        if (-not $read.Wait([int][Math]::Min(500, $left))) { throw 'readiness body read timed out' }
                        $count = $read.Result
                        if ($count -eq 0) { throw 'truncated readiness body' }
                        $offset += $count
                    }
                    if ([Text.Encoding]::ASCII.GetString($buffer) -ne $nonce) { throw 'readiness nonce mismatch' }
                    if ($process.HasExited) { throw "Python exited with code $($process.ExitCode) during readiness" }
                    return "http://127.0.0.1:$value"
                } catch { $lastProbe = $_.Exception.Message }
                finally { if ($response) { $response.Dispose() }; $request.Abort() }
            }
            Start-Sleep -Milliseconds 50
        }
        throw "Fixture readiness exceeded ${TimeoutMilliseconds}ms: $lastProbe"
    } catch {
        $reason = $_.Exception.Message
        try { Stop-FixtureProcess $c } catch { $reason += "; cleanup: $($_.Exception.Message)" }
        $diagnostic = "Fixture startup failed ($Python): $reason"
        foreach ($stream in 'Stdout', 'Stderr') {
            $log = Join-Path $c.T "fixture-$stream.log"
            if (Test-Path -LiteralPath $log) { $diagnostic += "`n${stream}: " + [IO.File]::ReadAllText($log) }
        }
        throw $diagnostic
    } finally {
        if (-not $c.Server) { $process.Dispose() }
    }
}
