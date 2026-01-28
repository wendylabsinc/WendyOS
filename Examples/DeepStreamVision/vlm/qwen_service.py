#!/usr/bin/env python3
"""
Qwen2.5-VL Vision-Language Model Service

Provides HTTP API for generating detailed image descriptions using Qwen2.5-VL-3B-Instruct.
Optimized for Jetson Orin Nano with INT4 quantization.

API Endpoints:
- POST /describe - Generate image description
- POST /question - Ask questions about an image
- GET /health - Health check
- GET /stats - GPU memory stats

Updated to use local model loading for fast iteration.
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
from transformers import AutoProcessor, Qwen2_5_VLForConditionalGeneration
from qwen_vl_utils import process_vision_info

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)

app = Flask(__name__)

# Global model and processor
model = None
processor = None
model_loaded = False


class Qwen25VLM:
    """Qwen2.5-VL Vision-Language Model wrapper"""

    def __init__(self, model_path: str = None):
        # Use direct path to pre-downloaded model (copied into image during build)
        self.model_path = model_path or "/app/models/Qwen2.5-VL-3B-Instruct"
        logger.info(f"Model path: {self.model_path}")
        self.model = None
        self.processor = None

    def load(self, use_int4: bool = True):
        """Load model with INT4 quantization for efficient edge deployment"""
        logger.info(f"Loading model from local directory: {self.model_path}")
        logger.info(f"INT4 quantization: {use_int4}")

        start_time = time.time()

        # Load processor from local model directory
        logger.info("Loading processor from local model...")
        self.processor = AutoProcessor.from_pretrained(
            self.model_path,
            trust_remote_code=True,
            local_files_only=True  # Use local files only, no downloads
        )
        logger.info("✅ Processor loaded successfully")

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

            self.model = Qwen2_5_VLForConditionalGeneration.from_pretrained(
                self.model_path,
                trust_remote_code=True,
                quantization_config=quantization_config,
                device_map='cuda',
                local_files_only=True  # Use local files only, no downloads
            )
        else:
            logger.info("Loading model with FP16 (higher memory usage)")
            self.model = Qwen2_5_VLForConditionalGeneration.from_pretrained(
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
            # Prepare messages in Qwen2-VL format
            messages = [
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "image",
                            "image": image,
                        },
                        {"type": "text", "text": prompt},
                    ],
                }
            ]

            # Prepare for inference
            text = self.processor.apply_chat_template(
                messages, tokenize=False, add_generation_prompt=True
            )
            image_inputs, video_inputs = process_vision_info(messages)
            inputs = self.processor(
                text=[text],
                images=image_inputs,
                videos=video_inputs,
                padding=True,
                return_tensors="pt",
            )
            inputs = inputs.to("cuda")

            # Generate response
            with torch.inference_mode():
                generated_ids = self.model.generate(
                    **inputs,
                    max_new_tokens=512,
                    temperature=0.7,
                    do_sample=True,
                )

            # Trim the generated IDs
            generated_ids_trimmed = [
                out_ids[len(in_ids):] for in_ids, out_ids in zip(inputs.input_ids, generated_ids)
            ]
            response = self.processor.batch_decode(
                generated_ids_trimmed, skip_special_tokens=True, clean_up_tokenization_spaces=False
            )[0]

            elapsed_ms = (time.time() - start_time) * 1000
            logger.info(f"Inference completed in {elapsed_ms:.1f}ms")

            return response

        except Exception as e:
            logger.error(f"Error during inference: {e}", exc_info=True)
            raise


def load_model():
    """Load Qwen2.5-VL model at startup"""
    global model, processor, model_loaded

    try:
        logger.info("="*60)
        logger.info("Starting Qwen2.5-VL VLM Service")
        logger.info("="*60)

        vlm = Qwen25VLM()
        vlm.load(use_int4=True)  # INT4 quantization - worked with transformers 4.x

        model = vlm.model
        processor = vlm.processor
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
        'model_name': 'Qwen2.5-VL-3B-Instruct',
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

        # Create VLM wrapper (uses global model/processor)
        vlm = Qwen25VLM()
        vlm.model = model
        vlm.processor = processor

        # Generate description
        description = vlm.describe(image, prompt)

        total_time_ms = (time.time() - request_start) * 1000

        return jsonify({
            'description': description,
            'processing_time_ms': round(total_time_ms, 2),
            'model': 'Qwen2.5-VL-3B-Instruct',
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
        vlm = Qwen25VLM()
        vlm.model = model
        vlm.processor = processor

        # Generate answer
        answer = vlm.describe(image, question_text)

        total_time_ms = (time.time() - request_start) * 1000

        return jsonify({
            'answer': answer,
            'question': question_text,
            'processing_time_ms': round(total_time_ms, 2),
            'model': 'Qwen2.5-VL-3B-Instruct'
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
