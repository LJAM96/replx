# Replx Edge production image (multi-arch amd64/arm64).
# Builder compiles Go + admin frontend; final image is distroless nonroot.
ARG GO_VERSION=1.25

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY migrations/ ./migrations/
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
  -o /out/replx-edge ./cmd/replx-edge

FROM node:22-bookworm-slim AS web-builder
WORKDIR /web
COPY web/admin/package.json ./
RUN npm install
COPY web/admin/ ./
RUN npm run build

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="replx-edge"
LABEL org.opencontainers.image.source="https://github.com/LJAM96/replx"
COPY --from=go-builder /out/replx-edge /app/replx-edge
COPY --from=web-builder /web/dist/ /app/web/dist/
# CA certs come from distroless base. Writable paths are mounted volumes.
# /data/cache /data/artwork /data/diagnostics, plus /tmp tmpfs.
USER 65532:65532
ENTRYPOINT ["/app/replx-edge"]
CMD ["serve"]
