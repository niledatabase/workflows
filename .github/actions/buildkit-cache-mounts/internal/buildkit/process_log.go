package buildkit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
)

// relogProcessOutput logs each line of output with log until output ends.
// A record of slog.JSONHandler keeps its level, message, and attributes, but not its time.
// Any other line is logged as an error.
func relogProcessOutput(ctx context.Context, log *slog.Logger, output io.Reader) error {
	scanner := bufio.NewScanner(output)
	for scanner.Scan() {
		level, message, attrs, err := decodeLogRecord(scanner.Bytes())
		if err != nil {
			log.ErrorContext(ctx, "cache mount process output", slog.String("line", scanner.Text()))
			continue
		}
		log.LogAttrs(ctx, level, message, attrs...) //nolint:sloglint // The message comes from the record of the cache mount process.
	}
	return scanner.Err()
}

func decodeLogRecord(line []byte) (slog.Level, string, []slog.Attr, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return 0, "", nil, errors.New("line is not a JSON object")
	}
	var level slog.Level
	var message string
	var attrs []slog.Attr
	hasLevel, hasMessage := false, false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return 0, "", nil, err
		}
		key, _ := token.(string)
		var value any
		if err := decoder.Decode(&value); err != nil {
			return 0, "", nil, err
		}
		switch key {
		case slog.TimeKey:
		case slog.LevelKey:
			text, _ := value.(string)
			if err := level.UnmarshalText([]byte(text)); err != nil {
				return 0, "", nil, err
			}
			hasLevel = true
		case slog.MessageKey:
			message, hasMessage = value.(string)
		default:
			attrs = append(attrs, slog.Any(key, value))
		}
	}
	if _, err := decoder.Token(); err != nil {
		return 0, "", nil, err
	}
	if len(bytes.TrimSpace(line[decoder.InputOffset():])) != 0 {
		return 0, "", nil, errors.New("data follows the JSON object")
	}
	if !hasLevel || !hasMessage {
		return 0, "", nil, errors.New("JSON object is not a log record")
	}
	return level, message, attrs, nil
}
