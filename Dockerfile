# Multi-stage build for drone telemetry server
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /drone-server ./cmd/server

# ---

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /drone-server /drone-server
COPY --from=builder /build/config.example.yaml /etc/drone-server/config.yaml

EXPOSE 14550/udp
EXPOSE 8080/tcp
EXPOSE 9090/tcp

USER nonroot:nonroot

ENTRYPOINT ["/drone-server"]
CMD ["-config", "/etc/drone-server/config.yaml"]
