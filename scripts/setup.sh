#!/usr/bin/env bash
# setup.sh — Initial setup for free-llm-proxy-router
set -euo pipefail

# Respect XDG_CONFIG_HOME (fallback to ~/.config)
XDG_CONFIG_HOME="${XDG_CONFIG_HOME:-$HOME/.config}"
CONFIG_DIR="$XDG_CONFIG_HOME/free-llm-proxy-router"
EXAMPLE_CONFIG_DIR="$(cd "$(dirname "$0")/.." && pwd)/configs"

echo "=== free-llm-proxy-router setup ==="

# Create data directory
mkdir -p "$CONFIG_DIR"
echo "Created: $CONFIG_DIR"

# Copy example config if none exists
if [ ! -f "$CONFIG_DIR/config.yaml" ]; then
  cp "$EXAMPLE_CONFIG_DIR/config.example.yaml" "$CONFIG_DIR/config.yaml"
  echo "Created: $CONFIG_DIR/config.yaml (from example)"
else
  echo "Config already exists: $CONFIG_DIR/config.yaml"
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
echo "  free-llm-scan update   # Discover free models"
echo "  free-llm-proxy         # Start the proxy on port 8080"
echo ""
echo "Test with:"
echo "  curl http://localhost:8080/v1/chat/completions \\"
echo '    -H "Content-Type: application/json" \'
echo '    -d '"'"'{"model":"auto","messages":[{"role":"user","content":"Hello!"}]}'"'"
