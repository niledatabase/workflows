package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
)

const (
	transferTimeout   = 30 * time.Minute
	uploadBlockSize   = 4 << 20 // 4 MiB
	uploadConcurrency = 16
	// Block size of downloadCacheHttpClientConcurrent in @actions/cache 6.1.0 (https://github.com/actions/toolkit/blob/0786132e6a1a4451c2d392bbbf481c2b172d4312/packages/cache/src/internal/downloadUtils.ts).
	downloadBlockSize = 4 << 20 // 4 MiB
	// Default downloadConcurrency of @actions/cache 6.1.0 (getDownloadOptions in https://github.com/actions/toolkit/blob/0786132e6a1a4451c2d392bbbf481c2b172d4312/packages/cache/src/options.ts).
	downloadConcurrency = 8
)

// UploadResult is the JSON output of the archive upload command.
type UploadResult struct {
	// CompressedSize is the size of the uploaded archive in bytes, or 0 if the directory is empty and the command uploaded nothing.
	CompressedSize int64 `json:"compressed_size"`
}

// Upload uploads an archive of directory to the signed Azure Blob URL uploadURL and returns the size of the archive in bytes.
// If directory is empty, it uploads nothing and returns 0.
// It fails if the transfer takes longer than 30 minutes.
// Errors do not contain uploadURL.
func Upload(ctx context.Context, directory, uploadURL string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	empty, err := isEmptyDirectory(directory)
	if err != nil || empty {
		return 0, err
	}
	blobClient, err := blockblob.NewClientWithNoCredential(uploadURL, nil)
	if err != nil {
		return 0, errors.New("create block blob client")
	}

	transferCtx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	compressedOutput := &countingWriter{writer: writer}
	archiveDone := make(chan error, 1)
	go func() {
		archiveErr := WriteDirectory(transferCtx, compressedOutput, directory)
		writer.CloseWithError(archiveErr)
		archiveDone <- archiveErr
	}()
	_, uploadErr := blobClient.UploadStream(transferCtx, reader, &blockblob.UploadStreamOptions{
		BlockSize:   uploadBlockSize,
		Concurrency: uploadConcurrency,
	})
	if uploadErr != nil {
		uploadErr = transferError(transferCtx, "upload cache archive", uploadErr)
		cancel()
	}
	reader.CloseWithError(uploadErr)
	if err := errors.Join(<-archiveDone, uploadErr); err != nil {
		return 0, err
	}
	return compressedOutput.size, nil
}

// Download replaces the children of directory with the contents of the archive at the signed Azure Blob URL downloadURL.
// If the extraction fails, directory is empty.
// It fails if the transfer takes longer than 30 minutes.
// Errors do not contain downloadURL.
func Download(ctx context.Context, directory, downloadURL string) error {
	transferCtx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	blobClient, err := blockblob.NewClientWithNoCredential(downloadURL, nil)
	if err != nil {
		return errors.New("create block blob client")
	}
	properties, err := blobClient.GetProperties(transferCtx, nil)
	if err != nil {
		return transferError(transferCtx, "read cache archive properties", err)
	}
	if properties.ContentLength == nil {
		return errors.New("cache archive properties have no content length")
	}
	// If-Match makes all blocks come from the same blob version.
	accessConditions := &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: properties.ETag}}
	downloadBlock := func(ctx context.Context, offset int64, data []byte) error {
		response, err := blobClient.DownloadStream(ctx, &blob.DownloadStreamOptions{
			Range:            blob.HTTPRange{Offset: offset, Count: int64(len(data))},
			AccessConditions: accessConditions,
		})
		if err != nil {
			return transferError(ctx, "download cache archive", err)
		}
		blockBody := response.NewRetryReader(ctx, nil)
		defer blockBody.Close()
		if _, err := io.ReadFull(blockBody, data); err != nil {
			return transferError(ctx, "download cache archive", err)
		}
		return nil
	}

	reader, writer := io.Pipe()
	var download sync.WaitGroup
	download.Go(func() {
		writer.CloseWithError(downloadBlocks(transferCtx, writer, *properties.ContentLength, downloadBlock))
	})
	defer func() {
		reader.Close()
		cancel()
		download.Wait()
	}()
	if err := ExtractDirectory(transferCtx, reader, directory); err != nil {
		return fmt.Errorf("extract cache archive: %w", err)
	}
	return nil
}

type blockDownload struct {
	data []byte
	err  chan error
}

// downloadBlocks writes size bytes to w in order and runs up to downloadConcurrency downloadBlock calls at the same time.
// downloadBlock fills data with the bytes at offset.
func downloadBlocks(ctx context.Context, w io.Writer, size int64, downloadBlock func(ctx context.Context, offset int64, data []byte) error) error {
	ctx, cancel := context.WithCancel(ctx)
	var group sync.WaitGroup
	defer group.Wait()
	defer cancel()

	var pending []blockDownload
	for next := int64(0); next < size || len(pending) > 0; {
		for next < size && len(pending) < downloadConcurrency {
			offset := next
			block := blockDownload{data: make([]byte, min(downloadBlockSize, size-offset)), err: make(chan error, 1)}
			group.Go(func() { block.err <- downloadBlock(ctx, offset, block.data) })
			pending = append(pending, block)
			next += int64(len(block.data))
		}
		block := pending[0]
		pending = pending[1:]
		if err := <-block.err; err != nil {
			return err
		}
		if _, err := w.Write(block.data); err != nil {
			return err
		}
	}
	return nil
}

// transferError returns an error with the cause of err, but without the messages of Azure SDK and URL errors, because these messages can contain the signed URL.
func transferError(ctx context.Context, operation string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", operation, ctxErr)
	}
	if responseErr, ok := errors.AsType[*azcore.ResponseError](err); ok {
		if responseErr.ErrorCode == "" {
			return fmt.Errorf("%s: status %d", operation, responseErr.StatusCode)
		}
		return fmt.Errorf("%s: status %d: %s", operation, responseErr.StatusCode, responseErr.ErrorCode)
	}
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return fmt.Errorf("%s: %w", operation, urlErr.Err)
	}
	return errors.New(operation)
}

func isEmptyDirectory(directory string) (bool, error) {
	file, err := os.Open(directory)
	if err != nil {
		return false, fmt.Errorf("open archive source: %w", err)
	}
	defer file.Close()
	if _, err := file.Readdirnames(1); err != nil {
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		return false, fmt.Errorf("read archive source: %w", err)
	}
	return false, nil
}

type countingWriter struct {
	writer io.Writer
	size   int64
}

func (w *countingWriter) Write(data []byte) (int, error) {
	written, err := w.writer.Write(data)
	w.size += int64(written)
	return written, err
}
