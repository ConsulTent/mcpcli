package tus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// defaultChunkSize is the client-side chunk size for Upload when the caller
// passes chunkSize <= 0 — one rule, no special case. 64 MiB sits below the
// default librarium per-PATCH cap (max_file_size, 100 MiB → 413).
const defaultChunkSize = 64 << 20

// isPermanentPatchErr reports whether a PATCH failure is a 4xx-class
// sentinel abort (D5): those statuses never recover by resending.
func isPermanentPatchErr(err error) bool {
	return errors.Is(err, ErrUnauthorized) ||
		errors.Is(err, ErrMismatchedOffset) ||
		errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrSizeExceeded)
}

// Upload performs the full tus orchestration for one file (D3): Create (or
// HEAD-resume), then chunked PATCHes with io.NewSectionReader bodies, the
// offset always advancing from the PATCH RESPONSE's Upload-Offset — never
// from local accounting. chunkSize <= 0 means defaultChunkSize.
//
// Failure handling:
//   - a canceled/expired context aborts immediately, wrapping the context
//     error (%w — errors.Is classifies context.Canceled vs
//     DeadlineExceeded) with ZERO resync HEADs;
//   - any other PATCH failure triggers a HEAD resync (a tusd 409 carries no
//     Upload-Offset, so HEAD is the only recovery primitive) and the
//     resynced offset classifies the outcome: == size → success (the
//     server may have landed it); > offset → advance, consume one retry,
//     continue (tusd partial-append semantics — a blind retry from a stale
//     offset would corrupt the upload); == offset → 4xx-class sentinels
//     abort permanently, 5xx/network consumes a retry; resync 404 → abort
//     "no longer in staging", never fake success, never re-Create
//     (duplicate-upload guard).
//
// retry is the caller's retry budget for retry-classified failures (the CLI
// defaults it to 3; <= 0 means no retries — the resync HEAD still fires,
// because it classifies, it does not retry).
//
// onProgress fires after each successful PATCH with (sent, total). Errors
// stay low-context: URL+offset context is added at exactly ONE site — the
// CLI failure line — except where the diagnosis IS the context (the staged
// URL in the swept/past-size aborts, both offsets in the stall abort). The
// staged URL is nonetheless RETURNED alongside every error once known
// (after Create succeeds; the resume target is the caller's input), so the
// CLI failure line can name the wire target without a second channel.
func (c *Client) Upload(ctx context.Context, f *os.File, size int64, metadata map[string]string, chunkSize int64, resumeURL string, retry int, onProgress func(sent, total int64)) (string, error) {
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	attempts := retry

	var (
		url    string
		offset int64
		err    error
	)
	if resumeURL == "" {
		url, err = c.Create(ctx, size, metadata)
		if err != nil {
			return "", err
		}
	} else {
		resumed, _, exists, herr := c.HeadOffset(ctx, resumeURL)
		if herr != nil {
			return "", herr
		}
		if !exists {
			return "", fmt.Errorf("tus: resume target %s is no longer in staging; it may have landed or been swept — verify server-side", resumeURL)
		}
		if resumed > size {
			return "", fmt.Errorf("tus: the staged upload %s is at offset %d, past the file's size %d; the file may have changed since it was staged", resumeURL, resumed, size)
		}
		if resumed == size {
			return resumeURL, nil // fully staged; nothing to PATCH
		}
		url = resumeURL
		offset = resumed
	}

	for offset < size {
		n := min(chunkSize, size-offset)
		newOff, perr := c.Patch(ctx, url, offset, io.NewSectionReader(f, offset, n), n)
		if perr == nil {
			if newOff <= offset {
				return "", fmt.Errorf("tus: the server accepted the PATCH at offset %d but reported no progress (Upload-Offset %d)", offset, newOff)
			}
			offset = newOff
			if onProgress != nil {
				onProgress(offset, size)
			}
			continue
		}

		// A canceled/expired context aborts before anything else — even
		// before a resync HEAD — wrapping the context error so errors.Is
		// classifies operator Ctrl-C vs timeout (D3).
		if cerr := ctx.Err(); cerr != nil {
			return url, fmt.Errorf("tus: upload interrupted: %w", cerr)
		}

		// HEAD-resync: the server's offset is the sole recovery primitive.
		rOff, _, exists, herr := c.HeadOffset(ctx, url)
		if herr != nil {
			// The resync itself failed: retry from the same offset when the
			// budget allows (a partial append self-corrects via the next
			// resync's 409); otherwise surface the resync error.
			if attempts <= 0 {
				return url, herr
			}
			attempts--
			continue
		}
		if !exists {
			return url, fmt.Errorf("tus: upload %s is no longer in staging; it may have landed or been swept — verify server-side", url)
		}
		switch {
		case rOff == size:
			// The server has every byte (and may already have landed it);
			// completion is not a retry.
			return url, nil
		case rOff > offset:
			// tusd partial append: continue from the server offset, costing
			// one retry.
			if attempts <= 0 {
				return url, perr
			}
			attempts--
			offset = rOff
		case rOff == offset:
			if isPermanentPatchErr(perr) {
				return url, fmt.Errorf("tus: the upload stalled at client offset %d (the server is at offset %d): %w", offset, rOff, perr)
			}
			if attempts <= 0 {
				return url, perr
			}
			attempts--
		default:
			// The server lost ground — fail closed instead of corrupting
			// the upload by re-sending acknowledged bytes.
			return url, fmt.Errorf("tus: the server resynced to offset %d, behind the last sent offset %d; the upload state is inconsistent", rOff, offset)
		}
	}
	return url, nil
}
