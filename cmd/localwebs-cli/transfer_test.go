package main

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveSource(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "folder")
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0700); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello"), 0600); err != nil { t.Fatal(err) }
	name, size, source, err := source([]string{dir})
	if err != nil { t.Fatal(err) }
	defer source.Close()
	if name != "folder.zip" || size != nil { t.Fatalf("unexpected archive offer: %q, %v", name, size) }
	data, err := io.ReadAll(source)
	if err != nil { t.Fatal(err) }
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil { t.Fatal(err) }
	if len(z.File) != 1 || z.File[0].Name != "folder/test.txt" { t.Fatalf("unexpected archive entries: %+v", z.File) }
	r, err := z.File[0].Open()
	if err != nil { t.Fatal(err) }
	defer r.Close()
	content, err := io.ReadAll(r)
	if err != nil || string(content) != "hello" { t.Fatalf("archive content %q: %v", content, err) }
}

func TestUnsafeNamesAndAmbiguousPeer(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../test", `..\test`, "bad\nname"} {
		if safeName(name) { t.Fatalf("accepted unsafe name %q", name) }
	}
	c := client{peers: []string{"12345678000000000000000000000000", "12345678111111111111111111111111"}}
	if _, err := c.peer("12345678"); err == nil { t.Fatal("accepted ambiguous device ID") }
	if got, err := c.peer("123456780"); err != nil || got != c.peers[0] { t.Fatalf("unique peer: %q, %v", got, err) }
}

func TestRejectDuplicateArchiveNames(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, dir := range []string{a, b} {
		if err := os.WriteFile(filepath.Join(dir, "same"), []byte("x"), 0600); err != nil { t.Fatal(err) }
	}
	if _, _, r, err := source([]string{filepath.Join(a, "same"), filepath.Join(b, "same")}); err == nil {
		r.Close()
		t.Fatal("accepted duplicate archive names")
	}
}
