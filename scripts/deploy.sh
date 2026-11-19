#!/bin/bash
set -e

# Resolve repository root from script directory
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_FILE="$REPO_DIR/deploy.log"

exec >> "$LOG_FILE" 2>&1
echo "=== Deployment started at $(date) ==="

cd "$REPO_DIR"

echo "Fetching latest changes..."
git fetch origin tg-tree
git reset --hard origin/tg-tree

echo "Compiling Go binary..."
go build -o telegram-commander ./cmd/telegram-commander

echo "Restarting service in PM2..."
pm2 restart telegram-commander || pm2 start ./telegram-commander --name "telegram-commander" --cwd "$REPO_DIR" -- run --config "$REPO_DIR/config.yaml"

echo "=== Deployment completed successfully at $(date) ==="
