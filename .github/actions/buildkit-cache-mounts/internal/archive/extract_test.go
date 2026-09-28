package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExtractDirectoryHandlesEntryOrderAndReplacement(t *testing.T) {
	modTime := time.Unix(1_600_000_000, 0)
	header := func(name string, typeflag byte, mode int64, linkname string) tar.Header {
		return tar.Header{Name: name, Typeflag: typeflag, Mode: mode, Linkname: linkname, Uid: os.Getuid(), Gid: os.Getgid(), ModTime: modTime}
	}
	archive := makeArchive(t, []tarEntry{
		{header: header("a/b/first", tar.TypeReg, 0o600, ""), data: []byte("first")},
		{header: header("c/other", tar.TypeReg, 0o600, ""), data: []byte("other")},
		{header: header("a/b/second", tar.TypeReg, 0o600, ""), data: []byte("second")},
		{header: header("a/", tar.TypeDir, 0o750, "")},
		{header: header("replaced/", tar.TypeDir, 0o700, "")},
		{header: header("replaced/child", tar.TypeReg, 0o600, ""), data: []byte("child")},
		{header: header("replaced", tar.TypeReg, 0o640, ""), data: []byte("file")},
		{header: header("c/other", tar.TypeSymlink, 0o777, "../a/b/first")},
		{header: header("c/hardlink-to-symlink", tar.TypeLink, 0o777, "c/other")},
	})
	destination := t.TempDir()
	if err := ExtractDirectory(t.Context(), bytes.NewReader(archive), destination); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{"a/b/first": "first", "a/b/second": "second", "replaced": "file"} {
		contents, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != want {
			t.Fatalf("%s contains %q, want %q", name, contents, want)
		}
	}
	for name, want := range map[string]fs.FileMode{"a": fs.ModeDir | 0o750, "a/b": fs.ModeDir | 0o755, "c": fs.ModeDir | 0o755, "replaced": 0o640} {
		info, err := os.Lstat(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != want {
			t.Fatalf("%s mode is %v, want %v", name, info.Mode(), want)
		}
	}
	for _, name := range []string{"a", "a/b/first", "c/other"} {
		info, err := os.Lstat(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(modTime) {
			t.Fatalf("%s modification time is %s, want %s", name, info.ModTime(), modTime)
		}
	}
	symlinkInfo, err := os.Lstat(filepath.Join(destination, "c/other"))
	if err != nil {
		t.Fatal(err)
	}
	hardlinkInfo, err := os.Lstat(filepath.Join(destination, "c/hardlink-to-symlink"))
	if err != nil {
		t.Fatal(err)
	}
	if symlinkInfo.Mode()&fs.ModeSymlink == 0 || !os.SameFile(symlinkInfo, hardlinkInfo) {
		t.Fatalf("hard link %v does not reference symlink %v", hardlinkInfo.Mode(), symlinkInfo.Mode())
	}
}

func TestExtractDirectoryRejectsUnsafeArchives(t *testing.T) {
	tests := map[string][]tarEntry{
		"parent traversal": {{header: tar.Header{Name: "../outside", Typeflag: tar.TypeReg, Mode: 0o600}}},
		"symlink parent": {
			{header: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "..", Mode: 0o777}},
			{header: tar.Header{Name: "link/outside", Typeflag: tar.TypeReg, Mode: 0o600}},
		},
		"nested symlink parent": {
			{header: tar.Header{Name: "a/", Typeflag: tar.TypeDir, Mode: 0o700}},
			{header: tar.Header{Name: "a/link", Typeflag: tar.TypeSymlink, Linkname: "../..", Mode: 0o777}},
			{header: tar.Header{Name: "a/link/outside", Typeflag: tar.TypeReg, Mode: 0o600}, data: []byte("changed")},
		},
		"hardlink traversal": {{header: tar.Header{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "../outside"}}},
	}
	for name, entries := range tests {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/existing=%t", name, existing), func(t *testing.T) {
				for index := range entries {
					entries[index].header.Uid = os.Getuid()
					entries[index].header.Gid = os.Getgid()
				}
				parent := t.TempDir()
				destination := filepath.Join(parent, "destination")
				if err := os.Mkdir(destination, 0o700); err != nil {
					t.Fatal(err)
				}
				outside := filepath.Join(parent, "outside")
				if existing {
					if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := ExtractDirectory(t.Context(), bytes.NewReader(makeArchive(t, entries)), destination); err == nil {
					t.Fatal("unsafe archive was accepted")
				}
				requireEmptyDirectory(t, destination)
				if !existing {
					if _, err := os.Lstat(outside); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("archive created a file outside the destination: %v", err)
					}
					return
				}
				contents, err := os.ReadFile(outside)
				if err != nil {
					t.Fatal(err)
				}
				if string(contents) != "unchanged" {
					t.Fatalf("outside file contains %q", contents)
				}
			})
		}
	}
}

func TestExtractDirectoryRejectsInvalidArchives(t *testing.T) {
	source := t.TempDir()
	for file := range 1000 {
		if err := os.WriteFile(filepath.Join(source, fmt.Sprintf("small-%d", file)), bytes.Repeat([]byte("x"), 4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var archive bytes.Buffer
	if err := WriteDirectory(t.Context(), &archive, source); err != nil {
		t.Fatal(err)
	}
	chunks := splitChunks(t, archive.Bytes())
	if len(chunks) < 2 {
		t.Fatalf("archive has %d chunks, want at least 2", len(chunks))
	}
	last := chunks[len(chunks)-1]
	tarStream := makeTarStream(t, []tarEntry{{header: tar.Header{Name: "value", Typeflag: tar.TypeReg, Mode: 0o600}, data: bytes.Repeat([]byte("x"), 1024)}})
	join := func(chunks ...rawChunk) []byte {
		var data []byte
		for _, chunk := range chunks {
			data = append(data, chunk.data...)
		}
		return data
	}
	corrupt := func(chunk rawChunk, offset int) rawChunk {
		chunk.data = bytes.Clone(chunk.data)
		chunk.data[offset] ^= 0xff
		return chunk
	}
	tests := map[string][]byte{
		"missing last chunk":     join(chunks[:len(chunks)-1]...),
		"data after last chunk":  append(bytes.Clone(archive.Bytes()), 0),
		"chunk ends in an entry": makeChunk(t, tarStream[:blockSize+100], chunkLast),
		"sparse file":            makeChunk(t, makeSparseTarStream(t), chunkLast),
		"invalid chunk header":   join(append([]rawChunk{corrupt(chunks[0], 0)}, chunks[1:]...)...),
		"corrupt frame checksum": join(append(chunks[:len(chunks)-1:len(chunks)-1], corrupt(last, len(last.data)-1))...),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			destination := t.TempDir()
			if err := ExtractDirectory(t.Context(), bytes.NewReader(data), destination); err == nil {
				t.Fatal("invalid archive was accepted")
			}
			requireEmptyDirectory(t, destination)
		})
	}
}

// makeSparseTarStream returns a tar stream with a GNU sparse file in PAX format 0.1, because tar.Writer cannot write sparse files.
func makeSparseTarStream(t *testing.T) []byte {
	t.Helper()
	stream := makeTarStream(t, []tarEntry{
		{header: tar.Header{Name: "PaxHeaders/value", Typeflag: tar.TypeReg}, data: []byte("26 GNU.sparse.numblocks=1\n22 GNU.sparse.map=0,2\n21 GNU.sparse.size=4\n")},
		{header: tar.Header{Name: "value", Typeflag: tar.TypeReg, Mode: 0o600}, data: []byte("ab")},
	})
	// Make the first entry a PAX extended header and update the checksum (POSIX.1-2024, pax, ustar Interchange Format).
	header := stream[:blockSize]
	header[156] = tar.TypeXHeader
	copy(header[148:156], "        ")
	checksum := 0
	for _, value := range header {
		checksum += int(value)
	}
	copy(header[148:156], fmt.Sprintf("%06o\x00 ", checksum))
	return stream
}

func TestExtractDirectoryHonorsCancellationDuringRead(t *testing.T) {
	archive := makeArchive(t, []tarEntry{{header: tar.Header{Name: "value", Typeflag: tar.TypeReg, Mode: 0o600}, data: bytes.Repeat([]byte("x"), 1024)}})
	ctx, cancel := context.WithCancel(t.Context())
	reader := &cancelingReader{reader: bytes.NewReader(archive), cancel: cancel}
	if err := ExtractDirectory(ctx, reader, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("ExtractDirectory returned %v, want context cancellation", err)
	}
}

type cancelingReader struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r *cancelingReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data[:min(len(data), 512)])
	r.cancel()
	return n, err
}
