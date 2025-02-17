package uploader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/lxe/r2cli/internal/progress"
)

const (
	minPartSize = 8 * 1024 * 1024 // 8MB minimum part size
)

type Uploader struct {
	client     *s3.Client
	bucket     string
	key        string
	input      io.Reader
	numWorkers int
	partSize   int64
	fileSize   int64
	progress   *progress.Bar
	debug      bool
}

// streamingBuffer manages the streaming upload state
type streamingBuffer struct {
	chunks       map[int64][]byte // Buffers out-of-order chunks
	nextToUpload int64            // Next part number to upload
	bufferedSize atomic.Int64     // Total size of buffered chunks
	mu           sync.Mutex       // Protects chunks map
}

func isStdinPiped() bool {
	stat, _ := os.Stdin.Stat()
	return (stat.Mode() & os.ModeCharDevice) == 0
}

func New(client *s3.Client, bucket, key string, input io.Reader, numWorkers int, showProgress bool) (*Uploader, error) {
	debug := os.Getenv("DEBUG") == "1"

	return &Uploader{
		client:     client,
		bucket:     bucket,
		key:        key,
		input:      input,
		numWorkers: numWorkers,
		partSize:   minPartSize,
		progress:   progress.NewBar(-1, showProgress), // -1 for unknown total size
		debug:      debug,
	}, nil
}

func (u *Uploader) debugf(format string, args ...interface{}) {
	if u.debug {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func (u *Uploader) checkExists(ctx context.Context) (bool, error) {
	u.debugf("Checking if %s already exists", u.key)

	_, err := u.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(u.key),
	})

	if err != nil {
		// Check if the error is because the object doesn't exist
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check if object exists: %w", err)
	}

	return true, nil
}

func (u *Uploader) Upload(ctx context.Context) error {
	u.debugf("Starting streaming upload of %s", u.key)

	// Check if file already exists
	exists, err := u.checkExists(ctx)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("file %s already exists in bucket %s", u.key, u.bucket)
	}

	// Create multipart upload
	createOutput, err := u.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(u.key),
	})
	if err != nil {
		return fmt.Errorf("failed to create multipart upload: %w", err)
	}

	uploadID := createOutput.UploadId
	u.debugf("Created multipart upload with ID: %s", *uploadID)

	// Initialize streaming buffer
	buffer := &streamingBuffer{
		chunks:       make(map[int64][]byte),
		nextToUpload: 1, // Start with part 1
	}

	// Channel for completed parts
	completedParts := make(chan types.CompletedPart, u.numWorkers)
	parts := make([]types.CompletedPart, 0)
	var uploadErr atomic.Value

	// Start upload workers
	var uploadWg sync.WaitGroup
	for i := 0; i < u.numWorkers; i++ {
		uploadWg.Add(1)
		go func(workerID int) {
			defer uploadWg.Done()
			u.debugf("Worker %d started", workerID)

			for {
				// Get next chunk to upload
				buffer.mu.Lock()
				data, exists := buffer.chunks[buffer.nextToUpload]
				if !exists {
					buffer.mu.Unlock()
					if uploadErr.Load() != nil {
						return
					}
					time.Sleep(100 * time.Millisecond)
					continue
				}

				partNum := buffer.nextToUpload
				buffer.nextToUpload++
				buffer.mu.Unlock()

				// Upload the part
				completedPart, err := u.uploadPart(ctx, bytes.NewReader(data), uploadID, partNum)
				if err != nil {
					u.debugf("Worker %d failed to upload part %d: %v", workerID, partNum, err)
					uploadErr.Store(err)
					return
				}

				buffer.mu.Lock()
				delete(buffer.chunks, partNum)
				buffer.bufferedSize.Add(-int64(len(data)))
				buffer.mu.Unlock()

				u.debugf("Worker %d completed part %d", workerID, partNum)
				u.progress.Add(int64(len(data)))
				completedParts <- completedPart
			}
		}(i)
	}

	// Start reader goroutine
	var readWg sync.WaitGroup
	readWg.Add(1)
	go func() {
		defer readWg.Done()
		partNum := int64(1)
		buf := make([]byte, u.partSize)

		for {
			n, err := io.ReadFull(u.input, buf)
			if err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					if n > 0 {
						buffer.mu.Lock()
						buffer.chunks[partNum] = make([]byte, n)
						copy(buffer.chunks[partNum], buf[:n])
						buffer.bufferedSize.Add(int64(n))
						buffer.mu.Unlock()
					}
					break
				}
				uploadErr.Store(fmt.Errorf("failed to read input: %w", err))
				return
			}

			buffer.mu.Lock()
			buffer.chunks[partNum] = make([]byte, n)
			copy(buffer.chunks[partNum], buf[:n])
			buffer.bufferedSize.Add(int64(n))
			buffer.mu.Unlock()

			partNum++
		}
		u.debugf("Finished reading input, total parts: %d", partNum)
	}()

	// Collect completed parts
	go func() {
		for part := range completedParts {
			parts = append(parts, part)
		}
	}()

	// Wait for reading to complete
	readWg.Wait()
	if err := uploadErr.Load(); err != nil {
		return err.(error)
	}

	// Wait for all uploads to complete
	close(completedParts)
	uploadWg.Wait()

	if err := uploadErr.Load(); err != nil {
		return err.(error)
	}

	u.debugf("All parts uploaded successfully, completing multipart upload")

	// Complete multipart upload
	_, err = u.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(u.bucket),
		Key:      aws.String(u.key),
		UploadId: uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	u.debugf("Upload completed successfully")
	u.progress.Finish()
	return nil
}

func (u *Uploader) uploadPart(ctx context.Context, reader io.Reader, uploadID *string, partNumber int64) (types.CompletedPart, error) {
	// Upload part
	uploadOutput, err := u.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(u.bucket),
		Key:        aws.String(u.key),
		PartNumber: aws.Int32(int32(partNumber)),
		UploadId:   uploadID,
		Body:       reader,
	})
	if err != nil {
		return types.CompletedPart{}, fmt.Errorf("failed to upload part %d: %w", partNumber, err)
	}

	return types.CompletedPart{
		ETag:       uploadOutput.ETag,
		PartNumber: aws.Int32(int32(partNumber)),
	}, nil
}

func (u *Uploader) getPartSize(partNumber int64) int64 {
	if partNumber*u.partSize > u.fileSize {
		return u.fileSize - (partNumber-1)*u.partSize
	}
	return u.partSize
}
