package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestGuideNamesThisBinary(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	guide()
	os.Stdout = stdout
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	bin, _ := os.Executable()
	text := string(out)
	for _, want := range []string{bin + " send <address> --as <agent>", "WHEN BAF RINGS", "--desktop", "baf " + versionString()} {
		if !strings.Contains(text, want) {
			t.Errorf("guide is missing %q", want)
		}
	}
	if strings.Contains(text, "{baf}") || strings.Contains(text, "{version}") {
		t.Error("guide has an unfilled placeholder")
	}
}
