package desktop

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExisting(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "movie.mkv")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	for path, want := range map[string]string{
		file:                                   file,
		dir:                                    dir,
		filepath.Join(dir, "missing.mkv"):      dir,
		filepath.Join(dir, "Album", "01.flac"): dir,
	} {
		got, err := existing(path)
		require.NoError(t, err)
		require.Equal(t, want, got, path)
	}
}
