# Go HelloWorld

Requires Go 1.24+. From the repository root:

```sh
cd examples/go
go run .
```

Expected output: `WendyHelloWorld() = 42`.

This is a separate Go module. Its `go.mod` replaces the Core dependency with the repository at `../..`. The call goes directly into Go, without C or a native-library build.
