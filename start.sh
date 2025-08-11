#!/bin/bash
set -Eeuo pipefail

# This function will be executed when the script receives a SIGINT or SIGTERM signal.
cleanup() {
    echo "Caught signal, shutting down gracefully..."
    # Kill all background processes that are children of this script.
    kill $(jobs -p) || true
    # Wait for all background processes to terminate.
    wait || true
    echo "Shutdown complete."
}

# Register the 'cleanup' function to be called on SIGINT and SIGTERM.
trap cleanup SIGINT SIGTERM

# Configurable app directory for arbitrary frameworks
APP_DIR=${APP_DIR:-/app/applet}

wait_for_http() {
    local url="$1"
    local timeout_seconds="${2:-120}"
    local interval_seconds="${3:-2}"
    local elapsed=0
    echo "Waiting for $url to become ready..."
    until curl -fsS "$url" > /dev/null 2>&1; do
        if [ "$elapsed" -ge "$timeout_seconds" ]; then
            echo "Timeout waiting for $url after ${timeout_seconds}s"
            return 1
        fi
        sleep "$interval_seconds"
        elapsed=$((elapsed + interval_seconds))
    done
    echo "$url is ready."
}

# Ensure dependencies are present before starting the dev server
ensure_deps_installed() {
    if [ ! -d node_modules ]; then
        echo "node_modules missing. Installing dependencies..."
        if [ -f package-lock.json ]; then npm ci; else npm install; fi
        return
    fi

    # Detect the dev script and check for a corresponding CLI in node_modules/.bin
    local dev_script
    dev_script=$(node -e 'try{const p=require("./package.json"); console.log((p.scripts&&p.scripts.dev)||"");}catch(e){console.log("");}')
    local required_bin=""
    if echo "$dev_script" | grep -qE "\\bvite\\b"; then required_bin="vite"; fi
    if echo "$dev_script" | grep -qE "\\bnext\\b"; then required_bin="next"; fi
    if echo "$dev_script" | grep -qE "\\bng\\b"; then required_bin="ng"; fi

    if [ -n "$required_bin" ] && [ ! -x "node_modules/.bin/$required_bin" ]; then
        echo "Dependency CLI '$required_bin' not found. Installing dependencies..."
        if [ -f package-lock.json ]; then npm ci; else npm install; fi
    fi
}

# Start the file_handler API in the background.
echo "Starting file_handler service..."
cd /app/file_handler
PORT=8000 npm start &

# Wait for the management API to be ready, then let it manage the dev server
wait_for_http "http://localhost:8000/healthz" 120 2 || true

# Ask the management API to start the app dev server (ensures consistent PID management)
echo "Starting app dev server via management API..."
curl -fsS -X POST http://localhost:8000/dev/start \
  -H 'Content-Type: application/json' \
  -d '{"port":3000, "prewarm": true, "prewarmPaths": ["/", "/api/hello"]}' \
  || true

# Start nginx immediately so the container is ready on :8080.
# Nginx will serve a warmup page until backends are reachable.
echo "Starting nginx..."
nginx -g 'daemon off;' &
NGINX_PID=$!

# Prewarm common pages in background to reduce first-request latency
(
  sleep 1
  # TODO: make this a more generic endpoint like /
  for path in "/api/hello"; do
    curl -fsS "http://localhost:3000${path}" >/dev/null 2>&1 || true
  done
)

# Keep container alive as long as nginx is running.
wait "$NGINX_PID"

echo "nginx exited. Cleaning up remaining processes..."
cleanup
