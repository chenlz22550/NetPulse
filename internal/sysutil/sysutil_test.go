package sysutil

import "testing"

func TestAppBundle(t *testing.T) {
	cases := map[string]string{
		"/Applications/netPing.app/Contents/MacOS/netPing": "/Applications/netPing.app",
		"/Users/a/dist/netPing":                            "",
		"/x/Contents/MacOS/netPing":                        "",
	}
	for exe, want := range cases {
		if got := appBundle(exe); got != want {
			t.Errorf("appBundle(%q)=%q want %q", exe, got, want)
		}
	}
}
