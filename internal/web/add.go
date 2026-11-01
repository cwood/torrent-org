package web

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"

	"github.com/cwood/torrent-org/internal/intake"
	"github.com/cwood/torrent-org/internal/rules"
	"github.com/cwood/torrent-org/internal/torrent"
)

// maxTorrentSize bounds uploads; real .torrent files are rarely over a few
// hundred KiB even with tens of thousands of files.
const maxTorrentSize = 10 << 20

type addPage struct {
	chrome
	Error string
	// The fields below are set once a file has been uploaded.
	Torrent   *torrent.Torrent
	Size      int64
	Metainfo  string
	Upload    string
	Placement rules.Placement
	Picker    picker
	Files     fileChoices
	Duplicate bool
}

// fileChoice is a file in a filepicker.
type fileChoice struct {
	torrent.File
	Checked bool
}

type fileChoices []fileChoice

// Count is how many files are checked.
func (fs fileChoices) Count() int {
	n := 0
	for _, f := range fs {
		if f.Checked {
			n++
		}
	}
	return n
}

// Size is the total size of the checked files.
func (fs fileChoices) Size() int64 {
	var n int64
	for _, f := range fs {
		if f.Checked {
			n += f.Size
		}
	}
	return n
}

func newAddPage() addPage {
	return addPage{chrome: chrome{"Add torrent", "add"}}
}

func (s *Server) addForm(w http.ResponseWriter, r *http.Request) {
	s.page(w, http.StatusOK, "add", newAddPage())
}

// previewAdd accepts the add page's upload form, the fallback for browsers
// without JavaScript, and hands off to the preview URL like the CLI does.
func (s *Server) previewAdd(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTorrentSize)
	f, _, err := r.FormFile("torrent")
	if err != nil {
		s.addError(w, "Choose a .torrent file to upload.")
		return
	}
	metainfo, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		s.addError(w, fmt.Sprintf("Could not read the upload: %v", err))
		return
	}
	if _, err := torrent.Parse(bytes.NewReader(metainfo)); err != nil {
		s.addError(w, "Cannot add this torrent: not a valid .torrent file: "+err.Error())
		return
	}
	redirect(w, r, "/add/"+s.uploads.put(metainfo))
}

// addState is what the add form carries between requests.
type addState struct {
	Metainfo []byte
	// Upload is the hand-off ID, so pages like the rule editor can link back
	// to the preview.
	Upload string
	Choice string
	// Selected is the set of checked file indexes; nil checks every file.
	Selected map[int]bool
}

// formState reads the add form, rendering an error page when the carried
// .torrent is missing or mangled.
func (s *Server) formState(w http.ResponseWriter, r *http.Request) (addState, bool) {
	metainfo, err := base64.StdEncoding.DecodeString(r.FormValue("metainfo"))
	if err != nil || len(metainfo) == 0 {
		s.addError(w, "The upload was lost; please choose the file again.")
		return addState{}, false
	}
	st := addState{
		Metainfo: metainfo,
		Upload:   r.FormValue("upload"),
		Choice:   r.FormValue("rule"),
		Selected: selectedFiles(r),
	}
	return st, true
}

// selectedFiles is the set of file indexes checked in a filepicker. r's
// form must already be parsed.
func selectedFiles(r *http.Request) map[int]bool {
	selected := make(map[int]bool)
	for _, v := range r.Form["file"] {
		if i, err := strconv.Atoi(v); err == nil {
			selected[i] = true
		}
	}
	return selected
}

// choose re-renders the preview when the rule picker changes, keeping the
// file selection.
func (s *Server) choose(w http.ResponseWriter, r *http.Request) {
	if st, ok := s.formState(w, r); ok {
		s.showAdd(w, r, http.StatusOK, st, "")
	}
}

// add adds the previewed torrent with the chosen rule and files.
func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	st, ok := s.formState(w, r)
	if !ok {
		return
	}
	t, p, err := s.placeUpload(r, st.Metainfo, st.Choice)
	if err != nil {
		s.addError(w, "Cannot add this torrent: "+err.Error())
		return
	}
	var skip []int
	for _, f := range t.Files {
		if !st.Selected[f.Index] {
			skip = append(skip, f.Index)
		}
	}
	if len(skip) == len(t.Files) {
		s.showAdd(w, r, http.StatusUnprocessableEntity, st, "Select at least one file to download.")
		return
	}

	err = s.intake.Add(r.Context(), intake.Addition{
		Metainfo: st.Metainfo, Hash: t.Hash, Placement: p, Skip: skip, Manual: st.Choice != choiceAuto,
	})
	if errors.Is(err, intake.ErrDuplicate) {
		s.showAdd(w, r, http.StatusConflict, st, "")
		return
	}
	if err != nil {
		s.showAdd(w, r, http.StatusBadGateway, st, err.Error())
		return
	}
	redirect(w, r, "/")
}

// placeUpload parses metainfo and resolves its placement for choice: the
// rules' pick, the default location, or a specific rule ID.
func (s *Server) placeUpload(r *http.Request, metainfo []byte, choice string) (torrent.Torrent, rules.Placement, error) {
	t, err := torrent.Parse(bytes.NewReader(metainfo))
	if err != nil {
		return torrent.Torrent{}, rules.Placement{}, fmt.Errorf("not a valid .torrent file: %w", err)
	}
	rs, trackers, err := s.ruleset(r.Context())
	if err != nil {
		return torrent.Torrent{}, rules.Placement{}, err
	}
	p, err := s.placeChoice(t, choice, rs, trackers)
	return t, p, err
}

// showAdd renders the preview for st.
func (s *Server) showAdd(w http.ResponseWriter, r *http.Request, code int, st addState, errMsg string) {
	t, p, err := s.placeUpload(r, st.Metainfo, st.Choice)
	if err != nil {
		s.addError(w, "Cannot add this torrent: "+err.Error())
		return
	}
	rs, err := s.store.Rules(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	page := newAddPage()
	page.Error = errMsg
	page.Torrent = &t
	page.Metainfo = base64.StdEncoding.EncodeToString(st.Metainfo)
	page.Upload = st.Upload
	page.Placement = p
	page.Picker = picker{Rules: rs, Choice: st.Choice, Action: "/add/choose", Target: "add-preview"}
	for _, f := range t.Files {
		page.Size += f.Size
		page.Files = append(page.Files, fileChoice{File: f, Checked: st.Selected == nil || st.Selected[f.Index]})
	}
	if statuses, err := s.client.Torrents(r.Context()); err == nil {
		page.Duplicate = slices.ContainsFunc(statuses, func(st torrent.Status) bool { return st.Hash == t.Hash })
	}
	s.page(w, code, "add", page)
}

func (s *Server) addError(w http.ResponseWriter, msg string) {
	page := newAddPage()
	page.Error = msg
	s.page(w, http.StatusUnprocessableEntity, "add", page)
}
