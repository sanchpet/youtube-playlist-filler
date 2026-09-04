package ytapi_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"

	"github.com/sanchpet/youtube-playlist-filler/internal/ytapi"
)

// stub stands in for the API so the request the generated client actually puts on the wire can be
// inspected. Mocking the client would only test the mock; this tests the URL.
type stub struct {
	t        *testing.T
	requests []url.Values
	paths    []string
	bodies   []string
	handler  func(w http.ResponseWriter, r *http.Request)
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.requests = append(s.requests, r.URL.Query())
	s.paths = append(s.paths, r.URL.Path)
	s.bodies = append(s.bodies, string(body))
	w.Header().Set("Content-Type", "application/json")
	s.handler(w, r)
}

func newClient(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*ytapi.Client, *stub) {
	t.Helper()

	s := &stub{t: t, handler: h}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	// The trailing slash matters: the generated client concatenates the base path with the method
	// name rather than resolving a URL reference.
	svc, err := youtube.NewService(t.Context(),
		option.WithEndpoint(srv.URL+"/"),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return ytapi.New(svc, slog.New(slog.NewTextHandler(io.Discard, nil))), s
}

func playlistPage(ids []string, next string) string {
	type item struct {
		ContentDetails struct {
			VideoID string `json:"videoId"`
		} `json:"contentDetails"`
	}
	resp := struct {
		Items         []item `json:"items"`
		NextPageToken string `json:"nextPageToken,omitempty"`
	}{NextPageToken: next}
	for _, id := range ids {
		var it item
		it.ContentDetails.VideoID = id
		resp.Items = append(resp.Items, it)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		panic(err)
	}
	return string(out)
}

func TestPlaylistVideoIDsFollowsEveryPage(t *testing.T) {
	pages := []string{
		playlistPage([]string{"a", "b"}, "PAGE2"),
		playlistPage([]string{"c"}, "PAGE3"),
		playlistPage([]string{"d"}, ""),
	}
	n := 0
	c, s := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, pages[n])
		n++
	})

	ids, calls, err := c.PlaylistVideoIDs(t.Context(), "PLtest", ytapi.AllPages)
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"a", "b", "c", "d"}; !slices.Equal(ids, want) {
		t.Errorf("got %v, want %v", ids, want)
	}
	if calls != 3 {
		t.Errorf("billed %d calls, want 3", calls)
	}
	q := s.requests[0]
	if got := q.Get("part"); got != "contentDetails" {
		t.Errorf("part = %q, want contentDetails", got)
	}
	if got := q.Get("maxResults"); got != "50" {
		t.Errorf("maxResults = %q, want 50: fewer pages cost the same each", got)
	}
	if got := q.Get("playlistId"); got != "PLtest" {
		t.Errorf("playlistId = %q", got)
	}
	if got := s.requests[1].Get("pageToken"); got != "PAGE2" {
		t.Errorf("second page asked for token %q, want PAGE2", got)
	}
}

// The bound is what separates the two schedules. It has to stop at the page it was given even
// though the response is still offering a nextPageToken.
func TestPlaylistVideoIDsStopsAtThePageBound(t *testing.T) {
	c, s := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, playlistPage([]string{"a", "b"}, "MORE"))
	})

	ids, calls, err := c.PlaylistVideoIDs(t.Context(), "UUtest", 1)
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"a", "b"}; !slices.Equal(ids, want) {
		t.Errorf("got %v, want %v", ids, want)
	}
	if calls != 1 || len(s.requests) != 1 {
		t.Errorf("billed %d calls over %d requests, want 1 each", calls, len(s.requests))
	}
}

// A page that fails aborts the read. Returning what it had would be a short answer that reads as a
// complete one, and the ids it missed are exactly the ids that would be added a second time.
func TestPlaylistVideoIDsAbortsOnAFailedPage(t *testing.T) {
	n := 0
	c, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if n == 0 {
			n++
			_, _ = io.WriteString(w, playlistPage([]string{"a"}, "PAGE2"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":404,"errors":[{"reason":"playlistNotFound"}]}}`)
	})

	ids, _, err := c.PlaylistVideoIDs(t.Context(), "PLtest", ytapi.AllPages)
	if err == nil {
		t.Fatal("want an error")
	}
	if ids != nil {
		t.Errorf("returned %v alongside the error", ids)
	}
}

// Fifty ids in one call, comma-joined into a single id parameter. The variadic form of the
// generated setter would send them as repeated parameters instead.
func TestDurationsSendsOneCommaJoinedBatch(t *testing.T) {
	c, s := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"items":[
			{"id":"v1","contentDetails":{"duration":"PT1H2M3S"}},
			{"id":"v2","contentDetails":{"duration":"PT11S"}},
			{"id":"v3","contentDetails":{"duration":"not a duration"}}
		]}`)
	})

	got, calls, err := c.Durations(t.Context(), []string{"v1", "v2", "v3", "gone"})
	if err != nil {
		t.Fatal(err)
	}

	if calls != 1 {
		t.Errorf("billed %d calls, want 1", calls)
	}
	if ids := s.requests[0]["id"]; len(ids) != 1 || ids[0] != "v1,v2,v3,gone" {
		t.Errorf("id parameter = %v, want one comma-joined value", ids)
	}
	if want := time.Hour + 2*time.Minute + 3*time.Second; got["v1"] != want {
		t.Errorf("v1 = %s, want %s", got["v1"], want)
	}
	// An unparseable duration and an id the API did not return are both left out rather than
	// defaulted: a zero duration would sail past a filter that only had a ceiling.
	for _, absent := range []string{"v3", "gone"} {
		if d, ok := got[absent]; ok {
			t.Errorf("%s = %s, want it left out", absent, d)
		}
	}
}

func TestDurationsRefusesAnOversizedBatch(t *testing.T) {
	c, s := newClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an oversized batch must not reach the API")
	})

	var ids []string
	for i := range ytapi.BatchSize + 1 {
		ids = append(ids, fmt.Sprintf("v%d", i))
	}
	if _, _, err := c.Durations(t.Context(), ids); err == nil {
		t.Fatal("want an error")
	}
	if len(s.requests) != 0 {
		t.Errorf("made %d requests", len(s.requests))
	}
}

// Setting snippet.position declares the playlist manually ordered, after which YouTube rejects
// every insert that does not carry one. The body must not mention it.
func TestInsertOmitsPosition(t *testing.T) {
	c, s := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"item1"}`)
	})

	if err := c.Insert(t.Context(), "PLtest", "vid1"); err != nil {
		t.Fatal(err)
	}

	body := s.bodies[0]
	if strings.Contains(body, "position") {
		t.Errorf("insert body carries a position: %s", body)
	}
	for _, want := range []string{`"playlistId":"PLtest"`, `"videoId":"vid1"`, `"kind":"youtube#video"`} {
		if !strings.Contains(body, want) {
			t.Errorf("insert body is missing %s: %s", want, body)
		}
	}
	if got := s.requests[0].Get("part"); got != "snippet" {
		t.Errorf("part = %q, want snippet", got)
	}
}

func TestInsertSurfacesAFullPlaylist(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":403,"errors":[
			{"reason":"playlistContainsMaximumNumberOfVideos"}]}}`)
	})

	err := c.Insert(t.Context(), "PLtest", "vid1")
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ytapi.ErrPlaylistFull) {
		t.Errorf("got %v, want it to wrap ErrPlaylistFull", err)
	}
}
