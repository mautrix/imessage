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

## rustpush overlay patches

`third_party/rustpush-upstream/` is a fresh clone of `OpenBubbles/rustpush`
at the SHA pinned in `third_party/rustpush-upstream.sha`. We don't fork —
we apply small, surgical sed patches at build time inside the
`ensure-rustpush-source` Makefile target. Each patch is one-line, idempotent,
and gated on the pristine upstream marker so reruns are a no-op.

### Anatomy of a patch

```make
if grep -q '<original-upstream-text>' $(RUSTPUSH_DIR)/<file> 2>/dev/null; then \
    echo "<what this is fixing>..."; \
    sed -i.bak 's|<old>|<new>|' $(RUSTPUSH_DIR)/<file> && rm -f $(RUSTPUSH_DIR)/<file>.bak; \
fi; \
```

- **Guard** (`grep -q`): matches the pristine upstream line. Once sed runs,
  the marker is gone — so the next `make` finds nothing to patch.
- **`sed -i.bak ... && rm -f ...bak`**: works on both BSD (macOS) and GNU sed.
- **Multi-line replacements** are awkward in cross-platform sed; collapse
  the new code onto a single line, or use two `-e` operations chained
  (one substitution + one delete) when an adjacent line also needs to
  go. See the `statuskit get_key` patch for an example of the latter
  (substitute the first line into a sentinel comment, delete the second).

### Why patches live in the Makefile, not on disk

`ensure-rustpush-source` runs `git reset --hard HEAD && git clean -fd` on the
worktree whenever the on-disk HEAD ≠ `RUSTPUSH_PIN`. Direct edits to files in
`third_party/rustpush-upstream/` survive across `make` runs at the same pin,
but vanish the moment the SHA is bumped — and they never reach CI or other
contributors. **Always put the fix in the Makefile.** A direct file edit only
exists on the editor's disk.

### CI verification

`.github/workflows/ci.yml` has a `verify-rustpush-patches` job that runs
`make ensure-rustpush-source` and `grep -Fq` for a unique marker from each
patch's *new* (post-sed) text. If a future SHA bump lands on a rustpush
revision where upstream reworded the guarded line, the sed silently
no-ops and the build still goes green with the patch dropped on the floor.
This job catches that case explicitly so the regression fails CI before
it ships.

### Adding a new patch

1. Add the `if grep -q …; sed -i.bak …; fi` block to `ensure-rustpush-source`
   in the `Makefile` (alongside the existing patches around lines 200–250).
2. Add a `check "<short desc>" "<file>" "<marker>"` line to the
   `verify-rustpush-patches` job in `.github/workflows/ci.yml`. The marker
   must be a substring of the *patched-in* text, unique enough that
   unrelated upstream changes won't accidentally satisfy it.
3. Run `make ensure-rustpush-source` locally and inspect the file to
   confirm the transform is correct, then run `make build` to confirm
   the result still compiles.
4. If the same fix is appropriate upstream, open a PR against
   `OpenBubbles/rustpush`. The local sed continues to apply until the
   PR merges and the SHA pin is bumped past it; at that point, the
   `grep` guard naturally stops matching and the patch self-deactivates.

### Removing a patch at SHA-bump time

When upstream merges an equivalent fix and you bump
`third_party/rustpush-upstream.sha`, **remove the Makefile sed block AND
its `check` line in the same commit.** The patch's `grep -q` guard will
stop matching (so the sed silently no-ops — correct), but the CI
`check` line is a *positive* assertion: it requires the post-sed marker
to be present in the source. If upstream fixed the same bug with a
different shape (e.g. `Vec::retain` vs our `any`+push), the marker
won't match and `verify-rustpush-patches` fails the SHA bump until you
delete the now-unnecessary `check`. That failure is the intended
forcing function — it's how we discover at bump time that a local
patch can be retired.
