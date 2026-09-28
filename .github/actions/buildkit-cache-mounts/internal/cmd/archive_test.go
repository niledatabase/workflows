package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSignedURL(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     string
		wantErr  bool
	}{
		{name: "trailing newline is removed", contents: "https://example.test/cache?sig=secret\n", want: "https://example.test/cache?sig=secret"},
		{name: "empty file is rejected", wantErr: true},
		{name: "embedded newline is rejected", contents: "https://example.test/\nother", wantErr: true},
		{name: "oversized file is rejected", contents: strings.Repeat("a", maximumSignedURLSize+1), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "url")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readSignedURL(path)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("readSignedURL returned %q, %v; want %q, error=%t", got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestArchiveLogsFailureAsJSONRecordWithoutBuilder(t *testing.T) {
	command := New(slog.New(slog.DiscardHandler))
	var stderr bytes.Buffer
	command.SetErr(&stderr)
	command.SetArgs([]string{"archive", "upload", "--directory", "/cache", "--url-file", filepath.Join(t.TempDir(), "missing")})
	err := command.ExecuteContext(t.Context())
	var record struct {
		Level string
		Msg   string
		Error string
	}
	if !errors.Is(err, ErrReported) || json.Unmarshal(stderr.Bytes(), &record) != nil ||
		record.Level != "ERROR" || record.Msg != "archive command failed" || !strings.HasPrefix(record.Error, "open signed URL file") {
		t.Fatalf("archive upload returned %v and logged %q, want ErrReported and a JSON error record for the signed URL file", err, stderr.String())
	}
}

func TestArchiveUploadSkipsEmptyDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		t.Errorf("unexpected upload request %s %s", request.Method, request.URL)
	}))
	defer server.Close()
	urlFile := filepath.Join(t.TempDir(), "url")
	if err := os.WriteFile(urlFile, []byte(server.URL+"/cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := New(slog.New(slog.DiscardHandler))
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetArgs([]string{"archive", "upload", "--directory", t.TempDir(), "--url-file", urlFile})
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "{\"compressed_size\":0}\n"; got != want {
		t.Fatalf("archive upload printed %q, want %q", got, want)
	}
}
