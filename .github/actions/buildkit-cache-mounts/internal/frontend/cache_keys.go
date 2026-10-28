package frontend

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

type cacheKeys struct {
	key         string
	restoreKeys []string
}

// validate checks that the keys that mountKeys returns for k are valid GitHub Actions cache keys.
func (k cacheKeys) validate() error {
	if len(k.restoreKeys) > maxRestoreKeys {
		return fmt.Errorf("received %d restore keys; maximum is %d", len(k.restoreKeys), maxRestoreKeys)
	}
	// The prefix of mountKeys: mountCacheKeyPrefix, the hex SHA-256 cacheMount.IdentityHash, and a hyphen.
	prefixLength := len(mountCacheKeyPrefix) + sha256.Size*2 + 1
	for _, candidate := range append([]string{k.key}, k.restoreKeys...) {
		if prefixLength+len(candidate) > maxCacheKeyLength {
			return fmt.Errorf("cache key exceeds %d characters after adding the mount identity", maxCacheKeyLength)
		}
		if strings.Contains(candidate, ",") {
			return fmt.Errorf("cache key contains a comma")
		}
	}
	return nil
}

// mountKeys returns the keys of the cache mount with identityHash.
func (k cacheKeys) mountKeys(identityHash string) cacheKeys {
	prefix := mountCacheKeyPrefix + identityHash + "-"
	mountKeys := cacheKeys{key: prefix + k.key, restoreKeys: make([]string, len(k.restoreKeys))}
	for index, restoreKey := range k.restoreKeys {
		mountKeys.restoreKeys[index] = prefix + restoreKey
	}
	return mountKeys
}
