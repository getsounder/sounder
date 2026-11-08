# 桌面「本地容器」与云端 K8s 使用同一镜像 digest。
FROM golang:1.27.3 AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} \
    CGO_ENABLED=0
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags "-s -w" -o /out/sounder-worker ./cmd/sounder-worker

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      bash ca-certificates git ripgrep fd-find curl \
    && ln -sf "$(command -v fdfind)" /usr/local/bin/fd \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 pig

COPY --from=build /out/sounder-worker /usr/local/bin/sounder-worker

USER 10001
ENV PIG_HOME=/data \
    HOME=/home/pig
WORKDIR /workspace
EXPOSE 8080

ENTRYPOINT ["sounder-worker"]
CMD ["-addr=0.0.0.0:8080", "-data=/data", "-max=4"]
