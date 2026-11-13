# Keep PowerShell variables out of Make's recipe string: a sh.exe on PATH can
# expand $LASTEXITCODE before powershell.exe receives an inline -Command.
param(
    [Parameter(Mandatory = $true)]
    [string]$LinkerFlags
)
$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force -Path bin | Out-Null
go build -ldflags $LinkerFlags -o bin/wendy.exe ./cmd/wendy
exit $LASTEXITCODE
