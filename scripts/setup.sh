#!/usr/bin/env bash
# setup.sh — Initial setup for picoclaw-free-llm
set -euo pipefail

PICOCLAW_DIR="$HOME/.picoclaw-free-llm"
CONFIG_DIR="$(cd "$(dirname "$0")/.." && pwd)/configs"

echo "=== picoclaw-free-llm setup ==="

# Create data directory
mkdir -p "$PICOCLAW_DIR"
echo "Created: $PICOCLAW_DIR"

# Copy example config if none exists
if [ ! -f "$PICOCLAW_DIR/config.yaml" ]; then
  cp "$CONFIG_DIR/config.example.yaml" "$PICOCLAW_DIR/config.yaml"
  echo "Created: $PICOCLAW_DIR/config.yaml (from example)"
else
  echo "Config already exists: $PICOCLAW_DIR/config.yaml"
fi

echo ""
echo "=== API key setup ==="
echo "Set the following environment variables for each provider you want to use:"
echo ""
echo "  export OPENROUTER_API_KEY=...   # https://openrouter.ai/settings/keys"
echo "  export GROQ_API_KEY=...         # https://console.groq.com/keys"
echo "  export GEMINI_API_KEY=...       # https://aistudio.google.com/app/apikey"
echo "  export MISTRAL_API_KEY=...      # https://console.mistral.ai/api-keys"
echo "  export CEREBRAS_API_KEY=...     # https://cloud.cerebras.ai/"
echo "  export TOGETHER_API_KEY=...     # https://api.together.xyz/settings/api-keys"
echo "  export DEEPSEEK_API_KEY=...     # https://platform.deepseek.com/api_keys"
echo "  export COHERE_API_KEY=...       # https://dashboard.cohere.com/api-keys"
echo "  export NVIDIA_API_KEY=...       # https://build.nvidia.com/settings/api-key"
echo "  export HF_API_KEY=...           # https://huggingface.co/settings/tokens"
echo ""
echo "After setting API keys, run:"
echo "  picoclaw-scan update   # Discover free models"
echo "  picoclaw-proxy         # Start the proxy on port 8080"
echo ""
echo "Test with:"
echo "  curl http://localhost:8080/v1/chat/completions \\"
echo '    -H "Content-Type: application/json" \'
echo '    -d '"'"'{"model":"auto","messages":[{"role":"user","content":"Hello!"}]}'"'"
