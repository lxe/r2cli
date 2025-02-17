# r2cli

A fast, parallel file transfer tool for Cloudflare R2 with efficient streaming support. This tool uses multiple concurrent connections to maximize transfer speeds and efficiently handles large files using multipart operations.

## Features

- Parallel downloads and uploads using multiple workers
- Efficient streaming support for downloads (write-as-you-download)
- Progress bar with transfer speed
- Support for large files using multipart transfers
- S3-compatible API support for R2
- Safe uploads - won't overwrite existing files
- Pipe support for stdin/stdout operations
- Optional progress bar for piped operations

## Memory Usage

Memory usage is optimized for both uploads and downloads:
- **Downloads**: Only out-of-order chunks are buffered, with sequential chunks written immediately to output
- **Uploads**: Uses streaming with minimal buffering, only keeping chunks that are waiting to be uploaded

## Installation

```bash
go install github.com/lxe/r2cli/cmd/r2cli@latest
```

## Environment Variables

The following environment variables are used for authentication. The R2-prefixed variables take precedence if both are set:

Primary (recommended):
- `R2_ACCESS_KEY_ID`: Your R2 access key ID
- `R2_SECRET_ACCESS_KEY`: Your R2 secret access key

Alternative:
- `ACCESS_KEY_ID`: Your R2 access key ID
- `SECRET_ACCESS_KEY`: Your R2 secret access key

Debug mode:
- `DEBUG`: Set to 1 to enable detailed debug logging

## Quick Start

Basic upload (will fail if file already exists):
```bash
r2cli upload -i file.bin -r2 "r2://<account>.r2.cloudflarestorage.com/bucket/file.bin" -c 16
```

Basic download:
```bash
r2cli download -r2 "r2://<account>.r2.cloudflarestorage.com/bucket/file.bin" -o file.bin -c 16
```

With authentication:
```bash
# Using environment variables
export R2_ACCESS_KEY_ID=xxx
export R2_SECRET_ACCESS_KEY=xxx
r2cli download ...

# Or inline with the command
R2_ACCESS_KEY_ID=xxx R2_SECRET_ACCESS_KEY=xxx r2cli download ...
```

With debug logging:
```bash
DEBUG=1 r2cli download ...
```

## Command Details

### Upload

```bash
r2cli upload [options]
  -i string
        Input file path (use - for stdin) (default: -)
  -r2 string
        R2 URL in the format "r2://<endpoint>/<bucket>/<key>" (required)
  -c int
        Number of concurrent workers (default: number of CPU cores)
  -s, -silent
        Suppress progress bar
```

Example:
```bash
r2cli upload \
  -i large-file.bin \
  -r2 "r2://abc123.r2.cloudflarestorage.com/mybucket/large-file.bin" \
  -c 16 \
  -silent
```

Note: The upload will fail if the destination file already exists in R2. This is to prevent accidental overwrites.

### Download

```bash
r2cli download [options]
  -r2 string
        R2 URL in the format "r2://<endpoint>/<bucket>/<key>" (required)
  -o string
        Output file path (use - for stdout) (default: -)
  -c int
        Number of concurrent workers (default: number of CPU cores)
  -s, -silent
        Suppress progress bar
```

Example:
```bash
r2cli download \
  -r2 "r2://abc123.r2.cloudflarestorage.com/mybucket/large-file.bin" \
  -o large-file.bin \
  -c 16 \
  -silent
```

## Streaming Support

Both download and upload functionality support efficient streaming with minimal buffering:

```bash
# Download and decompress on the fly - chunks are written as they arrive
r2cli download -r2 "r2://abc123.r2.cloudflarestorage.com/file.tar.zst" -o - | zstd -d | tar x

# Upload with streaming - data is uploaded as it's read
tar czf - directory/ | r2cli upload -r2 "r2://abc123.r2.cloudflarestorage.com/backup.tar.gz"

# Chain multiple operations with minimal memory usage
cat file | pv | r2cli upload -r2 "r2://abc123.r2.cloudflarestorage.com/file"
```

## Implementation Details

### Downloads
- Uses 25MB chunk size for downloads
- Maintains strict ordering for streaming output
- Only buffers out-of-order chunks
- Writes sequential chunks immediately
- Progress tracks actual downloaded bytes

### Uploads
- Uses 8MB minimum part size for multipart uploads
- Streams input data with minimal buffering
- Only buffers chunks waiting to be uploaded
- Maintains part order for multipart upload
- Progress tracks uploaded parts

## Progress Bar Behavior

The progress bar is:
- Shown by default for all operations
- Written to stderr when stdout is being used for data
- Can be disabled with `-s` or `--silent` flag
- Shows download/upload speed and completion percentage

## Performance Tips

1. The number of workers (`-c`) should be adjusted based on your network and CPU capabilities. The default is set to the number of CPU cores.

2. Enable debug logging to see detailed transfer information:
   ```bash
   DEBUG=1 r2cli ...
   ```

3. For piped operations, consider using `pv` to get additional transfer statistics:
   ```bash
   r2cli download ... | pv | zstd -d > file
   ```

## Requirements

- Go 1.21 or later
- Valid R2 credentials (access key and secret key) 