//go:build integration

package cache_test

import (
	"bytes"
	"crypto/rand"
	"io"
	"log/slog"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"

	actionscache "github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/github/actions/cache"
	"github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/github/actions/cache/cachetest"
)

func TestArchiveProtocolRoundTrip(t *testing.T) {
	service := cachetest.New(t, "")
	client, err := actionscache.NewClient(service.ResultsURL(), cachetest.RuntimeToken)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	log := slog.New(slog.DiscardHandler)

	data := make([]byte, 9<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	key := "prefix-mount-sha"
	uploadURL, err := client.ReserveArchive(ctx, log, key)
	if err != nil {
		t.Fatal(err)
	}
	if uploadURL == "" {
		t.Fatal("cache archive was not reserved")
	}
	if url, err := client.ReserveArchive(ctx, log, key); err != nil || url != "" {
		t.Fatalf("reserve pending archive returned %q, %v; want an empty URL and no error", url, err)
	}
	blobClient, err := blockblob.NewClientWithNoCredential(uploadURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blobClient.UploadStream(ctx, bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}
	if err := client.FinalizeArchive(ctx, log, key, int64(len(data))); err != nil {
		t.Fatal(err)
	}

	if url, err := client.ReserveArchive(ctx, log, key); err != nil || url != "" {
		t.Fatalf("reserve existing archive returned %q, %v; want an empty URL and no error", url, err)
	}

	matchedKey, downloadURL, err := client.LookupArchive(ctx, log, "missing", []string{"prefix-"})
	if err != nil {
		t.Fatal(err)
	}
	if downloadURL == "" || matchedKey != key {
		t.Fatalf("lookup returned key %q and URL %q", matchedKey, downloadURL)
	}
	downloadClient, err := blockblob.NewClientWithNoCredential(downloadURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	downloadResponse, err := downloadClient.DownloadStream(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer downloadResponse.Body.Close()
	restored, err := io.ReadAll(downloadResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, data) {
		t.Fatal("restored archive differs from uploaded archive")
	}
}
