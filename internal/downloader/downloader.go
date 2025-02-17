package downloader

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/lxe/r2cli/internal/progress"
)

const (
	// Size of individual download chunks. This balances memory usage with request overhead.
	// Too small: more requests, higher overhead
	// Too large: more memory usage, longer before streaming starts
	chunkSize = 25 * 1024 * 1024 // 25MB micro chunk size
)

// Enable debug logging via DEBUG=1 environment variable
var debugEnabled = os.Getenv("DEBUG") == "1"

func debugf(format string, v ...interface{}) {
	if debugEnabled {
		log.Printf(format, v...)
	}
}

// chunk represents a downloaded portion of the file
type chunk struct {
	start, end int64  // Byte range positions
	data       []byte // Actual chunk data
	err        error  // Any error during download
}

// Downloader manages parallel downloads while maintaining streaming order
type Downloader struct {
	client         *s3.Client
	bucket         string
	key            string
	output         io.Writer
	numWorkers     int
	fileSize       int64
	progress       *progress.Bar
	downloadedSize atomic.Int64 // Tracks total bytes downloaded for progress
}

// downloadState maintains the streaming state
type downloadState struct {
	chunks       map[int64][]byte // Buffers out-of-order chunks by start position
	nextToWrite  int64            // Position of next byte to write (maintains order)
	bufferedSize atomic.Int64     // Total size of chunks in buffer
}

func isStdoutPiped() bool {
	stat, _ := os.Stdout.Stat()
	return (stat.Mode() & os.ModeCharDevice) == 0
}

// New creates a downloader with the specified parameters
func New(client *s3.Client, bucket, key string, output io.Writer, numWorkers int, showProgress bool) (*Downloader, error) {
	ctx := context.Background()

	// Get file size for progress tracking and chunk calculations
	headOutput, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get object info: %w", err)
	}

	return &Downloader{
		client:     client,
		bucket:     bucket,
		key:        key,
		output:     output,
		numWorkers: numWorkers,
		fileSize:   aws.ToInt64(headOutput.ContentLength),
		progress:   progress.NewBar(aws.ToInt64(headOutput.ContentLength), showProgress),
	}, nil
}

// Download performs a parallel, streaming download of the file
func (d *Downloader) Download(ctx context.Context) error {
	startTime := time.Now()
	debugf("Starting download of %s/%s (size: %.2f MB) with %d workers",
		d.bucket, d.key, float64(d.fileSize)/1024/1024, d.numWorkers)

	// Initialize streaming state
	state := &downloadState{
		chunks:      make(map[int64][]byte),
		nextToWrite: 0, // Start writing from beginning of file
	}

	// Channel for completed chunks, buffered to prevent worker stalling
	completedChunks := make(chan chunk, d.numWorkers)
	var downloadErr atomic.Value
	var wg sync.WaitGroup
	nextChunkPos := atomic.Int64{} // Atomic counter for assigning chunks to workers

	// Start the chunk writer goroutine that maintains streaming order
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		if err := d.processCompletedChunks(state, completedChunks); err != nil {
			downloadErr.Store(err)
		}
	}()

	// Start worker pool - each worker continuously downloads next available chunk
	for i := 0; i < d.numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				// Atomically get next chunk position
				pos := nextChunkPos.Add(chunkSize)
				startPos := pos - chunkSize
				if startPos >= d.fileSize {
					return // No more chunks to download
				}

				debugf("Starting download of chunk at position %.2f MB",
					float64(startPos)/1024/1024)

				// Calculate chunk range, handling last chunk case
				end := min(startPos+chunkSize-1, d.fileSize-1)
				data, err := d.downloadChunk(ctx, startPos, end)
				if err != nil {
					downloadErr.Store(err)
					return
				}

				// Update progress tracking
				chunkSize := end - startPos + 1
				d.downloadedSize.Add(chunkSize)
				if d.progress != nil {
					d.progress.Add(chunkSize)
				}

				downloadDuration := time.Since(startTime)
				downloadSpeed := float64(chunkSize) / 1024 / 1024 / downloadDuration.Seconds()
				debugf("Chunk downloaded: start=%.2f MB, size=%.2f MB, speed=%.2f MB/s",
					float64(startPos)/1024/1024,
					float64(chunkSize)/1024/1024,
					downloadSpeed)

				// Send completed chunk to writer
				completedChunks <- chunk{
					start: startPos,
					end:   end,
					data:  data,
				}
			}
		}()
	}

	// Wait for all downloads and writing to complete
	wg.Wait()
	close(completedChunks)
	<-writerDone

	if err := downloadErr.Load(); err != nil {
		return err.(error)
	}

	totalDuration := time.Since(startTime)
	avgSpeed := float64(d.fileSize) / 1024 / 1024 / totalDuration.Seconds()
	debugf("Download completed in %.2fs, average speed: %.2f MB/s",
		totalDuration.Seconds(), avgSpeed)

	if d.progress != nil {
		d.progress.Finish()
	}
	return nil
}

// processCompletedChunks handles the ordered streaming of downloaded chunks
func (d *Downloader) processCompletedChunks(state *downloadState, completedChunks <-chan chunk) error {
	for c := range completedChunks {
		// Buffer the chunk for potential out-of-order arrival
		state.chunks[c.start] = c.data
		state.bufferedSize.Add(c.end - c.start + 1)

		debugf("Buffered chunk: start=%.2f MB, buffered_total=%.2f MB",
			float64(c.start)/1024/1024,
			float64(state.bufferedSize.Load())/1024/1024)

		// Try to write as many sequential chunks as possible
		for {
			data, exists := state.chunks[state.nextToWrite]
			if !exists {
				// Gap found, need to wait for the next sequential chunk
				debugf("Gap found at %.2f MB, waiting for more chunks",
					float64(state.nextToWrite)/1024/1024)
				break
			}

			// Write the next chunk in sequence
			writeStartTime := time.Now()
			if _, err := d.output.Write(data); err != nil {
				return fmt.Errorf("failed to write chunk: %w", err)
			}

			chunkSize := int64(len(data))
			writeDuration := time.Since(writeStartTime)
			writeSpeed := float64(chunkSize) / 1024 / 1024 / writeDuration.Seconds()

			debugf("Wrote chunk: pos=%.2f MB, size=%.2f MB, write_speed=%.2f MB/s",
				float64(state.nextToWrite)/1024/1024,
				float64(chunkSize)/1024/1024,
				writeSpeed)

			// Clean up written chunk and advance write position
			delete(state.chunks, state.nextToWrite)
			state.nextToWrite += chunkSize
			state.bufferedSize.Add(-chunkSize)
		}
	}
	return nil
}

// downloadChunk downloads a specific byte range of the file
func (d *Downloader) downloadChunk(ctx context.Context, start, end int64) ([]byte, error) {
	input := &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", start, end)),
	}

	output, err := d.client.GetObject(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to download chunk: %w", err)
	}
	defer output.Body.Close()

	return io.ReadAll(output.Body)
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
