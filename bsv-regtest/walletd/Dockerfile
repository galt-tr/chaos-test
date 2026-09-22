# walletd: the go-wallet-toolbox BRC-100 wallet as its own service, with a small HTTP API
# (deposit address, state, outputs, actions, build/send, internalize). Its own Go module and
# image: go-wallet-toolbox needs gorm's real sqlite driver, which the other modules replace with
# a cgo-free one, and keeping it apart keeps ~300 modules off their dependency graphs.
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
