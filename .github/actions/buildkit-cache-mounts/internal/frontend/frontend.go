package frontend

import (
	"context"
	"crypto/rand"
	"fmt"
	"maps"
	"strings"

	"github.com/moby/buildkit/frontend/gateway/client"
	gwpb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/solver/pb"
)

const (
	keyOpt         = "build-arg:BUILDKIT_CACHE_MOUNTS_KEY"
	restoreKeysOpt = "build-arg:BUILDKIT_CACHE_MOUNTS_RESTORE_KEYS"
	// The Dockerfile frontend forwards a build without cmdline to the image in BUILDKIT_SYNTAX, or else to the image in the syntax directive, and sets cmdline and source (Build and forwardGateway in https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerfile/builder/build.go).
	syntaxOpt  = "build-arg:BUILDKIT_SYNTAX"
	cmdlineOpt = "cmdline"
	sourceOpt  = "source"
	// requestIDOpt selects a subrequest, such as a lint or an outline, instead of a build (https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerui/requests.go).
	requestIDOpt       = "requestid"
	dockerfileFrontend = "dockerfile.v0"
)

// Frontend is a BuildKit frontend that builds a Dockerfile with the Dockerfile frontend of the builder and keeps the cache mounts of the build in the GitHub Actions cache.
type Frontend struct {
	transferArgs func(Transfer) []string
}

// New returns a Frontend whose transfer steps run the command-line arguments that transferArgs returns for a transfer, in the image of the frontend.
func New(transferArgs func(Transfer) []string) Frontend {
	return Frontend{transferArgs: transferArgs}
}

// Build builds the Dockerfile with the Dockerfile frontend of the builder.
// The build argument BUILDKIT_CACHE_MOUNTS_KEY sets the cache key, and BUILDKIT_CACHE_MOUNTS_RESTORE_KEYS sets comma-separated prefixes for the restore when no exact key matches.
// With a key, Build restores each cache mount of the build from the GitHub Actions cache before the build, and saves each non-empty cache mount after a successful build.
// The restore skips a cache mount without input that has files.
// The transfers read the build secrets ACTIONS_RUNTIME_TOKEN and ACTIONS_RESULTS_URL.
// A failed transfer adds a build warning and does not fail the build.
func (f Frontend) Build(ctx context.Context, c client.Client) (*client.Result, error) {
	opts := c.BuildOpts().Opts
	keys, dockerfileOpts := splitOpts(opts)
	request, err := dockerfileRequest(ctx, c, dockerfileOpts)
	if err != nil {
		return nil, err
	}
	if keys.key == "" || dockerfileOpts[requestIDOpt] != "" {
		return c.Solve(ctx, request)
	}
	if err := keys.validate(); err != nil {
		return solveWithoutCache(ctx, c, request, err)
	}
	platform, err := workerPlatform(c)
	if err != nil {
		return solveWithoutCache(ctx, c, request, err)
	}
	mounts, localInputIDs, err := discoverCacheMounts(ctx, c, dockerfileOpts)
	if err != nil {
		return solveWithoutCache(ctx, c, request, fmt.Errorf("discover cache mounts: %w", err))
	}
	for _, id := range localInputIDs {
		if err := warn(ctx, c, fmt.Sprintf("Skipped cache mount %s, because its input reads local files", id)); err != nil {
			return nil, err
		}
	}

	transfer := cacheTransfer{client: c, image: opts[sourceOpt], platform: platform, keys: keys, nonce: rand.Text(), args: f.transferArgs}
	if err := transfer.run(ctx, Restore, mounts); err != nil {
		return nil, err
	}
	// Without Evaluate, the RUN instructions run only when BuildKit exports the result, after the save.
	request.Evaluate = true
	result, err := c.Solve(ctx, request)
	if err != nil {
		return nil, err
	}
	if err := transfer.run(ctx, Save, mounts); err != nil {
		return nil, err
	}
	return result, nil
}

// splitOpts returns the cache keys in the frontend options opts, and the options for the Dockerfile frontend.
func splitOpts(opts map[string]string) (cacheKeys, map[string]string) {
	keys := cacheKeys{key: strings.TrimSpace(opts[keyOpt])}
	for restoreKey := range strings.SplitSeq(opts[restoreKeysOpt], ",") {
		if restoreKey = strings.TrimSpace(restoreKey); restoreKey != "" {
			keys.restoreKeys = append(keys.restoreKeys, restoreKey)
		}
	}
	dockerfileOpts := maps.Clone(opts)
	delete(dockerfileOpts, keyOpt)
	delete(dockerfileOpts, restoreKeysOpt)
	delete(dockerfileOpts, sourceOpt)
	// After the removal of BUILDKIT_SYNTAX and cmdline, the syntax directive of the Dockerfile applies.
	// Without BUILDKIT_SYNTAX, the syntax directive selected this frontend, and cmdline prevents a forward to this frontend again.
	if _, ok := dockerfileOpts[syntaxOpt]; ok {
		delete(dockerfileOpts, syntaxOpt)
		delete(dockerfileOpts, cmdlineOpt)
	}
	return keys, dockerfileOpts
}

// dockerfileRequest returns a request for a build with the Dockerfile frontend of the builder, the options opts, and the inputs of this frontend, like forwardGateway in https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerfile/builder/build.go.
func dockerfileRequest(ctx context.Context, c client.Client, opts map[string]string) (client.SolveRequest, error) {
	request := client.SolveRequest{Frontend: dockerfileFrontend, FrontendOpt: opts}
	caps := c.BuildOpts().Caps
	if caps.Supports(gwpb.CapFrontendInputs) == nil {
		inputs, err := c.Inputs(ctx)
		if err != nil {
			return client.SolveRequest{}, fmt.Errorf("get frontend inputs: %w", err)
		}
		request.FrontendInputs = make(map[string]*pb.Definition, len(inputs))
		for name, input := range inputs {
			definition, err := input.Marshal(ctx)
			if err != nil {
				return client.SolveRequest{}, fmt.Errorf("marshal frontend input %q: %w", name, err)
			}
			request.FrontendInputs[name] = definition.ToPB()
		}
	}
	return request, nil
}

// solveWithoutCache adds a build warning for cause and solves request.
func solveWithoutCache(ctx context.Context, c client.Client, request client.SolveRequest, cause error) (*client.Result, error) {
	if err := warn(ctx, c, fmt.Sprintf("Skipped cache mounts: %v", cause)); err != nil {
		return nil, err
	}
	return c.Solve(ctx, request)
}

func warn(ctx context.Context, c client.Client, message string) error {
	return c.Warn(ctx, "", message, client.WarnOpts{})
}
