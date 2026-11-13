# Read-only native identity/driver snapshot. Never opens a COM handle.
$ErrorActionPreference = 'Stop'
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
$system = Get-CimInstance Win32_ComputerSystemProduct
$os = Get-CimInstance Win32_OperatingSystem
$drivers = @(Get-CimInstance Win32_PnPSignedDriver)
$ports = @(Get-CimInstance Win32_PnPEntity | Where-Object {
    $_.PNPClass -eq 'Ports' -and $_.Present -and $_.PNPDeviceID -like 'USB\VID_*'
} | ForEach-Object {
    $device = $_
    $driver = @($drivers | Where-Object { $_.DeviceID -eq $device.PNPDeviceID })
    if ($driver.Count -ne 1) { throw "Missing or ambiguous serial driver for $($device.PNPDeviceID)" }
    $location = Get-PnpDeviceProperty -InstanceId $device.PNPDeviceID -KeyName DEVPKEY_Device_LocationPaths
    $match = [regex]::Match(($device.Name + ' ' + $device.Caption), '\(COM[0-9]+\)')
    if (-not $match.Success) { throw "No COM identity for $($device.PNPDeviceID)" }
    [ordered]@{
        port = $match.Value.Trim('(', ')'); instance_id = $device.PNPDeviceID
        status = $device.Status; error_code = $device.ConfigManagerErrorCode
        locations = @($location.Data); driver_version = $driver[0].DriverVersion
        driver_provider = $driver[0].DriverProviderName; driver_inf = $driver[0].InfName
        driver_signed = $driver[0].IsSigned
    }
})
[ordered]@{
    machine_uuid = $system.UUID.ToLowerInvariant(); sid = $identity.User.Value
    os_build = ($os.Version + '/' + $os.BuildNumber)
    elevated = $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    ports = @($ports)
} | ConvertTo-Json -Depth 8 -Compress
