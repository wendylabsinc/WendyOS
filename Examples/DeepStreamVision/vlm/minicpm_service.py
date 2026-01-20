#!/usr/bin/env python3
"""
MiniCPM-V Vision-Language Model Service

Provides HTTP API for generating detailed image descriptions using MiniCPM-V-2.6.
Optimized for Jetson Orin Nano with INT4 quantization.

API Endpoints:
- POST /describe - Generate image description
- POST /question - Ask questions about an image
- GET /health - Health check
- GET /stats - GPU memory stats

Updated to use MODEL_PATH environment variable for local model loading.
"""

import logging
import time
import base64
import os
from io import BytesIO
from typing import Dict, Optional
from datetime import datetime

import torch
from PIL import Image
from flask import Flask, request, jsonify
from transformers import AutoModel, AutoTokenizer

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)

app = Flask(__name__)

# Global model and tokenizer
model = None
tokenizer = None
model_loaded = False


class MiniCPMVLM:
    """MiniCPM-V Vision-Language Model wrapper"""

    def __init__(self, model_path: str = None):
        # Use direct path to pre-downloaded model (copied into image during build)
        # Model is kept separate from Docker build cache for faster iteration
        self.model_path = model_path or "/app/models/MiniCPM-V-2_6"
        logger.info(f"Model path: {self.model_path}")
        self.model = None
        self.tokenizer = None

    def load(self, use_int4: bool = True):
        """Load model with INT4 quantization for efficient edge deployment"""
        logger.info(f"Loading model from local directory: {self.model_path}")
        logger.info(f"INT4 quantization: {use_int4}")

        start_time = time.time()

        # Load tokenizer from local model directory
        logger.info("Loading tokenizer from local model...")
        self.tokenizer = AutoTokenizer.from_pretrained(
            self.model_path,
            trust_remote_code=True,
            local_files_only=True  # Use local files only, no downloads
        )
        logger.info("✅ Tokenizer loaded successfully")

        # Load model with quantization using Jetson-compatible bitsandbytes
        if use_int4:
            logger.info("Loading model with INT4 quantization (Jetson-optimized bitsandbytes)")
            from transformers import BitsAndBytesConfig

            quantization_config = BitsAndBytesConfig(
                load_in_4bit=True,
                bnb_4bit_compute_dtype=torch.float16,
                bnb_4bit_use_double_quant=True,
                bnb_4bit_quant_type="nf4"
            )

            self.model = AutoModel.from_pretrained(
                self.model_path,
                trust_remote_code=True,
                quantization_config=quantization_config,
                device_map='cuda',
                local_files_only=True  # Use local files only, no downloads
            )
        else:
            logger.info("Loading model with FP16 (higher memory usage)")
            self.model = AutoModel.from_pretrained(
                self.model_path,
                trust_remote_code=True,
                torch_dtype=torch.float16,
                device_map='cuda',
                local_files_only=True  # Use local files only, no downloads
            )

        # Set to eval mode
        self.model.eval()

        elapsed = time.time() - start_time
        logger.info(f"✅ Model loaded successfully in {elapsed:.1f}s")

        # Print memory usage
        if torch.cuda.is_available():
            memory_allocated = torch.cuda.memory_allocated() / 1024**3
            memory_reserved = torch.cuda.memory_reserved() / 1024**3
            logger.info(f"GPU Memory: {memory_allocated:.2f}GB allocated, {memory_reserved:.2f}GB reserved")

    def describe(self, image: Image.Image, prompt: str = "Describe this image in detail.") -> str:
        """Generate detailed description for an image"""
        start_time = time.time()

        try:
            # Prepare messages
            msgs = [{'role': 'user', 'content': prompt}]

            # Generate response
            with torch.inference_mode():
                response = self.model.chat(
                    image=image,
                    msgs=msgs,
                    tokenizer=self.tokenizer,
                    sampling=True,
                    temperature=0.7,
                    max_new_tokens=512
                )

            elapsed_ms = (time.time() - start_time) * 1000
            logger.info(f"Inference completed in {elapsed_ms:.1f}ms")

            return response

        except Exception as e:
            logger.error(f"Error during inference: {e}", exc_info=True)
            raise


def load_model():
    """Load MiniCPM-V model at startup"""
    global model, tokenizer, model_loaded

    try:
        logger.info("="*60)
        logger.info("Starting MiniCPM-V VLM Service")
        logger.info("="*60)

        vlm = MiniCPMVLM()
        vlm.load(use_int4=True)  # INT4 quantization using Jetson-optimized bitsandbytes

        model = vlm.model
        tokenizer = vlm.tokenizer
        model_loaded = True

        logger.info("="*60)
        logger.info("✅ Service ready to accept requests")
        logger.info("API available at http://0.0.0.0:8090")
        logger.info("="*60)

    except Exception as e:
        logger.error(f"Failed to load model: {e}", exc_info=True)
        model_loaded = False


@app.route('/health')
def health():
    """Health check endpoint"""
    return jsonify({
        'status': 'healthy' if model_loaded else 'loading',
        'model_loaded': model_loaded,
        'model_name': 'MiniCPM-V-2.6',
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
        # Parse request
        data = request.json
        if not data or 'image' not in data:
            return jsonify({'error': 'Missing image in request'}), 400

        image_b64 = data['image']
        prompt = data.get('prompt', 'Describe this image in detail, including objects, people, activities, and scene context.')

        # Decode image
        try:
            image_bytes = base64.b64decode(image_b64)
            image = Image.open(BytesIO(image_bytes))

            # Convert to RGB if needed
            if image.mode != 'RGB':
                image = image.convert('RGB')

        except Exception as e:
            logger.error(f"Error decoding image: {e}")
            return jsonify({'error': 'Invalid image data'}), 400

        logger.info(f"Processing image of size {image.size}")

        # Create VLM wrapper (uses global model/tokenizer)
        vlm = MiniCPMVLM()
        vlm.model = model
        vlm.tokenizer = tokenizer

        # Generate description
        description = vlm.describe(image, prompt)

        total_time_ms = (time.time() - request_start) * 1000

        return jsonify({
            'description': description,
            'processing_time_ms': round(total_time_ms, 2),
            'model': 'MiniCPM-V-2.6',
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
        # Parse request
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

        except Exception as e:
            logger.error(f"Error decoding image: {e}")
            return jsonify({'error': 'Invalid image data'}), 400

        logger.info(f"Answering question: {question_text}")

        # Create VLM wrapper
        vlm = MiniCPMVLM()
        vlm.model = model
        vlm.tokenizer = tokenizer

        # Generate answer
        answer = vlm.describe(image, question_text)

        total_time_ms = (time.time() - request_start) * 1000

        return jsonify({
            'answer': answer,
            'question': question_text,
            'processing_time_ms': round(total_time_ms, 2),
            'model': 'MiniCPM-V-2.6'
        })

    except Exception as e:
        logger.error(f"Error processing question: {e}", exc_info=True)
        return jsonify({'error': str(e)}), 500


@app.route('/stats')
def stats():
    """Get GPU memory stats"""
    if not torch.cuda.is_available():
        return jsonify({'error': 'CUDA not available'}), 500

    stats = {
        'cuda_available': torch.cuda.is_available(),
        'device_name': torch.cuda.get_device_name(0),
        'memory_allocated_gb': round(torch.cuda.memory_allocated() / 1024**3, 2),
        'memory_reserved_gb': round(torch.cuda.memory_reserved() / 1024**3, 2),
        'max_memory_allocated_gb': round(torch.cuda.max_memory_allocated() / 1024**3, 2),
        'model_loaded': model_loaded
    }

    return jsonify(stats)


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
