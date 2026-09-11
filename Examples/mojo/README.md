# Mojo 1.0 and MAX tutorials

Runnable source for the docs' **Mojo and MAX** tutorial group. The tutorial pages contain the same files so readers can create each project without cloning the repository.

| Directory | What it runs | Runtime |
| --- | --- | --- |
| [hello-world](hello-world/) | Greeting and integer SIMD expression | Compiled Mojo 1.0.0, CPU |
| [simple-web-server](simple-web-server/) | Mojo entry point calling a Python HTTP bridge | Mojo 1.0.0 and CPython, port 8080 |
| [max-graph](max-graph/) | ReLU of two summed vectors, checked against NumPy | MAX 26.5.0, CPU or GPU |
| [max-inference-service](max-inference-service/) | MiniLM text embeddings via `/v1/embeddings` | MAX 26.5.0, CPU or GPU, port 8000 |

Install the Wendy CLI and run Docker on the development machine. Follow the [Mojo CPU requirements](https://mojolang.org/docs/requirements/) and [MAX requirements](https://max.modular.com/stable/packages/). The toolchains stay inside the images.

Set your default device with the discovery picker:

```sh
wendy discover
```

Select your device with the arrow keys, then press `d` to make it the default. Press Ctrl+C to leave discovery. If you already know its hostname, you can set it directly instead:

```sh
wendy device set-default your-device.local
```

From this repository's root, run an example on the default device:

```sh
wendy run --no-restart --prefix Examples/mojo/hello-world
```

Change `--prefix` to run another example. Use `--no-restart` for the finite hello-world and graph programs so they finish once; Wendy normally restarts apps unless you stop them. Run the web and embedding servers without that flag. An attached run stops its server on Ctrl+C. If the image-diff transfer stalls with your agent version, add `--chunking off` to use the standard transfer path.

The MAX examples default to CPU. For GPU runs, add `{ "type": "gpu" }` to the selected example's `wendy.json` entitlements, then pass:

- Graph: `--env MAX_DEVICE=gpu`
- Embedding service: `--env MAX_DEVICE=gpu:0`

The embedding image includes a pinned MiniLM model revision and disables remote telemetry and Hugging Face downloads at runtime. Its build requires internet access. After the server is ready, verify actual inference from the development machine:

```sh
python3 Examples/mojo/max-inference-service/verify.py http://your-device.local:8000
```

Replace `your-device.local` with your device's hostname. The client checks model registration, response indexes, two finite nonzero 384-dimensional vectors, and computes cosine similarity.

Tutorial source:

- [Hello world](../../go/internal/cli/assets/docs/guides/tutorials/mojo/hello-world.mdx)
- [Web server](../../go/internal/cli/assets/docs/guides/tutorials/mojo/simple-web-server.mdx)
- [MAX graph](../../go/internal/cli/assets/docs/guides/tutorials/mojo/max-graph.mdx)
- [MAX embeddings](../../go/internal/cli/assets/docs/guides/tutorials/mojo/max-inference-service.mdx)

## Hardware validation

Validated on an 8 GB NVIDIA Jetson Orin Nano (ARM64, SM87) with WendyOS 0.19.1, JetPack 7.2, and CUDA 13.2. Deployment used the Wendy CLI's Docker builder and `--chunking off`.

- Mojo hello world printed the greeting and `[3, 5, 7, 9]`, then exited with code 0.
- The Mojo/Python web server returned the expected JSON for `/` and `/health`, and HTTP 404 for `/missing`.
- The MAX graph matched NumPy on both CPU and GPU: `[0.0, 3.0, 0.0, 4.0]`.
- MiniLM serving passed `verify.py` on CPU and GPU, returning two finite, nonzero 384-dimensional vectors from the packaged local model.

These checks establish correctness for this configuration, not throughput or support for every Jetson software version or model size.
