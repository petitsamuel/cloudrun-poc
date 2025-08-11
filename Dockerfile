# Use the official Node.js 22 image.
FROM node:22-slim

# Install nginx and curl (used for readiness checks in start.sh)
RUN apt-get update && apt-get install -y --no-install-recommends nginx curl \
    && rm -rf /var/lib/apt/lists/*

# Set the working directory
WORKDIR /app

# Choose which sample app source to use. Options: next (Next.js), react, angular
ARG APP_SOURCE=next

# Copy package.json and package-lock.json for file_handler and install dependencies
COPY file_handler/package*.json ./file_handler/
RUN cd file_handler && npm ci

# Copy package.json and package-lock.json for the chosen app source and install dependencies
COPY ${APP_SOURCE}/package*.json ./applet/
RUN cd applet && if [ -f package-lock.json ]; then npm ci; else npm install; fi

# Copy the rest of the application code
COPY file_handler/ ./file_handler/
COPY ${APP_SOURCE}/ ./applet/

# Copy the nginx configuration file
COPY nginx.conf /etc/nginx/nginx.conf

# Copy the start script
COPY start.sh .

# Make the start script executable
RUN chmod +x ./start.sh

# Reduce noise
ENV NEXT_TELEMETRY_DISABLED=1

# Expose the port nginx is listening on
EXPOSE 8080

# Start the services
CMD ["./start.sh"]
