# Dev notes

## FFI boundary (Go ↔ Rust)

**Never hand-edit** `pkg/rustpushgo/rustpushgo.go` or `rustpushgo.h`. Always regenerate.

### Linux / macOS

```bash
make bindings   # requires uniffi-bindgen-go on PATH
make build
```

Install `uniffi-bindgen-go` (must match UniFFI 0.25.0 as pinned in `pkg/rustpushgo/Cargo.toml`):
```bash
cargo install uniffi-bindgen-go --git https://github.com/NordSecurity/uniffi-bindgen-go --tag v0.2.2+v0.25.0
```

### Windows

The Makefile's `bindings` target hardcodes UNIX static-lib naming
(`librustpushgo.a`). MSVC Rust emits `rustpushgo.lib` instead, so `make
bindings` on Windows can't find the artifact. Use the Windows-native helper
scripts under `dev/` instead:

```cmd
dev\windows-dev-env.bat && dev\windows-bindings.bat
```

- `dev\windows-dev-env.bat` — one-shot environment bootstrap: vcvarsall,
  LIB/INCLUDE, PATH (cargo, Strawberry Perl, CMake, Python, protoc, LLVM),
  LIBCLANG_PATH, `python3` shim, and a first-run `cargo install` for
  `uniffi-bindgen-go`. Idempotent — safe to re-run.
- `dev\windows-bindings.bat` — mirrors `make bindings`: builds the crate if
  the `.lib` is missing, runs `uniffi-bindgen-go` against `rustpushgo.lib`,
  then runs `python3 scripts/patch_bindings.py`. Respects `CARGO_TARGET_DIR`
  if set (so users with cloud-sync folders can redirect cargo output
  elsewhere to avoid file-lock races on openssl-sys intermediates).

The one-time winget installs needed for the dev-env script to work are
documented at the top of `dev\windows-dev-env.bat`.
