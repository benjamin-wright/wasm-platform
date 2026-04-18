# syntax=docker/dockerfile:1
# Build context: components/gateway/
# Named build context: --build-context workspace=<repo-root>
#
# Requires BuildKit (default in Docker >= 23.0).
# WORKSPACE_DEPS_IMAGE must be the tag produced by Dockerfile.deps; it
# contains the Rust toolchain, protobuf-compiler, and pre-cooked target/.

# ── Stage 1: binary builder ───────────────────────────────────────────────────
# All third-party crates are already compiled in the base image.
# Only application source is compiled here.
ARG WORKSPACE_DEPS_IMAGE=lukemathwalker/cargo-chef:latest-rust-1.91-slim-bookworm
FROM ${WORKSPACE_DEPS_IMAGE} AS builder
COPY --from=workspace Cargo.toml Cargo.lock ./
COPY --parents --from=workspace components/*/Cargo.toml examples/*/Cargo.toml lib/*/Cargo.toml ./
RUN find components examples lib -maxdepth 2 -name Cargo.toml -exec dirname {} \; \
    | xargs -I{} sh -c 'mkdir -p {}/src && touch {}/src/lib.rs {}/src/main.rs'
COPY --parents --from=workspace proto/gateway/v1/gateway.proto ./
COPY --from=workspace lib/ ./lib/
COPY . ./components/gateway/
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=cargo-git,target=/usr/local/cargo/git \
    cargo build --release --bin gateway && \
    cp target/release/gateway /build/gateway

# ── Stage 4: runtime ──────────────────────────────────────────────────────────
FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=builder /build/gateway /usr/local/bin/gateway
EXPOSE 3000
CMD ["/usr/local/bin/gateway"]
