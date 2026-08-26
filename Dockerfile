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

# No HEALTHCHECK here on purpose: distroless has no shell or curl to run one.
# Health is checked over HTTP at GET /healthz by Coolify or the compose file.
ENTRYPOINT ["/telemetry-ingest"]
