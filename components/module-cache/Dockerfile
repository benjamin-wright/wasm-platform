# syntax=docker/dockerfile:1
# Build context: components/module-cache/
# Named build context: --build-context workspace=<repo-root>
#
# Requires BuildKit (default in Docker >= 23.0).
# WORKSPACE_DEPS_IMAGE must be the tag produced by Dockerfile.deps; it
# contains the Rust toolchain and pre-cooked target/.

# ── Stage 1: binary builder ───────────────────────────────────────────────────
# All third-party crates are already compiled in the base image.
# Only application source is compiled here.
ARG WORKSPACE_DEPS_IMAGE=lukemathwalker/cargo-chef:latest-rust-1.91-slim-bookworm
FROM ${WORKSPACE_DEPS_IMAGE} AS builder
COPY --from=workspace Cargo.toml Cargo.lock ./
COPY --parents --from=workspace components/*/Cargo.toml examples/*/Cargo.toml ./
RUN find components examples -maxdepth 2 -name Cargo.toml -exec dirname {} \; \
    | xargs -I{} sh -c 'mkdir -p {}/src && touch {}/src/lib.rs {}/src/main.rs'
COPY . ./components/module-cache/
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=cargo-git,target=/usr/local/cargo/git \
    --mount=type=cache,id=module-cache-target,target=/build/target \
    cargo build --release --bin module-cache && \
    cp target/release/module-cache /build/module-cache

# ── Stage 4: runtime ──────────────────────────────────────────────────────────
# distroless/cc-debian12 provides glibc and libgcc_s required by the standard
# Rust target. Switch to distroless/static-debian12 only if `ldd module-cache`
# confirms no dynamic library dependencies.
FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=builder /build/module-cache /usr/local/bin/module-cache
EXPOSE 3000
CMD ["/usr/local/bin/module-cache"]
