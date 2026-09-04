// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package cmd

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/stretchr/testify/require"
)

// TestKVGetWriterIsAtomic pins the destination-file contract for `kv get`.
// The stream can fail partway -- most easily offline, where the first chunk
// may be served from the local cache and a later one is not
// (docs/kv_offline.md D2) -- and a destination that already held bytes at
// that point would be a silently truncated file. Nothing is visible at the
// destination until Commit.
func TestKVGetWriterIsAtomic(t *testing.T) {
	var m libclient.MetaContext // unused by the file branch of openWriter

	noStrays := func(t *testing.T, dir string) {
		t.Helper()
		ents, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range ents {
			require.NotContains(t, e.Name(), ".tmp", "no temp file left behind")
		}
	}

	t.Run("commit publishes the content", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "out.bin")

		w, err := openWriter(m, dest, 0o600, false, false)
		require.NoError(t, err)
		_, err = w.Write([]byte("all of it"))
		require.NoError(t, err)
		require.NoError(t, w.Commit())
		require.NoError(t, w.Close())

		got, err := os.ReadFile(dest)
		require.NoError(t, err)
		require.Equal(t, []byte("all of it"), got)
		noStrays(t, dir)
	})

	t.Run("abandoned transfer leaves no file", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "out.bin")

		w, err := openWriter(m, dest, 0o600, false, false)
		require.NoError(t, err)
		_, err = w.Write([]byte("only the cached prefix"))
		require.NoError(t, err)
		// No Commit: this is the mid-stream failure.
		require.NoError(t, w.Close())

		_, err = os.Stat(dest)
		require.True(t, os.IsNotExist(err),
			"a partial transfer must not leave a truncated file")
		ents, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Empty(t, ents, "no temp file left behind")
	})

	t.Run("abandoned overwrite leaves the original intact", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "out.bin")
		require.NoError(t, os.WriteFile(dest, []byte("the original"), 0o600))

		w, err := openWriter(m, dest, 0o600, true, false)
		require.NoError(t, err)
		_, err = w.Write([]byte("partial"))
		require.NoError(t, err)
		require.NoError(t, w.Close())

		got, err := os.ReadFile(dest)
		require.NoError(t, err)
		require.Equal(t, []byte("the original"), got,
			"a failed --force overwrite must not damage the existing file")
		noStrays(t, dir)
	})

	t.Run("without force an existing destination is refused", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "out.bin")
		require.NoError(t, os.WriteFile(dest, []byte("mine"), 0o600))

		_, err := openWriter(m, dest, 0o600, false, false)
		require.Error(t, err)
		require.True(t, os.IsExist(err), "expected an exists error, got %v", err)

		got, err := os.ReadFile(dest)
		require.NoError(t, err)
		require.Equal(t, []byte("mine"), got)
		noStrays(t, dir)
	})

	t.Run("force writes through a symlink, not over it", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.bin")
		link := filepath.Join(dir, "link.bin")
		require.NoError(t, os.WriteFile(target, []byte("old"), 0o600))
		require.NoError(t, os.Symlink(target, link))

		w, err := openWriter(m, link, 0o600, true, false)
		require.NoError(t, err)
		_, err = w.Write([]byte("new"))
		require.NoError(t, err)
		require.NoError(t, w.Commit())

		// The link survives, and the content landed on its target.
		fi, err := os.Lstat(link)
		require.NoError(t, err)
		require.NotZero(t, fi.Mode()&os.ModeSymlink, "the symlink must not be replaced")
		got, err := os.ReadFile(target)
		require.NoError(t, err)
		require.Equal(t, []byte("new"), got)
	})

	t.Run("a directory destination is refused up front", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "sub")
		require.NoError(t, os.Mkdir(sub, 0o700))

		// --force skips the exclusivity claim, so without an explicit check
		// this would only fail at the rename -- after a whole download.
		_, err := openWriter(m, sub, 0o600, true, false)
		require.Error(t, err)

		ents, err := os.ReadDir(sub)
		require.NoError(t, err)
		require.Empty(t, ents, "no temp file left inside the destination")
	})

	t.Run("a special file is written through, not renamed over", func(t *testing.T) {
		dir := t.TempDir()
		fifo := filepath.Join(dir, "pipe")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Skipf("mkfifo unsupported here: %v", err)
		}

		// A reader has to be attached or the open blocks.
		opened := make(chan struct{})
		go func() {
			f, err := os.OpenFile(fifo, os.O_RDONLY, 0)
			if err == nil {
				io.Copy(io.Discard, f)
				f.Close()
			}
			close(opened)
		}()

		w, err := openWriter(m, fifo, 0o600, true, false)
		require.NoError(t, err)
		_, err = w.Write([]byte("through the pipe"))
		require.NoError(t, err)
		require.NoError(t, w.Commit())
		require.NoError(t, w.Close())
		<-opened

		// The FIFO is still a FIFO: renaming over it would have left a
		// regular file, and would have needed a temp file in its directory.
		fi, err := os.Stat(fifo)
		require.NoError(t, err)
		require.NotZero(t, fi.Mode()&os.ModeNamedPipe, "the FIFO must not be replaced")
		noStrays(t, dir)
	})

	t.Run("committed file carries the requested mode", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "out.bin")

		w, err := openWriter(m, dest, 0o640, false, false)
		require.NoError(t, err)
		_, err = w.Write([]byte("x"))
		require.NoError(t, err)
		require.NoError(t, w.Commit())

		fi, err := os.Stat(dest)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o640), fi.Mode().Perm())
	})
}
