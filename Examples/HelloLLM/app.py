#!/usr/bin/env python3
"""
HelloLLM - Text Generation on NVIDIA Jetson using HuggingFace Transformers

Demonstrates GPU-accelerated inference with a small language model (DistilGPT2).
Based on the NVIDIA Jetson AI Lab API Examples tutorial.

Runs a demo, then starts a web interface for interactive prompts.
"""

import sys
import os
import time
import argparse
from threading import Thread

# Global model and tokenizer (loaded once, used by web server)
model = None
tokenizer = None
gpu_available = False


def print_section(title):
    """Print formatted section header"""
    print(f"\n{'=' * 60}")
    print(f"  {title}")
    print("=" * 60)


def check_gpu_availability():
    """Verify CUDA/GPU is available for PyTorch"""
    import torch

    print_section("GPU Availability Check")

    print(f"PyTorch Version: {torch.__version__}")
    print(f"CUDA Built: {torch.version.cuda or 'No'}")
    print(f"CUDA Available: {torch.cuda.is_available()}")

    if torch.cuda.is_available():
        print(f"CUDA Version: {torch.version.cuda}")
        print(f"cuDNN Version: {torch.backends.cudnn.version()}")
        print(f"GPU Count: {torch.cuda.device_count()}")
        print(f"GPU Name: {torch.cuda.get_device_name(0)}")

        props = torch.cuda.get_device_properties(0)
        print(f"GPU Memory: {props.total_memory / 1024**3:.2f} GB")
        print(f"Compute Capability: {props.major}.{props.minor}")
        return True
    else:
        print("\nWARNING: CUDA not available")
        print("Common causes:")
        print("  - GPU entitlement not configured in wendy.json")
        print("  - NVIDIA container runtime not available")
        print("  - Running on non-GPU hardware")
        print("\nWill use CPU (significantly slower)")
        return False


def load_model(model_name="distilbert/distilgpt2"):
    """Load the model and tokenizer"""
    from transformers import AutoModelForCausalLM, AutoTokenizer
    import torch

    print_section(f"Loading Model: {model_name}")

    device = "cuda" if torch.cuda.is_available() else "cpu"
    print(f"Target device: {device}")

    start_time = time.time()

    # Load tokenizer
    print("Loading tokenizer...")
    tok = AutoTokenizer.from_pretrained(model_name)

    # Set pad token if not set (required for batch generation)
    if tok.pad_token is None:
        tok.pad_token = tok.eos_token

    # Load model with GPU placement and float16 for efficiency
    print("Loading model (this may take a moment on first run)...")
    mdl = AutoModelForCausalLM.from_pretrained(
        model_name,
        torch_dtype=torch.float16 if device == "cuda" else torch.float32,
    )
    mdl = mdl.to(device)

    load_time = time.time() - start_time
    print(f"Model loaded in {load_time:.2f} seconds")
    print(f"Model device: {next(mdl.parameters()).device}")

    # Report memory usage
    if device == "cuda":
        allocated = torch.cuda.memory_allocated() / 1024**2
        print(f"GPU Memory Used: {allocated:.2f} MB")

    return mdl, tok


def generate_text_simple(mdl, tok, prompt, max_new_tokens=50):
    """Simple text generation (non-streaming)"""
    import torch

    print_section("Text Generation")
    print(f"Prompt: \"{prompt}\"")
    print(f"Max new tokens: {max_new_tokens}")

    # Encode input
    inputs = tok(prompt, return_tensors="pt").to(mdl.device)

    # Generate
    start_time = time.time()
    with torch.no_grad():
        outputs = mdl.generate(
            **inputs,
            max_new_tokens=max_new_tokens,
            do_sample=True,
            temperature=0.7,
            top_p=0.9,
            pad_token_id=tok.eos_token_id,
        )
    gen_time = time.time() - start_time

    # Decode
    generated_text = tok.decode(outputs[0], skip_special_tokens=True)
    num_tokens = outputs.shape[1] - inputs["input_ids"].shape[1]
    tokens_per_sec = num_tokens / gen_time if gen_time > 0 else 0

    print(f"\nGenerated {num_tokens} tokens in {gen_time:.2f}s ({tokens_per_sec:.1f} tok/s)")
    print("-" * 40)
    print(generated_text)
    print("-" * 40)

    return generated_text, tokens_per_sec


def generate_text_streaming(mdl, tok, prompt, max_new_tokens=100):
    """Streaming text generation with TextIteratorStreamer"""
    from transformers import TextIteratorStreamer
    import torch

    print_section("Streaming Text Generation")
    print(f"Prompt: \"{prompt}\"")
    print(f"Max new tokens: {max_new_tokens}")
    print("\nGenerating (tokens appear as produced):")
    print("-" * 40)

    # Encode input
    inputs = tok(prompt, return_tensors="pt").to(mdl.device)

    # Setup streamer
    streamer = TextIteratorStreamer(
        tok, skip_prompt=True, skip_special_tokens=True
    )

    # Generation parameters
    generation_kwargs = dict(
        **inputs,
        max_new_tokens=max_new_tokens,
        do_sample=True,
        temperature=0.7,
        top_p=0.9,
        streamer=streamer,
        pad_token_id=tok.eos_token_id,
    )

    # Run generation in background thread
    start_time = time.time()
    thread = Thread(target=mdl.generate, kwargs=generation_kwargs)
    thread.start()

    # Stream output tokens
    generated_text = ""
    for text in streamer:
        print(text, end="", flush=True)
        generated_text += text

    thread.join()
    gen_time = time.time() - start_time

    print()
    print("-" * 40)
    print(f"Generation time: {gen_time:.2f} seconds")

    return generated_text


def run_benchmark(mdl, tok, num_iterations=3):
    """Run simple benchmark for performance measurement"""
    import torch

    print_section("Performance Benchmark")

    prompt = "The future of artificial intelligence"
    max_tokens = 50

    print(f"Prompt: \"{prompt}\"")
    print(f"Tokens per iteration: {max_tokens}")
    print(f"Iterations: {num_iterations}")
    print()

    times = []
    for i in range(num_iterations):
        inputs = tok(prompt, return_tensors="pt").to(mdl.device)

        if torch.cuda.is_available():
            torch.cuda.synchronize()
        start = time.time()

        with torch.no_grad():
            outputs = mdl.generate(
                **inputs,
                max_new_tokens=max_tokens,
                do_sample=False,  # Deterministic for benchmarking
                pad_token_id=tok.eos_token_id,
            )

        if torch.cuda.is_available():
            torch.cuda.synchronize()
        elapsed = time.time() - start
        times.append(elapsed)
        print(f"  Iteration {i + 1}: {elapsed:.3f}s")

    avg_time = sum(times) / len(times)
    tokens_per_sec = max_tokens / avg_time

    print(f"\nResults:")
    print(f"  Average generation time: {avg_time:.3f}s")
    print(f"  Throughput: {tokens_per_sec:.1f} tokens/second")

    return tokens_per_sec


def run_demo(mdl, tok, has_gpu):
    """Run the demo with predefined prompts"""
    prompts = [
        "Once upon a time in a land far away,",
        "The key to success in programming is",
        "Artificial intelligence will change the world by",
    ]

    # Simple generation demo
    generate_text_simple(mdl, tok, prompts[0], max_new_tokens=50)

    # Streaming generation demo
    generate_text_streaming(mdl, tok, prompts[1], max_new_tokens=75)

    # Additional generation
    generate_text_simple(mdl, tok, prompts[2], max_new_tokens=50)

    # Benchmark
    throughput = run_benchmark(mdl, tok)

    # Summary
    print_section("Demo Summary")
    print("Demo completed successfully!")
    print()
    if has_gpu:
        print(f"  GPU: ENABLED")
        print(f"  Throughput: {throughput:.1f} tokens/second")
    else:
        print("  GPU: NOT AVAILABLE (used CPU)")
    print()

    return throughput


def generate_for_web(prompt, max_new_tokens=100):
    """Generate text for web interface"""
    import torch

    global model, tokenizer

    inputs = tokenizer(prompt, return_tensors="pt").to(model.device)

    start_time = time.time()
    with torch.no_grad():
        outputs = model.generate(
            **inputs,
            max_new_tokens=max_new_tokens,
            do_sample=True,
            temperature=0.7,
            top_p=0.9,
            pad_token_id=tokenizer.eos_token_id,
        )
    gen_time = time.time() - start_time

    generated_text = tokenizer.decode(outputs[0], skip_special_tokens=True)
    num_tokens = outputs.shape[1] - inputs["input_ids"].shape[1]
    tokens_per_sec = num_tokens / gen_time if gen_time > 0 else 0

    return {
        "text": generated_text,
        "tokens": num_tokens,
        "time": round(gen_time, 2),
        "tokens_per_sec": round(tokens_per_sec, 1),
    }


def create_web_app():
    """Create Flask web application"""
    from flask import Flask, request, jsonify

    app = Flask(__name__)

    HTML_PAGE = """
<!DOCTYPE html>
<html>
<head>
    <title>HelloLLM - Jetson Text Generation</title>
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
        * { box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            max-width: 800px;
            margin: 0 auto;
            padding: 20px;
            background: #1a1a2e;
            color: #eee;
        }
        h1 { color: #76b900; margin-bottom: 5px; }
        .subtitle { color: #888; margin-bottom: 30px; }
        .input-group { margin-bottom: 20px; }
        label { display: block; margin-bottom: 8px; font-weight: 500; }
        textarea {
            width: 100%;
            padding: 12px;
            border: 2px solid #333;
            border-radius: 8px;
            font-size: 16px;
            background: #16213e;
            color: #eee;
            resize: vertical;
        }
        textarea:focus { outline: none; border-color: #76b900; }
        .controls {
            display: flex;
            gap: 15px;
            align-items: center;
            flex-wrap: wrap;
        }
        button {
            background: #76b900;
            color: #000;
            border: none;
            padding: 12px 30px;
            border-radius: 8px;
            font-size: 16px;
            font-weight: 600;
            cursor: pointer;
        }
        button:hover { background: #8ed100; }
        button:disabled { background: #444; color: #888; cursor: not-allowed; }
        .slider-group {
            display: flex;
            align-items: center;
            gap: 10px;
        }
        input[type="range"] { width: 120px; }
        .output-box {
            background: #16213e;
            border: 2px solid #333;
            border-radius: 8px;
            padding: 20px;
            min-height: 150px;
            white-space: pre-wrap;
            word-wrap: break-word;
            font-family: 'Courier New', monospace;
            line-height: 1.6;
        }
        .stats {
            margin-top: 15px;
            padding: 10px 15px;
            background: #0f3460;
            border-radius: 6px;
            font-size: 14px;
            color: #76b900;
        }
        .loading {
            color: #888;
            font-style: italic;
        }
        .gpu-badge {
            display: inline-block;
            padding: 4px 10px;
            border-radius: 4px;
            font-size: 12px;
            font-weight: 600;
            margin-left: 10px;
        }
        .gpu-enabled { background: #76b900; color: #000; }
        .gpu-disabled { background: #e94560; color: #fff; }
    </style>
</head>
<body>
    <h1>HelloLLM
        <span class="gpu-badge """ + ("gpu-enabled" if gpu_available else "gpu-disabled") + """">
            """ + ("GPU Enabled" if gpu_available else "CPU Only") + """
        </span>
    </h1>
    <p class="subtitle">Text Generation on NVIDIA Jetson using DistilGPT2</p>

    <div class="input-group">
        <label for="prompt">Enter a prompt (the model will complete your text):</label>
        <textarea id="prompt" rows="3" placeholder="Once upon a time...">The future of artificial intelligence is</textarea>
    </div>

    <div class="controls">
        <button id="generate" onclick="generateText()">Generate</button>
        <div class="slider-group">
            <label for="tokens">Max tokens:</label>
            <input type="range" id="tokens" min="20" max="200" value="100">
            <span id="tokensValue">100</span>
        </div>
    </div>

    <div class="input-group" style="margin-top: 25px;">
        <label>Generated Output:</label>
        <div id="output" class="output-box">Generated text will appear here...</div>
        <div id="stats" class="stats" style="display: none;"></div>
    </div>

    <script>
        document.getElementById('tokens').addEventListener('input', function() {
            document.getElementById('tokensValue').textContent = this.value;
        });

        async function generateText() {
            const prompt = document.getElementById('prompt').value;
            const maxTokens = document.getElementById('tokens').value;
            const button = document.getElementById('generate');
            const output = document.getElementById('output');
            const stats = document.getElementById('stats');

            if (!prompt.trim()) {
                output.textContent = 'Please enter a prompt.';
                return;
            }

            button.disabled = true;
            button.textContent = 'Generating...';
            output.innerHTML = '<span class="loading">Generating text...</span>';
            stats.style.display = 'none';

            try {
                const response = await fetch('/generate', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ prompt: prompt, max_tokens: parseInt(maxTokens) })
                });

                const data = await response.json();

                if (data.error) {
                    output.textContent = 'Error: ' + data.error;
                } else {
                    output.textContent = data.text;
                    stats.innerHTML = `Generated <strong>${data.tokens}</strong> tokens in <strong>${data.time}s</strong> (<strong>${data.tokens_per_sec}</strong> tokens/sec)`;
                    stats.style.display = 'block';
                }
            } catch (error) {
                output.textContent = 'Error: ' + error.message;
            }

            button.disabled = false;
            button.textContent = 'Generate';
        }

        // Allow Ctrl+Enter to generate
        document.getElementById('prompt').addEventListener('keydown', function(e) {
            if (e.ctrlKey && e.key === 'Enter') {
                generateText();
            }
        });
    </script>
</body>
</html>
"""

    @app.route("/")
    def index():
        return HTML_PAGE

    @app.route("/generate", methods=["POST"])
    def generate():
        try:
            data = request.get_json()
            prompt = data.get("prompt", "")
            max_tokens = data.get("max_tokens", 100)

            if not prompt:
                return jsonify({"error": "No prompt provided"})

            max_tokens = max(20, min(200, int(max_tokens)))
            result = generate_for_web(prompt, max_tokens)
            return jsonify(result)

        except Exception as e:
            return jsonify({"error": str(e)})

    return app


def main():
    global model, tokenizer, gpu_available

    parser = argparse.ArgumentParser(
        description="HelloLLM - Text Generation on NVIDIA Jetson"
    )
    parser.add_argument(
        "--skip-demo",
        action="store_true",
        help="Skip the demo and go directly to web server",
    )
    parser.add_argument(
        "--demo-only",
        action="store_true",
        help="Run demo only, skip web server",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=8080,
        help="Port for web server (default: 8080)",
    )
    args = parser.parse_args()

    print("=" * 60)
    print("  HelloLLM - Text Generation on NVIDIA Jetson")
    print("  Using HuggingFace Transformers + GPU Acceleration")
    print("=" * 60)

    # Check GPU
    gpu_available = check_gpu_availability()

    # Load model
    model, tokenizer = load_model("distilbert/distilgpt2")

    # Run demo unless skipped
    if not args.skip_demo:
        run_demo(model, tokenizer, gpu_available)

    # Start web server unless demo-only
    if not args.demo_only:
        print_section("Starting Web Interface")
        print(f"Web interface available at: http://wendyos-device-name.local:{args.port}")
        print("Press Ctrl+C to stop the server")
        print()

        app = create_web_app()
        app.run(host="0.0.0.0", port=args.port, debug=False)

    print("\nModel: distilbert/distilgpt2 (82M parameters)")
    print("This example is based on the NVIDIA Jetson AI Lab tutorials.")

    return 0


if __name__ == "__main__":
    try:
        exit_code = main()
        sys.exit(exit_code)
    except KeyboardInterrupt:
        print("\n\nInterrupted by user")
        sys.exit(1)
    except Exception as e:
        print(f"\n\nError: {e}")
        import traceback

        traceback.print_exc()
        sys.exit(1)
