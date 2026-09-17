# Orchestrator image: builds the React UI, embeds it, builds the Go binary.
FROM docker.io/library/node:22-alpine AS ui
WORKDIR /ui
COPY sim/ui/package.json sim/ui/package-lock.json* ./
RUN npm install --no-audit --no-fund
COPY sim/ui/ ./
# vite.config.ts writes to ../../internal/api/uidist relative to sim/ui; recreate that layout.
# UI=0 skips the UI build (API-only image; the embedded placeholder page is served).
ARG UI=1
RUN mkdir -p /internal/api/uidist && sed -i "s#'../../internal/api/uidist'#'/internal/api/uidist'#" vite.config.ts && \
    if [ "$UI" = "1" ]; then npm run build; else cp index.html /internal/api/uidist/index.html; fi

FROM docker.io/library/golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /internal/api/uidist ./internal/api/uidist
RUN CGO_ENABLED=0 go build -trimpath -o /out/orchestrator ./cmd/orchestrator

FROM docker.io/library/alpine:3.21
RUN apk add --no-cache ca-certificates curl && mkdir -p /data /config /scenarios
COPY --from=builder /out/orchestrator /usr/local/bin/orchestrator
COPY scenarios/ /scenarios/
ENV INVENTORY=/config/inventory.json KEYS=/config/keys.json DATA=/data SCENARIOS=/scenarios LISTEN=:8600
EXPOSE 8600 9909
ENTRYPOINT ["orchestrator"]
