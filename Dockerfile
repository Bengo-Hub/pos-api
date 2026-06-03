# syntax=docker/dockerfile:1
# Uses online tagged auth-client (go.mod replace => github.com/Bengo-Hub/auth-client v0.3.1).
# Build from repo root: docker build -f pos-service/pos-api/Dockerfile -t pos-api:local .

FROM golang:1.26-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./

# go.mod requires go >= 1.26; allow auto-toolchain as a fallback if the base image lags.
ENV GOTOOLCHAIN=auto
RUN go mod download

COPY . .

# Build all binaries: api, migrate, seed
RUN CGO_ENABLED=0 go build -o /out/pos-api ./cmd/api && \
    CGO_ENABLED=0 go build -o /out/pos-migrate ./cmd/migrate && \
    CGO_ENABLED=0 go build -o /out/pos-seed ./cmd/seed

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=builder /out/pos-api /usr/local/bin/pos-api
COPY --from=builder /out/pos-migrate /usr/local/bin/pos-migrate
COPY --from=builder /out/pos-seed /usr/local/bin/pos-seed
COPY internal/ent/migrate/migrations ./internal/ent/migrate/migrations
# Entrypoint script: run seed, then start server
COPY scripts/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
RUN mkdir -p ./config/certs
USER app
EXPOSE 4000
ENV PORT=4000
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
