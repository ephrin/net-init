# integration-test/test-node.dockerfile
FROM node:18-alpine AS app-base

# Install wget for internal health checks via exec
RUN apk add --no-cache wget

WORKDIR /app

# Copy application code
COPY server.js .
# If you had package.json, copy and npm install here

# --- Final Application Image ---
FROM app-base

# Copy the pre-built net-init binary
COPY --from=net-init-builder-img /usr/local/bin/net-init /usr/local/bin/net-init
RUN chmod +x /usr/local/bin/net-init

# Note: NETINIT_* and TEST_VAR variables are set via compose.yaml
# Note: EXPOSE instructions are removed as ports are not published to host

# Set net-init as the entrypoint
ENTRYPOINT ["/usr/local/bin/net-init"]

# Define the application command
CMD [ "node", "server.js" ]