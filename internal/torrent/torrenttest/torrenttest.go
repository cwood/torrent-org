// Package torrenttest builds .torrent fixtures for tests.
package torrenttest

import (
	"bytes"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/stretchr/testify/require"
)

// Encode returns the bencoded .torrent file for info announcing to announce.
func Encode(t *testing.T, info metainfo.Info, announce metainfo.AnnounceList) []byte {
	t.Helper()
	info.PieceLength = 16384
	infoBytes, err := bencode.Marshal(info)
	require.NoError(t, err)
	mi := metainfo.MetaInfo{InfoBytes: infoBytes, AnnounceList: announce}
	if len(announce) > 0 {
		mi.Announce = announce[0][0]
	}
	var buf bytes.Buffer
	require.NoError(t, mi.Write(&buf))
	return buf.Bytes()
}
