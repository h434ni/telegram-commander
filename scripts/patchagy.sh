#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -gt 0 ]; then
    echo "Error: Custom path or arguments are not supported. The path is read directly from config.properties." >&2
    exit 1
fi

# Ensure ~/.local/bin is in PATH for uv and other tools
export PATH="$HOME/.local/bin:$PATH"

REPO_DIR="$HOME/apps/MyGithub/AvenCores-open-antigravity-patcher"

if [ ! -d "$REPO_DIR" ]; then
    echo "Error: Repository directory not found: $REPO_DIR" >&2
    exit 1
fi

cd "$REPO_DIR"

if command -v uv >/dev/null 2>&1; then
    exec uv run scripts/patch_agy.py
elif [ -x "$REPO_DIR/.venv/bin/python" ]; then
    exec "$REPO_DIR/.venv/bin/python" scripts/patch_agy.py
else
    exec python3 scripts/patch_agy.py
fi
