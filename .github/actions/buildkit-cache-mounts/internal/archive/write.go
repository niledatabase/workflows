package archive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

const (
	blockSize = 512
	// estimatedHeaderSize is the size of a USTAR header with a PAX header for the modification time.
	estimatedHeaderSize = 3 * blockSize
)

// WriteDirectory writes an archive of the children of directory to output.
// It skips sockets and entries that disappear while it reads directory.
func WriteDirectory(ctx context.Context, output io.Writer, directory string) error {
	root, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open archive source: %w", err)
	}
	defer root.Close()
	concurrency := runtime.GOMAXPROCS(0)
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(concurrency))
	if err != nil {
		return fmt.Errorf("create Zstandard encoder: %w", err)
	}
	defer encoder.Close()

	ctx, cancel := context.WithCancel(ctx)
	w := &archiveWriter{
		ctx:          ctx,
		root:         int(root.Fd()),
		output:       output,
		encoder:      encoder,
		window:       2 * concurrency,
		links:        map[fileID]string{},
		tarBuffers:   make(chan *chunkBuffer, 2*concurrency),
		frameBuffers: make(chan *chunkBuffer, 2*concurrency),
	}
	defer func() {
		cancel()
		w.group.Wait()
	}()
	err = w.writeDirectory(root, "")
	if err == nil {
		err = w.finish()
	}
	if err != nil {
		return fmt.Errorf("write archive: %w", err)
	}
	return nil
}

// archiveWriter walks a directory, groups its entries into chunks, and writes the chunks in order while it builds up to window chunks at the same time.
type archiveWriter struct {
	ctx     context.Context
	root    int
	output  io.Writer
	encoder *zstd.Encoder
	window  int
	group   sync.WaitGroup
	// pending holds the chunks that are not written yet, in archive order.
	pending []*pendingChunk
	plan    chunkPlan
	// links maps the files with more than one link to the name of their first entry.
	links map[fileID]string
	// tarBuffers and frameBuffers hold unused buffers for the tar stream parts and the frames of chunks.
	tarBuffers, frameBuffers chan *chunkBuffer
}

type fileID struct {
	device, inode uint64
}

type pendingChunk struct {
	done chan error
	// frame holds the chunk header and the Zstandard frame.
	frame *chunkBuffer
}

type chunkPlan struct {
	headers []*tar.Header
	// size is the estimated size of the tar stream part.
	size int64
	last bool
}

func (w *archiveWriter) writeDirectory(directory *os.File, prefix string) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return err
	}
	slices.Sort(names)
	fd := int(directory.Fd())
	for _, name := range names {
		if err := w.writeEntry(fd, prefix, name); err != nil {
			return err
		}
	}
	return nil
}

func (w *archiveWriter) writeEntry(parent int, prefix, name string) error {
	entryPath := prefix + name
	var stat unix.Stat_t
	if err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return &os.PathError{Op: "fstatat", Path: entryPath, Err: err}
	}
	header := &tar.Header{
		Name:    entryPath,
		Mode:    int64(stat.Mode & 0o7777),
		Uid:     int(stat.Uid),
		Gid:     int(stat.Gid),
		ModTime: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec),
		// Without PAX, the tar writer rounds the modification time to seconds.
		Format: tar.FormatPAX,
	}
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		if err != nil {
			return &os.PathError{Op: "openat", Path: entryPath, Err: err}
		}
		directory := os.NewFile(uintptr(fd), entryPath)
		defer directory.Close()
		header.Typeflag = tar.TypeDir
		header.Name += "/"
		if header.PAXRecords, err = readFileXattrs(fd); err != nil {
			return &os.PathError{Op: "read extended attributes", Path: entryPath, Err: err}
		}
		if err := w.addEntry(header); err != nil {
			return err
		}
		return w.writeDirectory(directory, header.Name)
	case unix.S_IFREG:
		if !w.isHardlink(header, &stat) {
			header.Typeflag = tar.TypeReg
			header.Size = stat.Size
		}
		return w.addEntry(header)
	case unix.S_IFLNK:
		target, err := readlinkat(parent, name, stat.Size)
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		if err != nil {
			return &os.PathError{Op: "readlinkat", Path: entryPath, Err: err}
		}
		header.Typeflag = tar.TypeSymlink
		header.Linkname = target
	case unix.S_IFIFO:
		header.Typeflag = tar.TypeFifo
	case unix.S_IFCHR, unix.S_IFBLK:
		header.Typeflag = tar.TypeChar
		if stat.Mode&unix.S_IFMT == unix.S_IFBLK {
			header.Typeflag = tar.TypeBlock
		}
		header.Devmajor = int64(unix.Major(stat.Rdev))
		header.Devminor = int64(unix.Minor(stat.Rdev))
	default:
		// A socket works only while a process listens on it.
		return nil
	}
	records, err := readXattrs(
		func(names []byte) (int, error) { return listxattrat(parent, name, unix.AT_SYMLINK_NOFOLLOW, names) },
		func(key string, value []byte) (int, error) {
			return getxattrat(parent, name, unix.AT_SYMLINK_NOFOLLOW, key, value)
		},
	)
	if err != nil {
		return &os.PathError{Op: "read extended attributes", Path: entryPath, Err: err}
	}
	header.PAXRecords = records
	return w.addEntry(header)
}

// isHardlink returns whether an earlier entry has the regular file of stat, and makes header a hard link to the earlier entry.
func (w *archiveWriter) isHardlink(header *tar.Header, stat *unix.Stat_t) bool {
	if stat.Nlink <= 1 {
		return false
	}
	id := fileID{device: stat.Dev, inode: stat.Ino}
	target, ok := w.links[id]
	if !ok {
		w.links[id] = header.Name
		return false
	}
	header.Typeflag = tar.TypeLink
	header.Linkname = target
	return true
}

func (w *archiveWriter) addEntry(header *tar.Header) error {
	size := estimatedHeaderSize + header.Size + blockPadding(header.Size)
	if w.plan.size+size > chunkTargetSize && len(w.plan.headers) > 0 {
		if err := w.flush(false); err != nil {
			return err
		}
	}
	w.plan.headers = append(w.plan.headers, header)
	w.plan.size += size
	return nil
}

// flush starts to build the planned chunk and, if window chunks are pending, writes the first pending chunk.
func (w *archiveWriter) flush(last bool) error {
	plan := w.plan
	plan.last = last
	w.plan = chunkPlan{}
	chunk := &pendingChunk{done: make(chan error, 1)}
	w.group.Go(func() {
		var err error
		chunk.frame, err = w.buildChunk(plan)
		chunk.done <- err
	})
	w.pending = append(w.pending, chunk)
	if len(w.pending) < w.window {
		return nil
	}
	return w.writePending()
}

func (w *archiveWriter) writePending() error {
	chunk := w.pending[0]
	w.pending = w.pending[1:]
	if err := <-chunk.done; err != nil {
		return err
	}
	defer putBuffer(w.frameBuffers, chunk.frame)
	_, err := w.output.Write(chunk.frame.data)
	return err
}

func (w *archiveWriter) finish() error {
	if err := w.flush(true); err != nil {
		return err
	}
	for len(w.pending) > 0 {
		if err := w.writePending(); err != nil {
			return err
		}
	}
	return nil
}

// buildChunk returns the chunk header and the Zstandard frame of plan.
func (w *archiveWriter) buildChunk(plan chunkPlan) (*chunkBuffer, error) {
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	tarBuffer := getBuffer(w.tarBuffers, chunkTargetSize+chunkTargetSize/4)
	defer putBuffer(w.tarBuffers, tarBuffer)

	tarWriter := tar.NewWriter(tarBuffer)
	for _, header := range plan.headers {
		if err := w.writeTarEntry(tarWriter, tarBuffer, header); err != nil {
			return nil, err
		}
	}
	header := chunkHeader{}
	var err error
	if plan.last {
		header.flags = chunkLast
		err = tarWriter.Close()
	} else {
		err = tarWriter.Flush()
	}
	if err != nil {
		return nil, err
	}

	header.uncompressedSize = uint64(len(tarBuffer.data))
	frame := getBuffer(w.frameBuffers, chunkTargetSize/2)
	frame.data = w.encoder.EncodeAll(tarBuffer.data, append(frame.data, make([]byte, chunkHeaderSize)...))
	header.compressedSize = uint64(len(frame.data) - chunkHeaderSize)
	header.append(frame.data[:0])
	return frame, nil
}

// writeTarEntry writes the tar entry of header, with the data of a regular file, to tarWriter, which writes to buffer.
func (w *archiveWriter) writeTarEntry(tarWriter *tar.Writer, buffer *chunkBuffer, header *tar.Header) error {
	if header.Typeflag != tar.TypeReg {
		return tarWriter.WriteHeader(header)
	}
	fd, err := w.openFile(header.Name)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	fileHeader := *header
	if fileHeader.PAXRecords, err = readFileXattrs(fd); err != nil {
		return &os.PathError{Op: "read extended attributes", Path: header.Name, Err: err}
	}
	if err := tarWriter.WriteHeader(&fileHeader); err != nil {
		return err
	}
	return copyFileData(buffer, tarWriter, fd, header.Name, header.Size)
}

func (w *archiveWriter) openFile(path string) (int, error) {
	fd, err := unix.Openat2(w.root, path, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return 0, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return fd, nil
}

// copyFileData reads size bytes from fd into the free space of buffer and writes them to tarWriter, which writes to buffer.
func copyFileData(buffer *chunkBuffer, tarWriter *tar.Writer, fd int, path string, size int64) error {
	for offset := int64(0); offset < size; {
		region := buffer.spare(int(size - offset))
		n, err := unix.Pread(fd, region, offset)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return &os.PathError{Op: "pread", Path: path, Err: err}
		}
		if n == 0 {
			return fmt.Errorf("%s: file size changed while archiving", path)
		}
		if _, err := tarWriter.Write(region[:n]); err != nil {
			return err
		}
		offset += int64(n)
	}
	return nil
}

// blockPadding returns the size of the tar padding after data of size bytes.
func blockPadding(size int64) int64 {
	return -size & (blockSize - 1)
}

func readFileXattrs(fd int) (map[string]string, error) {
	return readXattrs(
		func(names []byte) (int, error) { return unix.Flistxattr(fd, names) },
		func(name string, value []byte) (int, error) { return unix.Fgetxattr(fd, name, value) },
	)
}

func readlinkat(dirfd int, name string, size int64) (string, error) {
	buffer := make([]byte, max(size, 255)+1)
	for {
		n, err := unix.Readlinkat(dirfd, name, buffer)
		if err != nil {
			return "", err
		}
		if n < len(buffer) {
			return string(buffer[:n]), nil
		}
		buffer = make([]byte, 2*len(buffer))
	}
}
