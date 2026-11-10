# Build stage
FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go openapi.json ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /api .

# Final stage: static binary, no shell, no libc
FROM gcr.io/distroless/static-debian12:nonroot
ARG BUILD_COMMIT=unknown
LABEL org.opencontainers.image.revision=$BUILD_COMMIT \
      org.opencontainers.image.source="https://github.com/PulsHealth/pulshealth"
COPY --from=build /api /api
EXPOSE 8081
USER 65532:65532
ENTRYPOINT ["/api"]
