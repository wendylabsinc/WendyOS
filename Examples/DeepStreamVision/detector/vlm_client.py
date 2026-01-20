#!/usr/bin/env python3
"""
VLM Client for DeepStream Detector

Provides integration between YOLO detector and MiniCPM-V VLM service.
Sends high-confidence detections to VLM for detailed descriptions.

Usage:
    vlm = VLMClient(service_url="http://localhost:8090")

    # In detector probe callback:
    if detection.confidence > 0.8:
        crop = frame[y1:y2, x1:x2]
        description = vlm.describe(crop)
        print(f"YOLO: {detection.class}")
        print(f"VLM:  {description}")
"""

import base64
import logging
from io import BytesIO
from typing import Optional, Dict
import requests
import numpy as np
from PIL import Image

logger = logging.getLogger(__name__)


class VLMClient:
    """Client for MiniCPM-V VLM service"""

    def __init__(self, service_url: str = "http://localhost:8090", timeout: float = 30.0):
        """
        Initialize VLM client

        Args:
            service_url: URL of VLM service
            timeout: Request timeout in seconds
        """
        self.service_url = service_url
        self.timeout = timeout
        self.available = False

        # Check if service is available
        self._check_availability()

    def _check_availability(self):
        """Check if VLM service is available"""
        try:
            response = requests.get(
                f"{self.service_url}/health",
                timeout=2.0
            )
            if response.status_code == 200:
                data = response.json()
                self.available = data.get('model_loaded', False)
                if self.available:
                    logger.info(f"✅ VLM service available at {self.service_url}")
                else:
                    logger.warning(f"VLM service responding but model not loaded yet")
            else:
                self.available = False
                logger.warning(f"VLM service not available: {response.status_code}")
        except Exception as e:
            self.available = False
            logger.warning(f"VLM service not available: {e}")

    def _numpy_to_base64(self, image: np.ndarray) -> str:
        """
        Convert numpy array to base64 string

        Args:
            image: Numpy array (H, W, 3) in BGR format (OpenCV style)

        Returns:
            Base64 encoded JPEG string
        """
        # Convert BGR to RGB
        if len(image.shape) == 3 and image.shape[2] == 3:
            image_rgb = image[:, :, ::-1]
        else:
            image_rgb = image

        # Convert to PIL Image
        pil_image = Image.fromarray(image_rgb.astype('uint8'))

        # Encode as JPEG
        buffer = BytesIO()
        pil_image.save(buffer, format='JPEG', quality=85)
        img_bytes = buffer.getvalue()

        # Base64 encode
        img_b64 = base64.b64encode(img_bytes).decode('utf-8')

        return img_b64

    def describe(
        self,
        image: np.ndarray,
        prompt: str = "Describe this image in detail, including objects, people, activities, and scene context."
    ) -> Optional[str]:
        """
        Get detailed description of image

        Args:
            image: Numpy array (H, W, 3) in BGR format
            prompt: Custom prompt (optional)

        Returns:
            Description text or None if service unavailable
        """
        if not self.available:
            self._check_availability()
            if not self.available:
                return None

        try:
            # Convert image to base64
            image_b64 = self._numpy_to_base64(image)

            # Make request
            response = requests.post(
                f"{self.service_url}/describe",
                json={
                    'image': image_b64,
                    'prompt': prompt
                },
                timeout=self.timeout
            )

            if response.status_code == 200:
                data = response.json()
                description = data['description']
                processing_time = data.get('processing_time_ms', 0)

                logger.debug(f"VLM description generated in {processing_time:.1f}ms")

                return description
            else:
                logger.error(f"VLM service error: {response.status_code}")
                return None

        except requests.exceptions.Timeout:
            logger.error(f"VLM request timeout after {self.timeout}s")
            return None
        except Exception as e:
            logger.error(f"Error calling VLM service: {e}")
            return None

    def question(self, image: np.ndarray, question: str) -> Optional[str]:
        """
        Ask specific question about image

        Args:
            image: Numpy array (H, W, 3) in BGR format
            question: Question to ask

        Returns:
            Answer text or None if service unavailable
        """
        if not self.available:
            self._check_availability()
            if not self.available:
                return None

        try:
            image_b64 = self._numpy_to_base64(image)

            response = requests.post(
                f"{self.service_url}/question",
                json={
                    'image': image_b64,
                    'question': question
                },
                timeout=self.timeout
            )

            if response.status_code == 200:
                data = response.json()
                return data['answer']
            else:
                logger.error(f"VLM service error: {response.status_code}")
                return None

        except Exception as e:
            logger.error(f"Error calling VLM service: {e}")
            return None

    def get_stats(self) -> Optional[Dict]:
        """Get VLM service statistics"""
        try:
            response = requests.get(
                f"{self.service_url}/stats",
                timeout=2.0
            )
            if response.status_code == 200:
                return response.json()
            return None
        except:
            return None


# Example usage prompts for different scenarios
PROMPTS = {
    'person': "Describe this person's appearance, clothing, and what they are doing.",
    'vehicle': "Describe this vehicle including its type, color, and any visible details.",
    'activity': "What activity is happening in this image? Describe in detail.",
    'safety': "Analyze this image for any safety concerns or unusual behavior.",
    'detailed': "Provide a comprehensive description of everything visible in this image.",
    'short': "Describe this image in one sentence.",
}


def get_prompt_for_class(class_name: str) -> str:
    """
    Get appropriate prompt based on detected class

    Args:
        class_name: YOLO class name (person, car, truck, etc.)

    Returns:
        Tailored prompt for that class
    """
    if class_name == 'person':
        return PROMPTS['person']
    elif class_name in ['car', 'truck', 'bus', 'motorcycle']:
        return PROMPTS['vehicle']
    else:
        return PROMPTS['detailed']
