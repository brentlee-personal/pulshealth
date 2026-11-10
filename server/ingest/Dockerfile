# Build stage
FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
# Surfaced by GET /v1/capabilities as "version"; empty (a plain docker build
# with no arg) reports "dev". ARG scope is per stage, so it is declared again
# below for the image label.
ARG BUILD_COMMIT
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.buildVersion=${BUILD_COMMIT}" -o /ingest .

# Final stage: static binary, no shell, no libc
FROM gcr.io/distroless/static-debian12:nonroot
ARG BUILD_COMMIT=unknown
LABEL org.opencontainers.image.revision=$BUILD_COMMIT \
      org.opencontainers.image.source="https://github.com/PulsHealth/pulshealth"
COPY --from=build /ingest /ingest
EXPOSE 8080
USER 65533:65533
ENTRYPOINT ["/ingest"]
