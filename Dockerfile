# Build stage
FROM golang:1.23 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /maritacaproxy ./cmd/maritacaproxy

# Runtime stage
FROM python:3.12-slim

# Install playwright + chromium
RUN pip install --no-cache-dir playwright \
    && playwright install chromium \
    && playwright install-deps chromium

# Copy binary
COPY --from=builder /maritacaproxy /usr/local/bin/maritacaproxy

# Create data directory
WORKDIR /app
RUN mkdir -p /app/data

# Default env
ENV PORT=3000
ENV HOST=0.0.0.0
ENV AUTO_ACCOUNT_HEADLESS=true
ENV TEMPMAIL_PROVIDER=mailtm

EXPOSE 3000

ENTRYPOINT ["maritacaproxy"]
