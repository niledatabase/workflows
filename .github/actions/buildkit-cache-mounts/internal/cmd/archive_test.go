package cmd

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/spf13/cobra"

	"github.com/niledatabase/workflows/.github/actions/buildkit-cache-mounts/internal/frontend"
)

func TestReadSecretFile(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     string
		wantErr  bool
	}{
		{name: "trailing newline is removed", contents: "token\n", want: "token"},
		{name: "empty file is rejected", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readSecretFile(path)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("readSecretFile returned %q, %v; want %q, error=%t", got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestArchiveCommandParsesArchiveArgs(t *testing.T) {
	for _, want := range []frontend.Transfer{
		{
			Operation: frontend.Restore, Directory: "/cache", Key: "key", RestoreKeys: []string{"branch-", "fallback-"},
			SkipNonEmpty: true, RuntimeTokenFile: "/run/secrets/token", ResultsURLFile: "/run/secrets/url",
		},
		{Operation: frontend.Save, Directory: "/cache", Key: "key", RuntimeTokenFile: "/run/secrets/token", ResultsURLFile: "/run/secrets/url"},
	} {
		t.Run(string(want.Operation), func(t *testing.T) {
			var got frontend.Transfer
			root := &cobra.Command{Use: "root"}
			root.AddCommand(newArchiveCommand(func(_ context.Context, transfer frontend.Transfer) error {
				got = transfer
				return nil
			}))
			root.SetArgs(archiveArgs(want)[1:])
			if err := root.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("archive command parsed %+v, want %+v", got, want)
			}
		})
	}
}

func TestArchiveSkipsCacheRequestsForEmptySaveAndNonEmptyRestore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		t.Errorf("unexpected cache request %s %s", request.Method, request.URL)
	}))
	defer server.Close()
	secrets := t.TempDir()
	tokenFile := filepath.Join(secrets, "token")
	resultsURLFile := filepath.Join(secrets, "results-url")
	for path, contents := range map[string]string{tokenFile: "token", resultsURLFile: server.URL} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nonEmptyDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmptyDirectory, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		transfer frontend.Transfer
	}{
		{name: "save of empty mount", transfer: frontend.Transfer{Operation: frontend.Save, Directory: t.TempDir()}},
		{name: "restore of non-empty mount", transfer: frontend.Transfer{Operation: frontend.Restore, Directory: nonEmptyDirectory, SkipNonEmpty: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transfer := test.transfer
			transfer.Key = "key"
			transfer.RuntimeTokenFile = tokenFile
			transfer.ResultsURLFile = resultsURLFile
			command := New(slog.New(slog.DiscardHandler))
			command.SetArgs(archiveArgs(transfer)[1:])
			if err := command.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
