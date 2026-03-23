# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.24 AS builder

WORKDIR /workspace

COPY components/wp-operator/go.mod components/wp-operator/go.sum ./
RUN go mod download

COPY components/wp-operator/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -a -o manager ./cmd/

# Runtime stage — static binary requires no libc
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /workspace/manager /manager

EXPOSE 8080 8081

ENTRYPOINT ["/manager"]
