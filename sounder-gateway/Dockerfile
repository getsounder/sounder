# Gateway 无业务状态，镜像可以很小；保留 curl 供 healthcheck 使用。
FROM golang:1.27.3 AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} \
    CGO_ENABLED=0
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags "-s -w" -o /out/sounder-gateway ./cmd/sounder-gateway

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/sounder-gateway /usr/local/bin/sounder-gateway

USER 65534:65534
EXPOSE 9090

ENTRYPOINT ["sounder-gateway"]
CMD ["-addr=0.0.0.0:9090"]
