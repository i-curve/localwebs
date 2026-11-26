ARG NODE_IMAGE=node:22-alpine
ARG GO_IMAGE=golang:1.26-alpine

FROM --platform=$BUILDPLATFORM $NODE_IMAGE AS web
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY index.html vite.config.js tsconfig.json ./
COPY src ./src
RUN npm run build

FROM --platform=$BUILDPLATFORM $GO_IMAGE AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go config.system.json ./
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -trimpath -tags nomsgpack -ldflags="-s -w" -o /localwebs . \
    && mkdir -m 700 /data && touch /data/.keep

FROM scratch
COPY --from=backend /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=backend /localwebs /localwebs
COPY --from=backend --chown=65532:65532 /data /data
COPY --from=web /src/dist /web
ENV LOCALWEBS_WEB_DIR=/web LOCALWEBS_CONFIG_FILE=/data/config.json
VOLUME ["/data"]
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/localwebs"]
