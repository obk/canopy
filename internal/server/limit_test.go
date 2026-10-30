package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requestFrom(addr string, header ...string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = addr
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Add(header[i], header[i+1])
	}
	return r
}

func TestClientOf(t *testing.T) {
	for _, c := range []struct {
		r    *http.Request
		want string
	}{
		{requestFrom("203.0.113.7:5000"), "203.0.113.7"},
		// Only a proxy nearby is believed.
		{requestFrom("203.0.113.7:5000", "X-Real-IP", "198.51.100.1"), "203.0.113.7"},
		{requestFrom("203.0.113.7:5000", "X-Forwarded-For", "198.51.100.1"), "203.0.113.7"},
		// Caddy, with Anubis and without.
		{requestFrom("127.0.0.1:5000", "X-Real-IP", "198.51.100.1", "X-Forwarded-For", "198.51.100.1, 127.0.0.1"), "198.51.100.1"},
		{requestFrom("127.0.0.1:5000", "X-Forwarded-For", "198.51.100.1"), "198.51.100.1"},
		// What the visitor wrote themselves comes before what the proxy added.
		{requestFrom("[::1]:5000", "X-Forwarded-For", "192.0.2.9, 198.51.100.1"), "198.51.100.1"},
		{requestFrom("10.0.0.2:5000", "X-Forwarded-For", "192.0.2.9", "X-Forwarded-For", "198.51.100.1, 10.0.0.3"), "198.51.100.1"},
		// A proxy that doesn't say who the visitor is.
		{requestFrom("127.0.0.1:5000"), ""},
		{requestFrom("192.168.1.4:5000", "X-Forwarded-For", "10.1.1.1"), ""},
		{requestFrom("127.0.0.1:5000", "X-Forwarded-For", "nonsense"), ""},
		// IPv6 by /64, mapped IPv4 as IPv4.
		{requestFrom("[2001:db8:1:2:3:4:5:6]:5000"), "2001:db8:1:2::/64"},
		{requestFrom("[::1]:5000", "X-Real-IP", "2001:db8:1:2::9"), "2001:db8:1:2::/64"},
		{requestFrom("[::ffff:203.0.113.7]:5000"), "203.0.113.7"},
	} {
		if got := clientOf(c.r); got != c.want {
			t.Errorf("clientOf(%s %v) = %q, want %q", c.r.RemoteAddr, c.r.Header, got, c.want)
		}
	}
}

// impatient is r, giving up on waiting for a slot after a moment.
func impatient(t *testing.T, r *http.Request) *http.Request {
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Millisecond)
	t.Cleanup(cancel)
	return r.WithContext(ctx)
}

func TestLimiterSharesSlotsBetweenVisitors(t *testing.T) {
	l := newLimiter(8) // 2 per visitor
	a, b := requestFrom("203.0.113.1:1"), requestFrom("203.0.113.2:1")
	var held []func()
	for range 2 {
		release, ok := l.acquire(a)
		if !ok {
			t.Fatal("a visitor didn't get its first two slots")
		}
		held = append(held, release)
	}
	if _, ok := l.acquire(impatient(t, a)); ok {
		t.Error("a visitor got a third slot of eight")
	}
	release, ok := l.acquire(impatient(t, b))
	if !ok {
		t.Fatal("another visitor got no slot while one held two")
	}
	release()
	held[0]()
	if release, ok := l.acquire(impatient(t, a)); !ok {
		t.Error("a freed slot didn't go back to its visitor")
	} else {
		release()
	}
	held[1]()
	if len(l.clients) != 0 {
		t.Errorf("visitors left behind with nothing running: %v", l.clients)
	}
}

func TestLimiterPoolBoundsEveryone(t *testing.T) {
	l := newLimiter(4) // 2 per visitor
	for _, addr := range []string{"203.0.113.1:1", "203.0.113.2:1"} {
		for range 2 {
			if _, ok := l.acquire(requestFrom(addr)); !ok {
				t.Fatalf("%s: no slot in a pool with room", addr)
			}
		}
	}
	// Full: neither a third visitor nor a proxy without a name gets in.
	for _, addr := range []string{"203.0.113.3:1", "127.0.0.1:1"} {
		if _, ok := l.acquire(impatient(t, requestFrom(addr))); ok {
			t.Errorf("%s got a slot in a full pool", addr)
		}
	}
	if n := l.clients["203.0.113.3"]; n != nil {
		t.Errorf("a visitor who gave up is still counted: %+v", n)
	}
}

// deadlineWriter records a response, and whether each write had a deadline
// set just before it, as progressWriter sets one.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	armed     bool // a deadline was set since the last write
	unguarded int  // writes without one
	biggest   int
}

func (d *deadlineWriter) SetWriteDeadline(time.Time) error {
	d.armed = true
	return nil
}

func (d *deadlineWriter) Write(b []byte) (int, error) {
	if !d.armed {
		d.unguarded++
	}
	d.armed = false
	d.biggest = max(d.biggest, len(b))
	return d.ResponseRecorder.Write(b)
}

// TestSlowReadersAreCutOff: what a repository sends, pages as well as
// downloads, goes out in pieces of at most stallChunk with a deadline before
// each, so a visitor who stops reading loses the request instead of keeping
// its slot.
func TestSlowReadersAreCutOff(t *testing.T) {
	s, run, work := projServer(t)
	var big strings.Builder
	for i := range 20000 {
		fmt.Fprintf(&big, "line %d\n", i)
	}
	os.WriteFile(filepath.Join(work, "big.txt"), []byte(big.String()), 0o644)
	run("add", "big.txt")
	run("commit", "-qm", "big")
	for _, url := range []string{"/proj/tree/main/item/big.txt", "/proj/blob/main/item/big.txt", "/proj/commit/main.patch"} {
		w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		s.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
		if w.Code != 200 || w.Body.Len() <= stallChunk || w.unguarded > 0 || w.biggest > stallChunk {
			t.Errorf("%s: %d, %d bytes, %d writes without a deadline, the biggest %d bytes",
				url, w.Code, w.Body.Len(), w.unguarded, w.biggest)
		}
	}
}
