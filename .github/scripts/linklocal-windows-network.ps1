param(
    [Parameter(Mandatory=$true)][ValidateSet('Snapshot','VerifySignature','Configure','Reverse','Cleanup')][string]$Action,
    [string]$Plan,
    [string]$DLL
)
$ErrorActionPreference = 'Stop'
if ($env:GITHUB_ACTIONS -ne 'true' -or $env:RUNNER_ENVIRONMENT -ne 'github-hosted' -or $env:OS -ne 'Windows_NT') {
    throw 'Windows link-local fixture requires the disposable hosted Windows runner.'
}
if ($Action -eq 'VerifySignature') {
    $signature = Get-AuthenticodeSignature -LiteralPath $DLL
    if ($signature.Status -ne 'Valid') { throw 'Pinned Wintun DLL signature is not valid.' }
    @{status=$signature.Status.ToString();subject=$signature.SignerCertificate.Subject} | ConvertTo-Json -Compress
    exit
}
function Snapshot {
    $adapters = @(Get-NetAdapter -IncludeHidden | Select-Object Name,InterfaceGuid,InterfaceIndex,InterfaceDescription,PnPDeviceID)
    $routes = @(Get-NetRoute -AddressFamily IPv4 -PolicyStore ActiveStore | Select-Object DestinationPrefix,NextHop,InterfaceIndex,RouteMetric)
    $addresses = @(Get-NetIPAddress -AddressFamily IPv4 | Select-Object IPAddress,PrefixLength,InterfaceIndex)
    $devices = @(Get-PnpDevice -Class Net | Where-Object { $_.InstanceId -like 'SWD\WINTUN\*' -or $_.FriendlyName -match 'Wintun' } | Select-Object InstanceId,FriendlyName)
    $drivers = @(Get-CimInstance Win32_SystemDriver | Where-Object { $_.Name -eq 'Wintun' } | Select-Object Name,State)
    return @{adapters=$adapters;routes=$routes;addresses=$addresses;wintun_devices=$devices;wintun_drivers=$drivers}
}
if ($Action -eq 'Snapshot') { Snapshot | ConvertTo-Json -Depth 8 -Compress; exit }
$p = Get-Content -Raw -LiteralPath $Plan | ConvertFrom-Json
if ($p.adapters.Count -ne 2 -or $p.nonce -notmatch '^[a-f0-9]{32}$') { throw 'Invalid owned adapter plan.' }
$owned = @()
foreach ($expected in $p.adapters) {
    if ($expected.name -notmatch '^wendy-ll-[a-f0-9]{12}-[ab]$' -or $expected.guid -notmatch '^\{[a-f0-9-]{36}\}$') { throw 'Invalid adapter identity.' }
    $actual = @(Get-NetAdapter -IncludeHidden | Where-Object { $_.InterfaceGuid.ToString().Trim('{}') -eq $expected.guid.Trim('{}') })
    if ($actual.Count -gt 1) { throw 'Ambiguous owned adapter GUID.' }
    if ($actual.Count -eq 0) {
        if ($Action -ne 'Cleanup') { throw "Owned adapter missing: $($expected.name)" }
        continue
    }
    if ($actual[0].Name -ne $expected.name -or $actual[0].InterfaceDescription -notmatch '^Wintun') { throw 'Refusing a replacement or foreign adapter.' }
    $owned += @{expected=$expected;actual=$actual[0]}
}
function RouteKeys($routes) {
    return @($routes | ForEach-Object { "$($_.DestinationPrefix)|$($_.NextHop)|$($_.InterfaceIndex)|$($_.RouteMetric)" } | Sort-Object)
}
function Assert-PreservedHost {
    $indices = @($owned | ForEach-Object { $_.actual.InterfaceIndex })
    $now = Snapshot
    $foreign = @($now.routes | Where-Object { $_.InterfaceIndex -notin $indices })
    if (Compare-Object (RouteKeys $p.baseline.routes) (RouteKeys $foreign)) { throw 'Nonowned IPv4 routes changed; no fixture repair is permitted.' }
    $metadata = @($foreign | Where-Object { $_.DestinationPrefix -eq '169.254.169.254/32' })
    if ($metadata.Count -lt 1) { throw 'Protected explicit metadata route is missing.' }
    if (@($foreign | Where-Object { $_.DestinationPrefix -like '169.254.*' -and $_.DestinationPrefix -notlike '*/32' }).Count) { throw 'Unsafe foreign link-local route.' }
}
if ($Action -eq 'Cleanup') {
    $cleanupErrors = @()
    try {
        $rule = Get-NetFirewallRule -Name $p.firewall_name -ErrorAction SilentlyContinue
        if ($rule) {
            if ($rule.Group -ne "Wendy link-local owned $($p.nonce)") { throw 'Refusing replacement firewall rule.' }
            $rule | Remove-NetFirewallRule
        }
    } catch { $cleanupErrors += $_.Exception.Message }
    foreach ($item in $owned) {
        try {
            $instance = $item.actual.PnPDeviceID
            if (-not $instance -or $instance -notlike 'SWD\WINTUN\*') { throw 'Refusing non-Wintun PnP cleanup.' }
            # Exact instance only, never a device class or driver package.
            $remove = Start-Process -FilePath "$env:SystemRoot\System32\pnputil.exe" -ArgumentList @('/remove-device', "`"$instance`"") -PassThru -NoNewWindow
            try {
                if (-not $remove.WaitForExit(8000)) { $remove.Kill(); $remove.WaitForExit(2000) | Out-Null; throw 'Owned adapter removal timed out.' }
                if ($remove.ExitCode -ne 0) { throw 'Owned adapter removal failed.' }
            } finally { $remove.Dispose() }
        } catch { $cleanupErrors += $_.Exception.Message }
    }
    foreach ($expected in $p.adapters) {
        if (@(Get-NetAdapter -IncludeHidden | Where-Object { $_.InterfaceGuid.ToString().Trim('{}') -eq $expected.guid.Trim('{}') }).Count) { $cleanupErrors += 'Owned adapter remains after cleanup.' }
    }
    # Owned routes disappear with their adapters. Wait for the kernel's table
    # update, never repair an existing route. The driver package stays on this
    # disposable runner and is never globally uninstalled by the fixture.
    $owned = @()
    $until = [DateTime]::UtcNow.AddSeconds(5)
    while ($true) {
        try { Assert-PreservedHost; break }
        catch {
            if ([DateTime]::UtcNow -ge $until) { $cleanupErrors += $_.Exception.Message; break }
            Start-Sleep -Milliseconds 100
        }
    }
    # The admitted baseline had no Wintun devices, including nonpresent ones.
    # Creation may have been interrupted before an interface GUID was exposed.
    # Report such orphan instances; never broaden removal beyond owned GUIDs.
    $remainingDevices = @(Get-PnpDevice -Class Net | Where-Object { $_.InstanceId -like 'SWD\WINTUN\*' -or $_.FriendlyName -match 'Wintun' })
    if ($remainingDevices.Count) { $cleanupErrors += "Unexpected Wintun PnP devices remain: $($remainingDevices.InstanceId -join ', ')" }
    if ($cleanupErrors.Count) { throw ($cleanupErrors -join '; ') }
    exit
}
Assert-PreservedHost
foreach ($item in $owned) {
    $index = $item.actual.InterfaceIndex
    $expected = $item.expected
    $metric = if ($Action -eq 'Reverse') { if ($expected.role -eq 'a') { 7000 } else { 5000 } } else { [int]$expected.metric }
    Set-NetIPInterface -InterfaceIndex $index -AddressFamily IPv4 -AutomaticMetric Disabled -InterfaceMetric $metric -PolicyStore ActiveStore
    if ($Action -eq 'Configure') {
        # Static addressing disables DHCP on this newly created interface. No
        # default gateway, DNS change, existing route or host adapter is edited.
        if ($expected.host -notin @('169.254.100.1','169.254.200.1')) { throw 'Invalid fixture IP.' }
        New-NetIPAddress -InterfaceIndex $index -AddressFamily IPv4 -IPAddress $expected.host -PrefixLength 16 -PolicyStore ActiveStore | Out-Null
        $readyUntil = [DateTime]::UtcNow.AddSeconds(8)
        while (-not (Get-NetIPAddress -InterfaceIndex $index -AddressFamily IPv4 -IPAddress $expected.host | Where-Object AddressState -eq 'Preferred')) {
            if ([DateTime]::UtcNow -gt $readyUntil) { throw 'Owned IPv4 address never became preferred.' }
            Start-Sleep -Milliseconds 100
        }
    }
}
Assert-PreservedHost

if ($Action -eq 'Configure') {
    if (Get-NetFirewallRule -Name $p.firewall_name -ErrorAction SilentlyContinue) { throw 'Firewall rule identity already exists.' }
    New-NetFirewallRule -Name $p.firewall_name -DisplayName $p.firewall_name -Group "Wendy link-local owned $($p.nonce)" -Direction Inbound -Action Allow -Protocol TCP -Program $p.binary -InterfaceAlias @($owned | ForEach-Object { $_.actual.Name }) -LocalAddress @($p.adapters | ForEach-Object { $_.host }) -RemoteAddress @($p.adapters | ForEach-Object { $_.peer }) -Profile Any | Out-Null
}
