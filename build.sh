#!/bin/bash

# Build script for Docker image using .dockerenv configuration

# Check if .dockerenv file exists
if [ ! -f ".dockerenv" ]; then
    echo "Error: .dockerenv file not found!"
    exit 1
fi

# Source the environment variables
set -a
source .dockerenv
set +a

# Build the Docker image with the variables from .dockerenv
echo "Building Docker image with configuration:"
echo "  APP_SOURCE: ${APP_SOURCE}"
echo "  NGINX_PORT: ${NGINX_PORT}"
echo "  CONTROL_PLANE_API_DIR: ${CONTROL_PLANE_API_DIR}"
echo "  CONTROL_PLANE_PORT: ${CONTROL_PLANE_PORT}"
echo "  DEFAULT_APP_PORT: ${DEFAULT_APP_PORT}"
echo "  APPLET_DIR: ${APPLET_DIR}"
echo ""

docker build \
    --build-arg APP_SOURCE="${APP_SOURCE}" \
    --build-arg NGINX_PORT="${NGINX_PORT}" \
    --build-arg CONTROL_PLANE_API_DIR="${CONTROL_PLANE_API_DIR}" \
    --build-arg APPLET_DIR="${APPLET_DIR}" \
    --build-arg CONTROL_PLANE_PORT="${CONTROL_PLANE_PORT}" \
    --build-arg DEFAULT_APP_PORT="${DEFAULT_APP_PORT}" \
    -t cloudrun-poc:latest .

echo "Build completed successfully!"
