# Build Go binary
FROM golang:1.25 AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG BUILDARGS
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 \
    go build -trimpath \
    -ldflags="-w -s -X=github.com/vocdoni/davinci-fold/internal.Version=$(git describe --always --tags --dirty --match='v[0-9]*' 2>/dev/null || echo dev)" \
    -o davinci-fold $BUILDARGS ./cmd/davinci-fold

# Final minimal image
FROM debian:bookworm-slim
WORKDIR /app

RUN apt-get update && \
    apt-get install --no-install-recommends -y ca-certificates && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /src/davinci-fold ./

RUN echo '#!/bin/sh' > entrypoint.sh && \
    echo 'exec ./davinci-fold "$@"' >> entrypoint.sh && \
    chmod +x entrypoint.sh

EXPOSE 8888
ENTRYPOINT ["/app/entrypoint.sh"]
