package camera

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The worked example from RFC 2617 section 3.5.
func TestDigestRFC2617Example(t *testing.T) {
	ch, err := parseDigest(`realm="testrealm@host.com", qop="auth,auth-int", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", opaque="5ccc069c403ebaf9f0171e9517f40e41"`)
	if err != nil {
		t.Fatal(err)
	}
	ch.cnonce = func() string { return "0a4f113b" }
	got := ch.authorize("GET", "/dir/index.html", "Mufasa", "Circle Of Life")
	for _, want := range []string{
		`response="6629fae49393a05397450978507c4ef1"`,
		`nc=00000001`,
		`qop=auth`,
		`opaque="5ccc069c403ebaf9f0171e9517f40e41"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("authorization %s\nmissing %s", got, want)
		}
	}
	if next := ch.authorize("GET", "/dir/index.html", "Mufasa", "Circle Of Life"); !strings.Contains(next, "nc=00000002") {
		t.Errorf("second use should count up: %s", next)
	}
}

var jpeg = []byte{0xFF, 0xD8, 0xFF, 0xE0, 'f', 'a', 'k', 'e'}

func TestSnapshotAnonymous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(jpeg)
	}))
	defer srv.Close()

	got, err := (&Camera{URL: srv.URL + "/axis-cgi/jpg/image.cgi"}).Snapshot(context.Background())
	if err != nil || string(got) != string(jpeg) {
		t.Fatalf("Snapshot = %v, %v", got, err)
	}
}

func TestSnapshotDigestThenReuse(t *testing.T) {
	challenges := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Digest ") || !strings.Contains(auth, `username="viewer"`) {
			challenges++
			w.Header().Set("WWW-Authenticate", `Digest realm="AXIS_ACCC8E000000", nonce="abc", qop="auth"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write(jpeg)
	}))
	defer srv.Close()

	cam := &Camera{URL: srv.URL + "/snap", Username: "viewer", Password: "secret"}
	for range 3 {
		if _, err := cam.Snapshot(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if challenges != 1 {
		t.Fatalf("camera challenged %d times; the nonce should be reused", challenges)
	}
}

func TestSnapshotRejectsNonJPEG(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>login</html>"))
	}))
	defer srv.Close()

	if _, err := (&Camera{URL: srv.URL}).Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "JPEG") {
		t.Fatalf("expected a not-a-JPEG error, got %v", err)
	}
}

func TestParseParams(t *testing.T) {
	got := parseParams(`realm="a, b", nonce=xyz, qop="auth,auth-int", algorithm=MD5`)
	want := map[string]string{"realm": "a, b", "nonce": "xyz", "qop": "auth,auth-int", "algorithm": "MD5"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// Basic credentials go only to a camera that asked for them, and then on every
// later snapshot without another round trip.
func TestSnapshotBasicOnlyWhenAsked(t *testing.T) {
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "pw" {
			w.Header().Set("WWW-Authenticate", `Basic realm="cam"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write(jpeg)
	}))
	defer srv.Close()
	c := &Camera{URL: srv.URL, Username: "admin", Password: "pw"}
	for range 2 {
		if _, err := c.Snapshot(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(auths) != 3 || auths[0] != "" || auths[1] == "" || auths[2] == "" {
		t.Fatalf("authorization headers sent: %q, want none, then basic twice", auths)
	}
}

// A password in the query string must not reach the error text.
func TestSnapshotErrorHidesQuery(t *testing.T) {
	c := &Camera{URL: "http://127.0.0.1:1/cgi-bin/snap?user=admin&password=s3cret"}
	_, err := c.Snapshot(context.Background())
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "127.0.0.1:1/cgi-bin/snap") {
		t.Fatalf("error %v", err)
	}
}
