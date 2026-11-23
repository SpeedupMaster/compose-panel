# Compose Panel: one static Go binary with the React UI embedded.
#   docker build -t compose-panel .
#   docker build --build-arg GOPROXY=https://goproxy.cn,direct --build-arg NPM_REGISTRY=https://registry.npmmirror.com -t compose-panel .

# ---- web ----
FROM node:20-alpine AS web
ARG NPM_REGISTRY=https://registry.npmjs.org
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm config set registry "$NPM_REGISTRY" && npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# ---- go ----
FROM golang:1-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web/embed.go ./web/embed.go
COPY --from=web /src/web/dist ./web/dist
RUN go vet ./... && go build -trimpath -ldflags="-s -w" -o /out/panel ./cmd/panel

# ---- compose ----
FROM alpine:3.20 AS compose-bin
RUN apk add --no-cache docker-cli-compose

# ---- runtime ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates git openssh-client \
 && mkdir -p /data/panel /run/caddy
COPY --from=compose-bin /usr/libexec/docker/cli-plugins/docker-compose /usr/local/bin/docker-compose
COPY --from=build /out/panel /usr/local/bin/panel
ENV PANEL_LISTEN=:7800 PANEL_DATA_DIR=/data/panel GOMEMLIMIT=192MiB GOGC=50
EXPOSE 7800
VOLUME ["/data/panel"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD wget -qO- http://127.0.0.1:7800/api/setup/status >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/panel"]
CMD ["serve"]
