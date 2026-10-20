# Multi-stage Dockerfile for Ephemeral Forum
FROM golang:1.25-bookworm AS builder

WORKDIR /src
RUN apt-get update && apt-get install -y gcc libc6-dev && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -ldflags="-s -w" -o /bin/forum ./cmd/forum \
 && CGO_ENABLED=1 go build -ldflags="-s -w" -o /bin/forumctl ./cmd/forumctl \
 && CGO_ENABLED=1 go build -ldflags="-s -w" -o /bin/merkleverify ./cmd/merkleverify

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y ca-certificates curl && rm -rf /var/lib/apt/lists/*
RUN useradd -u 1000 -m -s /bin/bash forum

WORKDIR /app
COPY --from=builder /bin/forum /usr/local/bin/forum
COPY --from=builder /bin/forumctl /usr/local/bin/forumctl
COPY --from=builder /bin/merkleverify /usr/local/bin/merkleverify
COPY web/static /app/web/static

RUN mkdir -p /app/threads /run/forum && chown -R forum:forum /app /run/forum

USER forum
VOLUME ["/app/threads"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/forum"]
CMD ["-listen", "0.0.0.0:8080", "-data", "/app/threads", "-static", "/app/web/static", "-sock", "/app/threads/admin.sock"]
