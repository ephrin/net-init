# net-init: Network-Aware Container Initialization

[![Go Report Card](https://goreportcard.com/badge/github.com/ephrin/net-init)](https://goreportcard.com/report/github.com/ephrin/net-init)
`net-init` is a lightweight Go utility designed to run as a container's entrypoint (PID 1). Its **primary goal** is to ensure that essential network dependencies (like databases, caches, APIs) are available **before starting the main application process** specified in the container's `CMD`. It also provides health and metrics endpoints reflecting the dependency status.

By default, `net-init` acts as a **guard**, preventing your application from starting until its required network services are reachable.

## Features

* **Dependency Checking:**
  * Validates network dependencies before starting the main application (default behavior).
  * Supports **TCP**, **UDP**, **HTTP**, **HTTPS** endpoints.
  * **Robust DNS resolution** with detection of Docker network placeholder responses.
  * Protocol-appropriate probing based on port number (e.g., HTTP for ports 80/443).
  * Supports custom readiness checks via **external scripts/commands** (`exec://`).
  * Handles multiple dependencies concurrently.
  * Configurable retry intervals and overall timeout for checks.
  * Basic TLS validation options (`NETINIT_TLS_SKIP_VERIFY`).
* **Health Reporting:**
  * Exposes an **HTTP health check endpoint** (e.g., `/health`) returning `200 OK` when all dependencies are ready (and the app is about to start/has started), `503 Service Unavailable` otherwise.
  * Exposes a **Prometheus metrics endpoint** (e.g., `/metrics`) detailing the status of each dependency and the overall readiness state.
* **Application Lifecycle:**
  * **Waits for dependencies** before launching the main application (`CMD`) as a child process (default behavior).
  * Optionally (**`NETINIT_START_IMMEDIATELY=true`**), starts the application immediately and checks dependencies in the background (only delays the `/health` endpoint).
  * **Manages the child process:** Forwards signals (`SIGTERM`, `SIGINT`) correctly to the child application and exits with the application's exit code.
  * Logs its own status and dependency check progress to stderr.
* **Lightweight & Efficient:**
  * Written in Go, compiled into a small, static binary.
  * Minimal dependencies.

## Why use `net-init`?

* **Reliable Startups:** Prevents applications from starting and potentially crashing before critical backend services are available.
* **Decoupling:** Separates the concern of infrastructure readiness checks from the application code.
* **Observability:** Provides clear health status via HTTP and detailed metrics via Prometheus.
* **Standardization:** Offers a consistent pattern for managing startup dependencies.

## Installation

There are several ways to install and use `net-init` in your container images:

**1. Build the `net-init` Binary:**

* Using Go:

        CGO_ENABLED=0 go build -ldflags="-w -s" -o net-init main.go

* Using Docker:

        docker build -t your-registry/net-init:latest .

**2. Copy into Application Image:**

        COPY --from=your-registry/net-init:latest /usr/local/bin/net-init /usr/local/bin/net-init
        RUN chmod +x /usr/local/bin/net-init

## Configuration

`net-init` is configured entirely through environment variables:

| Variable                         | Description                                                                                                                                                                            | Default        | Example                                                     |
| :------------------------------- | :------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | :------------- | :---------------------------------------------------------- |
| `NETINIT_WAIT`                   | Comma-separated list of dependencies to check. See format details below.                                                                                                               | *(none)* | `tcp://db:5432,http://api/health,exec://./check-cache.sh` |
| `NETINIT_HEALTHCHECK_PORT`       | Port for the HTTP health check and Prometheus metrics endpoints.                                                                                                                       | `8887`         | `8080`                                                      |
| `NETINIT_HEALTHCHECK_PATH`       | Path for the HTTP health check endpoint (returns 200/503).                                                                                                                             | `/health`      | `/readyz`                                                   |
| `NETINIT_METRICS_PATH`           | Path for the Prometheus metrics endpoint.                                                                                                                                              | `/metrics`     | `/metrics`                                                  |
| `NETINIT_TIMEOUT`                | Timeout in seconds for dependencies to become ready (default mode) or overall runtime if starting immediately. If timeout is reached before deps are ready, net-init exits with error. | `300` (5 mins) | `120`                                                       |
| `NETINIT_RETRY_INTERVAL`         | Interval in seconds between checks for *each* currently failing dependency.                                                                                                              | `5`            | `3`                                                         |
| `NETINIT_LOG_LEVEL`              | Logging level: `debug`, `info`, `warn`, `error`.                                                                                                                                       | `info`         | `debug`                                                     |
| `NETINIT_CUSTOM_CHECK_TIMEOUT`   | Timeout in seconds specifically for `exec://` custom check commands.                                                                                                                     | `10`           | `30`                                                        |
| `NETINIT_TLS_SKIP_VERIFY`        | Set to `true` to skip TLS certificate verification for `https://` checks. **Use with caution!** | `false`        | `true`                                                      |
| `NETINIT_TLS_CA_CERT_PATH`       | Path to a custom CA certificate file for `https://` checks. *(Coming soon - not yet implemented)* | *(none)* | `/etc/ssl/certs/my-ca.crt`                                  |
| **`NETINIT_START_IMMEDIATELY`** | Set to `true` to start the main application (`CMD`) immediately, before dependencies are ready (only delays `/health` becoming 200). **Default is `false` (waits for dependencies).** | `false`        | `true`                                                      |
| **`NETINIT_EXIT_AFTER_READY`** | Set to `true` to exit with success code (0) after dependencies are ready when no command is provided. If `false` (default), continues running until killed. This setting is ignored if a command is provided. | `false`        | `true`                                                      |

**Dependency String Format (`NETINIT_WAIT`):**

* **TCP:** `tcp://host:port` or simply `host:port` (e.g., `tcp://redis:6379`, `postgres-db:5432`)
  * Validates both DNS resolution and connection establishment
  * Detects and rejects Docker network placeholder responses
  * Uses protocol-specific probes based on port (HTTP for 80/443)
* **UDP:** `udp://host:port` (e.g., `udp://statsd:8125`) - *Note: Basic check.*
* **HTTP:** `http://host[:port][/path]` (e.g., `http://user-api/health`) - Checks for `2xx`.
  * Prevents HTTPS redirects from causing false positives
* **HTTPS:** `https://host[:port][/path]` (e.g., `https://secure-api/status`) - Checks for `2xx`.
* **Exec:** `exec://[/path/to/script] [arg1]...` (e.g., `exec://./check_db.sh`) - Checks for exit code `0`.

## Usage Pattern (Dockerfile Integration)

Here are complete examples showing how to integrate `net-init` in your Dockerfiles:

### Example: Node.js Application (Waits for Deps - Default)

        # Stage 1: Build net-init ...
        # Stage 2: Application Image
        FROM node:18-alpine
        COPY --from=netinit-builder /net-init /usr/local/bin/net-init
        RUN chmod +x /usr/local/bin/net-init
        WORKDIR /app
        COPY package*.json ./
        RUN npm install --production
        COPY . .
        # Configure net-init via ENV (or compose file)
        ENV NETINIT_WAIT="tcp://mongodb:27017,http://auth-service/health"
        ENV NETINIT_HEALTHCHECK_PORT=8080
        # ENV NETINIT_START_IMMEDIATELY=false # Default, not needed
        ENTRYPOINT ["/usr/local/bin/net-init"]
        CMD [ "node", "server.js" ]

### Example: PHP CLI Application (Starts Immediately - Optional)

        # Stage 1: Build net-init ...
        # Stage 2: Application Image
        FROM php:8.2-cli-alpine
        COPY --from=netinit-builder /net-init /usr/local/bin/net-init
        RUN chmod +x /usr/local/bin/net-init
        WORKDIR /app
        COPY . .
        # Configure net-init via ENV (or compose file)
        ENV NETINIT_WAIT="tcp://postgres:5432"
        ENV NETINIT_HEALTHCHECK_PORT=9001
        ENV NETINIT_START_IMMEDIATELY=true # Override default
        ENTRYPOINT ["/usr/local/bin/net-init"]
        CMD [ "php", "your_script.php" ]

## Advanced Network Checking Features

`net-init` implements several advanced techniques to ensure robust dependency checking, especially in containerized environments:

### Docker Network Detection

When running in Docker environments, network requests to non-existent services can sometimes receive misleading "placeholder" responses instead of connection failures. `net-init` specifically detects and rejects these Docker network responses to prevent false positives.

### Multi-Phase Connection Validation

For TCP dependencies, `net-init` performs multi-phase validation:
1. **DNS Resolution**: First explicitly checks if the hostname can be resolved, preventing false positives from Docker DNS handling.
2. **Connection Establishment**: Verifies that a real TCP connection can be established.
3. **Reachability-Based Validation**: For most services (database servers, message brokers, etc.), successful connection establishment is considered sufficient proof of reachability.
4. **Protocol-Specific Validation**: For HTTP ports (80/443), continues with additional protocol-specific probing and response validation to ensure the service is responding properly.

### HTTP Redirect Handling

For HTTP dependencies, `net-init` prevents false positives from HTTP-to-HTTPS redirects by using a client that doesn't follow redirects automatically.

## Prometheus Metrics

The following Prometheus metrics are exposed on the metrics endpoint (default: `/metrics`):

* `netinit_dependency_up{dependency="<dependency_string>"}`: (Gauge) `1` if up, `0` if down.
* `netinit_overall_status`: (Gauge) `1` if all dependencies ready, `0` otherwise.

## Project Structure

The project has been refactored into a modular structure for better maintainability and testability:

* **pkg/application** - Application execution lifecycle management
  * `runner.go` - Manages the execution of the specified command
* **pkg/dependencies** - Dependency checking and management
  * `dependency.go` - Dependency data structure and parsing
  * `checker.go` - Dependency check operations and state management
* **pkg/http** - HTTP server implementation
  * `server.go` - Health check and metrics HTTP server
* **pkg/config** - Configuration parsing and validation
* **pkg/checks** - Individual dependency check implementations (TCP, UDP, HTTP, Exec)

## Development

* **Prerequisites:** Go 1.22 or later
* **Building:** `go build` or with optimized flags: `CGO_ENABLED=0 go build -ldflags="-w -s" -o net-init`
* **Testing:** 
  * Run all tests: `go test -v ./...`
  * Run a specific test: `go test -v -run=TestName`
* **Multi-platform Builds:** Use the included build script: `./build.sh -t your-image:tag`
* **Integration Testing:** `cd integration-test && ./run-test.sh`
* **Dependencies:** Uses Go modules. Run `go mod tidy`

## Contributing

Contributions are welcome! Please feel free to open an issue or submit a pull request.

## License

MIT License - See LICENSE file for details.
