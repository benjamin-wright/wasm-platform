# syntax=docker/dockerfile:1

# Build context: component directory (components/wp-operator/).
# Requires BuildKit (default in Docker >= 23.0).

FROM golang:1.24 AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 GOOS=linux go build -a -o manager ./cmd/

# Runtime stage — CGO_ENABLED=0 produces a fully static binary; no libc required.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /workspace/manager /manager

EXPOSE 8080 8081

ENTRYPOINT ["/manager"]
