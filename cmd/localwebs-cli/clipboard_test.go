package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMacClipboardCommands(t *testing.T) {
	if runtime.GOOS != "darwin" { t.Skip("macOS clipboard commands") }
	dir := t.TempDir()
	scripts := map[string]string{
		"pbpaste": "#!/bin/sh\nprintf 'text\\n'\n",
		"pbcopy": "#!/bin/sh\n/bin/cat > \"$CLIPBOARD_TEST_OUTPUT\"\n",
		"osascript": "#!/bin/sh\nprintf '«data PNGf89504e470d0a»'\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil { t.Fatal(err) }
	}
	t.Setenv("PATH", dir)
	output := filepath.Join(dir, "copied")
	t.Setenv("CLIPBOARD_TEST_OUTPUT", output)
	textFile, _, err := clipboardRead(context.Background(), false)
	if err != nil { t.Fatal(err) }
	text, err := io.ReadAll(textFile)
	if err != nil || string(text) != "text\n" { t.Fatalf("read text %q: %v", text, err) }
	if err := textFile.Close(); err != nil { t.Fatal(err) }
	imageFile, _, err := clipboardRead(context.Background(), true)
	if err != nil { t.Fatal(err) }
	imagePath := imageFile.(*clipboardFile).Name()
	image, err := io.ReadAll(imageFile)
	if err != nil || !bytes.Equal(image, []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a}) { t.Fatalf("read image %x: %v", image, err) }
	if err := imageFile.Close(); err != nil { t.Fatal(err) }
	if _, err := os.Stat(imagePath); !os.IsNotExist(err) { t.Fatalf("temporary clipboard image not removed: %v", err) }
	if err := clipboardWrite(string(text)); err != nil { t.Fatal(err) }
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, text) { t.Fatalf("copied text %q: %v", got, err) }
}

func TestClipboardImageRejectsTruncatedAppleScriptOutput(t *testing.T) {
	if runtime.GOOS != "darwin" { t.Skip("macOS clipboard commands") }
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "osascript"), []byte("#!/bin/sh\nprintf '«data PNGf8950'\n"), 0700); err != nil { t.Fatal(err) }
	t.Setenv("PATH", dir)
	t.Setenv("TMPDIR", dir)
	if _, _, err := clipboardRead(context.Background(), true); err == nil { t.Fatal("accepted truncated image") }
	files, err := os.ReadDir(dir)
	if err != nil { t.Fatal(err) }
	if len(files) != 1 { t.Fatalf("temporary file was not removed: %v", files) }
}

func TestWaylandClipboardCommands(t *testing.T) {
	if runtime.GOOS != "linux" { t.Skip("Wayland clipboard commands") }
	dir := t.TempDir()
	scripts := map[string]string{
		"wl-paste": "#!/bin/sh\nif [ \"$2\" = image/png ]; then printf '\\211PNG\\r\\n'; else printf 'text\\n'; fi\n",
		"wl-copy": "#!/bin/sh\n/bin/cat > \"$CLIPBOARD_TEST_OUTPUT\"\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil { t.Fatal(err) }
	}
	t.Setenv("PATH", dir)
	output := filepath.Join(dir, "copied")
	t.Setenv("CLIPBOARD_TEST_OUTPUT", output)
	textFile, _, err := clipboardRead(context.Background(), false)
	if err != nil { t.Fatal(err) }
	text, err := io.ReadAll(textFile)
	if err != nil || string(text) != "text\n" { t.Fatalf("read text %q: %v", text, err) }
	if err := textFile.Close(); err != nil { t.Fatal(err) }
	imageFile, _, err := clipboardRead(context.Background(), true)
	if err != nil { t.Fatal(err) }
	image, err := io.ReadAll(imageFile)
	if err != nil || !bytes.Equal(image, []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a}) { t.Fatalf("read image %x: %v", image, err) }
	if err := imageFile.Close(); err != nil { t.Fatal(err) }
	if err := clipboardWrite(string(text)); err != nil { t.Fatal(err) }
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, text) { t.Fatalf("copied text %q: %v", got, err) }
}
