#!/usr/bin/env python3
"""
Example: Integrating VLM with DeepStream YOLO Detector

This is a standalone example showing the VLM integration pattern.
The main detector.py already includes this integration.
"""

import logging
from vlm_client import VLMClient, get_prompt_for_class

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


def main():
    """Example showing how to use VLM client"""
    
    # Initialize VLM client
    vlm_client = VLMClient(service_url="http://localhost:8090")
    
    if vlm_client.available:
        logger.info("✅ VLM service is available")
        
        # Get stats
        stats = vlm_client.get_stats()
        if stats:
            logger.info(f"VLM GPU Memory: {stats.get('memory_allocated_gb', 0):.2f}GB")
    else:
        logger.warning("⚠️  VLM service not available")
        logger.info("Start VLM service: cd vlm-minicpm && wendy run")


if __name__ == "__main__":
    main()
