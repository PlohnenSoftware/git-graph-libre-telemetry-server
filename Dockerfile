# Build stage. Nothing from here reaches the final image except the binary.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Copied first so the module download layer caches independently of source
# changes — the dependency set moves far less often than the code.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a static binary, which is what lets the runtime image
# be distroless/static with no libc. -trimpath keeps build paths out of the
# binary; -s -w drop the symbol table and DWARF info.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/telemetry-ingest .

# Runtime stage. distroless/static rather than scratch: it carries CA
# certificates (needed if DATABASE_URL ever points at a TLS Postgres) and
# tzdata, and it ships a nonroot user. No shell, no package manager, nothing
# to exec into.
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=build /out/telemetry-ingest /telemetry-ingest

# Documentation only; the port is set by PORT and published by compose.
EXPOSE 8080

USER nonroot:nonroot

# The binary is its own probe. Distroless has no shell and no curl, so there is
# nothing else in the image that could run a check — and the exec (JSON) form is
# mandatory here for the same reason: the shell form would need /bin/sh.
#
# This does not go through ENTRYPOINT; Docker execs it directly, which is why
# the full path is repeated.
HEALTHCHECK --interval=15s --timeout=8s --start-period=10s --retries=3 \
    CMD ["/telemetry-ingest", "healthcheck"]

ENTRYPOINT ["/telemetry-ingest"]
