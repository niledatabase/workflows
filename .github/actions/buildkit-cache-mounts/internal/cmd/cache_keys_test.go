package cmd

import (
	"slices"
	"strings"
	"testing"
)

func TestCacheKeysPreserveMountIsolationAndRestoreOrder(t *testing.T) {
	key, restoreKeys := cacheMountKeys("mount-a", "commit", []string{"branch-", "fallback-"})
	otherKey, otherRestoreKeys := cacheMountKeys("mount-b", "commit", []string{"branch-", "fallback-"})
	if key == otherKey || slices.Equal(restoreKeys, otherRestoreKeys) {
		t.Fatal("different mounts share cache keys")
	}
	prefix := strings.TrimSuffix(key, "commit")
	if !slices.Equal(restoreKeys, []string{prefix + "branch-", prefix + "fallback-"}) {
		t.Fatalf("restore keys are %v, want ordered mount-specific prefixes", restoreKeys)
	}
}

func TestValidateCacheKeyInputs(t *testing.T) {
	const validKey = "cache-key"
	const invalidKey = "key,with-comma"
	prefix, _ := cacheMountKeys(strings.Repeat("a", 64), "", nil)
	maximumKey := strings.Repeat("x", maxCacheKeyLength-len(prefix))
	tests := []struct {
		name        string
		key         string
		restoreKeys []string
		wantErr     bool
	}{
		{name: "empty key", wantErr: true},
		{name: "maximum key", key: maximumKey},
		{name: "key too long", key: maximumKey + "x", wantErr: true},
		{name: "restore key too long", key: validKey, restoreKeys: []string{maximumKey + "x"}, wantErr: true},
		{name: "comma in key", key: invalidKey, wantErr: true},
		{name: "comma in restore key", key: validKey, restoreKeys: []string{invalidKey}, wantErr: true},
		{name: "maximum restore keys", key: validKey, restoreKeys: slices.Repeat([]string{"prefix"}, maxRestoreKeys)},
		{name: "too many restore keys", key: validKey, restoreKeys: slices.Repeat([]string{"prefix"}, maxRestoreKeys+1), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateCacheKeyInputs(test.key, test.restoreKeys); (err != nil) != test.wantErr {
				t.Fatalf("validateCacheKeyInputs returned %v, want error=%t", err, test.wantErr)
			}
		})
	}
}
