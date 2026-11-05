package tailnet

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DavidGuooo/baf/internal/runfolder"
)

func TestDirsGrants(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "Documents", "GitHub")
	outside := filepath.Join(home, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(home, "alias")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		dirs       []string
		allowed    bool
	}{
		{"equal", root, []string{root}, true},
		{"inside missing", root + "/new/child", []string{root}, true},
		{"outside", outside, []string{root}, false},
		{"component", root + "-other", []string{root}, false},
		{"dotdot", root + "/../../outside", []string{root}, false},
		{"symlink escape", root + "/escape/new", []string{root}, false},
		{"clean before symlinks", root + "/escape/../new", []string{root}, true},
		{"star", outside, []string{"*"}, true},
		{"no dirs", root, nil, false},
		{"relative", "Documents/GitHub/repo", []string{"Documents/GitHub"}, true},
		{"tilde", "~/Documents/GitHub/repo", []string{"~/Documents/GitHub"}, true},
		{"home", "~", []string{"~"}, true},
		{"symlink grant", root + "/repo", []string{"alias"}, true},
		{"bad grant", root, []string{""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := runfolder.Resolve(tc.path, home)
			if err != nil {
				t.Fatal(err)
			}
			peer := Peer{Grants: []Grant{{To: []string{"claude"}}, {Dirs: tc.dirs}}}
			if got := peer.MayRunIn(dir, home); got != tc.allowed {
				t.Fatalf("MayRunIn(%s)=%v", dir, got)
			}
		})
	}
	if err := os.Symlink(filepath.Join(home, "missing"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if _, err := runfolder.Resolve(root+"/dangling/new", home); err == nil {
		t.Fatal("accepted dangling symlink")
	}
}
