# Contributing

- Issues and pull requests are welcome; small, focused changes land fastest.
- `compose.yaml` and everything under `config/` are **generated** from `cmd/gen/templates.go`.
  Change the template, run `make gen`, and commit both the template and the rendered files.
- Run `make test` and `make test-walletd` before opening a pull request (walletd is a separate
  Go module outside the workspace).
- If you touch the Makefile or the compose template, try both `RUNTIME=podman` and
  `RUNTIME=docker`; say in the PR which one you could actually run.
- Patches in `patches/teranode/` must apply to `TERANODE_REF` (`make build-teranode` stops
  loudly when one does not). Prefer upstreaming a change and note the upstream PR in
  `docs/patches.md`.
- The keys in `config/keys.json` are regtest-only development keys. Never add real keys, and
  never point this network at a public chain.
- Keep the README scannable: details belong under `docs/`.
- Commit messages: a short subject and a body that says why. No sign-off or attribution
  trailers are required.
- By contributing you agree that your contributions are licensed under the Apache License 2.0
  (see `LICENSE`).
