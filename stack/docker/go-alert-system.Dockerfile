# Builds the go-alert-system hub used as the private alert network's bootstrap/DHT server.
# Build context: a checkout of github.com/bsv-blockchain/go-alert-system (see Makefile,
# ALERT_SYSTEM_REF). Fully-qualified image names so podman needs no short-name prompt.
FROM docker.io/library/golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/go-alert-system ./cmd/go-alert-system

FROM docker.io/library/alpine:3.21
RUN apk add --no-cache ca-certificates curl && mkdir -p /.bitcoin /data && chown -R 65534:65534 /.bitcoin /data
COPY --from=builder /out/go-alert-system /go-alert-system
USER 65534:65534
ENV ALERT_SYSTEM_ENVIRONMENT=local
EXPOSE 3000 9906
CMD ["/go-alert-system"]
