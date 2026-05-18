#requires -Version 5.1
<#
.SYNOPSIS
  Fresh Windows 11 setup for Wendy development machines.

.DESCRIPTION
  Idempotently installs and configures common tools in the same spirit as
  set-up-ubuntu.sh. Run from an elevated PowerShell session.
#>

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$TraceCommands = if ($env:TRACE_COMMANDS) { $env:TRACE_COMMANDS } else { '1' }
$ScriptDir = if ($PSScriptRoot) { $PSScriptRoot } else { Split-Path -Parent $MyInvocation.MyCommand.Path }
$RepoRoot = (Resolve-Path (Join-Path $ScriptDir '..')).Path
$CurrentIdentity = [System.Security.Principal.WindowsIdentity]::GetCurrent()
$CurrentPrincipal = [System.Security.Principal.WindowsPrincipal]::new($CurrentIdentity)
$CurrentUser = $CurrentIdentity.Name
$CurrentUserName = [Environment]::UserName
$UserProfile = [Environment]::GetFolderPath('UserProfile')
$UserSid = $CurrentIdentity.User.Value

$script:ConfigureGit = $true
$script:GitName = ''
$script:GitEmail = ''
$script:AuthorizedLoginKeys = [System.Collections.Generic.List[string]]::new()
$script:InstallVisualStudioBuildTools = $true
$script:InstallSwiftToolchain = $true
$script:InstallWendyCli = $true
$script:ConfigureRemoteDesktop = $false
$script:ConfigurePowerSettings = $true
$script:EnableDeveloperMode = $true

function Write-Bold { param([string]$Message) Write-Host $Message -ForegroundColor White }
function Write-Info { param([string]$Message) Write-Host "`n==> $Message" -ForegroundColor Blue }
function Write-Ok { param([string]$Message) Write-Host "OK $Message" -ForegroundColor Green }
function Write-Warn { param([string]$Message) Write-Host "! $Message" -ForegroundColor Yellow }
function Fail { param([string]$Message) Write-Host "Error: $Message" -ForegroundColor Red; exit 1 }

function Invoke-External {
  param(
    [Parameter(Mandatory)][string]$FilePath,
    [Parameter()][string[]]$Arguments = @()
  )

  if ($TraceCommands -ne '0') {
    Write-Host ('+ {0} {1}' -f $FilePath, ($Arguments -join ' ')) -ForegroundColor DarkGray
  }

  & $FilePath @Arguments
  if ($LASTEXITCODE -ne 0) {
    throw "$FilePath exited with code $LASTEXITCODE"
  }
}

function Ask-YesNo {
  param(
    [Parameter(Mandatory)][string]$Prompt,
    [Parameter()][bool]$Default = $false
  )

  $suffix = if ($Default) { '[Y/n]' } else { '[y/N]' }
  while ($true) {
    $answer = Read-Host "$Prompt $suffix"
    if ([string]::IsNullOrWhiteSpace($answer)) { return $Default }
    switch -Regex ($answer.Trim()) {
      '^(y|yes)$' { return $true }
      '^(n|no)$' { return $false }
      default { Write-Warn 'Please answer yes or no.' }
    }
  }
}

function Test-IsAdministrator {
  return $CurrentPrincipal.IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Require-Windows11 {
  if (-not (Test-IsAdministrator)) {
    Fail 'Run this script from an elevated PowerShell session (Run as Administrator).'
  }

  $currentVersion = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion'
  $build = [int]$currentVersion.CurrentBuildNumber
  if ($build -lt 22000) {
    Fail "This script is intended for Windows 11; detected build $build."
  }
}

function Collect-Configuration {
  Write-Bold 'Fresh Windows 11 setup'
  Write-Host @'

This script is idempotent: it is safe to run repeatedly. Existing packages,
keys, SSH settings, PATH entries, and git settings will be reused or updated
without creating duplicates.
'@

  if (Ask-YesNo 'Configure global git identity?' $true) {
    $script:GitName = Read-Host 'Git user.name (leave empty to skip git configuration)'
    if ([string]::IsNullOrWhiteSpace($script:GitName)) {
      $script:ConfigureGit = $false
    } else {
      $script:GitEmail = Read-Host 'Git user.email (leave empty to skip git configuration)'
      $script:ConfigureGit = -not [string]::IsNullOrWhiteSpace($script:GitEmail)
      if (-not $script:ConfigureGit) { $script:GitName = '' }
    }
  } else {
    $script:ConfigureGit = $false
  }

  if (Ask-YesNo "Install additional SSH public keys into $CurrentUserName's authorized_keys?" $true) {
    Write-Host 'Paste one public key per prompt. Leave empty when done.'
    while ($true) {
      $key = Read-Host ('SSH public key {0}' -f ($script:AuthorizedLoginKeys.Count + 1))
      if ([string]::IsNullOrWhiteSpace($key)) { break }
      $script:AuthorizedLoginKeys.Add($key.Trim())
    }
  }

  $script:InstallVisualStudioBuildTools = Ask-YesNo 'Install Visual Studio Build Tools for C++/Swift builds?' $true
  $script:InstallSwiftToolchain = Ask-YesNo 'Install the Swift toolchain?' $true
  $script:InstallWendyCli = Ask-YesNo 'Install or update the Wendy CLI?' $true
  $script:EnableDeveloperMode = Ask-YesNo 'Enable Windows Developer Mode?' $true
  $script:ConfigureRemoteDesktop = Ask-YesNo 'Enable Remote Desktop?' $false
  $script:ConfigurePowerSettings = Ask-YesNo 'Disable AC sleep/blanking and screen locking?' $true
}

function Confirm-Plan {
  $gitSummary = if ($script:ConfigureGit) {
    "Global git user.name ($($script:GitName)) and user.email ($($script:GitEmail)) for $CurrentUser"
  } else {
    'Global git identity will not be changed'
  }

  $sshKeySummary = if ($script:AuthorizedLoginKeys.Count -gt 0) {
    "$($script:AuthorizedLoginKeys.Count) additional authorized SSH public key(s) for $CurrentUserName"
  } else {
    'No additional authorized SSH public keys'
  }

  $vsSummary = if ($script:InstallVisualStudioBuildTools) { 'Visual Studio Build Tools will be installed' } else { 'Visual Studio Build Tools will not be installed' }
  $swiftSummary = if ($script:InstallSwiftToolchain) { 'Swift toolchain will be installed' } else { 'Swift toolchain will not be installed' }
  $wendySummary = if ($script:InstallWendyCli) { 'Wendy CLI will be installed or updated' } else { 'Wendy CLI will not be installed' }
  $rdpSummary = if ($script:ConfigureRemoteDesktop) { 'Remote Desktop will be enabled' } else { 'Remote Desktop will not be changed' }
  $powerSummary = if ($script:ConfigurePowerSettings) { 'AC sleep/blanking and screen locking will be disabled' } else { 'Power and lock settings will not be changed' }
  $developerModeSummary = if ($script:EnableDeveloperMode) { 'Windows Developer Mode will be enabled' } else { 'Windows Developer Mode will not be changed' }

  Write-Host @"

This script will configure this machine by doing the following:

  - Install packages:
      OpenSSH server/client first, then Git, Go, Neovim, PowerShell 7,
      $swiftSummary
      $vsSummary

  - Configure:
      SSH login via OpenSSH Server as early as possible
      SSH key generation for $CurrentUserName
      $sshKeySummary
      Neovim as the default CLI editor
      $ScriptDir\bin on $CurrentUserName's user PATH
      $developerModeSummary
      Network discovery services and firewall rules
      $rdpSummary
      $powerSummary
      $wendySummary
      $gitSummary

This script does not install wendy-agent because the agent runs on Linux
devices. It installs the Windows Wendy CLI instead.
"@

  if (-not (Ask-YesNo 'Continue?' $false)) {
    Write-Host 'Aborted.'
    exit 0
  }
}

function Require-Winget {
  if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
    Fail 'winget was not found. Install App Installer from Microsoft Store, then rerun this script.'
  }
}

function Update-ProcessPath {
  $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  $env:Path = @($machinePath, $userPath) -join ';'
}

function Add-UserPathEntry {
  param([Parameter(Mandatory)][string]$PathEntry)

  $expanded = [Environment]::ExpandEnvironmentVariables($PathEntry)
  $current = [Environment]::GetEnvironmentVariable('Path', 'User')
  $entries = @()
  if (-not [string]::IsNullOrWhiteSpace($current)) {
    $entries = @($current -split ';' | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
  }

  $alreadyPresent = $false
  foreach ($entry in $entries) {
    if ([string]::Equals([Environment]::ExpandEnvironmentVariables($entry).TrimEnd('\'), $expanded.TrimEnd('\'), [System.StringComparison]::OrdinalIgnoreCase)) {
      $alreadyPresent = $true
      break
    }
  }

  if (-not $alreadyPresent) {
    $newPath = if ($entries.Count -gt 0) { ($entries + $PathEntry) -join ';' } else { $PathEntry }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
  }

  Update-ProcessPath
}

function Test-WingetPackageInstalled {
  param([Parameter(Mandatory)][string]$Id)

  $output = & winget list --id $Id --exact --source winget --disable-interactivity 2>$null
  return ($LASTEXITCODE -eq 0 -and ($output -match [regex]::Escape($Id)))
}

function Install-WingetPackage {
  param(
    [Parameter(Mandatory)][string]$Id,
    [Parameter(Mandatory)][string]$Name,
    [string]$Override = ''
  )

  if (Test-WingetPackageInstalled $Id) {
    Write-Ok "$Name is already installed"
    return
  }

  Write-Info "Installing $Name"
  $args = @(
    'install', '--id', $Id, '--exact', '--source', 'winget',
    '--accept-source-agreements', '--accept-package-agreements',
    '--disable-interactivity', '--silent'
  )
  if (-not [string]::IsNullOrWhiteSpace($Override)) {
    $args += @('--override', $Override)
  }

  Invoke-External 'winget' $args
  Update-ProcessPath
  Write-Ok "$Name installed"
}

function Install-OpenSshPackages {
  Write-Info 'Installing OpenSSH server/client first'

  foreach ($capabilityName in @('OpenSSH.Client~~~~0.0.1.0', 'OpenSSH.Server~~~~0.0.1.0')) {
    $capability = Get-WindowsCapability -Online -Name $capabilityName
    if ($capability.State -ne 'Installed') {
      Write-Host "Installing Windows capability $capabilityName"
      Add-WindowsCapability -Online -Name $capabilityName | Out-Null
    }
  }

  Set-Service -Name sshd -StartupType Automatic
  Start-Service -Name sshd -ErrorAction SilentlyContinue

  if (-not (Get-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -DisplayName 'OpenSSH Server (sshd)' -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22 | Out-Null
  } else {
    Enable-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' | Out-Null
  }

  Write-Ok 'OpenSSH packages installed'
}

function Set-SshdConfigValue {
  param(
    [Parameter(Mandatory)][string]$Path,
    [Parameter(Mandatory)][string]$Key,
    [Parameter(Mandatory)][string]$Value
  )

  if (-not (Test-Path $Path)) {
    New-Item -ItemType File -Path $Path -Force | Out-Null
  }

  $lines = @(Get-Content -Path $Path -ErrorAction SilentlyContinue)
  $found = $false
  $pattern = '^\s*#?\s*' + [regex]::Escape($Key) + '(\s+|$)'
  for ($i = 0; $i -lt $lines.Count; $i++) {
    if ($lines[$i] -match $pattern) {
      if (-not $found) {
        $lines[$i] = "$Key $Value"
        $found = $true
      }
    }
  }

  if (-not $found) {
    $lines += "$Key $Value"
  }

  Set-Content -Path $Path -Value $lines -Encoding ascii
}

function Configure-Ssh {
  Write-Info 'Enabling SSH login'

  $sshDir = Join-Path $env:ProgramData 'ssh'
  New-Item -ItemType Directory -Path $sshDir -Force | Out-Null
  $sshdConfig = Join-Path $sshDir 'sshd_config'

  Set-SshdConfigValue -Path $sshdConfig -Key 'PasswordAuthentication' -Value 'yes'
  Set-SshdConfigValue -Path $sshdConfig -Key 'KbdInteractiveAuthentication' -Value 'yes'
  Set-SshdConfigValue -Path $sshdConfig -Key 'PubkeyAuthentication' -Value 'yes'

  Restart-Service -Name sshd
  Write-Ok 'SSH login enabled'
}

function Protect-UserSshDirectory {
  param([Parameter(Mandatory)][string]$Path)
  Invoke-External 'icacls.exe' @($Path, '/inheritance:r', '/grant:r', "*${UserSid}:(OI)(CI)F", '*S-1-5-18:(OI)(CI)F') | Out-Null
}

function Protect-UserSshFile {
  param([Parameter(Mandatory)][string]$Path)
  Invoke-External 'icacls.exe' @($Path, '/inheritance:r', '/grant:r', "*${UserSid}:F", '*S-1-5-18:F') | Out-Null
}

function Protect-AdminAuthorizedKeysFile {
  param([Parameter(Mandatory)][string]$Path)
  Invoke-External 'icacls.exe' @($Path, '/inheritance:r', '/grant:r', '*S-1-5-32-544:F', '*S-1-5-18:F') | Out-Null
}

function Add-AuthorizedKey {
  param(
    [Parameter(Mandatory)][string]$Path,
    [Parameter(Mandatory)][string]$Key
  )

  $parts = $Key -split '\s+'
  if ($parts.Count -lt 2) {
    Write-Warn "Skipping malformed SSH public key: $Key"
    return
  }

  $keyType = $parts[0]
  $keyBody = $parts[1]
  $existing = @()
  if (Test-Path $Path) { $existing = @(Get-Content -Path $Path) }

  foreach ($line in $existing) {
    $lineParts = $line -split '\s+'
    if ($lineParts.Count -ge 2 -and $lineParts[0] -eq $keyType -and $lineParts[1] -eq $keyBody) {
      return
    }
  }

  Add-Content -Path $Path -Value $Key -Encoding ascii
}

function Configure-SshKeys {
  Write-Info 'Generating SSH keys and installing authorized login keys'

  $sshDir = Join-Path $UserProfile '.ssh'
  New-Item -ItemType Directory -Path $sshDir -Force | Out-Null
  Protect-UserSshDirectory $sshDir

  $privateKey = Join-Path $sshDir 'id_ed25519'
  $publicKey = Join-Path $sshDir 'id_ed25519.pub'

  if (-not (Test-Path $privateKey)) {
    Invoke-External 'ssh-keygen.exe' @('-t', 'ed25519', '-a', '100', '-N', '', '-C', "$CurrentUserName@$env:COMPUTERNAME-$(Get-Date -Format yyyyMMdd)", '-f', $privateKey)
  } elseif (-not (Test-Path $publicKey)) {
    Invoke-External 'ssh-keygen.exe' @('-y', '-f', $privateKey) | Out-File -FilePath $publicKey -Encoding ascii
  }

  Protect-UserSshFile $privateKey
  if (Test-Path $publicKey) { Protect-UserSshFile $publicKey }

  $authorizedKeys = Join-Path $sshDir 'authorized_keys'
  if (-not (Test-Path $authorizedKeys)) { New-Item -ItemType File -Path $authorizedKeys -Force | Out-Null }

  foreach ($key in $script:AuthorizedLoginKeys) {
    Add-AuthorizedKey -Path $authorizedKeys -Key $key
  }
  Protect-UserSshFile $authorizedKeys

  if (Test-IsAdministrator -and $script:AuthorizedLoginKeys.Count -gt 0) {
    $adminKeys = Join-Path (Join-Path $env:ProgramData 'ssh') 'administrators_authorized_keys'
    if (-not (Test-Path $adminKeys)) { New-Item -ItemType File -Path $adminKeys -Force | Out-Null }
    foreach ($key in $script:AuthorizedLoginKeys) {
      Add-AuthorizedKey -Path $adminKeys -Key $key
    }
    Protect-AdminAuthorizedKeysFile $adminKeys
  }

  Write-Ok 'SSH keys configured'
}

function Install-Packages {
  Require-Winget

  Write-Info 'Updating winget sources'
  Invoke-External 'winget' @('source', 'update', '--disable-interactivity')
  Write-Ok 'winget sources updated'

  Install-WingetPackage -Id 'Git.Git' -Name 'Git'
  Install-WingetPackage -Id 'GoLang.Go' -Name 'Go'
  Install-WingetPackage -Id 'Neovim.Neovim' -Name 'Neovim'
  Install-WingetPackage -Id 'Microsoft.PowerShell' -Name 'PowerShell 7'

  if ($script:InstallVisualStudioBuildTools) {
    Install-WingetPackage `
      -Id 'Microsoft.VisualStudio.2022.BuildTools' `
      -Name 'Visual Studio Build Tools' `
      -Override '--wait --passive --norestart --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended'
  } else {
    Write-Ok 'Visual Studio Build Tools not installed'
  }

  if ($script:InstallSwiftToolchain) {
    Install-WingetPackage -Id 'Swift.Toolchain' -Name 'Swift toolchain'
  } else {
    Write-Ok 'Swift toolchain not installed'
  }
}

function Configure-Editor {
  Write-Info 'Setting Neovim as the default CLI editor'
  [Environment]::SetEnvironmentVariable('EDITOR', 'nvim', 'User')
  [Environment]::SetEnvironmentVariable('VISUAL', 'nvim', 'User')
  $env:EDITOR = 'nvim'
  $env:VISUAL = 'nvim'
  Write-Ok 'Neovim is the default editor for new shells'
}

function Configure-WendyUtilitiesPath {
  Write-Info 'Adding utilities bin directory to the user PATH'

  $binDir = Join-Path $ScriptDir 'bin'
  if (-not (Test-Path $binDir)) {
    Write-Warn "$binDir does not exist; adding it to PATH anyway."
  }

  Add-UserPathEntry $binDir
  Write-Ok 'utilities bin directory is on the user PATH'
}

function Configure-DeveloperMode {
  if (-not $script:EnableDeveloperMode) {
    Write-Ok 'Developer Mode not changed'
    return
  }

  Write-Info 'Enabling Windows Developer Mode'
  $path = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\AppModelUnlock'
  if (-not (Test-Path $path)) { New-Item -Path $path -Force | Out-Null }
  New-ItemProperty -Path $path -Name 'AllowDevelopmentWithoutDevLicense' -Value 1 -PropertyType DWord -Force | Out-Null
  Write-Ok 'Developer Mode enabled'
}

function Configure-NetworkDiscovery {
  Write-Info 'Enabling network discovery services and mDNS-friendly firewall rules'

  foreach ($serviceName in @('Dnscache', 'FDResPub', 'fdPHost')) {
    $service = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
    if ($service) {
      Set-Service -Name $serviceName -StartupType Automatic
      Start-Service -Name $serviceName -ErrorAction SilentlyContinue
    }
  }

  Get-NetFirewallRule -DisplayGroup 'Network Discovery' -ErrorAction SilentlyContinue | Enable-NetFirewallRule | Out-Null

  $mdnsRule = Get-NetFirewallRule -DisplayName 'mDNS (UDP 5353-In)' -ErrorAction SilentlyContinue
  if ($mdnsRule) {
    Enable-NetFirewallRule -DisplayName 'mDNS (UDP 5353-In)' | Out-Null
  } else {
    New-NetFirewallRule `
      -DisplayName 'mDNS (UDP 5353-In)' `
      -Direction Inbound `
      -Action Allow `
      -Protocol UDP `
      -LocalPort 5353 | Out-Null
  }

  Write-Ok 'network discovery and mDNS firewall rules enabled where supported'
}

function Configure-RemoteDesktopAccess {
  if (-not $script:ConfigureRemoteDesktop) {
    Write-Ok 'Remote Desktop not changed'
    return
  }

  Write-Info 'Enabling Remote Desktop'
  Set-ItemProperty -Path 'HKLM:\System\CurrentControlSet\Control\Terminal Server' -Name 'fDenyTSConnections' -Value 0
  Enable-NetFirewallRule -DisplayGroup 'Remote Desktop' -ErrorAction SilentlyContinue | Out-Null
  Set-Service -Name TermService -StartupType Manual
  Start-Service -Name TermService -ErrorAction SilentlyContinue
  Write-Ok 'Remote Desktop enabled where supported by this Windows edition'
}

function Configure-PowerSettings {
  if (-not $script:ConfigurePowerSettings) {
    Write-Ok 'power settings not changed'
    return
  }

  Write-Info 'Disabling automatic sleep/blanking on AC power and screen locking'

  Invoke-External 'powercfg.exe' @('/change', 'standby-timeout-ac', '0')
  Invoke-External 'powercfg.exe' @('/change', 'monitor-timeout-ac', '0')
  Invoke-External 'powercfg.exe' @('/change', 'hibernate-timeout-ac', '0')

  $desktopPath = 'HKCU:\Control Panel\Desktop'
  Set-ItemProperty -Path $desktopPath -Name 'ScreenSaveActive' -Value '0'
  Set-ItemProperty -Path $desktopPath -Name 'ScreenSaverIsSecure' -Value '0'
  Set-ItemProperty -Path $desktopPath -Name 'ScreenSaveTimeOut' -Value '0'

  Write-Ok 'AC power policy configured; screen locking disabled for the current user'
}

function Install-WendyCli {
  if (-not $script:InstallWendyCli) {
    Write-Ok 'Wendy CLI not installed'
    return
  }

  Write-Info 'Installing or updating Wendy CLI'

  $arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64'; break }
    'ARM64' { 'arm64'; break }
    default { Fail "Unsupported Windows architecture: $env:PROCESSOR_ARCHITECTURE" }
  }

  $repo = 'wendylabsinc/wendy-agent'
  $tag = if ($env:WENDY_VERSION) {
    $env:WENDY_VERSION
  } else {
    (Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest" -UseBasicParsing).tag_name
  }

  if ([string]::IsNullOrWhiteSpace($tag)) { Fail 'Could not determine latest Wendy CLI version.' }

  $version = $tag.TrimStart('v')
  $artifact = "wendy-cli-windows-$arch-$version.zip"
  $url = "https://github.com/$repo/releases/download/$tag/$artifact"
  $installDir = Join-Path $UserProfile 'bin'
  $tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("wendy-cli-$([System.Guid]::NewGuid())")
  $zipPath = Join-Path $tempDir $artifact

  New-Item -ItemType Directory -Path $installDir -Force | Out-Null
  New-Item -ItemType Directory -Path $tempDir -Force | Out-Null

  try {
    Invoke-WebRequest -Uri $url -OutFile $zipPath -UseBasicParsing
    Expand-Archive -Path $zipPath -DestinationPath $tempDir -Force
    $source = Join-Path $tempDir "wendy-cli-windows-$arch\wendy.exe"
    if (-not (Test-Path $source)) { Fail "Downloaded archive did not contain $source" }
    Copy-Item -Path $source -Destination (Join-Path $installDir 'wendy.exe') -Force
  } finally {
    Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
  }

  Add-UserPathEntry $installDir
  Write-Ok "Wendy CLI installed to $installDir\wendy.exe"
}

function Configure-Git {
  if (-not $script:ConfigureGit) {
    Write-Ok 'git identity not changed'
    return
  }

  Write-Info "Configuring git identity for $CurrentUser"
  Update-ProcessPath
  Invoke-External 'git' @('config', '--global', 'user.name', $script:GitName)
  Invoke-External 'git' @('config', '--global', 'user.email', $script:GitEmail)
  Write-Ok 'git identity configured'
}

function Write-Summary {
  $ipAddress = (Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
    Where-Object { $_.IPAddress -notlike '169.254.*' -and $_.IPAddress -ne '127.0.0.1' } |
    Select-Object -First 1 -ExpandProperty IPAddress)

  $publicKeyPath = Join-Path (Join-Path $UserProfile '.ssh') 'id_ed25519.pub'
  $publicKey = if (Test-Path $publicKeyPath) { Get-Content -Path $publicKeyPath -Raw } else { 'not available' }

  if ([string]::IsNullOrWhiteSpace($ipAddress)) { $ipAddress = 'unknown' }

  Write-Bold "`nSetup complete"
  Write-Host @"

Useful connection details:
  Username:        $CurrentUserName
  Computer name:   $env:COMPUTERNAME
  mDNS name:       $env:COMPUTERNAME.local
  Primary IP:      $ipAddress
  SSH:             ssh $CurrentUserName@$env:COMPUTERNAME.local
  Remote Desktop:  RDP to $env:COMPUTERNAME.local if Remote Desktop is supported/enabled

Generated SSH public key:
  $($publicKey.Trim())

Open a new terminal for PATH/editor/environment changes to appear.
"@
}

function Main {
  Require-Windows11
  Collect-Configuration
  Confirm-Plan
  Install-OpenSshPackages
  Configure-Ssh
  Configure-SshKeys
  Install-Packages
  Configure-Editor
  Configure-WendyUtilitiesPath
  Configure-DeveloperMode
  Configure-NetworkDiscovery
  Configure-RemoteDesktopAccess
  Configure-PowerSettings
  Install-WendyCli
  Configure-Git
  Write-Summary
}

try {
  Main
} catch {
  Write-Host "`nError: setup failed: $($_.Exception.Message)" -ForegroundColor Red
  exit 1
}
