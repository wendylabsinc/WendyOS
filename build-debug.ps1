$env:CPATH="C:\vcpkg\installed\x64-windows\include"
$env:LIBRARY_PATH="C:\vcpkg\installed\x64-windows\lib"
Copy-Item C:\vcpkg\installed\x64-windows\lib\zlib.lib C:\vcpkg\installed\x64-windows\lib\z.lib
swift build --product wendy -c debug --static-swift-stdlib