package frontend

import (
	"maps"
	"slices"
	"testing"
)

func TestSplitOpts(t *testing.T) {
	const image = "ghcr.io/example/frontend@sha256:digest"
	tests := []struct {
		name           string
		opts           map[string]string
		wantDockerfile map[string]string
	}{
		{
			name: "selected with BUILDKIT_SYNTAX",
			opts: map[string]string{
				syntaxOpt: image, cmdlineOpt: image, sourceOpt: image,
				keyOpt: " key ", restoreKeysOpt: " branch-, ,fallback- ",
				"build-arg:NAME": "value", "target": "final",
			},
			wantDockerfile: map[string]string{"build-arg:NAME": "value", "target": "final"},
		},
		{
			name: "selected with the syntax directive",
			opts: map[string]string{
				cmdlineOpt: image, sourceOpt: image,
				keyOpt: "key", restoreKeysOpt: "branch-,fallback-",
				"build-arg:NAME": "value",
			},
			wantDockerfile: map[string]string{cmdlineOpt: image, "build-arg:NAME": "value"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			keys, dockerfileOpts := splitOpts(test.opts)
			if keys.key != "key" || !slices.Equal(keys.restoreKeys, []string{"branch-", "fallback-"}) {
				t.Fatalf("cache keys are %+v, want key and restore keys branch-, fallback-", keys)
			}
			if !maps.Equal(dockerfileOpts, test.wantDockerfile) {
				t.Fatalf("Dockerfile frontend options are %v, want %v", dockerfileOpts, test.wantDockerfile)
			}
		})
	}
}
