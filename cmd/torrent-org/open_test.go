package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServiceURL(t *testing.T) {
	tests := map[string]string{
		"127.0.0.1:9092": "http://127.0.0.1:9092",
		"0.0.0.0:9092":   "http://127.0.0.1:9092",
		":9092":          "http://127.0.0.1:9092",
		"[::]:9092":      "http://127.0.0.1:9092",
		"[::1]:9092":     "http://[::1]:9092",
		"nas.lan:9092":   "http://nas.lan:9092",
	}
	for addr, want := range tests {
		got, err := serviceURL(addr)
		require.NoError(t, err, addr)
		require.Equal(t, want, got, addr)
	}
	_, err := serviceURL("9092")
	require.Error(t, err)
}

func writeTorrent(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.torrent")
	require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	return p
}

func TestOpen(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/uploads", r.URL.Path)
		require.Equal(t, "application/x-bittorrent", r.Header.Get("Content-Type"))
		var err error
		body, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"url":"/add/abc"}`))
	}))
	t.Cleanup(srv.Close)

	var launched string
	err := open(t.Context(), srv.URL, writeTorrent(t, "d4:infod4:name1:xee"), func(u string) error {
		launched = u
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, "d4:infod4:name1:xee", string(body))
	require.Equal(t, srv.URL+"/add/abc", launched)
}

func TestOpenRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not a valid .torrent file: bad", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	err := open(t.Context(), srv.URL, writeTorrent(t, "junk"), func(string) error {
		t.Fatal("browser must not open")
		return nil
	})
	require.ErrorContains(t, err, "not a valid .torrent file: bad")
}

func TestOpenNotRunning(t *testing.T) {
	err := open(t.Context(), "http://127.0.0.1:1", writeTorrent(t, "x"), func(string) error { return nil })
	require.ErrorContains(t, err, "is torrent-org running at http://127.0.0.1:1?")
}

func TestOpenUI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)

	var launched string
	require.NoError(t, openUI(t.Context(), srv.URL, func(u string) error {
		launched = u
		return nil
	}))
	require.Equal(t, srv.URL+"/", launched)

	err := openUI(t.Context(), "http://127.0.0.1:1", func(string) error {
		t.Fatal("browser must not open")
		return nil
	})
	require.ErrorContains(t, err, "is torrent-org running")
}
