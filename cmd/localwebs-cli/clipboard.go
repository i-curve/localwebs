package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

func clipboardCommand(ctx context.Context, image bool) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		if image {
			cmd = exec.CommandContext(ctx, "osascript", "-e", "the clipboard as «class PNGf»")
		} else {
			cmd = exec.CommandContext(ctx, "pbpaste")
		}
	case "windows":
		script := "Add-Type -AssemblyName System.Windows.Forms; [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false); [Console]::Write([System.Windows.Forms.Clipboard]::GetText())"
		if image {
			script = "Add-Type -AssemblyName System.Windows.Forms; $i=[System.Windows.Forms.Clipboard]::GetImage(); if (!$i) { throw 'Clipboard has no image' }; $i.Save([Console]::OpenStandardOutput(), [System.Drawing.Imaging.ImageFormat]::Png)"
		}
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	default:
		mime := "text/plain"
		if image { mime = "image/png" }
		if path, err := exec.LookPath("wl-paste"); err == nil {
			cmd = exec.CommandContext(ctx, path, "--type", mime)
		} else if path, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.CommandContext(ctx, path, "-selection", "clipboard", "-out", "-t", mime)
		} else {
			return nil, errors.New("clipboard requires wl-paste or xclip")
		}
	}
	return cmd, nil
}

type appleHexReader struct {
	reader *bufio.Reader
	done bool
}

func (r *appleHexReader) Read(p []byte) (int, error) {
	if r.done { return 0, io.EOF }
	for i := range p {
		b, err := r.reader.ReadByte()
		if err != nil { return i, io.ErrUnexpectedEOF }
		if b == 0xc2 {
			end, err := r.reader.ReadByte()
			if err != nil || end != 0xbb { return i, errors.New("invalid clipboard image") }
			trailing, err := io.ReadAll(io.LimitReader(r.reader, 1024))
			if err != nil || strings.TrimSpace(string(trailing)) != "" {
				return i, errors.New("invalid clipboard image trailer")
			}
			r.done = true
			if i > 0 { return i, nil }
			return 0, io.EOF
		}
		p[i] = b
	}
	return len(p), nil
}

type clipboardFile struct { *os.File }

func (f *clipboardFile) Close() error {
	return errors.Join(f.File.Close(), os.Remove(f.File.Name()))
}

func clipboardRead(ctx context.Context, image bool) (result io.ReadCloser, size int64, err error) {
	cmd, err := clipboardCommand(ctx, image)
	if err != nil { return nil, 0, err }
	var data bytes.Buffer
	var file *clipboardFile
	var dst io.Writer = &data
	if image {
		f, e := os.CreateTemp("", "localwebs-clipboard-*")
		if e != nil { return nil, 0, e }
		file = &clipboardFile{f}
		dst = f
	}
	defer func() {
		if err != nil && file != nil {
			if e := file.Close(); e != nil { err = fmt.Errorf("%w (cleanup: %v)", err, e) }
		}
	}()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil { return nil, 0, err }
	if err = cmd.Start(); err != nil { return nil, 0, err }
	var reader io.Reader = stdout
	if image && runtime.GOOS == "darwin" {
		buffered := bufio.NewReader(stdout)
		prefix := make([]byte, len("«data PNGf"))
		if _, err = io.ReadFull(buffered, prefix); err == nil && string(prefix) != "«data PNGf" {
			err = errors.New("clipboard has no PNG image")
		}
		reader = hex.NewDecoder(&appleHexReader{reader: buffered})
	}
	limit := textLimit
	if image { limit = transferLimit }
	if err == nil {
		size, err = io.Copy(dst, io.LimitReader(reader, limit+1))
		if err == nil && size > limit { err = errors.New("clipboard content exceeds the transfer limit") }
	}
	if err != nil { _ = cmd.Process.Kill() }
	waitErr := cmd.Wait()
	if err != nil { return nil, 0, fmt.Errorf("read clipboard: %w", err) }
	if waitErr != nil { return nil, 0, fmt.Errorf("read clipboard: %w: %s", waitErr, strings.TrimSpace(stderr.String())) }
	if size == 0 { return nil, 0, errors.New("clipboard is empty or has no requested content") }
	if file == nil { return io.NopCloser(bytes.NewReader(data.Bytes())), size, nil }
	if _, err = file.Seek(0, io.SeekStart); err != nil { return nil, 0, err }
	return file, size, nil
}

func clipboardWrite(text string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin": cmd = exec.Command("pbcopy")
	case "windows":
		cmd = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", "Add-Type -AssemblyName System.Windows.Forms; [Console]::InputEncoding = [System.Text.UTF8Encoding]::new($false); [System.Windows.Forms.Clipboard]::SetText([Console]::In.ReadToEnd())")
	default:
		if path, err := exec.LookPath("wl-copy"); err == nil {
			cmd = exec.Command(path, "--type", "text/plain")
		} else if path, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command(path, "-selection", "clipboard", "-in")
		} else {
			return errors.New("clipboard requires wl-copy or xclip")
		}
	}
	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil { return fmt.Errorf("write clipboard: %w: %s", err, strings.TrimSpace(stderr.String())) }
	return nil
}
