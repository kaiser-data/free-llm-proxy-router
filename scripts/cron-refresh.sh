#!/usr/bin/env bash
# cron-refresh.sh — Weekly catalog refresh via LLM
# Add to crontab with: crontab -e
# Example: 0 9 * * 1 /path/to/picoclaw-free-llm/scripts/cron-refresh.sh
set -euo pipefail

PICOCLAW_SCAN="$(cd "$(dirname "$0")/.." && pwd)/bin/picoclaw-scan"
LOG_FILE="$HOME/.picoclaw-free-llm/refresh.log"

echo "[$(date)] Starting weekly catalog refresh" >> "$LOG_FILE"

# Scan for new models
"$PICOCLAW_SCAN" update >> "$LOG_FILE" 2>&1 && echo "[$(date)] Scan complete" >> "$LOG_FILE"

# LLM-powered diff refresh
"$PICOCLAW_SCAN" refresh-llm >> "$LOG_FILE" 2>&1 && echo "[$(date)] LLM refresh complete" >> "$LOG_FILE"

echo "[$(date)] Done" >> "$LOG_FILE"
