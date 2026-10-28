# BuildKit cache mounts

`buildkit-cache-mounts` is a BuildKit frontend that keeps the contents of Dockerfile cache mounts (`RUN --mount=type=cache`) in the GitHub Actions cache. It builds the Dockerfile with the Dockerfile frontend of the builder. Before the build, it restores each cache mount from the cache.

```yaml
- uses: docker/setup-buildx-action@v4
- uses: docker/build-push-action@v7
  with:
    build-args: |
      BUILDKIT_SYNTAX=ghcr.io/niledatabase/buildkit-cache-mounts@sha256:<digest>
      BUILDKIT_CACHE_MOUNTS_KEY=${{ runner.arch }}-${{ github.sha }}
      BUILDKIT_CACHE_MOUNTS_RESTORE_KEYS=${{ runner.arch }}-
    secret-envs: |
      ACTIONS_RUNTIME_TOKEN=ACTIONS_RUNTIME_TOKEN
      ACTIONS_RESULTS_URL=ACTIONS_RESULTS_URL
```

- `BUILDKIT_SYNTAX` selects the frontend. The syntax directive of the Dockerfile continues to select the version of the Dockerfile frontend.
- `BUILDKIT_CACHE_MOUNTS_KEY` is the cache key. Without it, the frontend only builds the Dockerfile. `BUILDKIT_CACHE_MOUNTS_RESTORE_KEYS` contains comma-separated key prefixes, in order of preference, for the restore when no exact key matches. The Dockerfile does not get these build arguments.
- `secret-envs` gives the runtime token and the Results service URL to the build. The runner sets them only for JavaScript and container actions, such as `docker/build-push-action` (actions/runner#3046).
- Each cache mount gets its own cache entry. Put `runner.arch` in the key, so that runners with different architectures do not share cache entries.
- The frontend skips mounts with `sharing=private` and mounts whose `from` source reads local files, such as the build context. The save skips empty mounts.
- The restore skips a mount that has files, for example from an earlier build on the same builder. BuildKit creates a mount with `from`, `uid`, `gid`, or `mode` from a source directory, so the restore always replaces the files of such a mount.
- A cache failure does not fail the build: the failed transfer step shows the error, and the build shows a warning.

## Development

Run these targets in this directory:

- `make test` runs the unit tests.
- `make integration` builds the binaries and runs the integration tests. These tests need Docker.
- `make lint` runs golangci-lint with `.golangci.yaml`.
- `make format` formats the code with the formatters in `.golangci.yaml`.

## Release

On each push to `main` that changes this directory, the release workflow builds the image `ghcr.io/niledatabase/buildkit-cache-mounts:<commit>` for `linux/amd64` and `linux/arm64`, pushes it, and attests it. The image build is reproducible: the digest changes only when the binaries, the `Dockerfile`, or the BuildKit version of the release workflow change. Reference the image by digest.
