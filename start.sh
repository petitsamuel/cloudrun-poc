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

# Start the file_handler API in the background.
echo "Starting file_handler service..."
cd /app/file_handler
PORT=8000 npm start &

# Wait for the management API to be ready
wait_for_http "http://localhost:8000/healthz" 120 2 || true

# Set up initial framework and start dev server
echo "Setting up initial framework..."
curl -fsS -X POST http://localhost:8000/dev/setup \
  -H 'Content-Type: application/json' \
  -d '{"framework": "FRAMEWORK_NEXTJS"}' \
  || echo "Initial setup failed, use /dev/context/reset to configure manually"

# Let the management API handle all framework setup and dev server management
echo "Management API ready. Use /dev/context/reset to switch frameworks and start dev servers."
echo "Available frameworks: FRAMEWORK_NEXTJS, FRAMEWORK_VITE, FRAMEWORK_UNSPECIFIED"

# Start nginx immediately so the container is ready on :8080.
# Nginx will serve a warmup page until backends are reachable.
echo "Starting nginx..."
nginx -g 'daemon off;' &
NGINX_PID=$!

# Keep container alive as long as nginx is running.
wait "$NGINX_PID"

echo "nginx exited. Cleaning up remaining processes..."
cleanup
