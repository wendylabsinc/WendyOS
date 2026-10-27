# JavaScript HelloWorld — Node and browser

Requires Go, a C compiler, Python 3.9+, and Node 22+. From the repository root, build both local backends and install the example's on-disk dependency:

```sh
python3 scripts/build.py native
python3 scripts/build.py wasm
npm ci --prefix distributions/javascript
python3 scripts/build.py node
cd examples/javascript
npm install --ignore-scripts
```

On Windows, use `python` instead of `python3` if needed.

## Node

```sh
npm start
```

Expected output: `WendyHelloWorld() = 42`.

## Browser

From `examples/javascript`, serve the checkout (including the local npm dependency):

```sh
python3 -m http.server 8080 --bind 127.0.0.1 --directory ../..
```

Open <http://127.0.0.1:8080/examples/javascript/> in a modern browser. The page and console display `WendyHelloWorld() = 42`. Stop the server with Ctrl-C. Use HTTP, not a `file://` URL.

Both runtimes execute the same `hello.mjs` with the same package import. Node uses npm's conditional export and calls the Go shared library. The browser import map selects that local package's browser entry, which runs the Go WASM build. No bundler, browser mock, or published Core package is needed.
