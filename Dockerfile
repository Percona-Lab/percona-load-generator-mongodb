# --- Stage 1: Builder ---
# We use the official Go image to compile the code
FROM golang:1.25 AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy all your source code into the container
COPY . .

# Disable CGO to ensure the binary runs on Alpine Linux
ENV CGO_ENABLED=0

# Build the binary
# We target main.go 
RUN go build -o plgm ./cmd/plgm

# --- Stage 2: Runner ---
# We use a tiny Alpine Linux image for the final container
FROM alpine:latest

WORKDIR /app

# Copy the compiled binary from the builder stage
COPY --from=builder /app/plgm .

# Copy the config file so the app can read it
COPY config.yaml .

# Also copy the resources folder so we have access to all queries/collections 
COPY resources/ resources/

# Command to run when the container starts
CMD ["./plgm", "config.yaml"]
