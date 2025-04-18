# Dockerfile

# Stage 1: Build net-init binary using cross-compilation
# Use a specific Go version known to support cross-compilation well
# Use BUILDPLATFORM to ensure the builder runs on its native architecture
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder

# Install any build tools if needed (likely none for static Go build)
# RUN apk add --no-cache git build-base

WORKDIR /build

# Copy go mod files and download dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the static binary specifically for the TARGETPLATFORM
# TARGETPLATFORM is automatically provided by buildx (e.g., linux/amd64, linux/arm64)
ARG TARGETPLATFORM
RUN echo "Building net-init for $TARGETPLATFORM"

# Set GOOS and GOARCH for cross-compilation based on TARGETPLATFORM
# CGO_ENABLED=0 ensures static linking without C dependencies
RUN CGO_ENABLED=0 GOOS=$(echo $TARGETPLATFORM | cut -d/ -f1) GOARCH=$(echo $TARGETPLATFORM | cut -d/ -f2) go build -ldflags="-w -s" -o /net-init main.go

# Stage 2: Create the minimal final image based on the TARGETPLATFORM
# Use a minimal base image compatible with the target architecture
FROM --platform=$TARGETPLATFORM alpine:latest

# Copy only the compiled binary from the builder stage
COPY --from=builder /net-init /usr/local/bin/net-init

# Verify the binary architecture (optional debug step)
# RUN file /usr/local/bin/net-init

# Set metadata
LABEL org.opencontainers.image.source="https://github.com/ephrin/net-init"

# Optional: If you want this image to be directly runnable (e.g., for testing)
# you could add an entrypoint, but for just distributing the binary,
# copying from this image (like in the integration test) is common.
# ENTRYPOINT ["/usr/local/bin/net-init"]
# CMD ["--help"] # Example default command