# syntax=docker/dockerfile:1
# Uses online tagged auth-client (go.mod replace => github.com/Bengo-Hub/auth-client v0.3.1).
# Build from repo root: docker build -f pos-service/pos-api/Dockerfile -t pos-api:local .

FROM golang:1.24-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY pos-service/pos-api/go.mod pos-service/pos-api/go.sum ./
RUN go mod download
COPY pos-service/pos-api .

RUN CGO_ENABLED=0 go build -o /out/pos ./cmd/api

FROM alpine:3.20
RUN addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=builder /out/pos /app/service
RUN mkdir -p ./config/certs
USER app
EXPOSE 4000
ENV PORT=4000
ENTRYPOINT ["/app/service"]
