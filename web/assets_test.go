package web

import (
	"io/fs"
	"testing"
)

func TestStaticAssetsPresent(t *testing.T) {
	fsys := Static()
	for _, name := range []string{"index.html", "login.html", "app.js", "style.css", "charts.js", "topology.js"} {
		data, err := fs.ReadFile(fsys, name)
		if err != nil || len(data) == 0 {
			t.Fatalf("missing or empty static asset %s: err=%v len=%d", name, err, len(data))
		}
	}
}
