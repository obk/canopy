package server

import (
	"mime"
	"net/http/httptest"
	"testing"
)

// TestArchiveNames: a version's archive leaves out the v, proj-1 for v1;
// any other ref keeps its name, one starting with v too.
func TestArchiveNames(t *testing.T) {
	s, run, _ := projServer(t)
	run("branch", "visual")
	run("branch", "feature/x")
	for ref, want := range map[string]string{"v1": "proj-1.zip", "visual": "proj-visual.zip", "feature/x": "proj-feature-x.zip"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", "/proj/archive/"+ref+".zip", nil))
		_, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
		if w.Code != 200 || err != nil || params["filename"] != want {
			t.Errorf("%s: %d, %q (%v), want %s", ref, w.Code, params["filename"], err, want)
		}
	}
}
