package cmd

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

const mountCacheKeyPrefix = "buildkit-cache-mount-"

// Key limits of @actions/cache 6.1.0 (https://github.com/actions/toolkit/blob/0786132e6a1a4451c2d392bbbf481c2b172d4312/packages/cache/src/cache.ts): 512 characters, and 10 keys including the key.
const (
	maxCacheKeyLength = 512
	maxRestoreKeys    = 9
)

func validateCacheKeyInputs(key string, restoreKeys []string) error {
	if key == "" {
		return fmt.Errorf("key is empty")
	}
	if len(restoreKeys) > maxRestoreKeys {
		return fmt.Errorf("received %d restore keys; maximum is %d", len(restoreKeys), maxRestoreKeys)
	}
	// The prefix of cacheMountKeys: mountCacheKeyPrefix, the hex SHA-256 Mount.IdentityHash, and a hyphen.
	prefixLength := len(mountCacheKeyPrefix) + sha256.Size*2 + 1
	for _, candidate := range append([]string{key}, restoreKeys...) {
		if prefixLength+len(candidate) > maxCacheKeyLength {
			return fmt.Errorf("cache key exceeds %d characters after adding the mount identity", maxCacheKeyLength)
		}
		if strings.Contains(candidate, ",") {
			return fmt.Errorf("cache key contains a comma")
		}
	}
	return nil
}

func cacheMountKeys(identityHash, key string, restoreKeys []string) (string, []string) {
	prefix := mountCacheKeyPrefix + identityHash + "-"
	mountRestoreKeys := make([]string, len(restoreKeys))
	for index, restoreKey := range restoreKeys {
		mountRestoreKeys[index] = prefix + restoreKey
	}
	return prefix + key, mountRestoreKeys
}
