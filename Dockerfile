# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
# The whole dependency set is pure Go (including modernc.org/sqlite), so the
# binary builds fully static with CGO disabled and runs on distroless/static
# with no libc.
FROM golang:1.25 AS build
WORKDIR /src

# Resolve modules first so this layer caches unless go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/datadock .

# Create the data directory here (distroless has no shell to mkdir/chown), so
# it lands in the image owned by the nonroot uid and a fresh volume inherits it.
RUN mkdir -p /data

# ---- runtime stage ---------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --chown=65532:65532 /data /data
COPY --from=build /out/datadock /usr/local/bin/datadock

WORKDIR /data
# Mount a volume here to persist the embedded DB across container restarts.
VOLUME ["/data"]
EXPOSE 8080

# Probe via the binary itself — distroless has no shell/wget/curl. The
# -healthcheck flag GETs /healthz on the loopback -addr and exits 0/1.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/datadock", "-addr", ":8080", "-healthcheck"]

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/datadock"]
# Bind all interfaces inside the container; publish/proxy it from the host.
CMD ["-addr", ":8080", "-db", "/data/datadock.db"]
