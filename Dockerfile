# Stage 1: Build the UI and binary
FROM golang:1.22-alpine AS builder

# Install build dependencies
RUN apk add --no-cache make git

# Set working directory
WORKDIR /app

# Copy dependency files
COPY go.mod go.sum Makefile ./
RUN make install-dep

# Copy the rest of the application source code
COPY . .

# Build the application using Makefile
RUN CGO_ENABLED=0 make build

# Stage 2: Final image
FROM alpine:latest

# Set working directory
WORKDIR /app

# Copy the binary and config from the builder stage
COPY --from=builder /app/bin/lwnsimulator ./lwnsbin
COPY --from=builder /app/bin/config.json .

# Expose ports based on config.json (8000 for web interface and 8001 for metrics)
EXPOSE 8000
EXPOSE 8001

# Command to run the application
ENTRYPOINT ["./lwnsbin"]
