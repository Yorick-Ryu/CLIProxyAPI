FROM rust:1.92-bookworm AS codex-deflate

WORKDIR /codec

COPY third_party/gorilla-websocket/codex-deflate/ ./

RUN cargo build --locked --release \
    && objcopy --remove-section=.llvmbc --remove-section=.llvmcmd target/release/libcpa_codex_deflate.a

FROM golang:1.26-bookworm AS builder

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends build-essential git && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
COPY third_party/gorilla-websocket ./third_party/gorilla-websocket

RUN go mod download

COPY . .
COPY --from=codex-deflate /codec/target/release/libcpa_codex_deflate.a ./third_party/gorilla-websocket/codex-deflate/target/release/libcpa_codex_deflate.a

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN cd third_party/gorilla-websocket && CGO_ENABLED=1 go test -race -tags codex_zlib_rs ./...

RUN CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -tags codex_zlib_rs -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

FROM debian:bookworm

RUN apt-get update && apt-get install -y --no-install-recommends tzdata ca-certificates && rm -rf /var/lib/apt/lists/*

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
