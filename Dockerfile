# net-init-project/Dockerfile
# Stage 1: Build net-init
FROM golang:1.22-alpine AS builder

WORKDIR /build

# Copy go mod files and download dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the static binary
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o /net-init main.go

# Stage 2: Create the minimal final image containing just the binary
FROM alpine:latest
COPY --from=builder /net-init /usr/local/bin/net-init
# Optional: Add entrypoint/cmd if you want this image runnable by itself
# ENTRYPOINT ["/usr/local/bin/net-init"]