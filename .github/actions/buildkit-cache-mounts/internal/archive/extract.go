package archive

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

const (
	impliedDirectoryMode = 0o755
	sparsePAXPrefix      = "GNU.sparse."
)

// ExtractDirectory replaces the children of directory with the contents of the archive in input and reads input to the end.
// An entry replaces an earlier entry with the same name in the same chunk, except that a directory entry keeps an existing directory.
// It rejects entries below symbolic links and GNU sparse files.
// If the extraction fails, directory is empty.
func ExtractDirectory(ctx context.Context, input io.Reader, directory string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := clearDirectory(directory); err != nil {
		return fmt.Errorf("clear archive destination: %w", err)
	}
	if err := extractArchive(ctx, &contextReader{ctx: ctx, Reader: input}, directory); err != nil {
		if clearErr := clearDirectory(directory); clearErr != nil {
			err = errors.Join(err, fmt.Errorf("clear archive destination: %w", clearErr))
		}
		return fmt.Errorf("extract archive: %w", err)
	}
	return nil
}

type chunkJob struct {
	index  int
	header chunkHeader
	frame  *chunkBuffer
}

func extractArchive(ctx context.Context, input io.Reader, directory string) error {
	rootDirectory, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer rootDirectory.Close()
	rootFile, err := rootDirectory.Open(".")
	if err != nil {
		return err
	}
	defer rootFile.Close()
	root := int(rootFile.Fd())
	workers := runtime.GOMAXPROCS(0)
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(workers), zstd.WithDecodeAllCapLimit(true))
	if err != nil {
		return fmt.Errorf("create Zstandard decoder: %w", err)
	}
	defer decoder.Close()

	group, groupCtx := errgroup.WithContext(ctx)
	jobs := make(chan chunkJob, workers)
	frameBuffers := make(chan *chunkBuffer, 2*workers)
	extractors := make([]*extractor, workers)
	for index := range extractors {
		e := &extractor{root: root, rootDirectory: rootDirectory}
		extractors[index] = e
		group.Go(func() error {
			for job := range jobs {
				err := e.extractChunk(groupCtx, decoder, job)
				putBuffer(frameBuffers, job.frame)
				if err != nil {
					return err
				}
			}
			return nil
		})
	}
	group.Go(func() error {
		defer close(jobs)
		return readChunks(groupCtx, input, jobs, frameBuffers)
	})
	if err := group.Wait(); err != nil {
		return err
	}
	return finishExtraction(root, rootDirectory, extractors)
}

// readChunks reads the chunks of the archive in input into buffers from frameBuffers and sends them to jobs.
func readChunks(ctx context.Context, input io.Reader, jobs chan<- chunkJob, frameBuffers chan *chunkBuffer) error {
	for index := 0; ; index++ {
		var headerData [chunkHeaderSize]byte
		if _, err := io.ReadFull(input, headerData[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("archive ends before the last chunk")
			}
			return err
		}
		header, err := parseChunkHeader(headerData[:])
		if err != nil {
			return fmt.Errorf("chunk %d: %w", index, err)
		}
		frame := getBuffer(frameBuffers, chunkTargetSize/2)
		frame.data = slices.Grow(frame.data, int(header.compressedSize))[:header.compressedSize]
		if _, err := io.ReadFull(input, frame.data); err != nil {
			return err
		}
		select {
		case jobs <- chunkJob{index: index, header: header, frame: frame}:
		case <-ctx.Done():
			return ctx.Err()
		}
		if header.flags&chunkLast != 0 {
			break
		}
	}
	if _, err := io.ReadFull(input, make([]byte, 1)); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("archive contains data after the last chunk")
		}
		return err
	}
	return nil
}

// entryKey is the position of an entry in the archive.
type entryKey struct {
	chunk, entry int
}

type deferredEntry struct {
	key    entryKey
	name   string
	header *tar.Header
}

// extractor creates the entries of chunks below a root directory.
// It opens parent directories with RESOLVE_NO_SYMLINKS, so it rejects entries below symbolic links.
// It defers hard links and directory times to finishExtraction, because hard link targets and directory children can be in other chunks.
type extractor struct {
	root int
	// rootDirectory is the root directory for the creation of missing parent directories and the removal of replaced entries.
	rootDirectory *os.Root
	// directories holds the directory entries in the order of the chunk.
	directories []deferredEntry
	hardlinks   []deferredEntry
	// impliedDirectories holds the names of parent directories that the extractor created with their missing parents.
	impliedDirectories []string
	decoded            []byte
}

func (e *extractor) extractChunk(ctx context.Context, decoder *zstd.Decoder, job chunkJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := decoder.DecodeAll(job.frame.data, slices.Grow(e.decoded[:0], int(job.header.uncompressedSize)))
	if err != nil {
		return fmt.Errorf("decompress chunk %d: %w", job.index, err)
	}
	e.decoded = data
	if len(data) != int(job.header.uncompressedSize) {
		return fmt.Errorf("chunk %d has %d bytes, want %d", job.index, len(data), job.header.uncompressedSize)
	}

	chunkReader := bytes.NewReader(data)
	reader := tar.NewReader(chunkReader)
	for index := 0; ; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("chunk %d: %w", job.index, err)
		}
		// The tar reader reads the data of a regular file only on Read, so the data starts at the offset of chunkReader (https://github.com/golang/go/blob/go1.27.3/src/archive/tar/reader.go).
		if err := e.extract(entryKey{chunk: job.index, entry: index}, header, data[len(data)-chunkReader.Len():]); err != nil {
			return err
		}
	}
	return nil
}

// extract creates the entry of header.
// For a regular file, data starts with the file data.
func (e *extractor) extract(key entryKey, header *tar.Header, data []byte) error {
	if header.Typeflag == tar.TypeXGlobalHeader {
		return nil
	}
	name := path.Clean(strings.TrimLeft(header.Name, "/"))
	if name == "." {
		return nil
	}
	if !filepath.IsLocal(name) {
		return fmt.Errorf("invalid entry name %q", header.Name)
	}
	if header.Typeflag == tar.TypeLink {
		e.hardlinks = append(e.hardlinks, deferredEntry{key: key, name: name, header: header})
		return nil
	}
	parent, base, err := e.openParent(name, true)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	switch header.Typeflag {
	case tar.TypeReg:
		return e.createFile(parent, base, name, header, data)
	case tar.TypeDir:
		if err := e.createDirectory(parent, base, name, header); err != nil {
			return err
		}
		e.directories = append(e.directories, deferredEntry{key: key, name: name, header: header})
		return nil
	case tar.TypeSymlink:
		if err := e.replaceExisting(name, func() error { return unix.Symlinkat(header.Linkname, parent, base) }); err != nil {
			return &os.PathError{Op: "symlinkat", Path: name, Err: err}
		}
		return setMetadata(parent, base, name, header, false, true)
	case tar.TypeBlock, tar.TypeChar, tar.TypeFifo:
		return e.createNode(parent, base, name, header)
	default:
		return fmt.Errorf("unhandled tar header type %d of %q", header.Typeflag, header.Name)
	}
}

// openParent opens the parent directory of the local path name and returns it and the last element of name.
// If createMissing is true, it creates missing parent directories.
// The caller must close the returned directory.
func (e *extractor) openParent(name string, createMissing bool) (int, string, error) {
	parentPath, base := path.Split(name)
	parentPath = path.Clean(parentPath)
	fd, err := e.openDirectory(parentPath)
	if errors.Is(err, unix.ENOENT) && createMissing {
		if err = e.rootDirectory.MkdirAll(parentPath, impliedDirectoryMode); err == nil {
			e.impliedDirectories = append(e.impliedDirectories, parentPath)
			fd, err = e.openDirectory(parentPath)
		}
	}
	if err != nil {
		return 0, "", &os.PathError{Op: "open parent directory", Path: name, Err: err}
	}
	return fd, base, nil
}

// openDirectory opens the directory name below the root with RESOLVE_NO_SYMLINKS.
func (e *extractor) openDirectory(name string) (int, error) {
	return unix.Openat2(e.root, name, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	})
}

func (e *extractor) createFile(parent int, base, name string, header *tar.Header, data []byte) error {
	// The data of a GNU sparse file in PAX format omits the holes.
	for record := range header.PAXRecords {
		if strings.HasPrefix(record, sparsePAXPrefix) {
			return fmt.Errorf("sparse file %q is not supported", name)
		}
	}
	if int64(len(data)) < header.Size {
		return fmt.Errorf("extract %q: %w", name, io.ErrUnexpectedEOF)
	}
	var fd int
	err := e.replaceExisting(name, func() (err error) {
		fd, err = unix.Openat(parent, base, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, entryMode(header)&0o777)
		return err
	})
	if err != nil {
		return &os.PathError{Op: "openat", Path: name, Err: err}
	}
	err = writeAll(fd, data[:header.Size])
	if err == nil {
		err = setFileMetadata(fd, header)
	}
	if closeErr := unix.Close(fd); err == nil && closeErr != nil {
		err = &os.PathError{Op: "close", Err: closeErr}
	}
	if err != nil {
		return fmt.Errorf("extract %q: %w", name, err)
	}
	return nil
}

func setFileMetadata(fd int, header *tar.Header) error {
	if err := unix.Fchown(fd, header.Uid, header.Gid); err != nil {
		return &os.PathError{Op: "fchown", Err: err}
	}
	if err := setXattrs(header, func(key string, value []byte) error { return unix.Fsetxattr(fd, key, value, 0) }); err != nil {
		return err
	}
	// Fchown clears the set-user-ID and set-group-ID bits, so Fchmod must run after it.
	if err := unix.Fchmod(fd, entryMode(header)); err != nil {
		return &os.PathError{Op: "fchmod", Err: err}
	}
	times := entryTimespecs(header)
	if err := unix.UtimesNanoAt(fd, "", times[:], unix.AT_EMPTY_PATH); err != nil {
		return &os.PathError{Op: "utimensat", Err: err}
	}
	return nil
}

func writeAll(fd int, data []byte) error {
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return &os.PathError{Op: "write", Err: err}
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// createDirectory creates the directory entry base in parent, or sets the metadata of an existing directory.
// The caller must set the times after the extraction of the children.
func (e *extractor) createDirectory(parent int, base, name string, header *tar.Header) error {
	err := unix.Mkdirat(parent, base, entryMode(header)&0o777)
	if errors.Is(err, unix.EEXIST) {
		var stat unix.Stat_t
		if err = unix.Fstatat(parent, base, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil && stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			if err = e.rootDirectory.RemoveAll(name); err == nil {
				err = unix.Mkdirat(parent, base, entryMode(header)&0o777)
			}
		}
	}
	if err != nil {
		return &os.PathError{Op: "mkdirat", Path: name, Err: err}
	}
	return setMetadata(parent, base, name, header, true, false)
}

func (e *extractor) createHardlink(name string, header *tar.Header) error {
	target := path.Clean(strings.TrimLeft(header.Linkname, "/"))
	if target == "." || !filepath.IsLocal(target) {
		return fmt.Errorf("invalid hard link target %q of %q", header.Linkname, header.Name)
	}
	parent, base, err := e.openParent(name, true)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	targetParent, targetBase, err := e.openParent(target, false)
	if err != nil {
		return err
	}
	defer unix.Close(targetParent)
	if err := e.replaceExisting(name, func() error { return unix.Linkat(targetParent, targetBase, parent, base, 0) }); err != nil {
		return &os.PathError{Op: "linkat", Path: name, Err: err}
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(parent, base, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &os.PathError{Op: "fstatat", Path: name, Err: err}
	}
	// A hard link to a symbolic link has no mode, and changing its times would change the times of the symbolic link target.
	isSymlink := stat.Mode&unix.S_IFMT == unix.S_IFLNK
	return setMetadata(parent, base, name, header, !isSymlink, !isSymlink)
}

func (e *extractor) createNode(parent int, base, name string, header *tar.Header) error {
	mode := entryMode(header)
	switch header.Typeflag {
	case tar.TypeBlock:
		mode |= unix.S_IFBLK
	case tar.TypeChar:
		mode |= unix.S_IFCHR
	case tar.TypeFifo:
		mode |= unix.S_IFIFO
	}
	if header.Devmajor < 0 || header.Devmajor > math.MaxUint32 || header.Devminor < 0 || header.Devminor > math.MaxUint32 {
		return fmt.Errorf("device number %d:%d of %q is out of range", header.Devmajor, header.Devminor, header.Name)
	}
	device := int(unix.Mkdev(uint32(header.Devmajor), uint32(header.Devminor)))
	if err := e.replaceExisting(name, func() error { return unix.Mknodat(parent, base, mode, device) }); err != nil {
		return &os.PathError{Op: "mknodat", Path: name, Err: err}
	}
	return setMetadata(parent, base, name, header, true, true)
}

// finishExtraction creates the hard links in archive order, sets the mode of implied directories, and sets the times of directory entries in archive order.
func finishExtraction(root int, rootDirectory *os.Root, extractors []*extractor) error {
	var hardlinks, directories []deferredEntry
	for _, e := range extractors {
		hardlinks = append(hardlinks, e.hardlinks...)
		directories = append(directories, e.directories...)
	}
	compareKeys := func(a, b deferredEntry) int {
		return cmp.Or(cmp.Compare(a.key.chunk, b.key.chunk), cmp.Compare(a.key.entry, b.key.entry))
	}
	slices.SortFunc(hardlinks, compareKeys)
	slices.SortFunc(directories, compareKeys)

	e := &extractor{root: root, rootDirectory: rootDirectory}
	for _, hardlink := range hardlinks {
		if err := e.createHardlink(hardlink.name, hardlink.header); err != nil {
			return err
		}
	}
	// All directories without a directory entry are implied, because ExtractDirectory clears the destination first.
	impliedDirectories := map[string]bool{}
	for _, extractor := range append(extractors, e) {
		for _, name := range extractor.impliedDirectories {
			for ; name != "."; name = path.Dir(name) {
				impliedDirectories[name] = true
			}
		}
	}
	for _, directory := range directories {
		delete(impliedDirectories, directory.name)
	}
	for name := range impliedDirectories {
		if err := e.setImpliedDirectoryMode(name); err != nil {
			return err
		}
	}
	for _, directory := range directories {
		parent, base, err := e.openParent(directory.name, false)
		if err != nil {
			return err
		}
		times := entryTimespecs(directory.header)
		err = unix.UtimesNanoAt(parent, base, times[:], unix.AT_SYMLINK_NOFOLLOW)
		unix.Close(parent)
		if err != nil {
			return &os.PathError{Op: "utimensat", Path: directory.name, Err: err}
		}
	}
	return nil
}

// setImpliedDirectoryMode sets the mode of the directory name to 0755, because MkdirAll in openParent applies the umask.
// It ignores a directory that a later entry replaced.
func (e *extractor) setImpliedDirectoryMode(name string) error {
	fd, err := e.openDirectory(name)
	if err == nil {
		err = unix.Fchmodat(fd, "", impliedDirectoryMode, unix.AT_EMPTY_PATH)
		unix.Close(fd)
	}
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return nil
	}
	if err != nil {
		return &os.PathError{Op: "set mode of implied directory", Path: name, Err: err}
	}
	return nil
}

// setMetadata sets the owner, extended attributes, and, if setMode and setTimes are true, the mode and times of the entry base in parent.
func setMetadata(parent int, base, name string, header *tar.Header, setMode, setTimes bool) error {
	if err := unix.Fchownat(parent, base, header.Uid, header.Gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &os.PathError{Op: "fchownat", Path: name, Err: err}
	}
	if err := setXattrs(header, func(key string, value []byte) error {
		return setxattrat(parent, base, unix.AT_SYMLINK_NOFOLLOW, key, value)
	}); err != nil {
		return fmt.Errorf("extract %q: %w", name, err)
	}
	// Fchownat clears the set-user-ID and set-group-ID bits, so Fchmodat must run after it.
	if setMode {
		if err := unix.Fchmodat(parent, base, entryMode(header), unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return &os.PathError{Op: "fchmodat2", Path: name, Err: err}
		}
	}
	if setTimes {
		times := entryTimespecs(header)
		if err := unix.UtimesNanoAt(parent, base, times[:], unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return &os.PathError{Op: "utimensat", Path: name, Err: err}
		}
	}
	return nil
}

// setXattrs sets the extended attributes in the PAX records of header with set.
// It ignores attributes that the file system does not support or does not permit.
func setXattrs(header *tar.Header, set func(key string, value []byte) error) error {
	for record, value := range header.PAXRecords {
		key, ok := strings.CutPrefix(record, xattrPAXPrefix)
		if !ok {
			continue
		}
		if err := set(key, []byte(value)); err != nil && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EPERM) {
			return &os.PathError{Op: "setxattr " + key, Err: err}
		}
	}
	return nil
}

// replaceExisting runs create and, if create fails with EEXIST, removes the entry name and runs create again.
func (e *extractor) replaceExisting(name string, create func() error) error {
	err := create()
	if !errors.Is(err, unix.EEXIST) {
		return err
	}
	if err := e.rootDirectory.RemoveAll(name); err != nil {
		return err
	}
	return create()
}

func entryMode(header *tar.Header) uint32 {
	return uint32(header.Mode) & 0o7777
}

// entryTimespecs returns the access and modification times of header for utimensat.
// It replaces times before the Unix epoch or after the end of int64 nanoseconds with the Unix epoch, and an access time before the modification time with the modification time.
func entryTimespecs(header *tar.Header) [2]unix.Timespec {
	accessTime := header.AccessTime
	if accessTime.Before(header.ModTime) {
		accessTime = header.ModTime
	}
	return [2]unix.Timespec{timespec(accessTime), timespec(header.ModTime)}
}

func timespec(t time.Time) unix.Timespec {
	if t.Before(time.Unix(0, 0)) || t.After(time.Unix(0, math.MaxInt64)) {
		t = time.Unix(0, 0)
	}
	return unix.NsecToTimespec(t.UnixNano())
}

func clearDirectory(directory string) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()

	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r *contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(data)
}
