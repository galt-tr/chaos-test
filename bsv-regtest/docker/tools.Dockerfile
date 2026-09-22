# Tools image: alertctl and stackctl built from this module (build context: the bsv-regtest directory).
FROM docker.io/library/golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/alertctl ./cmd/alertctl && \
    CGO_ENABLED=0 go build -trimpath -o /out/stackctl ./cmd/stackctl

FROM docker.io/library/alpine:3.21
RUN apk add --no-cache ca-certificates curl jq bash && mkdir -p /data
COPY --from=builder /out/alertctl /out/stackctl /usr/local/bin/
WORKDIR /data
ENTRYPOINT ["/bin/sh","-c"]
CMD ["sleep infinity"]
