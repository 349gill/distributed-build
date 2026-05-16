FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -o /out/orchestrator ./cmd/orchestrator
RUN CGO_ENABLED=0 go build -o /out/worker ./cmd/worker
RUN CGO_ENABLED=0 go build -o /out/client ./cmd/client

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/orchestrator /usr/local/bin/orchestrator
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/client /usr/local/bin/client
