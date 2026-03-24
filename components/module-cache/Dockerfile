# Build context: components/module-cache/
# Named build context: --build-context workspace=<repo-root>
#
# The primary context contains only this component's source. The workspace
# named context supplies Cargo manifests and source for all sibling members
# so cargo-chef can resolve the full dependency graph. Compiled artifacts are
# excluded from the workspace context via .dockerignore (target/ is ignored),
# so only source files are transferred.
#
# Requires BuildKit (default in Docker >= 23.0).

# ── Stage 1: cargo-chef installer ─────────────────────────────────────────────
# Pinned to the minimum Rust version declared in Cargo.toml.
FROM rust:1.91-slim-bookworm AS chef
RUN cargo install cargo-chef --locked
WORKDIR /build

# ── Stage 2: dependency planner ───────────────────────────────────────────────
FROM chef AS planner
COPY --from=workspace Cargo.toml Cargo.lock ./
COPY --from=workspace components/ ./components/
COPY --from=workspace examples/ ./examples/
COPY . ./components/module-cache/
RUN cargo chef prepare --recipe-path recipe.json

# ── Stage 3: binary builder ───────────────────────────────────────────────────
# Cooks all dependency crates before copying application source. Source-only
# changes will cache-hit at the `cargo chef cook` layer.
FROM chef AS builder
COPY --from=planner /build/recipe.json recipe.json
RUN cargo chef cook --release --recipe-path recipe.json
COPY --from=workspace Cargo.toml Cargo.lock ./
COPY --from=workspace components/ ./components/
COPY --from=workspace examples/ ./examples/
COPY . ./components/module-cache/
RUN cargo build --release --bin module-cache

# ── Stage 4: runtime ──────────────────────────────────────────────────────────
# distroless/cc-debian12 provides glibc and libgcc_s required by the standard
# Rust target. Switch to distroless/static-debian12 only if `ldd module-cache`
# confirms no dynamic library dependencies.
FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=builder /build/target/release/module-cache /usr/local/bin/module-cache
EXPOSE 3000
CMD ["/usr/local/bin/module-cache"]
