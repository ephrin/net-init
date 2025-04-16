# net-init: Network-Aware Container Initialization

[![Go Report Card](https://goreportcard.com/badge/github.com/ephrin/net-init)](https://goreportcard.com/report/github.com/ephrin/net-init)
`net-init` is a lightweight Go utility designed to run as a container's entrypoint (PID 1). It ensures that essential network dependencies (like databases, caches, APIs) are available *before* the container signals readiness, while still launching the main application process immediately. This prevents downstream issues and improves the reliability of service startup in orchestrated environments like Kubernetes or Rancher.

**Analogy:** Think of `net-init` as a **conscientious launch coordinator** for your application container. Before declaring the mission "fully ready" for external interaction (receiving traffic), the coordinator performs pre-flight checks on critical external systems (network dependencies). It allows the main mission preparations (starting your application process) to begin *immediately*, but it only gives the final "go" signal (via HTTP health checks and Prometheus metrics) once the external systems are confirmed operational.

## Features

* **Dependency Checking:**
  * Validates network dependencies before reporting readiness.
  * Supports **TCP**, **UDP**, **HTTP**, **HTTPS** endpoints.
  * Supports custom readiness checks via **external scripts/commands** (`exec://`).
  * Handles multiple dependencies concurrently.
  * Configurable retry intervals and overall timeout.
  * Basic TLS validation options (`NETINIT_TLS_SKIP_VERIFY`).
* **Health Reporting:**
  * Exposes an **HTTP health check endpoint** (e.g., `/health`) returning `200 OK` when all dependencies are ready, `503 Service Unavailable` otherwise. Perfect for orchestrator network health checks (like Rancher 1.6+ or Kubernetes HTTP readiness probes).
  * Exposes a **Prometheus metrics endpoint** (e.g., `/metrics`) detailing the status of each dependency and the overall readiness state for enhanced observability.
* **Application Lifecycle:**
  * **Executes the main application** (`CMD`) using `syscall.Exec`, ensuring proper signal handling (the application receives `SIGTERM`, `SIGINT` directly) and PID 1 responsibilities are passed on correctly.
  * Runs the application **regardless of initial dependency state**, allowing the app to start its internal initialization while `net-init` checks dependencies in the background.
  * Logs its own status and dependency check progress to stderr (configurable level and format).
* **Lightweight & Efficient:**
  * Written in Go, compiled into a small, static binary.
  * Minimal dependencies.

## Why use `net-init`?

* **Reliable Startups:** Prevents applications from receiving traffic before critical backend services are available, reducing initial bursts of errors.
* **Decoupling:** Separates the concern of infrastructure readiness checks from the application code. Your application doesn't need complex retry logic built-in just for initial startup connections (though internal retries for ongoing connections are still recommended).
* **Observability:** Provides clear health status via HTTP and detailed metrics via Prometheus, making it easier to diagnose startup issues in complex environments.
* **Standardization:** Offers a consistent pattern for managing startup dependencies across different application stacks (Node.js, Python, Java, PHP, etc.).

## Installation

You can integrate `net-init` into your application images using a multi-stage Docker build.

**1. Build the `net-init` Binary:**

You can build the binary locally or using Docker.

* **Using Go (Requires Go 1.21+):**
```bash
# Clone the repository (if you haven't already)
# git clone https://github.com/ephrin/net-init.git
# cd net-init

# Build the static binary
CGO_ENABLED=0 go build -ldflags="-w -s" -o net-init main.go
```
    This creates a static `net-init` binary in the current directory.

* **Using Docker (Multi-stage Build):**
  Use the provided `Dockerfile` in this repository to build an image containing the static binary:
```bash
  docker build -t your-registry/net-init:latest .
# Optionally push to your registry:
# docker push your-registry/net-init:latest
```
    This builds an image (e.g., `your-registry/net-init:latest`) containing the compiled `/usr/local/bin/net-init` binary based on a minimal image like Alpine.

**2. Copy into Application Image:**

In your application's `Dockerfile`, use a multi-stage build to copy the binary from the builder image or your local build context.

```dockerfile
# Assuming you built an image tagged 'your-registry/net-init:latest'
COPY --from=your-registry/net-init:latest /usr/local/bin/net-init /usr/local/bin/net-init

# Or if you built locally and the binary is in the build context:
# COPY net-init /usr/local/bin/net-init

# Ensure it's executable
RUN chmod +x /usr/local/bin/net-init
```

## Configuration

`net-init` is configured entirely through environment variables:

| Variable                     | Description                                                                                                                                  | Default          | Example                                                              |
| :--------------------------- | :------------------------------------------------------------------------------------------------------------------------------------------- | :--------------- | :------------------------------------------------------------------- |
| `NETINIT_WAIT`               | Comma-separated list of dependencies to check. See format details below.                                                                     | *(none)* | `tcp://db:5432,http://api/health,exec://./check-cache.sh`            |
| `NETINIT_HEALTHCHECK_PORT`   | Port for the HTTP health check and Prometheus metrics endpoints.                                                                             | `8887`           | `8080`                                                               |
| `NETINIT_HEALTHCHECK_PATH`   | Path for the HTTP health check endpoint (returns 200/503).                                                                                   | `/health`        | `/readyz`                                                            |
| `NETINIT_METRICS_PATH`       | Path for the Prometheus metrics endpoint.                                                                                                    | `/metrics`       | `/metrics`                                                           |
| `NETINIT_TIMEOUT`            | Overall timeout in seconds for *all* checks to become ready. `net-init` continues running checks but the context controlling them expires.     | `300` (5 mins)   | `120`                                                                |
| `NETINIT_RETRY_INTERVAL`     | Interval in seconds between checks for *each* currently failing dependency.                                                                    | `5`              | `3`                                                                  |
| `NETINIT_LOG_LEVEL`          | Logging level: `debug`, `info`, `warn`, `error`.                                                                                             | `info`           | `debug`                                                              |
| `NETINIT_LOG_FORMAT`         | Logging format (Currently defaults to JSON via slog, potentially `text` in future).                                                          | `json`           | `json`                                                               |
| `NETINIT_CUSTOM_CHECK_TIMEOUT`| Timeout in seconds specifically for `exec://` custom check commands.                                                                          | `10`             | `30`                                                                 |
| `NETINIT_TLS_SKIP_VERIFY`    | Set to `true` to skip TLS certificate verification for `https://` checks. **Use with caution, insecure!** | `false`          | `true`                                                               |
| `NETINIT_TLS_CA_CERT_PATH`   | Path to a custom CA certificate file for `https://` checks.                                                                                  | *(none)* | `/etc/ssl/certs/my-ca.crt`                                           |

**Dependency String Format (`NETINIT_WAIT`):**

* **TCP:** `tcp://host:port` or simply `host:port` (e.g., `tcp://redis:6379`, `postgres-db:5432`)
* **UDP:** `udp://host:port` (e.g., `udp://statsd:8125`) - *Note: UDP check only verifies if the host/port can be dialed, not true service readiness.*
* **HTTP:** `http://host[:port][/path]` (e.g., `http://user-api/health`, `http://service:8080/ping`) - Checks for a `2xx` status code.
* **HTTPS:** `https://host[:port][/path]` (e.g., `https://secure-api/status`) - Checks for a `2xx` status code, respects TLS settings.
* **Exec:** `exec://[/path/to/script] [arg1] [arg2]...` (e.g., `exec://./check_db.sh read`, `exec:///usr/local/bin/mycheck --mode=fast`) - Executes the script/command. Exiting with code `0` means success, non-zero means failure. Subject to `NETINIT_CUSTOM_CHECK_TIMEOUT`.

## Usage Pattern (Dockerfile Integration)

The standard pattern is to use `net-init` as the `ENTRYPOINT` and your application's normal start command as the `CMD`.

1.  **`COPY`** the `net-init` binary into your image (preferably using a multi-stage build).
2.  **`ENV`** set the `NETINIT_` configuration variables.
3.  **`ENTRYPOINT ["/usr/local/bin/net-init"]`** - Makes `net-init` run first.
4.  **`CMD ["your", "application", "command"]`** - This is the command `net-init` will execute via `exec()` once it starts.

### Example: Node.js Application

```dockerfile
# Stage 1: Build net-init (or use a pre-built image)
FROM golang:1.22-alpine AS netinit-builder
WORKDIR /build
# Add source code or fetch pre-built binary
# Example: Assuming source is in context
# COPY . .
# RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o /net-init main.go
# Example: Fetching a pre-built binary (replace with actual URL/method)
ARG NETINIT_VERSION=latest
ADD https://github.com/ephrin/net-init/releases/download/${NETINIT_VERSION}/net-init-alpine-amd64 /net-init
RUN chmod +x /net-init

# Stage 2: Application Image
FROM node:18-alpine

# Install net-init
COPY --from=netinit-builder /net-init /usr/local/bin/net-init
# Ensure executable just in case
RUN chmod +x /usr/local/bin/net-init

WORKDIR /app

# Install app dependencies
COPY package*.json ./
RUN npm install --production

# Bundle app source
COPY . .

# Configure net-init
ENV NETINIT_WAIT="tcp://mongodb:27017,http://auth-service/health"
ENV NETINIT_RETRY_INTERVAL=3
ENV NETINIT_HEALTHCHECK_PORT=8080 # Use app's port or a dedicated one
ENV NETINIT_LOG_LEVEL=info

EXPOSE 8080

# Set net-init as the entrypoint
ENTRYPOINT ["/usr/local/bin/net-init"]

# Define the application command
CMD [ "node", "server.js" ]
```

### Example: PHP CLI Application

```dockerfile
# Stage 1: Build net-init (or use a pre-built image)
FROM golang:1.22-alpine AS netinit-builder
WORKDIR /build
# Add source code or fetch pre-built binary
# Example: Assuming source is in context
# COPY . .
# RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o /net-init main.go
# Example: Fetching a pre-built binary (replace with actual URL/method)
ARG NETINIT_VERSION=latest
ADD https://github.com/ephrin/net-init/releases/download/${NETINIT_VERSION}/net-init-alpine-amd64 /net-init
RUN chmod +x /net-init

# Stage 2: Application Image
FROM php:8.2-cli-alpine

# Install net-init
COPY --from=netinit-builder /net-init /usr/local/bin/net-init
# Ensure executable just in case
RUN chmod +x /usr/local/bin/net-init

# Optional: Install PHP extensions needed by your script (e.g., pdo_pgsql, redis)
# RUN docker-php-ext-install pdo_pgsql redis

WORKDIR /app

# Copy application script(s)
COPY . .

# Configure net-init
ENV NETINIT_WAIT="tcp://postgres:5432,tcp://redis:6379"
ENV NETINIT_RETRY_INTERVAL=5
ENV NETINIT_HEALTHCHECK_PORT=9001 # Dedicated port for health/metrics
ENV NETINIT_LOG_LEVEL=info

# Expose the health check port if needed outside the container/pod
EXPOSE 9001

# Set net-init as the entrypoint
ENTRYPOINT ["/usr/local/bin/net-init"]

# Define the application command (e.g., run a PHP script)
CMD [ "php", "your_script.php" ]
```
*(Note: For PHP web applications using PHP-FPM or Apache, the `CMD` would be different, e.g., `["php-fpm"]` or `["apache2-foreground"]`)*

## Prometheus Metrics

If configured (which it is by default), `net-init` exposes metrics at `NETINIT_METRICS_PATH` (default `/metrics`) on the `NETINIT_HEALTHCHECK_PORT`. Key metrics include:

* `netinit_dependency_up{dependency="<dependency_string>"}`: (Gauge) `1` if the specific dependency is considered up, `0` if down.
* `netinit_overall_status`: (Gauge) `1` if all dependencies specified in `NETINIT_WAIT` are up, `0` otherwise.

These metrics provide valuable observability into the container initialization process.

## Development

* **Prerequisites:** Go 1.21 or later.
* **Building:** `go build -o net-init main.go`
* **Testing:** (Add details if tests are implemented, e.g., `go test ./...`)
* **Dependencies:** Uses Go modules. Run `go mod tidy` to manage dependencies.

## Contributing

Contributions are welcome! Please feel free to open an issue or submit a pull request. Follow standard Go coding practices and ensure code is formatted with `gofmt`. (Consider adding a `CONTRIBUTING.md` file for more details).

## License

This project is licensed under the [Your Chosen License - e.g., MIT License or Apache 2.0 License]. See the [LICENSE](LICENSE) file for details.
*(**Action:** You need to choose a license (e.g., MIT, Apache-2.0) and add a `LICENSE` file to the repository root)*