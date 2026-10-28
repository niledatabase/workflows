package frontend

import (
	"slices"
	"strings"
	"testing"
)

func TestCacheKeysPreserveMountIsolationAndRestoreOrder(t *testing.T) {
	keys := cacheKeys{key: "commit", restoreKeys: []string{"branch-", "fallback-"}}
	mountKeys := keys.mountKeys("mount-a")
	otherMountKeys := keys.mountKeys("mount-b")
	if mountKeys.key == otherMountKeys.key || slices.Equal(mountKeys.restoreKeys, otherMountKeys.restoreKeys) {
		t.Fatal("different mounts share cache keys")
	}
	prefix := strings.TrimSuffix(mountKeys.key, "commit")
	if !slices.Equal(mountKeys.restoreKeys, []string{prefix + "branch-", prefix + "fallback-"}) {
		t.Fatalf("restore keys are %v, want ordered mount-specific prefixes", mountKeys.restoreKeys)
	}
}

func TestCacheKeysValidate(t *testing.T) {
	const validKey = "cache-key"
	const invalidKey = "key,with-comma"
	prefix := cacheKeys{}.mountKeys(strings.Repeat("a", 64)).key
	maximumKey := strings.Repeat("x", maxCacheKeyLength-len(prefix))
	tests := []struct {
		name    string
		keys    cacheKeys
		wantErr bool
	}{
		{name: "maximum key", keys: cacheKeys{key: maximumKey}},
		{name: "key too long", keys: cacheKeys{key: maximumKey + "x"}, wantErr: true},
		{name: "restore key too long", keys: cacheKeys{key: validKey, restoreKeys: []string{maximumKey + "x"}}, wantErr: true},
		{name: "comma in key", keys: cacheKeys{key: invalidKey}, wantErr: true},
		{name: "comma in restore key", keys: cacheKeys{key: validKey, restoreKeys: []string{invalidKey}}, wantErr: true},
		{name: "maximum restore keys", keys: cacheKeys{key: validKey, restoreKeys: slices.Repeat([]string{"prefix"}, maxRestoreKeys)}},
		{name: "too many restore keys", keys: cacheKeys{key: validKey, restoreKeys: slices.Repeat([]string{"prefix"}, maxRestoreKeys+1)}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.keys.validate(); (err != nil) != test.wantErr {
				t.Fatalf("validate returned %v, want error=%t", err, test.wantErr)
			}
		})
	}
}
