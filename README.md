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
    # git clone [https://github.com/ephrin/net-init.git](https://github.com/ephrin/net-init.git)
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
