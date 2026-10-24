# syntax=docker/dockerfile:1
# The build stage runs natively and cross-compiles, so multi-arch builds
# don't need emulation.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION#v}" -o /out/scree ./cmd/scree \
 && scripts/third-party-licenses.sh /out/licenses/third_party \
 && cp LICENSE /out/licenses/ \
 && mkdir -p /out/data

# distroless/static has CA certificates for the HTTPS embeddings API and runs
# as an unprivileged user (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="scree" \
      org.opencontainers.image.description="Local semantic search over markdown notes, served over MCP" \
      org.opencontainers.image.source="https://github.com/jrapakko/scree" \
      org.opencontainers.image.licenses="MIT" \
      io.modelcontextprotocol.server.name="io.github.jrapakko/scree"
COPY --from=build /out/scree /usr/local/bin/scree
COPY --from=build /out/licenses /usr/share/doc/scree
# Owned by nonroot so a fresh named volume mounted here is writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENV XDG_DATA_HOME=/data
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/scree"]
CMD ["watch", "-notes", "/notes"]
