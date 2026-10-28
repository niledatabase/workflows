package frontend

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend/dockerfile/dockerfile2llb"
	"github.com/moby/buildkit/frontend/dockerui"
	"github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	srctypes "github.com/moby/buildkit/source/types"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// cacheMount is a cache mount of a Dockerfile build.
// RUN instructions that use the same cache ID with the same input share one cacheMount.
type cacheMount struct {
	// id is the BuildKit cache ID.
	// The Dockerfile frontend prefixes the ID from the Dockerfile with the BUILDKIT_CACHE_MOUNT_NS build argument and a slash (dispatchRunMounts in https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerfile/dockerfile2llb/convert_runmount.go).
	id              string
	sharing         pb.CacheSharingOpt
	inputDigest     string
	inputIndex      int64
	selector        string
	inputDefinition *pb.Definition
}

// IdentityHash returns a hex SHA-256 digest of the cache ID and the input of the mount.
// BuildKit keeps a separate cache for each cache ID and input (cacheRefGetter.getRefCacheDir in https://github.com/moby/buildkit/blob/v0.34.1/solver/llbsolver/mounts/mount.go).
func (m cacheMount) IdentityHash() string {
	identity := fmt.Sprintf("%s\x00%s\x00%d", m.id, m.inputDigest, m.inputIndex)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
}

// discoverCacheMounts reads the Dockerfile of the build with the Dockerfile frontend options opts and returns its cache mounts like cacheMountsFromDockerfile.
func discoverCacheMounts(ctx context.Context, c client.Client, opts map[string]string) ([]cacheMount, []string, error) {
	dockerfileClient, err := dockerui.NewClient(optsClient{Client: c, opts: opts})
	if err != nil {
		return nil, nil, err
	}
	dockerfile, err := dockerfileClient.ReadEntrypoint(ctx, "Dockerfile")
	if err != nil {
		return nil, nil, err
	}
	// The conversion options of the Dockerfile frontend, without Warn, because the build reports the warnings (Build in https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerfile/builder/build.go).
	return cacheMountsFromDockerfile(ctx, dockerfile.Data, dockerfile2llb.ConvertOpt{
		Config:       dockerfileClient.Config,
		Client:       dockerfileClient,
		SourceMap:    dockerfile.SourceMap,
		MetaResolver: c,
	})
}

// optsClient is a client with opts as frontend options.
type optsClient struct {
	client.Client
	opts map[string]string
}

func (c optsClient) BuildOpts() client.BuildOpts {
	buildOpts := c.Client.BuildOpts()
	buildOpts.Opts = c.opts
	return buildOpts
}

// cacheMountsFromDockerfile returns the cache mounts that a build of dockerfile with convertOpt uses, for each target platform of convertOpt or, without target platforms, for the default platform.
// It returns the mounts sorted by ID.
// It skips mounts with sharing=private and mounts whose input reads local files, and returns the sorted cache IDs of the mounts whose input reads local files.
func cacheMountsFromDockerfile(ctx context.Context, dockerfile []byte, convertOpt dockerfile2llb.ConvertOpt) ([]cacheMount, []string, error) {
	// The Dockerfile frontend converts the Dockerfile once per target platform (Client.Build in https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerui/build.go).
	// Without TargetPlatform, Dockerfile2LLB uses the build platform as implicit target platform, like a build without --platform (buildPlatformOpt in https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerfile/dockerfile2llb/platform.go).
	conversionPlatforms := []*ocispecs.Platform{nil}
	if len(convertOpt.TargetPlatforms) > 0 {
		conversionPlatforms = make([]*ocispecs.Platform, len(convertOpt.TargetPlatforms))
		for index := range convertOpt.TargetPlatforms {
			conversionPlatforms[index] = &convertOpt.TargetPlatforms[index]
		}
	}
	mountsByIdentity := map[string]cacheMount{}
	localInputIDs := map[string]bool{}
	for _, targetPlatform := range conversionPlatforms {
		platformName := "the build platform"
		if targetPlatform != nil {
			platformName = platforms.Format(*targetPlatform)
		}
		platformOpt := convertOpt
		// Dockerfile2LLB can change SOURCE_DATE_EPOCH in BuildArgs (setBuildArgValue in https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerfile/dockerfile2llb/convert.go).
		platformOpt.BuildArgs = maps.Clone(convertOpt.BuildArgs)
		platformOpt.TargetPlatform = targetPlatform
		convertedDockerfile, err := dockerfile2llb.Dockerfile2LLB(ctx, dockerfile, platformOpt)
		if err != nil {
			return nil, nil, fmt.Errorf("convert Dockerfile to LLB for %s: %w", platformName, err)
		}
		dockerfileDefinition, err := convertedDockerfile.State.Marshal(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal Dockerfile LLB for %s: %w", platformName, err)
		}
		if err := addCacheMounts(mountsByIdentity, localInputIDs, dockerfileDefinition); err != nil {
			return nil, nil, err
		}
	}

	mounts := slices.SortedFunc(maps.Values(mountsByIdentity), func(a, b cacheMount) int {
		if order := cmp.Compare(a.id, b.id); order != 0 {
			return order
		}
		return cmp.Compare(a.IdentityHash(), b.IdentityHash())
	})
	return mounts, slices.Sorted(maps.Keys(localInputIDs)), nil
}

func readsLocalSource(operationsByDigest map[digest.Digest]*pb.Op, root digest.Digest) bool {
	visited := map[digest.Digest]bool{}
	pending := []digest.Digest{root}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[current] {
			continue
		}
		visited[current] = true
		operation := operationsByDigest[current]
		if source := operation.GetSource(); source != nil && strings.HasPrefix(source.Identifier, srctypes.LocalScheme+"://") {
			return true
		}
		for _, input := range operation.GetInputs() {
			pending = append(pending, digest.Digest(input.Digest))
		}
	}
	return false
}

// addCacheMounts adds the cache mounts of dockerfileDefinition to mountsByIdentity, and the cache IDs of mounts whose input reads local files to localInputIDs.
func addCacheMounts(mountsByIdentity map[string]cacheMount, localInputIDs map[string]bool, dockerfileDefinition *llb.Definition) error {
	operations := make([]*pb.Op, len(dockerfileDefinition.Def))
	operationsByDigest := make(map[digest.Digest]*pb.Op, len(dockerfileDefinition.Def))
	for index, operationData := range dockerfileDefinition.Def {
		operation := new(pb.Op)
		if err := operation.UnmarshalVT(operationData); err != nil {
			return fmt.Errorf("decode LLB operation: %w", err)
		}
		operations[index] = operation
		operationsByDigest[digest.FromBytes(operationData)] = operation
	}

	for _, operation := range operations {
		execOperation := operation.GetExec()
		if execOperation == nil {
			continue
		}
		for _, operationMount := range execOperation.Mounts {
			if operationMount.MountType != pb.MountType_CACHE || operationMount.CacheOpt.Sharing == pb.CacheSharingOpt_PRIVATE {
				continue
			}
			cacheID := operationMount.CacheOpt.ID
			sharing := operationMount.CacheOpt.Sharing
			discoveredMount := cacheMount{id: cacheID, sharing: sharing}
			if operationMount.Input >= 0 {
				input := operation.Inputs[operationMount.Input]
				// The input digest does not change when the local files change, so a restore could replace the files of the input with the files of an earlier build.
				if readsLocalSource(operationsByDigest, digest.Digest(input.Digest)) {
					localInputIDs[cacheID] = true
					continue
				}
				inputDefinition := dockerfileDefinition.ToPB().CloneVT()
				terminalOperation := &pb.Op{Inputs: []*pb.Input{input.CloneVT()}}
				terminalData, err := terminalOperation.MarshalVT()
				if err != nil {
					return fmt.Errorf("inspect cache mount %q: marshal LLB input: %w", cacheID, err)
				}
				// The last operation of a marshaled LLB definition selects its result (State.Marshal in https://github.com/moby/buildkit/blob/v0.34.1/client/llb/state.go).
				inputDefinition.Def[len(inputDefinition.Def)-1] = terminalData
				discoveredMount.inputDigest = input.Digest
				discoveredMount.inputIndex = input.Index
				discoveredMount.selector = operationMount.Selector
				discoveredMount.inputDefinition = inputDefinition
			}
			identityHash := discoveredMount.IdentityHash()
			if existingMount, ok := mountsByIdentity[identityHash]; ok {
				// A RUN instruction with sharing=locked must not run while the transfer reads or writes the mount.
				if sharing == pb.CacheSharingOpt_LOCKED {
					existingMount.sharing = sharing
					mountsByIdentity[identityHash] = existingMount
				}
				continue
			}
			mountsByIdentity[identityHash] = discoveredMount
		}
	}
	return nil
}
