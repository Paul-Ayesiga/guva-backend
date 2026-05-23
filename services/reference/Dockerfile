# =============================================================================
# Multi-stage build for the reference service.
# Final image follows §13.2 — minimal base, no shell, no build tools.
# =============================================================================

# ---- 1. Build ----
FROM golang:1.22-alpine AS build
WORKDIR /src

# Cache module download as its own layer
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-s -w -X main.version=$(date -u +%Y%m%d-%H%M%S)" \
      -o /out/server ./cmd/server

# ---- 2. Runtime ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 7070
USER nonroot:nonroot
ENTRYPOINT ["/server"]
