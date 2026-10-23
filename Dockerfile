# ---- frontend build ----
FROM node:22-alpine AS webui
ARG NPM_REGISTRY=https://registry.npmmirror.com
WORKDIR /src
COPY webui/package.json webui/package-lock.json* ./webui/
RUN cd webui && (npm ci --no-audit --no-fund --registry=$NPM_REGISTRY || npm install --no-audit --no-fund --registry=$NPM_REGISTRY)
COPY webui ./webui
RUN mkdir -p internal/web/static && cd webui && npm run build

# ---- backend build ----
FROM golang:1.26-alpine AS build
ARG VERSION=dev
ARG GOPROXY=https://goproxy.cn,direct
ENV CGO_ENABLED=0 GOFLAGS=-trimpath GOPROXY=$GOPROXY
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=webui /src/internal/web/static ./internal/web/static
RUN go build -ldflags "-s -w -X main.Version=$VERSION" -o /out/mytun ./cmd/mytun

# ---- runtime ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 10001 mytun || true
COPY --from=build /out/mytun /usr/local/bin/mytun
RUN mkdir -p /data && chown -R 10001:10001 /data 2>/dev/null || true
VOLUME ["/data"]
# 8899 console, 33891 default tunnel port. Port maps and proxies add their own.
EXPOSE 8899 33891
WORKDIR /data
ENV MYTUN_HOME=/data
ENTRYPOINT ["/usr/local/bin/mytun"]
CMD ["--data", "/data", "--host", "0.0.0.0", "--port", "8899"]
