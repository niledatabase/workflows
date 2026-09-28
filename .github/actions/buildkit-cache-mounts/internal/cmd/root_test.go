package cmd

import (
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/containerd/platforms"
)

func TestRootFlagsBeforeSubcommandAreAccepted(t *testing.T) {
	command := New(slog.New(slog.DiscardHandler))
	command.SetOut(io.Discard)
	command.SetArgs([]string{"--builder", "example", "archive", "--help"})
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	builder, err := command.PersistentFlags().GetString("builder")
	if err != nil {
		t.Fatal(err)
	}
	if builder != "example" {
		t.Fatalf("builder is %q, want example", builder)
	}
}

func TestCacheCommandsRejectInvalidInputs(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		token      string
		resultsURL string
		want       string
	}{
		{name: "empty key", token: "token", resultsURL: "https://results.test", want: "key is empty"},
		{name: "invalid key", key: "key,invalid", token: "token", resultsURL: "https://results.test", want: "cache key contains a comma"},
		{name: "missing token", key: "key", resultsURL: "https://results.test", want: "ACTIONS_RUNTIME_TOKEN is empty"},
		{name: "invalid URL", key: "key", token: "token", resultsURL: "%", want: "parse GitHub Actions Results service URL"},
		{name: "relative URL", key: "key", token: "token", resultsURL: "/relative", want: "not an absolute URL"},
	}
	for _, operation := range []string{"restore", "save"} {
		for _, test := range tests {
			t.Run(operation+"/"+test.name, func(t *testing.T) {
				t.Setenv("ACTIONS_RUNTIME_TOKEN", test.token)
				command := New(slog.New(slog.DiscardHandler))
				command.SetArgs([]string{operation, "--builder", "unused", "--key", test.key, "--results-url", test.resultsURL})
				if err := command.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("command returned %v, want an error containing %q", err, test.want)
				}
			})
		}
	}
}

func TestParseBuildArgs(t *testing.T) {
	t.Setenv("CACHE_TEST_VALUE", "from environment")
	t.Setenv("CACHE_TEST_EMPTY", "")
	t.Setenv("CACHE_TEST_UNSET", "")
	if err := os.Unsetenv("CACHE_TEST_UNSET"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		input   string
		want    map[string]string
		wantErr bool
	}{
		{name: "empty"},
		{name: "blank lines", input: "\n  \n\n"},
		{name: "values and overrides", input: " NAME=first\nEMPTY=\nNAME=last\nEQUALS=a=b\n", want: map[string]string{"NAME": "last", "EMPTY": "", "EQUALS": "a=b"}},
		{name: "environment", input: "CACHE_TEST_VALUE\nCACHE_TEST_EMPTY\nCACHE_TEST_UNSET", want: map[string]string{"CACHE_TEST_VALUE": "from environment", "CACHE_TEST_EMPTY": ""}},
		{name: "explicit value overrides environment", input: "CACHE_TEST_VALUE=explicit", want: map[string]string{"CACHE_TEST_VALUE": "explicit"}},
		{name: "commas and multiline CSV", input: "LIST=a,b\n\"MULTI=line1\nline2\"\n", want: map[string]string{"LIST": "a,b", "MULTI": "line1\nline2"}},
		{name: "missing name", input: "=value", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseBuildArgs(test.input)
			if (err != nil) != test.wantErr || !maps.Equal(got, test.want) {
				t.Fatalf("parseBuildArgs returned %v, %v; want %v, error=%t", got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestParsePlatforms(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{name: "empty"},
		{name: "commas and newlines", input: " linux/amd64,linux/arm64\n\nlinux/arm/v7 \n", want: []string{"linux/amd64", "linux/arm64", "linux/arm/v7"}},
		{name: "normalized like the Dockerfile frontend", input: "linux/x86_64,linux/arm64/v8", want: []string{"linux/amd64", "linux/arm64"}},
		{name: "invalid platform", input: "linux/*", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parsePlatforms(test.input)
			var formatted []string
			for _, platform := range got {
				formatted = append(formatted, platforms.Format(platform))
			}
			if (err != nil) != test.wantErr || !slices.Equal(formatted, test.want) {
				t.Fatalf("parsePlatforms returned %v, %v; want %v, error=%t", formatted, err, test.want, test.wantErr)
			}
		})
	}
}
