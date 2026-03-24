# Build context: components/module-cache/
# Named build context: --build-context workspace=<repo-root>
#
# The primary context contains only this component's source. The workspace
# named context supplies the root Cargo.toml, Cargo.lock, and sibling
# Cargo.toml stubs that cargo-chef needs to resolve the full dependency graph.
# Source files from sibling workspace members are not required and are never
# sent to the daemon.
#
# NOTE: When a new workspace member is added to the root Cargo.toml, add a
# COPY line for its Cargo.toml and a mkdir/touch entry for its stub source
# in both the planner and builder stages. Stages are independent in a
# multi-stage build, so each must recreate the workspace skeleton.
#
# Requires BuildKit (default in Docker >= 23.0).

# ── Stage 1: cargo-chef installer ─────────────────────────────────────────────
# Pinned to the minimum Rust version declared in Cargo.toml.
FROM rust:1.91-slim-bookworm AS chef
RUN cargo install cargo-chef --locked
WORKDIR /build

# ── Stage 2: dependency planner ───────────────────────────────────────────────
# Copies only the Cargo manifest files cargo-chef needs to produce a complete
# recipe. Source files from sibling workspace members are not needed; minimal
# placeholder stubs satisfy cargo metadata's requirement for at least one target.
FROM chef AS planner
COPY --from=workspace Cargo.toml Cargo.lock ./
COPY --from=workspace components/execution-host/Cargo.toml ./components/execution-host/Cargo.toml
COPY --from=workspace examples/hello-world/Cargo.toml ./examples/hello-world/Cargo.toml
RUN mkdir -p components/execution-host/src examples/hello-world/src \
    && echo 'fn main() {}' > components/execution-host/src/main.rs \
    && touch examples/hello-world/src/lib.rs
COPY . ./components/module-cache/
RUN cargo chef prepare --recipe-path recipe.json

# ── Stage 3: binary builder ───────────────────────────────────────────────────
# Cooks all dependency crates before copying application source. Source-only
# changes will cache-hit at the `cargo chef cook` layer.
FROM chef AS builder
COPY --from=planner /build/recipe.json recipe.json
RUN cargo chef cook --release --recipe-path recipe.json
COPY --from=workspace Cargo.toml Cargo.lock ./
COPY --from=workspace components/execution-host/Cargo.toml ./components/execution-host/Cargo.toml
COPY --from=workspace examples/hello-world/Cargo.toml ./examples/hello-world/Cargo.toml
RUN mkdir -p components/execution-host/src examples/hello-world/src \
    && echo 'fn main() {}' > components/execution-host/src/main.rs \
    && touch examples/hello-world/src/lib.rs
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
