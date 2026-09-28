package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// LookupArchive returns the matched key and the signed download URL of the cache entry for key.
// If key has no entry, it uses the newest entry whose key starts with the first restore key that has a match.
// It returns empty strings and a nil error if no entry matches.
func (c *Client) LookupArchive(ctx context.Context, log *slog.Logger, key string, restoreKeys []string) (matchedKey, downloadURL string, err error) {
	request := cacheLookupRequest{Key: key, RestoreKeys: restoreKeys, Version: cacheVersion}
	var response cacheLookupResponse
	if err := c.callCacheService(ctx, log, "GetCacheEntryDownloadURL", request, &response); err != nil {
		return "", "", err
	}
	if !response.OK {
		return "", "", nil
	}
	if response.MatchedKey == "" || response.SignedDownloadURL == "" {
		return "", "", fmt.Errorf("cache lookup returned an incomplete response")
	}
	return response.MatchedKey, response.SignedDownloadURL, nil
}

// ReserveArchive creates the cache entry for key and returns the signed URL for the upload of its archive.
// It returns an empty URL and a nil error if the entry exists or another job reserved key.
func (c *Client) ReserveArchive(ctx context.Context, log *slog.Logger, key string) (string, error) {
	response := cacheCreateResponse{}
	if err := c.callCacheService(ctx, log, "CreateCacheEntry", cacheCreateRequest{Key: key, Version: cacheVersion}, &response); err != nil {
		// The cache service answers the Twirp error already_exists when an entry with the same key, version, and scope exists or is reserved (https://github.com/falcondev-oss/github-actions-cache-server/issues/276).
		// Twirp sends already_exists with status 409 (Twirp Wire Protocol v7, section Error Codes).
		if statusErr, ok := errors.AsType[*statusError](err); ok && statusErr.statusCode == http.StatusConflict {
			return "", nil
		}
		return "", err
	}
	if !response.OK {
		return "", fmt.Errorf("create cache entry: %s", response.Message)
	}
	if response.SignedUploadURL == "" {
		return "", fmt.Errorf("create cache entry returned an empty upload URL")
	}
	return response.SignedUploadURL, nil
}

// FinalizeArchive completes the cache entry for key after the upload of its archive of compressedSize bytes.
// LookupArchive finds the entry only after this call.
func (c *Client) FinalizeArchive(ctx context.Context, log *slog.Logger, key string, compressedSize int64) error {
	request := cacheFinalizeRequest{Key: key, SizeBytes: compressedSize, Version: cacheVersion}
	response := cacheFinalizeResponse{}
	if err := c.callCacheService(ctx, log, "FinalizeCacheEntryUpload", request, &response); err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("finalize cache entry: %s", response.Message)
	}
	return nil
}
