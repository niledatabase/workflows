# BuildKit cache mounts

The `buildkit-cache-mounts` actions keep the contents of Dockerfile cache mounts (`RUN --mount=type=cache`) in the GitHub Actions cache. Run `restore` before the image build and `save` after it, with the same Buildx builder. Use the same `file`, `target`, `platforms`, and `build-args` as the build.

```yaml
- id: buildx
  uses: docker/setup-buildx-action@v4
- uses: niledatabase/workflows/.github/actions/buildkit-cache-mounts/restore@main
  with:
    builder: ${{ steps.buildx.outputs.name }}
    key: ${{ runner.arch }}-${{ github.sha }}
    restore-keys: |
      ${{ runner.arch }}-
- uses: docker/build-push-action@v7
  with:
    builder: ${{ steps.buildx.outputs.name }}
- uses: niledatabase/workflows/.github/actions/buildkit-cache-mounts/save@main
  with:
    builder: ${{ steps.buildx.outputs.name }}
    key: ${{ runner.arch }}-${{ github.sha }}
```

- Each cache mount gets its own cache entry. Put `runner.arch` in the key, so that runners with different architectures do not share cache entries.
- The actions skip mounts with `sharing=private` and mounts whose `from` source reads local files, such as the build context. `save` skips empty mounts.
- A cache failure does not fail the job: the failed step shows an error, and the job continues. If the binary download or its attestation verification fails, the actions skip the cache.

## Development

Run these targets in this directory:

- `make test` runs the unit tests.
- `make integration` builds the binaries and runs the integration tests. These tests need Docker.
- `make lint` runs golangci-lint with `.golangci.yaml`.
- `make format` formats the code with the formatters in `.golangci.yaml`.

## Release

The actions download the binary from the release for the commit in the `binary-commit` input. `gh attestation verify` makes sure that the release workflow built the binary on `main` from that commit.

On each push to `main` that changes this directory, the release workflow builds the binaries. If they differ from the binaries of the `binary-commit` default, the workflow attests them and publishes the release `buildkit-cache-mounts-<commit>`.

To use a new release, set the `binary-commit` default in `restore/action.yaml` and `save/action.yaml` to the commit of the release. The `verify` job fails if the two defaults differ.
