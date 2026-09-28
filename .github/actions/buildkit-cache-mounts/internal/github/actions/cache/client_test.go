package cache

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "key"

func TestLookupArchiveRetriesServerErrors(t *testing.T) {
	testCases := []struct {
		name         string
		statusCodes  []int
		wantAttempts int
		wantErr      bool
	}{
		{name: "server errors then success", statusCodes: []int{http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusOK}, wantAttempts: 3},
		{name: "server error on every attempt", statusCodes: []int{http.StatusInternalServerError}, wantAttempts: cacheServiceMaxAttempts, wantErr: true},
		{name: "rate limit", statusCodes: []int{http.StatusTooManyRequests}, wantAttempts: 1, wantErr: true},
		{name: "client error", statusCodes: []int{http.StatusBadRequest}, wantAttempts: 1, wantErr: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var attempts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				attempt := int(attempts.Add(1))
				statusCode := testCase.statusCodes[min(attempt, len(testCase.statusCodes))-1]
				if statusCode != http.StatusOK {
					http.Error(response, http.StatusText(statusCode), statusCode)
					return
				}
				if _, err := response.Write([]byte(`{"ok":true,"signed_download_url":"https://blob.example/archive","matched_key":"key"}`)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()

			client, err := NewClient(server.URL, "token")
			if err != nil {
				t.Fatal(err)
			}
			client.retryInitialInterval = time.Millisecond
			_, downloadURL, err := client.LookupArchive(t.Context(), slog.New(slog.DiscardHandler), testKey, nil)
			if gotErr := err != nil; gotErr != testCase.wantErr {
				t.Fatalf("lookup returned error %v, want error %t", err, testCase.wantErr)
			}
			if !testCase.wantErr && downloadURL == "" {
				t.Fatal("lookup returned no cache hit")
			}
			if got := int(attempts.Load()); got != testCase.wantAttempts {
				t.Fatalf("lookup sent %d requests, want %d", got, testCase.wantAttempts)
			}
		})
	}
}

func TestReserveArchiveCreateResponses(t *testing.T) {
	testCases := []struct {
		name         string
		statusCode   int
		body         string
		wantReserved bool
		wantErr      bool
	}{
		{name: "reserved", statusCode: http.StatusOK, body: `{"ok":true,"signed_upload_url":"https://blob.example/upload"}`, wantReserved: true},
		{name: "conflict", statusCode: http.StatusConflict, body: "cache entry with the same key, version, and scope already exists"},
		{name: "not ok", statusCode: http.StatusOK, body: `{"ok":false,"message":"cache write denied: policy"}`, wantErr: true},
		{name: "missing upload URL", statusCode: http.StatusOK, body: `{"ok":true}`, wantErr: true},
		{name: "invalid response", statusCode: http.StatusOK, body: `not JSON`, wantErr: true},
		{name: "unauthorized", statusCode: http.StatusUnauthorized, body: `denied`, wantErr: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if path.Base(request.URL.Path) != "CreateCacheEntry" {
					t.Errorf("unexpected method %s", request.URL.Path)
				}
				var body cacheCreateRequest
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Key != testKey || body.Version != cacheVersion {
					t.Errorf("create request is %#v", body)
				}
				response.WriteHeader(testCase.statusCode)
				if _, err := response.Write([]byte(testCase.body)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()

			client, err := NewClient(server.URL, "token")
			if err != nil {
				t.Fatal(err)
			}
			uploadURL, err := client.ReserveArchive(t.Context(), slog.New(slog.DiscardHandler), testKey)
			if gotErr := err != nil; gotErr != testCase.wantErr {
				t.Fatalf("reserve returned error %v, want error %t", err, testCase.wantErr)
			}
			reserved := uploadURL != ""
			if reserved != testCase.wantReserved {
				t.Fatalf("reserve returned reserved %t, want %t", reserved, testCase.wantReserved)
			}
			if reserved && uploadURL != "https://blob.example/upload" {
				t.Fatalf("reserve returned upload URL %q", uploadURL)
			}
		})
	}
}

func TestLookupArchiveResponses(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantHit bool
		wantErr bool
	}{
		{name: "miss", body: `{"ok":false}`},
		{name: "hit", body: `{"ok":true,"matched_key":"prefix-old","signed_download_url":"https://blob.example/archive"}`, wantHit: true},
		{name: "missing key", body: `{"ok":true,"signed_download_url":"https://blob.example/archive"}`, wantErr: true},
		{name: "missing URL", body: `{"ok":true,"matched_key":"prefix-old"}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || path.Base(request.URL.Path) != "GetCacheEntryDownloadURL" || request.Header.Get("Authorization") != "Bearer token" {
					t.Errorf("unexpected lookup request: %s %s", request.Method, request.URL.Path)
				}
				var body cacheLookupRequest
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Key != testKey || body.Version != cacheVersion || !slices.Equal(body.RestoreKeys, []string{"prefix-", "fallback-"}) {
					t.Errorf("lookup request is %#v", body)
				}
				if _, err := response.Write([]byte(test.body)); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.URL, "token")
			if err != nil {
				t.Fatal(err)
			}
			key, downloadURL, err := client.LookupArchive(t.Context(), slog.New(slog.DiscardHandler), testKey, []string{"prefix-", "fallback-"})
			hit := downloadURL != ""
			if (err != nil) != test.wantErr || hit != test.wantHit {
				t.Fatalf("lookup returned hit=%t, error=%v; want hit=%t, error=%t", hit, err, test.wantHit, test.wantErr)
			}
			if hit && (key != "prefix-old" || downloadURL != "https://blob.example/archive") {
				t.Fatalf("lookup returned key %q and URL %q", key, downloadURL)
			}
		})
	}
}

func TestFinalizeArchiveResponses(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "finalized", status: http.StatusOK, body: `{"ok":true}`},
		{name: "rejected", status: http.StatusOK, body: `{"ok":false,"message":"upload missing"}`, wantErr: true},
		{name: "unauthorized", status: http.StatusUnauthorized, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if path.Base(request.URL.Path) != "FinalizeCacheEntryUpload" {
					t.Errorf("unexpected method %s", request.URL.Path)
				}
				var body struct {
					Key       string          `json:"key"`
					SizeBytes json.RawMessage `json:"size_bytes"`
					Version   string          `json:"version"`
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Key != testKey || body.Version != cacheVersion || string(body.SizeBytes) != `"4294967296"` {
					t.Errorf("finalize request is %#v", body)
				}
				response.WriteHeader(test.status)
				if _, err := response.Write([]byte(test.body)); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.URL, "token")
			if err != nil {
				t.Fatal(err)
			}
			if err := client.FinalizeArchive(t.Context(), slog.New(slog.DiscardHandler), testKey, 1<<32); (err != nil) != test.wantErr {
				t.Fatalf("finalize returned %v, want error=%t", err, test.wantErr)
			}
		})
	}
}

func TestLookupArchiveHonorsCallerDeadline(t *testing.T) {
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-done
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, _, err = client.LookupArchive(ctx, slog.New(slog.DiscardHandler), testKey, nil)
	close(done)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lookup returned %v, want caller deadline exceeded", err)
	}
}
