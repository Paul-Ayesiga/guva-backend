# =============================================================================
# Multi-stage build for the identity service.
# Final image is distroless, non-root — same pattern as services/reference.
# =============================================================================

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-s -w -X main.version=$(date -u +%Y%m%d-%H%M%S)" \
      -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 7071
USER nonroot:nonroot
ENTRYPOINT ["/server"]
