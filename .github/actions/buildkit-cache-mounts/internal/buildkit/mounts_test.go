package buildkit

import (
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/solver/pb"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestCacheMountIdentityHash(t *testing.T) {
	inputDigest := "sha256:input"
	base := Mount{id: "/cache-id", sharing: pb.CacheSharingOpt_SHARED, inputDigest: inputDigest, inputIndex: 1, selector: "/a"}
	if base.IdentityHash() != (Mount{id: base.id, sharing: pb.CacheSharingOpt_LOCKED, inputDigest: inputDigest, inputIndex: 1, selector: "/b"}).IdentityHash() {
		t.Fatal("sharing or selector changed cache identity")
	}
	for _, different := range []Mount{
		{id: "/other", sharing: base.sharing, inputDigest: base.inputDigest, inputIndex: base.inputIndex},
		{id: base.id, sharing: base.sharing, inputDigest: "sha256:other", inputIndex: 1},
		{id: base.id, sharing: base.sharing, inputDigest: inputDigest, inputIndex: 2},
	} {
		if base.IdentityHash() == different.IdentityHash() {
			t.Fatalf("mount %#v has the same cache identity", different)
		}
	}
}

func TestCacheMountsFromDockerfile(t *testing.T) {
	dockerfile := `
ARG CACHE_PREFIX=default
FROM scratch AS context-seed
COPY seed /seed
FROM scratch AS generated-seed
RUN true
FROM scratch AS build
ARG CACHE_PREFIX
ARG TARGETARCH
ENV CACHE_ROOT=/var/cache
RUN --mount=type=cache,id=${CACHE_PREFIX}-${TARGETARCH},target=${CACHE_ROOT},sharing=locked,mode=0770,uid=1000,gid=1001 true
RUN --mount=type=cache,id=${CACHE_PREFIX}-${TARGETARCH},target=/other,sharing=locked,mode=0771,uid=1000,gid=1001 true
RUN --mount=type=cache,target=/root/.cache true
RUN --mount=type=cache,id=private,target=/private,sharing=private true
RUN --mount=type=cache,id=from-context,target=/from-context,from=context-seed,source=/seed true
RUN --mount=type=cache,id=from-generated,target=/from-generated,from=generated-seed true
RUN --mount=type=cache,id=merged,target=/merged,sharing=locked true
RUN --mount=type=cache,id=merged,target=/merged true
FROM scratch AS unused
RUN --mount=type=cache,id=unused,target=/unused true
FROM build AS final
RUN --mount=type=cache,id=final,target=/final true
`
	file := filepath.Join(t.TempDir(), "Dockerfile")
	if err := os.WriteFile(file, []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, namespace := range []string{"", "team/cache"} {
		t.Run("namespace="+namespace, func(t *testing.T) {
			mounts, err := cacheMountsFromDockerfile(t.Context(), slog.New(slog.DiscardHandler), nil, file, ocispecs.Platform{OS: "linux", Architecture: "amd64"}, nil, "final", map[string]string{
				"CACHE_PREFIX":            "ci",
				"BUILDKIT_CACHE_MOUNT_NS": namespace,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(mounts) != 6 {
				t.Fatalf("discovered %d mounts, want 6", len(mounts))
			}

			byID := map[string]Mount{}
			dynamicCount := 0
			for _, mount := range mounts {
				id, ok := strings.CutPrefix(mount.id, namespace+"/")
				if !ok {
					t.Fatalf("mount ID %q is outside namespace %q", mount.id, namespace)
				}
				byID[id] = mount
				if id == "ci-amd64" {
					dynamicCount++
				}
			}
			if dynamicCount != 2 {
				t.Fatalf("discovered %d dynamic mounts, want 2", dynamicCount)
			}
			for id, sharing := range map[string]pb.CacheSharingOpt{
				"ci-amd64":     pb.CacheSharingOpt_LOCKED,
				"/root/.cache": pb.CacheSharingOpt_SHARED,
				"final":        pb.CacheSharingOpt_SHARED,
				"merged":       pb.CacheSharingOpt_LOCKED,
			} {
				mount, ok := byID[id]
				if !ok || mount.sharing != sharing {
					t.Fatalf("mount %q: %#v, want sharing %s", id, mount, sharing)
				}
			}
			for _, id := range []string{"unused", "from-context", "private"} {
				if _, ok := byID[id]; ok {
					t.Fatalf("discovered excluded mount %q", id)
				}
			}
			if generated := byID["from-generated"]; generated.inputDefinition == nil {
				t.Fatalf("mount with generated input: %#v", generated)
			}
		})
	}
}

func TestCacheMountsFromDockerfileConvertsEachTargetPlatform(t *testing.T) {
	dockerfile := `
FROM scratch
ARG TARGETARCH
RUN --mount=type=cache,id=arch-${TARGETARCH},target=/arch true
RUN --mount=type=cache,id=shared,target=/shared true
`
	file := filepath.Join(t.TempDir(), "Dockerfile")
	if err := os.WriteFile(file, []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	buildPlatform := platforms.MustParse("linux/amd64")
	tests := []struct {
		name            string
		targetPlatforms []ocispecs.Platform
		wantIDs         []string
	}{
		{name: "build platform without target platforms", wantIDs: []string{"/arch-amd64", "/shared"}},
		{
			name:            "each target platform",
			targetPlatforms: []ocispecs.Platform{platforms.MustParse("linux/arm64"), platforms.MustParse("linux/riscv64")},
			wantIDs:         []string{"/arch-arm64", "/arch-riscv64", "/shared"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Without a frontend client, Dockerfile2LLB deletes SOURCE_DATE_EPOCH=context from its build arguments.
			buildArgs := map[string]string{"SOURCE_DATE_EPOCH": "context"}
			mounts, err := cacheMountsFromDockerfile(t.Context(), slog.New(slog.DiscardHandler), nil, file, buildPlatform, test.targetPlatforms, "", buildArgs)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, len(mounts))
			for index, mount := range mounts {
				ids[index] = mount.ID()
			}
			if !slices.Equal(ids, test.wantIDs) {
				t.Fatalf("discovered mount IDs %v, want %v", ids, test.wantIDs)
			}
			if !maps.Equal(buildArgs, map[string]string{"SOURCE_DATE_EPOCH": "context"}) {
				t.Fatalf("build arguments changed to %v", buildArgs)
			}
		})
	}
}
