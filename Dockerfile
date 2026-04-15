# ==========================================
# STAGE 1: Build the Custom k6 Binary
# ==========================================
FROM golang:1.25-alpine AS k6-builder

# Install git required for xk6
RUN apk add --no-cache git

# Install the xk6 builder tool
RUN go install go.k6.io/xk6/cmd/xk6@latest

# Build the custom k6 binary with the TimescaleDB extension
WORKDIR /build
RUN xk6 build --with github.com/grafana/xk6-output-timescaledb

# ==========================================
# STAGE 2: Build your Go Orchestrator API
# ==========================================
FROM golang:1.25-alpine AS api-builder
WORKDIR /app

# Copy dependency files first for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the code and build
COPY . .
RUN go build -o runner-api K6_runner.go

# ==========================================
# STAGE 3: Final Production Image
# ==========================================
FROM alpine:latest
WORKDIR /app

# Install CA certificates (required for k6 to make HTTPS requests)
RUN apk add --no-cache ca-certificates

# Copy the custom k6 binary from Stage 1
COPY --from=k6-builder /build/k6 /app/k6

# Copy your Go API binary from Stage 2
COPY --from=api-builder /app/runner-api /app/runner-api

# Create folders for volume mounts
RUN mkdir -p /app/scripts /app/logs

# Expose API and Metrics ports
EXPOSE 8080 2112

# Run the Go API
CMD ["/app/runner-api"]