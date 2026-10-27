# C HelloWorld

Requires Go, Python 3.9+, and a C compiler. From the repository root:

```sh
python3 scripts/build.py native
cd examples/c
mkdir -p .build
```

Compile against the local public header and shared library for your host.

## macOS arm64

```sh
CORE_DIR=../../.build/native/darwin-arm64
cc -std=c11 -I../../include hello.c "$CORE_DIR/libWendyCore.dylib" \
  -Wl,-rpath,"$PWD/$CORE_DIR" -o .build/hello
.build/hello
```

For an Intel Mac, use `darwin-x64` instead.

## Linux x64

```sh
CORE_DIR=../../.build/native/linux-x64
cc -std=c11 -I../../include hello.c "$CORE_DIR/libWendyCore.so" \
  -Wl,-rpath,"$PWD/$CORE_DIR" -o .build/hello
.build/hello
```

For Linux arm64, use `linux-arm64` instead.

## Windows x64 (Git Bash / MinGW GCC)

Use the same MinGW GCC installation used to build Core, with `gcc` on `PATH`:

```sh
CORE_DIR=../../.build/native/win32-x64
gcc -std=c11 -I../../include hello.c "$CORE_DIR/WendyCore.dll" -o .build/hello.exe
cp "$CORE_DIR/WendyCore.dll" .build/
.build/hello.exe
```

On Windows, use `python` instead of `python3` if needed.

Expected output: `WendyHelloWorld() = 42`. The function is exported by the Go shared library; it is not implemented in the example.
