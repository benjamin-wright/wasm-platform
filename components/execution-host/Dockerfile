# syntax=docker/dockerfile:1
# Build context: components/execution-host/
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
COPY --parents --from=workspace proto/configsync/v1/configsync.proto ./
COPY --from=workspace framework/runtime.wit ./framework/runtime.wit
COPY --from=workspace lib/ ./lib/
COPY . ./components/execution-host/
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=cargo-git,target=/usr/local/cargo/git \
    --mount=type=cache,id=execution-host-target,target=/build/target \
    cargo build --release --bin execution-host && \
    cp target/release/execution-host /build/execution-host

# ── Stage 4: runtime ──────────────────────────────────────────────────────────
# distroless/cc-debian12 provides glibc and libgcc_s, both required at runtime:
# glibc for the standard Rust target, libgcc_s for stack unwinding. Switch to
# distroless/base-debian12 only if `ldd execution-host` confirms no libgcc dep.
FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=builder /build/execution-host /usr/local/bin/execution-host
EXPOSE 3000
CMD ["/usr/local/bin/execution-host"]
