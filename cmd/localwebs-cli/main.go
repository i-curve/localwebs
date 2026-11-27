package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	ossignal "os/signal"
	"strings"
	"time"
)

const usage = `localwebs-cli [interactive|receive|send|peers] [options] [paths...]

Options:
  -server URL       Server address (default http://localhost:8080)
  -code CODE        Six-digit room code (or LOCALWEBS_ROOM)
  -dir DIR          Incoming files directory (default ./downloads)
  -to ID            Recipient ID or unambiguous prefix (send only)
  -text TEXT        Send text (send only)
  -stdin            Read text from standard input (send only)
  -clipboard        Send system clipboard text (send only)
  -image            Read a PNG image instead of text with -clipboard

Run without a command for interactive mode.
Run 'localwebs-cli completion bash|zsh|fish|powershell' for shell completion.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "completion" {
		if len(args) != 2 { return errors.New("usage: localwebs-cli completion bash|zsh|fish|powershell") }
		return completion(args[1])
	}
	command := "interactive"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	if command != "interactive" && command != "send" && command != "receive" && command != "peers" {
		return fmt.Errorf("unknown command %q\n%s", command, usage)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	server := fs.String("server", "http://localhost:8080", "")
	code := fs.String("code", os.Getenv("LOCALWEBS_ROOM"), "")
	dir := fs.String("dir", "downloads", "")
	to := fs.String("to", "", "")
	text := fs.String("text", "", "")
	stdin := fs.Bool("stdin", false, "")
	clipboard := fs.Bool("clipboard", false, "")
	image := fs.Bool("image", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) { return nil }
		return err
	}
	if command != "send" && (*to != "" || *text != "" || *stdin || *clipboard || *image || fs.NArg() != 0) {
		return errors.New("sending options and paths are only valid with send")
	}
	if command == "send" {
		sources := 0
		if *text != "" { sources++ }
		if *stdin { sources++ }
		if *clipboard { sources++ }
		if fs.NArg() > 0 { sources++ }
		if sources != 1 || *to == "" || (*image && !*clipboard) {
			return errors.New("send requires -to and exactly one of paths, -text, -stdin or -clipboard")
		}
	}
	input := bufio.NewReader(os.Stdin)
	if *code == "" && command == "interactive" {
		fmt.Fprint(os.Stderr, "Room code: ")
		line, err := input.ReadString('\n')
		if err != nil { return fmt.Errorf("read room code: %w", err) }
		*code = strings.TrimSpace(line)
	}
	ctx, cancel := ossignal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	c, err := connect(ctx, *server, *code)
	if err != nil { return err }
	defer c.close()
	fmt.Fprintf(os.Stderr, "Joined room; device %s; expires %s.\n", c.id, c.expires.Format(time.RFC3339))
	switch command {
	case "peers":
		printPeers(c)
		return nil
	case "send":
		peer, err := c.peer(*to)
		if err != nil { return err }
		var data []byte
		switch {
		case *stdin:
			data, err = io.ReadAll(io.LimitReader(input, textLimit+1))
		case *clipboard:
			return sendClipboard(ctx, c, peer, *image)
		case *text != "":
			data = []byte(*text)
		default:
			var name string
			var size *int64
			var src io.ReadCloser
			name, size, src, err = source(fs.Args())
			if err != nil { return err }
			defer src.Close()
			return c.send(ctx, peer, name, "", size, src)
		}
		if err != nil { return err }
		return sendText(ctx, c, peer, data)
	case "receive":
		return receiveLoop(ctx, c, *dir, nil)
	default:
		return receiveLoop(ctx, c, *dir, input)
	}
}

func sendText(ctx context.Context, c *client, peer string, data []byte) error {
	if len(data) == 0 { return errors.New("text is empty") }
	size := int64(len(data))
	if size > textLimit { return errors.New("text exceeds 1 MiB") }
	return c.send(ctx, peer, "clipboard.txt", "text", &size, bytes.NewReader(data))
}

func sendClipboard(ctx context.Context, c *client, peer string, image bool) (err error) {
	src, size, err := clipboardRead(ctx, image)
	if err != nil { return err }
	defer func() { err = errors.Join(err, src.Close()) }()
	name, content := "clipboard.txt", "text"
	if image { name, content = "clipboard.png", "" }
	return c.send(ctx, peer, name, content, &size, src)
}

func printPeers(c *client) {
	fmt.Fprintf(os.Stdout, "%s (this device)\n", c.id)
	for _, p := range c.peers { fmt.Fprintln(os.Stdout, p) }
}

func receiveLoop(ctx context.Context, c *client, dir string, input *bufio.Reader) error {
	var commands <-chan string
	if input != nil {
		lines := make(chan string)
		commands = lines
		go func() {
			defer close(lines)
			for {
				line, err := input.ReadString('\n')
				if len(line) > 0 {
					select { case lines <- strings.TrimSpace(line): case <-ctx.Done(): return }
				}
				if err != nil { return }
			}
		}()
		fmt.Fprintln(os.Stderr, "Commands: peers, send ID PATH, clipboard ID [image], copy, quit")
		printPeers(c)
	}
	timer := time.NewTimer(time.Until(c.expires))
	defer timer.Stop()
	var lastText string
	for {
		select {
		case <-ctx.Done(): return nil
		case <-timer.C: return errors.New("room session expired")
		case line, ok := <-commands:
			if !ok { return nil }
			parts := strings.SplitN(line, " ", 3)
			switch parts[0] {
			case "": continue
			case "quit", "exit": return nil
			case "peers": printPeers(c)
			case "copy":
				if lastText == "" { fmt.Fprintln(os.Stderr, "No received text to copy."); continue }
				if err := clipboardWrite(lastText); err != nil { fmt.Fprintln(os.Stderr, "Error:", err) } else { fmt.Fprintln(os.Stderr, "Copied.") }
			case "send", "clipboard":
				if len(parts) < 2 { fmt.Fprintln(os.Stderr, "Specify a recipient ID."); continue }
				peer, err := c.peer(parts[1])
				if err == nil && parts[0] == "send" {
					if len(parts) < 3 { err = errors.New("specify a file or folder path") } else {
						name, size, src, e := source([]string{parts[2]})
						if e != nil { err = e } else {
							err = c.send(ctx, peer, name, "", size, src)
							if e := src.Close(); err == nil { err = e }
						}
					}
				} else if err == nil {
					image := len(parts) == 3 && parts[2] == "image"
					if len(parts) == 3 && !image { err = errors.New("expected image") } else {
						err = sendClipboard(ctx, c, peer, image)
					}
				}
				if err != nil {
					if c.failed != nil { return err }
					fmt.Fprintln(os.Stderr, "Error:", err)
				}
			default: fmt.Fprintln(os.Stderr, "Unknown command.")
			}
		case e := <-c.events:
			if e.err != nil { return fmt.Errorf("connection lost: %w", e.err) }
			m, s, err := c.handleMessage(e)
			if err != nil { return err }
			if m.Type == "peers" && input != nil { printPeers(c) }
			if m.Type == "signal" && s.Kind == "offer" {
				text, err := c.receive(ctx, m.From, s, dir)
				if err != nil {
					if c.failed != nil { return err }
					fmt.Fprintln(os.Stderr, "Receive error:", err)
				} else if s.Content == "text" { lastText = text }
			}
		}
	}
}
