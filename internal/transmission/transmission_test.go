package transmission

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/hekmon/transmissionrpc/v3"
	"github.com/stretchr/testify/require"

	"github.com/cwood/torrent-org/internal/config"
	"github.com/cwood/torrent-org/internal/torrent"
)

type request struct {
	Method    string          `json:"method"`
	Arguments json.RawMessage `json:"arguments"`
	Tag       int             `json:"tag"`
}

// fakeRPC is a Transmission RPC endpoint that performs the session-id
// handshake, records each request and answers with the canned arguments for
// its method. Methods in fail answer with an error result.
type fakeRPC struct {
	t         *testing.T
	answers   map[string]string
	fail      map[string]bool
	requests  []request
	basicAuth [2]string
}

func newFake(t *testing.T, answers map[string]string) (*fakeRPC, string) {
	t.Helper()
	f := &fakeRPC{t: t, answers: map[string]string{
		"session-get": `{"rpc-version":19,"rpc-version-minimum":14}`,
	}}
	maps.Copy(f.answers, answers)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL + "/transmission/rpc"
}

func dial(t *testing.T, answers map[string]string) (*Client, *fakeRPC) {
	t.Helper()
	f, u := newFake(t, answers)
	c, err := New(config.Transmission{URL: u})
	require.NoError(t, err)
	require.NoError(t, c.Check(t.Context()))
	return c, f
}

func (f *fakeRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const sid = "session-1"
	if r.Header.Get("X-Transmission-Session-Id") != sid {
		w.Header().Set("X-Transmission-Session-Id", sid)
		w.WriteHeader(http.StatusConflict)
		return
	}
	user, pass, _ := r.BasicAuth()
	f.basicAuth = [2]string{user, pass}

	var req request
	require.NoError(f.t, json.NewDecoder(r.Body).Decode(&req))
	f.requests = append(f.requests, req)
	args, ok := f.answers[req.Method]
	if !ok {
		args = "{}"
	}
	result := "success"
	if f.fail[req.Method] {
		result = "no such file or directory"
	}
	_, err := w.Write([]byte(`{"result":"` + result + `","tag":` + strconv.Itoa(req.Tag) + `,"arguments":` + args + `}`))
	require.NoError(f.t, err)
}

// last returns the arguments of the most recent request for method.
func (f *fakeRPC) last(method string) map[string]any {
	f.t.Helper()
	for i := len(f.requests) - 1; i >= 0; i-- {
		if f.requests[i].Method == method {
			var args map[string]any
			require.NoError(f.t, json.Unmarshal(f.requests[i].Arguments, &args))
			return args
		}
	}
	f.t.Fatalf("no %s request", method)
	return nil
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name    string
		session string
		wantErr string
	}{
		{name: "4.1", session: `{"rpc-version":19,"rpc-version-minimum":14}`},
		{name: "too old for labels", session: `{"rpc-version":15,"rpc-version-minimum":14}`, wantErr: "lacks labels"},
		{name: "server too new", session: `{"rpc-version":30,"rpc-version-minimum":20}`, wantErr: "server requires 20"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, u := newFake(t, map[string]string{"session-get": tt.session})
			c, err := New(config.Transmission{URL: u})
			require.NoError(t, err)
			err = c.Check(t.Context())
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrIncompatible)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestCheckUnreachable(t *testing.T) {
	c, err := New(config.Transmission{URL: "http://127.0.0.1:1/transmission/rpc"})
	require.NoError(t, err)
	err = c.Check(t.Context())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrIncompatible)
}

func TestBasicAuth(t *testing.T) {
	f, u := newFake(t, nil)
	c, err := New(config.Transmission{URL: u, Username: "me", Password: "secret"})
	require.NoError(t, err)
	require.NoError(t, c.Check(t.Context()))
	require.Equal(t, [2]string{"me", "secret"}, f.basicAuth)
}

func TestTorrents(t *testing.T) {
	c, f := dial(t, map[string]string{"torrent-get": `{"torrents":[{
		"hashString": "abc123",
		"name": "Album",
		"downloadDir": "/data/torrents/red/audio",
		"labels": ["🎵 Audio", "mine"],
		"files": [
			{"name": "Album/01.flac", "length": 300, "bytesCompleted": 150},
			{"name": "Album/cover.jpg", "length": 2, "bytesCompleted": 2}
		],
		"trackers": [{"announce": "https://flacsfor.me/abc/announce", "id": 0, "tier": 0}],
		"percentDone": 0.5,
		"rateDownload": 1024,
		"rateUpload": 10,
		"eta": 150,
		"status": 4,
		"errorString": ""
	}, {
		"hashString": "def456",
		"name": "Movie",
		"downloadDir": "/home/me/Downloads",
		"labels": [],
		"percentDone": 1,
		"eta": 3600,
		"status": 5
	}]}`})

	got, err := c.Torrents(t.Context())
	require.NoError(t, err)
	require.Equal(t, []torrent.Status{
		{
			Torrent: torrent.Torrent{
				Hash:     "abc123",
				Name:     "Album",
				Trackers: []string{"https://flacsfor.me/abc/announce"},
				Files: []torrent.File{
					{Path: "Album/01.flac", Size: 300, Index: 0},
					{Path: "Album/cover.jpg", Size: 2, Index: 1},
				},
			},
			Dir:      "/data/torrents/red/audio",
			Group:    "🎵 Audio",
			Progress: 0.5,
			RateDown: 1024,
			RateUp:   10,
			ETA:      150 * time.Second,
			State:    torrent.StateDownloading,
		},
		{
			Torrent:  torrent.Torrent{Hash: "def456", Name: "Movie"},
			Dir:      "/home/me/Downloads",
			Progress: 1,
			State:    torrent.StateQueued,
		},
	}, got)
	require.ElementsMatch(t, fields, f.last("torrent-get")["fields"])
}

func TestState(t *testing.T) {
	want := map[int64]torrent.State{
		0: torrent.StateStopped,
		1: torrent.StateChecking,
		2: torrent.StateChecking,
		3: torrent.StateQueued,
		4: torrent.StateDownloading,
		5: torrent.StateQueued,
		6: torrent.StateSeeding,
	}
	for code, s := range want {
		require.Equal(t, s, state(transmissionrpc.TorrentStatus(code)), "status %d", code)
	}
}

func TestAdd(t *testing.T) {
	tests := []struct {
		name       string
		answer     string
		group      string
		skip       []int
		wantLabels []any
		wantSkip   []any
	}{
		{
			name:       "added with group and skipped files",
			answer:     `{"torrent-added":{"hashString":"abc123","id":1,"name":"x"}}`,
			group:      "🎵 Audio",
			skip:       []int{1, 3},
			wantLabels: []any{"🎵 Audio"},
			wantSkip:   []any{float64(1), float64(3)},
		},
		{
			name:       "duplicate without group",
			answer:     `{"torrent-duplicate":{"hashString":"abc123","id":1,"name":"x"}}`,
			wantLabels: []any{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, f := dial(t, map[string]string{"torrent-add": tt.answer})
			hash, err := c.Add(t.Context(), torrent.Addition{
				Metainfo: []byte("d4:infod4:name1:xee"),
				Dir:      "/data/torrents/red/audio",
				Group:    tt.group,
				Skip:     tt.skip,
			})
			require.NoError(t, err)
			require.Equal(t, "abc123", hash)
			want := map[string]any{
				"metainfo":     base64.StdEncoding.EncodeToString([]byte("d4:infod4:name1:xee")),
				"download-dir": "/data/torrents/red/audio",
				"labels":       tt.wantLabels,
			}
			if tt.wantSkip != nil {
				want["files-unwanted"] = tt.wantSkip
			}
			require.Equal(t, want, f.last("torrent-add"))
		})
	}
}

const found = `{"torrents":[{"id":7}]}`

// methods returns the RPC methods called after the session handshake.
func (f *fakeRPC) methods() []string {
	var out []string
	for _, r := range f.requests {
		if r.Method != "session-get" {
			out = append(out, r.Method)
		}
	}
	return out
}

func TestMove(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		fail    string
		want    []string
		wantErr string
	}{
		{
			name: "active torrent is stopped and restarted", status: 4,
			want: []string{"torrent-get", "torrent-stop", "torrent-set-location", "torrent-verify", "torrent-start"},
		},
		{
			name: "queued torrent counts as active", status: 3,
			want: []string{"torrent-get", "torrent-stop", "torrent-set-location", "torrent-verify", "torrent-start"},
		},
		{
			name: "stopped torrent is verified but stays stopped", status: 0,
			want: []string{"torrent-get", "torrent-set-location", "torrent-verify"},
		},
		{
			name: "failed move skips verify and still restarts", status: 6, fail: "torrent-set-location",
			want:    []string{"torrent-get", "torrent-stop", "torrent-set-location", "torrent-start"},
			wantErr: "no such file or directory",
		},
		{
			name: "failed verify still restarts", status: 6, fail: "torrent-verify",
			want:    []string{"torrent-get", "torrent-stop", "torrent-set-location", "torrent-verify", "torrent-start"},
			wantErr: "no such file or directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, f := dial(t, map[string]string{"torrent-get": `{"torrents":[{"id":7,"status":` + strconv.Itoa(tt.status) + `}]}`})
			f.fail = map[string]bool{tt.fail: true}

			err := c.Move(t.Context(), "abc123", "/data/torrents/red/audio")
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
			require.Equal(t, tt.want, f.methods())
			require.ElementsMatch(t, []any{"id", "status"}, f.last("torrent-get")["fields"])
			require.Equal(t, []any{"abc123"}, f.last("torrent-get")["ids"])
			require.Equal(t, map[string]any{
				"ids":      []any{float64(7)},
				"location": "/data/torrents/red/audio",
				"move":     true,
			}, f.last("torrent-set-location"))
		})
	}
}

func TestSetGroup(t *testing.T) {
	tests := []struct {
		name  string
		group string
		want  []any
	}{
		{name: "set", group: "🎵 Audio", want: []any{"🎵 Audio"}},
		{name: "empty clears", group: "", want: []any{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, f := dial(t, map[string]string{"torrent-get": found})
			require.NoError(t, c.SetGroup(t.Context(), "abc123", tt.group))
			require.Equal(t, map[string]any{"ids": []any{float64(7)}, "labels": tt.want}, f.last("torrent-set"))
		})
	}
}

func TestMutateUnknownHash(t *testing.T) {
	c, _ := dial(t, map[string]string{"torrent-get": `{"torrents":[]}`})
	require.ErrorIs(t, c.Move(t.Context(), "nope", "/x"), torrent.ErrNotFound)
	require.ErrorIs(t, c.SetGroup(t.Context(), "nope", "x"), torrent.ErrNotFound)
}

// TestLive runs read-only calls against a real daemon when
// TORRENT_ORG_TEST_RPC_URL is set, e.g. http://localhost:9091/transmission/rpc.
func TestLive(t *testing.T) {
	rpcURL := os.Getenv("TORRENT_ORG_TEST_RPC_URL")
	if rpcURL == "" {
		t.Skip("TORRENT_ORG_TEST_RPC_URL not set")
	}
	c, err := New(config.Transmission{
		URL:      rpcURL,
		Username: os.Getenv("TORRENT_ORG_TEST_RPC_USERNAME"),
		Password: os.Getenv(config.PasswordEnv),
	})
	require.NoError(t, err)
	require.NoError(t, c.Check(t.Context()))

	got, err := c.Torrents(t.Context())
	require.NoError(t, err)
	for _, s := range got {
		require.Len(t, s.Hash, 40)
		require.NotEmpty(t, s.Name)
		require.NotEmpty(t, s.Dir)
		_, err := c.lookup(t.Context(), s.Hash, "id")
		require.NoError(t, err)
	}
	t.Logf("listed %d torrents", len(got))
}
