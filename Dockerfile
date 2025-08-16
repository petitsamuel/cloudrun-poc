# Dockerfile

ARG APP_SOURCE=next
ARG CONTROL_PLANE_API_DIR=control-plane-api
ARG APPLET_DIR=applet
ARG NGINX_PORT=8080
ARG CONTROL_PLANE_PORT=8000
ARG DEFAULT_APP_PORT=3000

# Stage 1: Build the Go control plane binary
FROM golang:1.22-alpine AS builder

WORKDIR /src

# Copy Go module files
COPY controlplaneapi/go.mod .
COPY controlplaneapi/main.go .

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /control-plane-api .

# Stage 2: Create the final production image
FROM node:22-slim

ARG APP_SOURCE
ARG CONTROL_PLANE_API_DIR
ARG APPLET_DIR
ARG NGINX_PORT
ARG CONTROL_PLANE_PORT
ARG DEFAULT_APP_PORT

RUN apt-get update && apt-get install -y --no-install-recommends nginx curl gettext \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

RUN mkdir -p ${CONTROL_PLANE_API_DIR} ${APPLET_DIR}

COPY --from=builder /control-plane-api ${CONTROL_PLANE_API_DIR}/control-plane-api

COPY ${APP_SOURCE}/package*.json ${APPLET_DIR}/
# Use `npm ci` for faster, more reliable, and reproducible builds in CI/CD environments.
RUN cd ${APPLET_DIR} && npm install --no-fund --no-audit

# Copy the rest of the user's application code into the applet directory.
COPY ${APP_SOURCE}/ ${APPLET_DIR}/

# Copy our infrastructure configuration files.
COPY nginx.conf /etc/nginx/nginx.conf.template
COPY start.sh .

# Make the Go binary and start script executable.
RUN chmod +x ./start.sh ${CONTROL_PLANE_API_DIR}/control-plane-api

ENV NEXT_TELEMETRY_DISABLED=1 \
    APP_SOURCE=${APP_SOURCE} \
    NGINX_PORT=${NGINX_PORT} \
    CONTROL_PLANE_API_DIR=${CONTROL_PLANE_API_DIR} \
    APPLET_DIR=${APPLET_DIR} \
    CONTROL_PLANE_PORT=${CONTROL_PLANE_PORT} \
    APP_DIR=/app/${APPLET_DIR} \
    DEFAULT_APP_PORT=${DEFAULT_APP_PORT}

# Expose the port nginx will listen on.
EXPOSE ${NGINX_PORT}

# Copy the start script and make it executable.
# The command to run when the container starts.
CMD ["./start.sh"]
