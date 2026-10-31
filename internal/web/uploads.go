package web

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cwood/torrent-org/internal/torrent"
)

// uploadTTL is how long a handed-off .torrent waits for the browser.
const uploadTTL = 15 * time.Minute

// uploads holds .torrent files handed off by `torrent-org open` until the
// browser it launched loads the preview. A browser cannot be made to POST a
// file from the command line, so the CLI uploads first and opens a URL.
type uploads struct {
	mu    sync.Mutex
	files map[string]upload
}

type upload struct {
	metainfo []byte
	expires  time.Time
}

func newUploads() *uploads {
	return &uploads{files: make(map[string]upload)}
}

// put stores metainfo and returns its ID, pruning expired uploads.
func (u *uploads) put(metainfo []byte) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	for id, f := range u.files {
		if now.After(f.expires) {
			delete(u.files, id)
		}
	}
	id := rand.Text()
	u.files[id] = upload{metainfo: metainfo, expires: now.Add(uploadTTL)}
	return id
}

// get returns an unexpired upload. Uploads are not consumed, so reloading the
// preview keeps working until they expire.
func (u *uploads) get(id string) ([]byte, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f, ok := u.files[id]
	if !ok || time.Now().After(f.expires) {
		return nil, false
	}
	return f.metainfo, true
}

// createUpload accepts a raw .torrent body and returns the preview URL as
// {"url": "/add/<id>"}.
func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	metainfo, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTorrentSize))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if _, err := torrent.Parse(bytes.NewReader(metainfo)); err != nil {
		http.Error(w, "not a valid .torrent file: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"url": "/add/" + s.uploads.put(metainfo)})
}

// previewUpload shows the preview for a handed-off upload.
func (s *Server) previewUpload(w http.ResponseWriter, r *http.Request) {
	metainfo, ok := s.uploads.get(r.PathValue("id"))
	if !ok {
		page := newAddPage()
		page.Error = "That upload has expired; open the .torrent file again."
		s.page(w, http.StatusNotFound, "add", page)
		return
	}
	s.showAdd(w, r, http.StatusOK, addState{Metainfo: metainfo, Upload: r.PathValue("id"), Choice: choiceAuto}, "")
}
