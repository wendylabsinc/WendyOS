$env:CPATH="C:\vcpkg\installed\x64-windows\include"
$env:LIBRARY_PATH="C:\vcpkg\installed\x64-windows\lib"
Copy-Item C:\vcpkg\installed\x64-windows\lib\zlib.lib C:\vcpkg\installed\x64-windows\lib\z.lib
swift build --product wendy -c release --static-swift-stdlib

# Copy Swift runtime DLLs to build output directory
$BuildDir = swift build --show-bin-path -c release --static-swift-stdlib
$SwiftBin = (Get-Command swift).Source | Split-Path
$SwiftRuntime = Join-Path (Split-Path $SwiftBin) "bin"

$SwiftDlls = @(
    "swift_Concurrency.dll",
    "swiftCore.dll",
    "Foundation.dll",
    "FoundationNetworking.dll",
    "FoundationXML.dll",
    "dispatch.dll",
    "BlocksRuntime.dll",
    "swiftWinSDK.dll",
    "swiftCRT.dll",
    "swiftDispatch.dll",
    "swiftDistributed.dll",
    "swiftObservation.dll",
    "swiftRegex.dll",
    "swiftSwiftOnoneSupport.dll",
    "swift_RegexParser.dll",
    "swift_StringProcessing.dll",
    "swiftSynchronization.dll"
)

Write-Host "Copying Swift runtime DLLs to $BuildDir"
foreach ($dll in $SwiftDlls) {
    $dllPath = Join-Path $SwiftRuntime $dll
    if (Test-Path $dllPath) {
        Write-Host "  Copying $dll"
        Copy-Item $dllPath -Destination $BuildDir
    }
}

# Also copy zlib
Copy-Item "C:\vcpkg\installed\x64-windows\bin\zlib1.dll" -Destination $BuildDir
Write-Host "Build complete. Output in $BuildDir"
