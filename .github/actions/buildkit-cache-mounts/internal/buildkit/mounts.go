package buildkit

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend/dockerfile/dockerfile2llb"
	"github.com/moby/buildkit/frontend/dockerui"
	"github.com/moby/buildkit/solver/pb"
	srctypes "github.com/moby/buildkit/source/types"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// Mount is a cache mount of a Dockerfile build.
// RUN instructions that use the same cache ID with the same input share one Mount.
type Mount struct {
	id              string
	sharing         pb.CacheSharingOpt
	inputDigest     string
	inputIndex      int64
	selector        string
	inputDefinition *pb.Definition
}

// ID returns the BuildKit cache ID of the mount.
// The Dockerfile frontend prefixes the ID from the Dockerfile with the BUILDKIT_CACHE_MOUNT_NS build argument and a slash (dispatchRunMounts in https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerfile/dockerfile2llb/convert_runmount.go).
func (m Mount) ID() string {
	return m.id
}

// IdentityHash returns a hex SHA-256 digest of the cache ID and the input of the mount.
// BuildKit keeps a separate cache for each cache ID and input (cacheRefGetter.getRefCacheDir in https://github.com/moby/buildkit/blob/v0.34.1/solver/llbsolver/mounts/mount.go).
func (m Mount) IdentityHash() string {
	identity := fmt.Sprintf("%s\x00%s\x00%d", m.id, m.inputDigest, m.inputIndex)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
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

func cacheMountsFromDockerfile(
	ctx context.Context,
	log *slog.Logger,
	imageResolver llb.ImageMetaResolver,
	dockerfilePath string,
	buildPlatform ocispecs.Platform,
	targetPlatforms []ocispecs.Platform,
	targetStage string,
	buildArgs map[string]string,
) ([]Mount, error) {
	dockerfileData, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return nil, fmt.Errorf("read Dockerfile: %w", err)
	}

	// The Dockerfile frontend converts the Dockerfile once per target platform (Client.Build in https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerui/build.go).
	// Without TargetPlatform, Dockerfile2LLB uses the build platform as implicit target platform, like a build without --platform (buildPlatformOpt in https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerfile/dockerfile2llb/platform.go).
	conversionPlatforms := []*ocispecs.Platform{nil}
	if len(targetPlatforms) > 0 {
		conversionPlatforms = make([]*ocispecs.Platform, len(targetPlatforms))
		for index := range targetPlatforms {
			conversionPlatforms[index] = &targetPlatforms[index]
		}
	}
	llbCaps := pb.Caps.CapSet(pb.Caps.All())
	mountsByIdentity := map[string]Mount{}
	privateIDs := map[string]bool{}
	localInputIDs := map[string]bool{}
	for _, targetPlatform := range conversionPlatforms {
		platformName := platforms.Format(buildPlatform)
		if targetPlatform != nil {
			platformName = platforms.Format(*targetPlatform)
		}
		convertedDockerfile, err := dockerfile2llb.Dockerfile2LLB(ctx, dockerfileData, dockerfile2llb.ConvertOpt{
			MetaResolver:   imageResolver,
			LLBCaps:        &llbCaps,
			TargetPlatform: targetPlatform,
			Config: dockerui.Config{
				// Dockerfile2LLB can change SOURCE_DATE_EPOCH in BuildArgs (setBuildArgValue in https://github.com/moby/buildkit/blob/v0.34.1/frontend/dockerfile/dockerfile2llb/convert.go).
				BuildArgs:        maps.Clone(buildArgs),
				BuildPlatforms:   []ocispecs.Platform{buildPlatform},
				CacheIDNamespace: buildArgs["BUILDKIT_CACHE_MOUNT_NS"],
				Target:           targetStage,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("convert Dockerfile %q to LLB for %s: %w", dockerfilePath, platformName, err)
		}
		dockerfileDefinition, err := convertedDockerfile.State.Marshal(ctx)
		if err != nil {
			return nil, fmt.Errorf("marshal Dockerfile LLB for %s: %w", platformName, err)
		}
		if err := addCacheMounts(mountsByIdentity, privateIDs, localInputIDs, dockerfileDefinition); err != nil {
			return nil, err
		}
	}
	for _, id := range slices.Sorted(maps.Keys(privateIDs)) {
		log.InfoContext(ctx, "skipped private cache mount", slog.String("id", id))
	}
	for _, id := range slices.Sorted(maps.Keys(localInputIDs)) {
		log.WarnContext(ctx, "skipped cache mount whose input reads local files", slog.String("id", id))
	}

	return slices.SortedFunc(maps.Values(mountsByIdentity), func(a, b Mount) int {
		if order := cmp.Compare(a.id, b.id); order != 0 {
			return order
		}
		return cmp.Compare(a.IdentityHash(), b.IdentityHash())
	}), nil
}

// addCacheMounts adds the cache mounts of dockerfileDefinition to mountsByIdentity, and the cache IDs of skipped mounts to privateIDs and localInputIDs.
func addCacheMounts(mountsByIdentity map[string]Mount, privateIDs, localInputIDs map[string]bool, dockerfileDefinition *llb.Definition) error {
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
			if operationMount.MountType != pb.MountType_CACHE {
				continue
			}
			cacheID := operationMount.CacheOpt.ID
			sharing := operationMount.CacheOpt.Sharing
			if sharing == pb.CacheSharingOpt_PRIVATE {
				privateIDs[cacheID] = true
				continue
			}
			discoveredMount := Mount{id: cacheID, sharing: sharing}
			if operationMount.Input >= 0 {
				input := operation.Inputs[operationMount.Input]
				// runCacheMountCommand solves the input in a session without the build's local sources.
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
