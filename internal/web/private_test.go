package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/cwood/torrent-org/internal/rules"
	"github.com/cwood/torrent-org/internal/store"
	"github.com/cwood/torrent-org/internal/torrent"
)

// secrets are the strings no response to a locked page may contain.
var secrets = []string{"Hidden Movie", "🙈 Hush", "hidden.example", "hushdir", "hushalias"}

// vault sets up a public album beside a private rule holding a torrent, with
// an alias for the tracker only that torrent uses.
func (h *harness) vault() rules.Rule {
	h.t.Helper()
	h.audioRule()
	secret := rules.Rule{
		Name: "🙈 Hush", Color: "#af52de", Path: "{root}/hushdir", Private: true,
		Match: rules.Group{Mode: rules.ModeAll, Conditions: []rules.Condition{{Field: rules.FieldTracker, Op: rules.OpIs, Value: "hidden.example"}}},
	}
	var err error
	secret.ID, err = h.store.SaveRule(h.t.Context(), secret)
	require.NoError(h.t, err)
	require.NoError(h.t, h.store.SetTracker(h.t.Context(), "hidden.example", "hushalias"))
	require.NoError(h.t, h.store.Assign(h.t.Context(), store.Assignment{Hash: "ccc", RuleID: secret.ID}))

	hidden := movie("ccc", "Hidden Movie", root+"/hushdir")
	hidden.Trackers = []string{"https://hidden.example/a"}
	hidden.Group = secret.Name
	h.client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{album("aaa", "/dl"), hidden}, nil)
	return secret
}

// unlock enters passcode, choosing it when none is set, and returns the key.
func (h *harness) unlock(passcode string) string {
	h.t.Helper()
	res := h.post("/unlock", url.Values{"passcode": {passcode}, "confirm": {passcode}})
	require.Equal(h.t, http.StatusOK, res.Code, res.Body.String())
	var body struct {
		Key string `json:"key"`
	}
	require.NoError(h.t, json.Unmarshal(res.Body.Bytes(), &body))
	require.NotEmpty(h.t, body.Key)
	return body.Key
}

func (h *harness) getKeyed(path, key string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(unlockHeader, key)
	return h.do(req)
}

func requireNoSecrets(t *testing.T, body string) {
	t.Helper()
	for _, s := range secrets {
		require.NotContains(t, body, s)
	}
}

func TestPrivateHiddenUntilUnlocked(t *testing.T) {
	h := newHarness(t)
	secret := h.vault()

	for _, path := range []string{"/", "/dashboard", "/side", "/torrents/aaa", "/rules/new"} {
		res := h.get(path)
		require.Equal(t, http.StatusOK, res.Code, path)
		requireNoSecrets(t, res.Body.String())
	}
	requireNoSecrets(t, firstEvent(t, h, "/events"))
	for _, path := range []string{"/rules/" + itoa(secret.ID), "/rules/new?from=" + itoa(secret.ID), "/torrents/ccc"} {
		require.Equal(t, http.StatusNotFound, h.get(path).Code, path)
	}

	require.Equal(t, http.StatusSeeOther, h.post("/rules/"+itoa(secret.ID)+"/delete", nil).Code)
	require.Len(t, h.rules(), 2, "a locked page can't delete a rule it can't see")
}

func TestPrivateTotalsExcludeHidden(t *testing.T) {
	h := newHarness(t)
	h.vault()
	require.Contains(t, h.get("/side").Body.String(), `<span class="layer-title">Rules</span><span class="count">1</span>`)
	body := h.get("/").Body.String()
	require.Contains(t, body, `<span class="layer-title">Downloading</span>`+"\n"+`<span class="count">1</span>`)
}

func TestPrivateShownOnceUnlocked(t *testing.T) {
	h := newHarness(t)
	secret := h.vault()
	key := h.unlock("hunter2")

	side := h.getKeyed("/side", key).Body.String()
	require.Contains(t, side, "🙈 Hush")
	require.Contains(t, side, `<span class="tag">private</span>`)
	require.Contains(t, side, "hushalias")

	dash := h.getKeyed("/dashboard", key).Body.String()
	require.Contains(t, dash, "Hidden Movie")
	require.Contains(t, dash, `sse-connect="/events?key=`+key+`"`, "the stream keeps the page unlocked")
	require.Contains(t, firstEvent(t, h, "/events?key="+key), "Hidden Movie")

	detail := h.getKeyed("/torrents/ccc", key)
	require.Equal(t, http.StatusOK, detail.Code)
	require.Contains(t, detail.Body.String(), "<title>Torrent · torrent-org</title>", "titles reach the browser's history")
	require.Contains(t, h.getKeyed("/rules/"+itoa(secret.ID), key).Body.String(), `name="private" checked`)

	requireNoSecrets(t, h.get("/side").Body.String())

	req := httptest.NewRequest(http.MethodPost, "/lock", nil)
	req.Header.Set(unlockHeader, key)
	require.Equal(t, http.StatusNoContent, h.do(req).Code)
	requireNoSecrets(t, h.getKeyed("/side", key).Body.String())
}

func TestPrivateCheckboxNeedsUnlock(t *testing.T) {
	h := newHarness(t)
	require.NotContains(t, h.get("/rules/new").Body.String(), `name="private"`)

	require.Equal(t, http.StatusSeeOther, h.post("/rules/new", ruleValues("Sneaky", url.Values{"private": {"on"}})).Code)
	require.False(t, h.rules()[0].Private, "a locked page can't make a rule private")
}

func TestUnlockChoosesThenChecksPasscode(t *testing.T) {
	h := newHarness(t)
	require.Contains(t, h.get("/unlock").Body.String(), "Choose a passcode")

	for _, form := range []url.Values{
		{"passcode": {"abc"}, "confirm": {"abc"}},
		{"passcode": {"abcd"}, "confirm": {"abce"}},
		{"passcode": {strings.Repeat("x", 73)}, "confirm": {strings.Repeat("x", 73)}},
	} {
		require.Equal(t, http.StatusUnprocessableEntity, h.post("/unlock", form).Code)
	}
	h.unlock("hunter2")
	hash, set, err := h.store.Passcode(t.Context())
	require.NoError(t, err)
	require.True(t, set)
	require.NotContains(t, hash, "hunter2")
	require.Contains(t, h.get("/unlock").Body.String(), `<button class="primary">Unlock</button>`)

	h.unlock("hunter2")
	res := h.post("/unlock", url.Values{"passcode": {"wrong"}})
	require.Equal(t, http.StatusUnprocessableEntity, res.Code)
	require.Contains(t, res.Body.String(), "Incorrect passcode.")
	require.Equal(t, http.StatusTooManyRequests, h.post("/unlock", url.Values{"passcode": {"hunter2"}}).Code, "guesses wait, even a right one")
}

func TestUnlocksBackoff(t *testing.T) {
	now := time.Unix(0, 0)
	u := newUnlocks()
	u.now = func() time.Time { return now }

	require.False(t, u.waiting())
	u.failed()
	require.True(t, u.waiting())
	now = now.Add(time.Second)
	require.False(t, u.waiting(), "the first wait is a second")

	u.failed()
	now = now.Add(time.Second)
	require.True(t, u.waiting(), "each failure doubles it")
	for range 20 {
		u.failed()
	}
	now = now.Add(maxBackoff)
	require.False(t, u.waiting(), "capped")

	u.failed()
	u.issue()
	require.False(t, u.waiting(), "a success clears it")
}

func TestClientLinkHiddenOncePasscodeSet(t *testing.T) {
	h := newHarness(t)
	const shown = `aria-label="Transmission web interface"><svg`
	require.Contains(t, h.get("/add").Body.String(), shown)
	h.unlock("hunter2")
	require.Contains(t, h.get("/add").Body.String(), `aria-label="Transmission web interface" hidden><svg`)
}

func TestAddPreviewConcealsPrivateRule(t *testing.T) {
	h := newHarness(t)
	h.audioRule()
	rs := h.rules()
	rs[0].Private = true
	_, err := h.store.SaveRule(t.Context(), rs[0])
	require.NoError(t, err)
	h.client.EXPECT().Torrents(mock.Anything).Return(nil, nil).Once()

	_, body := h.preview(h.do(multipartUpload(t, albumTorrent(t))))
	require.Contains(t, body, "<dt>Destination</dt><dd>Automatic</dd>")
	require.NotContains(t, body, "🎵 Audio")
	require.NotContains(t, body, "/data/torrents/red/audio")
}

func TestPrivateTorrent(t *testing.T) {
	flac := rules.Group{Mode: rules.ModeAll, Conditions: []rules.Condition{{Field: rules.FieldExtension, Op: rules.OpIs, Value: "flac"}}}
	private := rules.Rule{ID: 1, Name: "p", Private: true, Match: flac}
	public := rules.Rule{ID: 2, Name: "q", Match: flac}
	never := rules.Rule{ID: 3, Name: "r"}
	tr := album("aaa", "/dl").Torrent

	tests := []struct {
		name string
		a    store.Assignment
		rs   []rules.Rule
		want bool
	}{
		{name: "best match is private", rs: []rules.Rule{private}, want: true},
		{name: "assigned to a private rule, even by hand", a: store.Assignment{RuleID: 1, Manual: true}, rs: []rules.Rule{private}, want: true},
		{name: "tie with a private rule", rs: []rules.Rule{private, public}, want: true},
		{name: "pinned elsewhere stays public", a: store.Assignment{RuleID: 2, Manual: true}, rs: []rules.Rule{private, public}},
		{name: "only public rules", rs: []rules.Rule{public, never}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, privateTorrent(tr, tt.a, tt.rs))
		})
	}
}
