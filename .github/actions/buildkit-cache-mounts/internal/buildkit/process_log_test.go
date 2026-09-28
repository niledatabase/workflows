package buildkit

import (
	"bytes"
	"log/slog"
	"testing"
)

func TestRelogProcessOutputKeepsRecordsAndLogsOtherLinesAsErrors(t *testing.T) {
	var processOutput bytes.Buffer
	processLog := slog.New(slog.NewJSONHandler(&processOutput, nil))
	processLog.WarnContext(t.Context(), "archive warning",
		slog.Int64("size_bytes", 5),
		slog.String("path", "a b"),
	)
	processOutput.WriteString("panic: boom\n")
	processOutput.WriteString(`{"msg":"no level"}` + "\n")
	processOutput.WriteString(`{"level":"INFO","msg":"truncated"` + "\n")
	processOutput.WriteString(`{"level":"INFO","msg":"trailing"} false` + "\n")

	var output bytes.Buffer
	log := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && attr.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attr
		},
	})).With(slog.String("id", "cache"))
	if err := relogProcessOutput(t.Context(), log, &processOutput); err != nil {
		t.Fatal(err)
	}

	want := `level=WARN msg="archive warning" id=cache size_bytes=5 path="a b"
level=ERROR msg="cache mount process output" id=cache line="panic: boom"
level=ERROR msg="cache mount process output" id=cache line="{\"msg\":\"no level\"}"
level=ERROR msg="cache mount process output" id=cache line="{\"level\":\"INFO\",\"msg\":\"truncated\""
level=ERROR msg="cache mount process output" id=cache line="{\"level\":\"INFO\",\"msg\":\"trailing\"} false"
`
	if output.String() != want {
		t.Fatalf("relogged output:\n%s\nwant:\n%s", output.String(), want)
	}
}
