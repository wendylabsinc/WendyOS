$ErrorActionPreference = 'Stop'

function Show-Usage {
    @"
Usage: E2ESanitizeXUnit.ps1 (--run-dir DIR | --file FILE)

Sanitize Swift Testing xUnit XML by replacing XML 1.0-invalid control
characters with printable escape text. When changes are needed, the original
file is preserved next to the sanitized file with a .raw.xml suffix.
"@ | Write-Output
}

function Get-RawPath([string]$Path) {
    $directory = Split-Path -Parent $Path
    $leaf = Split-Path -Leaf $Path
    if ($leaf.EndsWith('.xml', [System.StringComparison]::OrdinalIgnoreCase)) {
        $rawLeaf = $leaf.Substring(0, $leaf.Length - 4) + '.raw.xml'
    } else {
        $rawLeaf = $leaf + '.raw'
    }
    return Join-Path $directory $rawLeaf
}

function ConvertTo-SanitizedXMLText([string]$Text, [ref]$Changed) {
    $builder = [System.Text.StringBuilder]::new($Text.Length)
    foreach ($character in $Text.ToCharArray()) {
        $code = [int][char]$character
        $isInvalidControl = $code -lt 0x20 -and $code -ne 0x09 -and $code -ne 0x0A -and $code -ne 0x0D
        if ($isInvalidControl) {
            [void]$builder.Append('\u{')
            [void]$builder.Append(('{0:X4}' -f $code))
            [void]$builder.Append('}')
            $Changed.Value = $true
        } else {
            [void]$builder.Append($character)
        }
    }
    return $builder.ToString()
}

$RunDir = $null
$ResultPath = $null
$i = 0
while ($i -lt $args.Count) {
    switch ($args[$i]) {
        '--run-dir' { $RunDir = $args[$i + 1]; $i += 2; continue }
        '--file' { $ResultPath = $args[$i + 1]; $i += 2; continue }
        '--help' { Show-Usage; exit 0 }
        '-h' { Show-Usage; exit 0 }
        default { throw "Unknown option: $($args[$i])" }
    }
}

if ($RunDir -and $ResultPath) { throw 'ERROR: pass either --run-dir or --file, not both.' }
if ($RunDir) { $ResultPath = Join-Path $RunDir 'test-results.xml' }
if (-not $ResultPath) { throw 'ERROR: --run-dir or --file is required.' }
if (-not (Test-Path -LiteralPath $ResultPath -PathType Leaf)) { exit 0 }

$ResultPath = (Resolve-Path -LiteralPath $ResultPath).Path
$rawPath = Get-RawPath $ResultPath
$utf8 = [System.Text.UTF8Encoding]::new($false)
$text = [System.IO.File]::ReadAllText($ResultPath, $utf8)
$changed = $false
$sanitized = ConvertTo-SanitizedXMLText $text ([ref]$changed)

if (-not $changed) { exit 0 }

Copy-Item -LiteralPath $ResultPath -Destination $rawPath -Force
[System.IO.File]::WriteAllText($ResultPath, $sanitized, $utf8)

Write-Output '==> Sanitized Swift Testing xUnit results'
Write-Output "    XML: $ResultPath"
Write-Output "    Raw: $rawPath"
