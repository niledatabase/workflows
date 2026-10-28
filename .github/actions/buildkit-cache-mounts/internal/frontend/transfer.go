package frontend

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// Operation is a transfer direction between a cache mount and the GitHub Actions cache.
type Operation string

const (
	Restore Operation = "restore"
	Save    Operation = "save"
)

// Transfer is the restore or save of one cache mount in a transfer step.
type Transfer struct {
	Operation Operation
	// Directory is the path of the cache mount in the transfer step.
	Directory string
	// Key is the GitHub Actions cache key of the cache mount.
	Key string
	// RestoreKeys are the key prefixes, in order of preference, for a restore when no exact key matches.
	// A save has no restore keys.
	RestoreKeys []string
	// SkipNonEmpty makes a restore skip the cache mount if the cache mount has files.
	SkipNonEmpty bool
	// RuntimeTokenFile is the path of the secret file with the GitHub Actions runtime token.
	RuntimeTokenFile string
	// ResultsURLFile is the path of the secret file with the GitHub Actions Results service URL.
	ResultsURLFile string
}

const (
	cacheMountPath     = "/cache"
	runtimeTokenSecret = "ACTIONS_RUNTIME_TOKEN"
	resultsURLSecret   = "ACTIONS_RESULTS_URL"
	runtimeTokenPath   = "/run/secrets/actions-runtime-token"
	resultsURLPath     = "/run/secrets/actions-results-url"
	nonceEnv           = "BUILDKIT_CACHE_MOUNTS_NONCE"
	maxTransfers       = 4
)

// cacheTransfer restores and saves cache mounts in transfer steps that run the command-line arguments of args in image.
type cacheTransfer struct {
	client   client.Client
	image    string
	platform ocispecs.Platform
	keys     cacheKeys
	// nonce is a random value that makes the transfer steps of a build differ from the steps of other builds.
	nonce string
	args  func(Transfer) []string
}

// run runs operation for each mount in a separate transfer step, and adds a build warning for each failed step.
// It returns an error only if a warning fails or ctx ends.
func (t cacheTransfer) run(ctx context.Context, operation Operation, mounts []cacheMount) error {
	failures := make([]error, len(mounts))
	slots := make(chan struct{}, maxTransfers)
	var transfers sync.WaitGroup
	for index, mount := range mounts {
		transfers.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			failures[index] = t.transfer(ctx, operation, mount)
		})
	}
	transfers.Wait()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	for index, failure := range failures {
		if failure == nil {
			continue
		}
		if err := warn(ctx, t.client, fmt.Sprintf("Failed to %s cache mount %s: %v", operation, mounts[index].id, failure)); err != nil {
			return err
		}
	}
	return nil
}

func (t cacheTransfer) transfer(ctx context.Context, operation Operation, mount cacheMount) error {
	keys := t.keys.mountKeys(mount.IdentityHash())
	request := Transfer{
		Operation:        operation,
		Directory:        cacheMountPath,
		Key:              keys.key,
		RuntimeTokenFile: runtimeTokenPath,
		ResultsURLFile:   resultsURLPath,
	}
	if operation == Restore {
		request.RestoreKeys = keys.restoreKeys
		// BuildKit creates a cache mount that has an input from the files of the input, so only the files of a cache mount without input show that an earlier build used it (getRefCacheDirNoCache in https://github.com/moby/buildkit/blob/v0.34.1/solver/llbsolver/mounts/mount.go).
		request.SkipNonEmpty = mount.inputDefinition == nil
	}
	step, err := transferStep(t.image, t.platform, t.nonce, mount, t.args(request), fmt.Sprintf("[buildkit-cache-mounts] %s %s", operation, mount.id))
	if err != nil {
		return err
	}
	definition, err := step.Marshal(ctx)
	if err != nil {
		return fmt.Errorf("marshal transfer step: %w", err)
	}
	_, err = t.client.Solve(ctx, client.SolveRequest{Definition: definition.ToPB(), Evaluate: true})
	return err
}

// transferStep returns a step that runs args in image on platform, with mount at cacheMountPath and the runtime token and Results service URL as secret files.
func transferStep(image string, platform ocispecs.Platform, nonce string, mount cacheMount, args []string, name string) (llb.State, error) {
	source := llb.Scratch()
	mountOptions := []llb.MountOption{llb.AsPersistentCacheDir(mount.id, cacheMountSharing(mount.sharing))}
	if mount.inputDefinition != nil {
		input, err := llb.NewDefinitionOp(mount.inputDefinition)
		if err != nil {
			return llb.State{}, fmt.Errorf("load cache mount input: %w", err)
		}
		source = llb.NewState(input.Output())
		mountOptions = append(mountOptions, llb.SourcePath(mount.selector))
	}
	// The nonce makes BuildKit run the step instead of using a cached result.
	// llb.IgnoreCache would also do that, but BuildKit removes the cache mounts of a step with IgnoreCache before the step runs (detectPrunedCacheID in https://github.com/moby/buildkit/blob/v0.34.1/solver/llbsolver/vertex.go).
	return llb.Image(image, llb.Platform(platform)).AddEnv(nonceEnv, nonce).Run(
		llb.Args(args),
		llb.AddMount(cacheMountPath, source, mountOptions...),
		llb.AddSecret(runtimeTokenPath, llb.SecretID(runtimeTokenSecret)),
		llb.AddSecret(resultsURLPath, llb.SecretID(resultsURLSecret)),
		llb.WithCustomName(name),
	).Root(), nil
}

func cacheMountSharing(sharing pb.CacheSharingOpt) llb.CacheMountSharingMode {
	switch sharing {
	case pb.CacheSharingOpt_SHARED:
		return llb.CacheMountShared
	case pb.CacheSharingOpt_PRIVATE:
		return llb.CacheMountPrivate
	case pb.CacheSharingOpt_LOCKED:
		return llb.CacheMountLocked
	}
	return llb.CacheMountShared
}

// workerPlatform returns the platform of the default worker of the builder, so that the transfer steps run without emulation.
func workerPlatform(c client.Client) (ocispecs.Platform, error) {
	workers := c.BuildOpts().Workers
	if len(workers) == 0 || len(workers[0].Platforms) == 0 {
		return ocispecs.Platform{}, errors.New("builder reports no worker platform")
	}
	return workers[0].Platforms[0], nil
}
