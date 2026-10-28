package archive

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUploadRoundTrip(t *testing.T) {
	value := make([]byte, 17<<20)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "value"), value, 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	blocks := map[string][]byte{}
	var compressed bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch request.URL.Query().Get("comp") {
		case "":
			if _, err := io.Copy(&compressed, request.Body); err != nil {
				t.Error(err)
				http.Error(response, "read archive", http.StatusBadRequest)
				return
			}
		case "block":
			data, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				http.Error(response, "read block", http.StatusBadRequest)
				return
			}
			blocks[request.URL.Query().Get("blockid")] = data
		case "blocklist":
			var list struct {
				Latest []string `xml:"Latest"`
			}
			if err := xml.NewDecoder(request.Body).Decode(&list); err != nil {
				t.Error(err)
				http.Error(response, "read block list", http.StatusBadRequest)
				return
			}
			for _, id := range list.Latest {
				compressed.Write(blocks[id])
			}
		default:
			t.Errorf("unexpected upload request: %s", request.URL.Path)
			http.Error(response, "unexpected request", http.StatusBadRequest)
			return
		}
		response.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)

	size, err := Upload(t.Context(), source, server.URL+"/cache")
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if size != int64(compressed.Len()) || size == 0 {
		t.Fatalf("uploaded size is %d, received %d bytes", size, compressed.Len())
	}
	destination := t.TempDir()
	if err := ExtractDirectory(t.Context(), &compressed, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "value"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, value) {
		t.Fatal("restored contents differ from source")
	}
}

func TestTransferHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	url := "http://127.0.0.1:1/cache"
	if _, err := Upload(ctx, t.TempDir(), url); !errors.Is(err, context.Canceled) {
		t.Fatalf("Upload returned %v, want context cancellation", err)
	}
	if err := Download(ctx, t.TempDir(), url); !errors.Is(err, context.Canceled) {
		t.Fatalf("Download returned %v, want context cancellation", err)
	}
}

func TestUploadHonorsCancellationDuringRequest(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			return
		}
		close(started)
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "value"), []byte("cache value"), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := Upload(ctx, source, server.URL+"/cache")
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("upload did not send a request")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Upload returned %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not stop after cancellation")
	}
}

func TestTransferErrorsContainBlobStatusWithoutSignedURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("X-Ms-Error-Code", "AuthenticationFailed")
		http.Error(response, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	signedURL := server.URL + "/cache?sig=secret"
	wantCause := "status 403: AuthenticationFailed"
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "value"), []byte("cache value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Upload(ctx, source, signedURL); err == nil || !strings.Contains(err.Error(), wantCause) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Upload returned %v, want a Blob error with %q and without the signed URL", err, wantCause)
	}
	destination := t.TempDir()
	staleFile := filepath.Join(destination, "stale")
	if err := os.WriteFile(staleFile, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Download(ctx, destination, signedURL); err == nil || !strings.Contains(err.Error(), wantCause) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Download returned %v, want a Blob error with %q and without the signed URL", err, wantCause)
	}
	if data, err := os.ReadFile(staleFile); err != nil || string(data) != "unchanged" {
		t.Fatalf("failed download changed the destination: %q, %v", data, err)
	}
}

func TestUploadPreservesSourceError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	t.Run("missing directory", func(t *testing.T) {
		if _, err := Upload(ctx, filepath.Join(t.TempDir(), "missing"), server.URL+"/cache"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Upload returned %v, want the source error", err)
		}
	})
	t.Run("unreadable child", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can read files without permission bits")
		}
		source := t.TempDir()
		if err := os.WriteFile(filepath.Join(source, "unreadable"), []byte("cache value"), 0o000); err != nil {
			t.Fatal(err)
		}
		if _, err := Upload(ctx, source, server.URL+"/cache"); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("Upload returned %v, want the source error", err)
		}
	})
}

func TestDownloadResumesAfterInterruptedRead(t *testing.T) {
	source := t.TempDir()
	value := make([]byte, 1<<20)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "value"), value, 0o600); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	if err := WriteDirectory(t.Context(), &compressed, source); err != nil {
		t.Fatal(err)
	}
	data := compressed.Bytes()
	partialSize := len(data) / 2
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, httpRequest *http.Request) {
		response.Header().Set("ETag", `"archive-v1"`)
		request := requests.Add(1)
		if request > 1 && httpRequest.Header.Get("If-Match") != `"archive-v1"` {
			t.Errorf("request %d If-Match %q, want archive ETag", request, httpRequest.Header.Get("If-Match"))
			http.Error(response, "unexpected ETag", http.StatusBadRequest)
			return
		}
		switch request {
		case 1:
			if httpRequest.Method != http.MethodHead {
				t.Errorf("first request method %s, want HEAD", httpRequest.Method)
			}
			response.Header().Set("Content-Length", fmt.Sprint(len(data)))
		case 2:
			if got, want := httpRequest.Header.Get("X-Ms-Range"), fmt.Sprintf("bytes=0-%d", len(data)-1); got != want {
				t.Errorf("block range %q, want %q", got, want)
				http.Error(response, "unexpected range", http.StatusBadRequest)
				return
			}
			response.Header().Set("Content-Length", fmt.Sprint(len(data)))
			if _, err := response.Write(data[:partialSize]); err != nil {
				t.Errorf("write partial archive: %v", err)
			}
		case 3:
			if got, want := httpRequest.Header.Get("X-Ms-Range"), fmt.Sprintf("bytes=%d-%d", partialSize, len(data)-1); got != want {
				t.Errorf("retry range %q, want %q", got, want)
				http.Error(response, "unexpected range", http.StatusBadRequest)
				return
			}
			response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", partialSize, len(data)-1, len(data)))
			response.WriteHeader(http.StatusPartialContent)
			if _, err := response.Write(data[partialSize:]); err != nil {
				t.Errorf("write remaining archive: %v", err)
			}
		default:
			t.Errorf("unexpected request %d", requests.Load())
			http.Error(response, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	destination := t.TempDir()
	if err := Download(t.Context(), destination, server.URL+"/cache"); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
	contents, err := os.ReadFile(filepath.Join(destination, "value"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, value) {
		t.Fatal("restored contents differ from source")
	}
}

func TestDownloadReadsArchiveInBlocks(t *testing.T) {
	source := t.TempDir()
	value := make([]byte, 5*downloadBlockSize/2)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "value"), value, 0o600); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	if err := WriteDirectory(t.Context(), &compressed, source); err != nil {
		t.Fatal(err)
	}
	archive := compressed.Bytes()
	// ExtractDirectory rejects the archive while the download writes the first block.
	corruptArchive := bytes.Clone(archive)
	corruptArchive[0] ^= 0xff
	var ranges atomic.Int32
	failedRange := fmt.Sprintf("bytes=%d-%d", downloadBlockSize, 2*downloadBlockSize-1)
	for _, failure := range []string{"", "failed block", "corrupt archive"} {
		blob := archive
		if failure == "corrupt archive" {
			blob = corruptArchive
		}
		httpServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			if request.Header.Get("X-Ms-Range") != "" {
				ranges.Add(1)
			}
			if failure == "failed block" && request.Header.Get("X-Ms-Range") == failedRange {
				http.Error(response, "forbidden", http.StatusForbidden)
				return
			}
			serveBlob(t, response, request, blob)
		}))
		defer httpServer.Close()

		destination := t.TempDir()
		err := Download(t.Context(), destination, httpServer.URL+"/cache")
		if failure != "" {
			if err == nil {
				t.Fatalf("Download accepted a %s", failure)
			}
			requireEmptyDirectory(t, destination)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := ranges.Load(); got != 3 {
			t.Fatalf("Download sent %d range requests, want 3", got)
		}
		contents, err := os.ReadFile(filepath.Join(destination, "value"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(contents, value) {
			t.Fatal("restored contents differ from source")
		}
	}
}

func TestDownloadBlocksWritesBlocksInOrder(t *testing.T) {
	size := int64(downloadConcurrency*2*downloadBlockSize + downloadBlockSize/2)
	var mu sync.Mutex
	active, maximumActive := 0, 0
	var output bytes.Buffer
	err := downloadBlocks(t.Context(), &output, size, func(_ context.Context, offset int64, data []byte) error {
		mu.Lock()
		active++
		maximumActive = max(maximumActive, active)
		mu.Unlock()
		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
		}()
		// Later blocks complete first.
		time.Sleep(time.Duration(size-offset) / downloadBlockSize * time.Millisecond)
		for index := range data {
			data[index] = byte((offset + int64(index)) % 251)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	contents := output.Bytes()
	if int64(len(contents)) != size {
		t.Fatalf("wrote %d bytes, want %d", len(contents), size)
	}
	for index, value := range contents {
		if value != byte(index%251) {
			t.Fatalf("byte %d is %d, want %d", index, value, index%251)
		}
	}
	if maximumActive > downloadConcurrency {
		t.Fatalf("%d downloads ran at the same time, maximum is %d", maximumActive, downloadConcurrency)
	}
}

func TestDownloadBlocksReturnsDownloadError(t *testing.T) {
	downloadErr := errors.New("download failed")
	var output bytes.Buffer
	err := downloadBlocks(t.Context(), &output, 3*downloadBlockSize, func(_ context.Context, offset int64, _ []byte) error {
		if offset == downloadBlockSize {
			return downloadErr
		}
		return nil
	})
	if !errors.Is(err, downloadErr) {
		t.Fatalf("downloadBlocks returned %v, want download error", err)
	}
	if !bytes.Equal(output.Bytes(), make([]byte, downloadBlockSize)) {
		t.Fatalf("wrote %d bytes before the error, want the first block", output.Len())
	}
}

func TestDownloadBlocksStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{}, downloadConcurrency)
	go func() {
		<-started
		cancel()
	}()
	err := downloadBlocks(ctx, io.Discard, 100*downloadBlockSize, func(ctx context.Context, _ int64, _ []byte) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("downloadBlocks returned %v, want context cancellation", err)
	}
}

// serveBlob answers the Azure Blob requests of Download: HEAD for the blob properties, and GET with an x-ms-range header.
func serveBlob(t *testing.T, response http.ResponseWriter, request *http.Request, blob []byte) {
	t.Helper()
	response.Header().Set("ETag", `"archive"`)
	data, status := blob, http.StatusOK
	if value := request.Header.Get("X-Ms-Range"); value != "" {
		var start, last int
		if _, err := fmt.Sscanf(value, "bytes=%d-%d", &start, &last); err != nil || start > last || last >= len(blob) {
			http.Error(response, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		data, status = blob[start:last+1], http.StatusPartialContent
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, last, len(blob)))
	}
	response.Header().Set("Content-Length", fmt.Sprint(len(data)))
	response.WriteHeader(status)
	if request.Method == http.MethodGet {
		// Download closes the other connections when a block fails.
		if _, err := response.Write(data); err != nil {
			t.Logf("serve blob: %v", err)
		}
	}
}
