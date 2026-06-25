// Copyright (c) 2021-2025, Ludvig Lundgren and the autobrr contributors.
// SPDX-License-Identifier: GPL-2.0-or-later

package releasedownload

import (
	"context"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autobrr/autobrr/internal/domain"

	"github.com/avast/retry-go/v4"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type recordingTimer struct {
	delays []time.Duration
}

func (t *recordingTimer) After(d time.Duration) <-chan time.Time {
	t.delays = append(t.delays, d)

	ch := make(chan time.Time, 1)
	ch <- time.Now()
	return ch
}

func TestDownloadTorrentRetryOptions(t *testing.T) {
	t.Run("uses configured attempts and caps exponential backoff", func(t *testing.T) {
		svc := &DownloadService{log: zerolog.Nop()}
		timer := &recordingTimer{}
		attempts := 0

		opts := append(svc.downloadTorrentRetryOptions(), retry.WithTimer(timer))
		err := retry.Do(func() error {
			attempts++
			return stderrors.New("temporary failure")
		}, opts...)

		require.Error(t, err)
		require.Equal(t, int(downloadTorrentAttempts), attempts)
		require.Len(t, timer.delays, int(downloadTorrentAttempts)-1)
		require.Equal(t, []time.Duration{
			10 * time.Second,
			20 * time.Second,
			40 * time.Second,
			80 * time.Second,
			160 * time.Second,
			180 * time.Second,
		}, timer.delays[:6])
	})

	t.Run("honors retry-after delay", func(t *testing.T) {
		svc := &DownloadService{log: zerolog.Nop()}
		timer := &recordingTimer{}
		attempts := 0

		opts := append(svc.downloadTorrentRetryOptions(), retry.WithTimer(timer))
		err := retry.Do(func() error {
			attempts++
			if attempts == 1 {
				return &RetriableError{
					Err:        stderrors.New("rate limited"),
					RetryAfter: 3 * time.Minute,
				}
			}
			return nil
		}, opts...)

		require.NoError(t, err)
		require.Equal(t, 2, attempts)
		require.Equal(t, []time.Duration{3 * time.Minute}, timer.delays)
	})
}

func TestRetryableRequest(t *testing.T) {
	torrentBytes, err := os.ReadFile(filepath.Join("..", "domain", "testdata", "archlinux-2011.08.19-netinstall-i686.iso.torrent"))
	require.NoError(t, err)

	tests := []struct {
		name          string
		status        int
		headers       map[string]string
		body          []byte
		wantRecover   bool
		wantRetriable bool
	}{
		{
			name:   "valid torrent",
			status: http.StatusOK,
			body:   torrentBytes,
		},
		{
			name:        "auth error is unrecoverable",
			status:      http.StatusUnauthorized,
			body:        []byte("unauthorized"),
			wantRecover: false,
		},
		{
			name:        "transient server error is recoverable",
			status:      http.StatusServiceUnavailable,
			body:        []byte("unavailable"),
			wantRecover: true,
		},
		{
			name:          "rate limit with retry-after is retriable",
			status:        http.StatusTooManyRequests,
			headers:       map[string]string{"Retry-After": "45"},
			body:          []byte("rate limited"),
			wantRecover:   true,
			wantRetriable: true,
		},
		{
			name:        "rate limit retry-after zero is unrecoverable",
			status:      http.StatusTooManyRequests,
			headers:     map[string]string{"Retry-After": "0"},
			body:        []byte("rate limited"),
			wantRecover: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for key, value := range tt.headers {
					w.Header().Set(key, value)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write(tt.body)
			}))
			defer ts.Close()

			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL, nil)
			require.NoError(t, err)

			tmpFile, err := os.CreateTemp(t.TempDir(), "autobrr-")
			require.NoError(t, err)
			defer tmpFile.Close()

			release := &domain.Release{
				Indexer: domain.IndexerMinimal{
					Name: "Mock Indexer",
				},
				TorrentName: "Test.Release-GROUP",
				DownloadURL: ts.URL,
			}

			err = retryableRequest(ts.Client(), req, release, tmpFile)()

			if tt.status == http.StatusOK {
				require.NoError(t, err)
				require.NotEmpty(t, release.TorrentTmpFile)
				require.NotEmpty(t, release.TorrentHash)
				require.NotZero(t, release.Size)
				return
			}

			require.Error(t, err)
			require.Equal(t, tt.wantRecover, retry.IsRecoverable(err))

			var retriable *RetriableError
			require.Equal(t, tt.wantRetriable, stderrors.As(err, &retriable))
			if tt.wantRetriable {
				require.Equal(t, 45*time.Second, retriable.RetryAfter)
			}
		})
	}
}
