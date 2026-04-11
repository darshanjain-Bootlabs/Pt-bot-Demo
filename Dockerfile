# Use Go base image
FROM golang:1.23

# Install k6 - download binary instead of using apt (which doesn't support ARM64)
RUN apt-get update && apt-get install -y wget && \
    wget -q -O /tmp/k6-linux-arm64.tar.gz https://github.com/grafana/k6/releases/download/v0.52.0/k6-v0.52.0-linux-arm64.tar.gz && \
    tar xzf /tmp/k6-linux-arm64.tar.gz -C /tmp && \
    mv /tmp/k6-v0.52.0-linux-arm64/k6 /usr/local/bin/k6 && \
    chmod +x /usr/local/bin/k6 && \
    rm -rf /tmp/k6* && \
    apt-get remove -y wget && apt-get clean

# Set working directory
WORKDIR /app

# Copy files
COPY . .

# Build Go app
RUN go mod tidy && go build -o app .

# Run app
CMD ["./app"]