package archive

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

func TestDirectoryRoundTrip(t *testing.T) {
	source := t.TempDir()
	modTime := time.Unix(1_700_000_000, 0)
	if err := os.Mkdir(filepath.Join(source, "directory"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "directory"), 0o750|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(source, "directory", ".value")
	if err := os.WriteFile(filePath, []byte("cache value"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filePath, 0o640|os.ModeSetuid|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filePath, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	xattrSupported := true
	if err := unix.Setxattr(filePath, "user.cache", []byte("attribute"), 0); err != nil {
		if !errors.Is(err, unix.ENOTSUP) {
			t.Fatal(err)
		}
		xattrSupported = false
	}
	if xattrSupported {
		if err := unix.Setxattr(filepath.Join(source, "directory"), "user.directory", []byte("directory attribute"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filePath, filepath.Join(source, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".value", filepath.Join(source, "directory", "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(source, "fifo"), 0o620); err != nil {
		t.Fatal(err)
	}
	// Many small files and a file larger than chunkTargetSize make an archive with several chunks.
	large := make([]byte, 5*chunkTargetSize/2+123)
	if _, err := rand.Read(large); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"large": large, "empty": nil}
	for directory := range 4 {
		for file := range 600 {
			files[fmt.Sprintf("small/%d/%d", directory, file)] = bytes.Repeat([]byte{byte(file)}, 3000+file)
		}
	}
	for name, data := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(source, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var archive bytes.Buffer
	if err := WriteDirectory(t.Context(), &archive, source); err != nil {
		t.Fatal(err)
	}
	if chunks := splitChunks(t, archive.Bytes()); len(chunks) < 4 {
		t.Fatalf("archive has %d chunks, want at least 4", len(chunks))
	}
	sourceEntries := -1
	if err := filepath.WalkDir(source, func(string, fs.DirEntry, error) error {
		sourceEntries++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	decoder, err := zstd.NewReader(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	tarReader := tar.NewReader(decoder)
	tarEntries := 0
	for {
		if _, err := tarReader.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("archive is not a Zstandard-compressed tar stream: %v", err)
		}
		tarEntries++
	}
	if tarEntries != sourceEntries {
		t.Fatalf("tar stream has %d entries, want %d", tarEntries, sourceEntries)
	}

	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "stale"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ExtractDirectory(t.Context(), bytes.NewReader(archive.Bytes()), destination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "stale")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale file still exists: %v", err)
	}
	files["directory/.value"] = []byte("cache value")
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("restored %s differs from source", name)
		}
	}
	fileInfo, err := os.Stat(filepath.Join(destination, "directory", ".value"))
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o640 || fileInfo.Mode()&(os.ModeSetuid|os.ModeSetgid) != os.ModeSetuid|os.ModeSetgid {
		t.Fatalf("restored mode is %v", fileInfo.Mode())
	}
	if !fileInfo.ModTime().Equal(modTime) {
		t.Fatalf("restored modification time is %s, want %s", fileInfo.ModTime(), modTime)
	}
	sourceInfo, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	sourceStat, destinationStat := sourceInfo.Sys().(*syscall.Stat_t), fileInfo.Sys().(*syscall.Stat_t)
	if sourceStat.Uid != destinationStat.Uid || sourceStat.Gid != destinationStat.Gid {
		t.Fatalf("restored ownership is %d:%d, want %d:%d", destinationStat.Uid, destinationStat.Gid, sourceStat.Uid, sourceStat.Gid)
	}
	if xattrSupported {
		for _, xattr := range []struct{ name, key, value string }{
			{name: "directory/.value", key: "user.cache", value: "attribute"},
			{name: "directory", key: "user.directory", value: "directory attribute"},
		} {
			value := make([]byte, 64)
			n, err := unix.Getxattr(filepath.Join(destination, xattr.name), xattr.key, value)
			if err != nil {
				t.Fatal(err)
			}
			if string(value[:n]) != xattr.value {
				t.Fatalf("restored xattr %s of %s is %q, want %q", xattr.key, xattr.name, value[:n], xattr.value)
			}
		}
	}
	directoryInfo, err := os.Stat(filepath.Join(destination, "directory"))
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o750 || directoryInfo.Mode()&os.ModeSticky == 0 {
		t.Fatalf("restored directory mode is %v", directoryInfo.Mode())
	}
	hardlinkInfo, err := os.Stat(filepath.Join(destination, "hardlink"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fileInfo, hardlinkInfo) {
		t.Fatal("hardlink does not reference the restored file")
	}
	linkTarget, err := os.Readlink(filepath.Join(destination, "directory", "symlink"))
	if err != nil {
		t.Fatal(err)
	}
	if linkTarget != ".value" {
		t.Fatalf("symlink target is %q", linkTarget)
	}
	sourceFIFOInfo, err := os.Lstat(filepath.Join(source, "fifo"))
	if err != nil {
		t.Fatal(err)
	}
	fifoInfo, err := os.Lstat(filepath.Join(destination, "fifo"))
	if err != nil {
		t.Fatal(err)
	}
	if fifoInfo.Mode()&os.ModeNamedPipe == 0 || fifoInfo.Mode().Perm() != sourceFIFOInfo.Mode().Perm() {
		t.Fatalf("restored FIFO mode is %v, want %v", fifoInfo.Mode(), sourceFIFOInfo.Mode())
	}
}

func BenchmarkWriteDirectory(b *testing.B) {
	source, size := makeBenchmarkSource(b)
	b.SetBytes(size)
	for b.Loop() {
		if err := WriteDirectory(b.Context(), io.Discard, source); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExtractDirectory(b *testing.B) {
	source, size := makeBenchmarkSource(b)
	var archive bytes.Buffer
	if err := WriteDirectory(b.Context(), &archive, source); err != nil {
		b.Fatal(err)
	}
	destination := b.TempDir()
	b.SetBytes(size)
	for b.Loop() {
		if err := ExtractDirectory(b.Context(), bytes.NewReader(archive.Bytes()), destination); err != nil {
			b.Fatal(err)
		}
	}
}

// makeBenchmarkSource returns a directory with many small files and a few large files, like a package or build cache, and the size of the files.
func makeBenchmarkSource(b *testing.B) (string, int64) {
	b.Helper()
	source := b.TempDir()
	random := make([]byte, 32<<20)
	if _, err := rand.Read(random); err != nil {
		b.Fatal(err)
	}
	var size int64
	writeFile := func(name string, fileSize int) {
		// Half of the data repeats, so that the data compresses like source code or build output.
		data := slices.Concat(random[:fileSize/2], bytes.Repeat([]byte("cache"), fileSize/10))
		if err := os.WriteFile(filepath.Join(source, name), data, 0o644); err != nil {
			b.Fatal(err)
		}
		size += int64(len(data))
	}
	for directory := range 100 {
		if err := os.Mkdir(filepath.Join(source, fmt.Sprint(directory)), 0o755); err != nil {
			b.Fatal(err)
		}
		for file := range 100 {
			writeFile(fmt.Sprintf("%d/%d", directory, file), 512+(directory*100+file)*37%16384)
		}
	}
	for file := range 4 {
		writeFile(fmt.Sprintf("large-%d", file), 16<<20)
	}
	return source, size
}

type tarEntry struct {
	header tar.Header
	data   []byte
}

// makeArchive returns an archive with one chunk that contains a tar stream of entries.
func makeArchive(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	return makeChunk(t, makeTarStream(t, entries), chunkLast)
}

func makeTarStream(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, entry := range entries {
		header := entry.header
		header.Size = int64(len(entry.data))
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

// makeChunk returns a chunk with the tar stream part data.
func makeChunk(t *testing.T, data []byte, flags uint32) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	frame := encoder.EncodeAll(data, nil)
	header := chunkHeader{compressedSize: uint64(len(frame)), uncompressedSize: uint64(len(data)), flags: flags}
	return append(header.append(nil), frame...)
}

func requireEmptyDirectory(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory contains %d entries after failed extraction", len(entries))
	}
}

type rawChunk struct {
	header chunkHeader
	// data holds the chunk header and the Zstandard frame.
	data []byte
}

func splitChunks(t *testing.T, archive []byte) []rawChunk {
	t.Helper()
	var chunks []rawChunk
	for len(archive) > 0 {
		header, err := parseChunkHeader(archive)
		if err != nil {
			t.Fatal(err)
		}
		size := chunkHeaderSize + int(header.compressedSize)
		chunks = append(chunks, rawChunk{header: header, data: archive[:size]})
		archive = archive[size:]
	}
	return chunks
}
