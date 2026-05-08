# Builder
FROM golang:1.24-alpine AS builder
WORKDIR /otelite
# Download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /otelite-bin .

# Final Image
FROM alpine:3.21
RUN apk add --no-cache ca-certificates sqlite
COPY --from=builder /otelite-bin /usr/local/bin/otelite
EXPOSE 4318
ENTRYPOINT ["otelite", "server", "-port", "4318", "-db", "/data/otel.db"]
