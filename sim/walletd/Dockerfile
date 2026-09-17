# walletd: the go-wallet-toolbox BRC-100 wallet as its own service.
#
# Separate image because it is a separate Go module: the toolbox cannot be compiled into the
# orchestrator (chaos-test replaces gorm's sqlite driver for its alert datastore with one that
# lacks the API the toolbox needs), and keeping it out also keeps ~300 modules off the
# orchestrator's dependency graph.
FROM docker.io/library/golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/walletd .

FROM docker.io/library/alpine:3.21
RUN apk add --no-cache ca-certificates curl
COPY --from=builder /out/walletd /usr/local/bin/walletd
ENV LISTEN=:8700 KEYS=/config/keys.json BSV_NETWORK=tstn
EXPOSE 8700
ENTRYPOINT ["walletd"]
