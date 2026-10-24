# syntax=docker/dockerfile:1
FROM docker.io/library/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/scree ./cmd/scree \
 && mkdir -p /out/data

# distroless/static has CA certificates for the HTTPS embeddings API and runs
# as an unprivileged user (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/scree /usr/local/bin/scree
# Owned by nonroot so a fresh named volume mounted here is writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENV SCREE_NOTES=/notes \
    SCREE_DB=/data/index.db
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/scree"]
CMD ["watch"]
