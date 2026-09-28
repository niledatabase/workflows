package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"
)

const (
	cacheServiceName   = "github.actions.results.api.v1.CacheService"
	httpRequestTimeout = time.Minute
	// Retry limits of the @actions/cache 6.1.0 Twirp client (https://github.com/actions/toolkit/blob/0786132e6a1a4451c2d392bbbf481c2b172d4312/packages/cache/src/internal/shared/cacheTwirpClient.ts).
	cacheServiceMaxAttempts   = 5
	cacheServiceRetryInterval = 3 * time.Second
)

// The cache service returns only entries with the same version, so a change of the archive format must change this value (getCacheVersion in https://github.com/actions/toolkit/blob/0786132e6a1a4451c2d392bbbf481c2b172d4312/packages/cache/src/internal/cacheUtils.ts).
var cacheVersion = fmt.Sprintf("%x", sha256.Sum256([]byte("buildkit-cache-mounts|tar|zstd|1")))

// Client calls the cache service of the GitHub Actions Results service.
// Its methods retry requests that fail with a server error and log a warning before each retry.
type Client struct {
	baseURL              *url.URL
	token                string
	retryInitialInterval time.Duration
}

type cacheLookupRequest struct {
	Key         string   `json:"key"`
	RestoreKeys []string `json:"restore_keys"`
	Version     string   `json:"version"`
}

type cacheLookupResponse struct {
	OK                bool   `json:"ok"`
	SignedDownloadURL string `json:"signed_download_url"`
	MatchedKey        string `json:"matched_key"`
}

type cacheCreateRequest struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

type cacheCreateResponse struct {
	OK              bool   `json:"ok"`
	SignedUploadURL string `json:"signed_upload_url"`
	Message         string `json:"message"`
}

type cacheFinalizeRequest struct {
	Key       string `json:"key"`
	SizeBytes int64  `json:"size_bytes,string"`
	Version   string `json:"version"`
}

type cacheFinalizeResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// NewClient returns a client for the Results service at resultsURLValue that authenticates with the runtime token.
func NewClient(resultsURLValue, token string) (*Client, error) {
	baseURL, err := url.Parse(resultsURLValue)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub Actions Results service URL: %w", err)
	}
	if baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("GitHub Actions Results service URL is not an absolute URL")
	}
	return &Client{baseURL: baseURL, token: token, retryInitialInterval: cacheServiceRetryInterval}, nil
}

func (c *Client) callCacheService(ctx context.Context, log *slog.Logger, method string, requestBody, responseBody any) error {
	requestJSON, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}
	retrySchedule := backoff.NewExponentialBackOff()
	retrySchedule.InitialInterval = c.retryInitialInterval
	_, err = backoff.Retry(ctx, func() (struct{}, error) {
		err := c.sendCacheServiceRequest(ctx, method, requestJSON, responseBody)
		if statusErr, ok := errors.AsType[*statusError](err); ok && statusErr.retryable() {
			return struct{}{}, err
		}
		return struct{}{}, backoff.Permanent(err)
	},
		backoff.WithBackOff(retrySchedule),
		backoff.WithMaxTries(cacheServiceMaxAttempts),
		backoff.WithNotify(func(err error, delay time.Duration) {
			log.WarnContext(ctx, "retrying cache service request",
				slog.String("method", method),
				slog.Duration("delay", delay.Round(time.Millisecond)),
				slog.Any("error", err),
			)
		}),
	)
	retryErr := backoff.AsRetryError(err)
	switch {
	case retryErr == nil:
		return err
	case errors.Is(retryErr.Cause, backoff.ErrPermanent):
		return retryErr.LastErr
	case errors.Is(retryErr.Cause, backoff.ErrExhausted):
		return fmt.Errorf("%d attempts failed: %w", cacheServiceMaxAttempts, retryErr.LastErr)
	}
	return err
}

func (c *Client) sendCacheServiceRequest(ctx context.Context, method string, requestJSON []byte, responseBody any) error {
	endpointURL := c.baseURL.JoinPath("twirp", cacheServiceName, method)
	requestCtx, cancel := context.WithTimeout(ctx, httpRequestTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpointURL.String(), bytes.NewReader(requestJSON))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.token)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("User-Agent", "buildkit-cache-mounts")
	httpResponse, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("call %s: %w", method, err)
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return fmt.Errorf("call %s: %w", method, &statusError{statusCode: httpResponse.StatusCode, err: httpResponseError(httpResponse)})
	}
	if err := json.NewDecoder(httpResponse.Body).Decode(responseBody); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	return nil
}

type statusError struct {
	statusCode int
	err        error
}

func (e *statusError) Error() string {
	return e.err.Error()
}

func (e *statusError) Unwrap() error {
	return e.err
}

// retryable reports the status codes that the @actions/cache 6.1.0 Twirp client retries (isRetryableHttpStatusCode in https://github.com/actions/toolkit/blob/0786132e6a1a4451c2d392bbbf481c2b172d4312/packages/cache/src/internal/shared/cacheTwirpClient.ts).
func (e *statusError) retryable() bool {
	switch e.statusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func httpResponseError(response *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read %s response: %w", response.Status, err)
	}
	return fmt.Errorf("%s: %s", response.Status, strings.TrimSpace(string(body)))
}
