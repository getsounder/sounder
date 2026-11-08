FROM golang:1.27.3 AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} \
    CGO_ENABLED=0
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags "-s -w" -o /out/sounder-server ./cmd/sounder-server

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/sounder-server /usr/local/bin/sounder-server

USER 65534:65534
EXPOSE 8000

ENTRYPOINT ["sounder-server"]
CMD ["-addr=0.0.0.0:8000", "-gateway=http://gateway:9090"]
