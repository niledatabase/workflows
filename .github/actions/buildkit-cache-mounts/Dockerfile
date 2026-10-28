# The build context contains the binaries of make build.
FROM scratch
ARG TARGETARCH
COPY buildkit-cache-mounts-linux-${TARGETARCH} /buildkit-cache-mounts
# The frontend uses the network only in transfer steps.
LABEL moby.buildkit.frontend.network.none="true"
# The capabilities of the Dockerfile frontend, which builds each forwarded build (https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerfile/cmd/dockerfile-frontend/Dockerfile).
LABEL moby.buildkit.frontend.caps="moby.buildkit.frontend.inputs,moby.buildkit.frontend.subrequests,moby.buildkit.frontend.contexts,moby.buildkit.frontend.gitquerystring,moby.buildkit.frontend.contexts.zstd"
ENTRYPOINT ["/buildkit-cache-mounts", "frontend"]
