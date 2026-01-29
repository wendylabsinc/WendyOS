#!/usr/bin/env python3
"""
Qwen3-VL Vision-Language Model Service

Provides HTTP API for generating image descriptions using Qwen3-VL-2B-Instruct.
Optimized for Jetson Orin Nano with INT4 quantization.

API Endpoints:
- POST /describe - Generate image description
- POST /question - Ask questions about an image
- GET /health - Health check
- GET /stats - GPU memory stats
"""

import logging
import time
import base64
from io import BytesIO
from datetime import datetime

import torch
from PIL import Image
from flask import Flask, request, jsonify
from transformers import AutoProcessor, Qwen3VLForConditionalGeneration, BitsAndBytesConfig
from qwen_vl_utils import process_vision_info

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)

# OpenTelemetry logging - ships logs to WendyOS OTel collector
try:
    from opentelemetry.sdk._logs import LoggerProvider
    from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
    from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
    from opentelemetry.sdk.resources import Resource
    from opentelemetry._logs import set_logger_provider
    import opentelemetry.sdk._logs as otel_logs

    resource = Resource.create({"service.name": "vlm"})
    logger_provider = LoggerProvider(resource=resource)
    logger_provider.add_log_record_processor(
        BatchLogRecordProcessor(OTLPLogExporter(endpoint="http://127.0.0.1:4318/v1/logs"))
    )
    set_logger_provider(logger_provider)

    otel_handler = otel_logs.LoggingHandler(logger_provider=logger_provider)
    logging.getLogger().addHandler(otel_handler)
    logger.info("OpenTelemetry logging enabled")
except ImportError:
    logger.warning("OpenTelemetry SDK not available - logs will not be shipped to OTel")
except Exception as e:
    logger.warning(f"Failed to initialize OpenTelemetry logging: {e}")

app = Flask(__name__)


@app.after_request
def add_cors_headers(response):
    response.headers['Access-Control-Allow-Origin'] = '*'
    response.headers['Access-Control-Allow-Methods'] = 'GET, POST, OPTIONS'
    response.headers['Access-Control-Allow-Headers'] = 'Content-Type'
    return response


# Global model and processor
model = None
processor = None
model_loaded = False

# Configuration
MODEL_PATH = "/app/models/Qwen3-VL-2B-Instruct"
MODEL_NAME = "Qwen3-VL-2B-Instruct"
MAX_IMAGE_SIZE = 672  # Resize large images for faster processing


def load_model():
    """Load Qwen3-VL model with INT4 quantization"""
    global model, processor, model_loaded

    logger.info("=" * 60)
    logger.info(f"Loading {MODEL_NAME} with INT4 quantization")
    logger.info(f"Model path: {MODEL_PATH}")
    logger.info("=" * 60)

    try:
        # Log environment info
        logger.info(f"PyTorch version: {torch.__version__}")
        logger.info(f"CUDA available: {torch.cuda.is_available()}")
        if torch.cuda.is_available():
            logger.info(f"CUDA device: {torch.cuda.get_device_name(0)}")
            total_mem = torch.cuda.get_device_properties(0).total_memory
            logger.info(f"GPU memory: {total_mem / 1024**3:.2f} GB")

        # Load processor
        logger.info("Loading processor...")
        processor = AutoProcessor.from_pretrained(
            MODEL_PATH,
            trust_remote_code=True,
            local_files_only=True
        )
        logger.info("Processor loaded successfully")

        # Configure INT4 quantization with NF4 (best memory efficiency)
        # Note: No skip_modules available for 4-bit, so entire model gets INT4
        logger.info("Configuring INT4 quantization with NF4...")
        quantization_config = BitsAndBytesConfig(
            load_in_4bit=True,
            bnb_4bit_compute_dtype=torch.float16,
            bnb_4bit_use_double_quant=True,
            bnb_4bit_quant_type="nf4"
        )

        # Load model with INT4 quantization
        logger.info("Loading model (this may take a minute)...")

        # Try Flash Attention 2 if available (significant speedup)
        attn_impl = None
        try:
            import flash_attn
            attn_impl = "flash_attention_2"
            logger.info("Flash Attention 2 available - enabling")
        except ImportError:
            logger.info("Flash Attention 2 not available - using default attention")

        model_kwargs = {
            "trust_remote_code": True,
            "quantization_config": quantization_config,
            "device_map": 'cuda',
            "local_files_only": True,
        }
        if attn_impl:
            model_kwargs["attn_implementation"] = attn_impl

        model = Qwen3VLForConditionalGeneration.from_pretrained(
            MODEL_PATH,
            **model_kwargs
        )

        # Set to eval mode
        model.eval()

        if torch.cuda.is_available():
            mem = torch.cuda.memory_allocated() / 1024**3
            logger.info(f"Model loaded - GPU memory: {mem:.2f}GB")

        # Note: torch.compile is incompatible with bitsandbytes quantized models
        # The compiled model loses the CB attribute needed for 4-bit inference
        # Skipping torch.compile when using INT4 quantization

        model_loaded = True

        # Log memory usage
        if torch.cuda.is_available():
            allocated = torch.cuda.memory_allocated() / 1024**3
            reserved = torch.cuda.memory_reserved() / 1024**3
            logger.info(f"GPU Memory: {allocated:.2f}GB allocated, {reserved:.2f}GB reserved")

        logger.info("=" * 60)
        logger.info("Model loaded successfully!")
        logger.info(f"Model: {MODEL_NAME}")
        logger.info("Quantization: INT4 (NF4)")
        logger.info("API available at http://0.0.0.0:8090")
        logger.info("=" * 60)

        return True

    except Exception as e:
        logger.error(f"Failed to load model: {e}", exc_info=True)
        model_loaded = False
        return False


def generate_response(image: Image.Image, prompt: str, max_tokens: int = 256) -> str:
    """Generate response for an image using the loaded model"""
    global model, processor

    if not model_loaded:
        raise RuntimeError("Model not loaded")

    # Prepare messages in Qwen VL format
    messages = [
        {
            "role": "user",
            "content": [
                {"type": "image", "image": image},
                {"type": "text", "text": prompt},
            ],
        }
    ]

    # Apply chat template
    text = processor.apply_chat_template(
        messages, tokenize=False, add_generation_prompt=True
    )

    # Process vision info
    image_inputs, video_inputs = process_vision_info(messages)

    # Prepare inputs
    inputs = processor(
        text=[text],
        images=image_inputs,
        videos=video_inputs,
        padding=True,
        return_tensors="pt",
    ).to("cuda")

    # Generate response with optimizations
    with torch.inference_mode():
        generated_ids = model.generate(
            **inputs,
            max_new_tokens=max_tokens,
            do_sample=False,  # Greedy decoding - faster than sampling
            use_cache=True,   # Enable KV cache
            pad_token_id=processor.tokenizer.pad_token_id,
        )

    # Decode output
    generated_ids_trimmed = [
        out_ids[len(in_ids):]
        for in_ids, out_ids in zip(inputs.input_ids, generated_ids)
    ]
    response = processor.batch_decode(
        generated_ids_trimmed,
        skip_special_tokens=True,
        clean_up_tokenization_spaces=False
    )[0]

    return response


@app.route('/health')
def health():
    """Health check endpoint"""
    return jsonify({
        'status': 'healthy' if model_loaded else 'loading',
        'model_loaded': model_loaded,
        'model_name': MODEL_NAME,
        'quantization': 'INT4',
        'timestamp': datetime.utcnow().isoformat()
    })


@app.route('/describe', methods=['POST'])
def describe():
    """Generate detailed description for an image"""
    if not model_loaded:
        return jsonify({'error': 'Model not loaded yet'}), 503

    request_start = time.time()

    try:
        data = request.json
        if not data or 'image' not in data:
            return jsonify({'error': 'Missing image in request'}), 400

        image_b64 = data['image']
        prompt = data.get('prompt', 'Describe this image in detail, including objects, people, activities, and scene context.')

        # Decode image
        try:
            image_bytes = base64.b64decode(image_b64)
            image = Image.open(BytesIO(image_bytes))
            if image.mode != 'RGB':
                image = image.convert('RGB')
            # Resize large images for faster processing
            if max(image.size) > MAX_IMAGE_SIZE:
                ratio = MAX_IMAGE_SIZE / max(image.size)
                new_size = (int(image.size[0] * ratio), int(image.size[1] * ratio))
                image = image.resize(new_size, Image.LANCZOS)
                logger.info(f"Resized image from {image.size} to {new_size}")
        except Exception as e:
            logger.error(f"Error decoding image: {e}")
            return jsonify({'error': 'Invalid image data'}), 400

        logger.info(f"Processing image of size {image.size}")

        # Generate description
        description = generate_response(image, prompt)

        total_time_ms = (time.time() - request_start) * 1000
        logger.info(f"Inference completed in {total_time_ms:.1f}ms")

        return jsonify({
            'description': description,
            'processing_time_ms': round(total_time_ms, 2),
            'model': MODEL_NAME,
            'quantization': 'INT4',
            'image_size': list(image.size)
        })

    except Exception as e:
        logger.error(f"Error processing request: {e}", exc_info=True)
        return jsonify({'error': str(e)}), 500


@app.route('/question', methods=['POST'])
def question():
    """Ask a specific question about an image"""
    if not model_loaded:
        return jsonify({'error': 'Model not loaded yet'}), 503

    request_start = time.time()

    try:
        data = request.json
        if not data or 'image' not in data or 'question' not in data:
            return jsonify({'error': 'Missing image or question in request'}), 400

        image_b64 = data['image']
        question_text = data['question']

        # Decode image
        try:
            image_bytes = base64.b64decode(image_b64)
            image = Image.open(BytesIO(image_bytes))
            if image.mode != 'RGB':
                image = image.convert('RGB')
            # Resize large images for faster processing
            if max(image.size) > MAX_IMAGE_SIZE:
                ratio = MAX_IMAGE_SIZE / max(image.size)
                new_size = (int(image.size[0] * ratio), int(image.size[1] * ratio))
                image = image.resize(new_size, Image.LANCZOS)
        except Exception as e:
            logger.error(f"Error decoding image: {e}")
            return jsonify({'error': 'Invalid image data'}), 400

        logger.info(f"Answering question: {question_text}")

        # Generate answer
        answer = generate_response(image, question_text)

        total_time_ms = (time.time() - request_start) * 1000

        return jsonify({
            'answer': answer,
            'question': question_text,
            'processing_time_ms': round(total_time_ms, 2),
            'model': MODEL_NAME
        })

    except Exception as e:
        logger.error(f"Error processing question: {e}", exc_info=True)
        return jsonify({'error': str(e)}), 500


@app.route('/stats')
def stats():
    """Get GPU memory stats"""
    if not torch.cuda.is_available():
        return jsonify({'error': 'CUDA not available'}), 500

    return jsonify({
        'cuda_available': torch.cuda.is_available(),
        'device_name': torch.cuda.get_device_name(0),
        'memory_allocated_gb': round(torch.cuda.memory_allocated() / 1024**3, 2),
        'memory_reserved_gb': round(torch.cuda.memory_reserved() / 1024**3, 2),
        'max_memory_allocated_gb': round(torch.cuda.max_memory_allocated() / 1024**3, 2),
        'model_loaded': model_loaded,
        'model_name': MODEL_NAME,
        'quantization': 'INT4'
    })


if __name__ == '__main__':
    # Load model at startup
    load_model()

    # Start Flask server
    app.run(
        host='0.0.0.0',
        port=8090,
        debug=False,
        threaded=True
    )
