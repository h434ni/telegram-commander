#!/bin/bash
set -e

# Resolve repository root from script location (portable across any machine/folder)
REPO_DIR="${REPO_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
BRANCH="${DEPLOY_BRANCH:-$(git -C "$REPO_DIR" branch --show-current 2>/dev/null || echo "main")}"
REMOTE="${DEPLOY_REMOTE:-origin}"
APP_NAME="${PM2_APP_NAME:-telegram-commander}"
CONFIG_PATH="${CONFIG_PATH:-$REPO_DIR/config.yaml}"
LOG_FILE="${DEPLOY_LOG:-$REPO_DIR/deploy.log}"

exec >> "$LOG_FILE" 2>&1
echo "=== Deployment started at $(date) ==="

cd "$REPO_DIR"

echo "Fetching latest changes from $REMOTE/$BRANCH..."
git fetch "$REMOTE" "$BRANCH"
git reset --hard "$REMOTE/$BRANCH"

echo "Compiling Go binary..."
go build -o "$APP_NAME" ./cmd/telegram-commander

if command -v pm2 >/dev/null 2>&1; then
    echo "Restarting service in PM2..."
    pm2 restart "$APP_NAME" || pm2 start "./$APP_NAME" --name "$APP_NAME" --cwd "$REPO_DIR" -- run --config "$CONFIG_PATH"
elif command -v systemctl >/dev/null 2>&1; then
    echo "Restarting via systemctl..."
    systemctl --user restart "$APP_NAME" 2>/dev/null || sudo systemctl restart "$APP_NAME" 2>/dev/null || true
fi

echo "=== Deployment completed successfully at $(date) ==="
