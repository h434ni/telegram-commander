#!/bin/bash
set -e

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

# Install PM2 if not present
if ! command -v pm2 >/dev/null 2>&1; then
    echo "PM2 not found. Installing pm2 globally..."
    npm install -g pm2 || sudo npm install -g pm2
fi

echo "Restarting service in PM2..."
pm2 restart "$APP_NAME" || pm2 start "./$APP_NAME" --name "$APP_NAME" --cwd "$REPO_DIR" -- run --config "$CONFIG_PATH"
pm2 save

echo "=== Deployment completed successfully at $(date) ==="
