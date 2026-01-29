# Hugging Face Setup for VLM Service

The VLM service uses **Qwen3-VL-2B-Instruct** for scene understanding. The model is pre-downloaded in this repository, so you can run it without a Hugging Face token.

## Do I Need a Token?

**No** - The model files are already in `models/Qwen3-VL-2B-Instruct/`. The service works out of the box.

**Yes, if you want to:**
- Download model updates
- Use different Qwen models
- Access gated models

## Getting a Hugging Face Token (Optional)

### 1. Create Account
Visit https://huggingface.co/join

### 2. Generate Token
1. Go to https://huggingface.co/settings/tokens
2. Click "New token"
3. Name: `deepstream-vlm`
4. Type: **Read** (not write)
5. Copy the token (starts with `hf_...`)

### 3. Use the Token

**Option A: Environment variable (recommended)**
```bash
export HF_TOKEN=hf_your_token_here
cd vlm
wendy run --device <device>.local --detach
```

**Option B: Add to Dockerfile**
```dockerfile
ENV HF_TOKEN=hf_your_token_here
```

## Model Information

| Model | Size | Memory | Quantization |
|-------|------|--------|--------------|
| Qwen3-VL-2B-Instruct | ~4GB | ~1.5GB VRAM | INT4 |

The model is quantized to INT4 for efficient inference on Jetson devices.

## Troubleshooting

### Model not loading
```bash
# Check VLM health
curl http://<device>.local:8090/health

# Should show:
# {"model_loaded": true, "model_name": "Qwen3-VL-2B-Instruct", "quantization": "INT4"}
```

### Out of memory
- VLM requires ~1.5GB GPU memory
- Stop other GPU processes before starting VLM
- VLM and detector can run together on Orin (8GB VRAM)

### 401 Unauthorized (if using token)
- Verify token at https://huggingface.co/settings/tokens
- Make sure token has Read access
- Check HF_TOKEN is set: `echo $HF_TOKEN`

## Security Notes

- Never commit tokens to git
- Use Read-only tokens (not Write)
- Add `.env` files to `.gitignore`
