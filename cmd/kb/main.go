package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ── ANSI colors ───────────────────────────────────────────────────────────────

const (
	bold   = "\033[1m"
	dim    = "\033[2m"
	cyan   = "\033[36m"
	green  = "\033[32m"
	yellow = "\033[33m"
	pink   = "\033[38;5;205m"
	reset  = "\033[0m"
)

func b(s string) string  { return bold + s + reset }
func c(s string) string  { return cyan + s + reset }
func g(s string) string  { return green + s + reset }
func y(s string) string  { return yellow + s + reset }
func p(s string) string  { return pink + s + reset }
func d(s string) string  { return dim + s + reset }

// ── Socket ────────────────────────────────────────────────────────────────────

func sockPath() string {
	return fmt.Sprintf("/run/user/%d/kernclip-bus.sock", os.Getuid())
}

func connect() (net.Conn, error) {
	c, err := net.Dial("unix", sockPath())
	if err != nil {
		return nil, fmt.Errorf("kernclip-busd not running — start it with: %s", g("kb start"))
	}
	return c, nil
}

func roundtrip(req map[string]any) (map[string]any, error) {
	conn, err := connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	var resp map[string]any
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	if ok, _ := resp["ok"].(bool); !ok {
		msg, _ := resp["error"].(string)
		return nil, fmt.Errorf("%s", msg)
	}
	return resp, nil
}

// ── Commands ──────────────────────────────────────────────────────────────────

// mimeFromExt guesses MIME type from a file extension, falling back to
// application/octet-stream for unknowns.
func mimeFromExt(fpath string) string {
	ext := strings.ToLower(filepath.Ext(fpath))
	// mime.TypeByExtension uses the OS MIME database; supplement common ones.
	known := map[string]string{
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".gif":  "image/gif",
		".webp": "image/webp",
		".svg":  "image/svg+xml",
		".pdf":  "application/pdf",
		".mp3":  "audio/mpeg",
		".wav":  "audio/wav",
		".mp4":  "video/mp4",
		".json": "application/json",
		".txt":  "text/plain",
		".md":   "text/markdown",
		".sh":   "text/x-shellscript",
	}
	if m, ok := known[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		return m
	}
	return "application/octet-stream"
}

func cmdPub(args []string) {
	if len(args) < 1 {
		fatal("usage: kb pub <topic> [data] [--mime type] [--file path]\n       echo data | kb pub <topic>")
	}
	topic := args[0]
	mimeType := "text/plain"
	var data string
	var filePath string

	// parse flags: --mime, --file
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--mime":
			if i+1 < len(rest) {
				mimeType = rest[i+1]
				rest = append(rest[:i], rest[i+2:]...)
				i--
			}
		case "--file":
			if i+1 < len(rest) {
				filePath = rest[i+1]
				rest = append(rest[:i], rest[i+2:]...)
				i--
			}
		}
	}

	if filePath != "" {
		// File mode: read, base64-encode, auto-detect MIME unless overridden
		raw, err := os.ReadFile(filePath)
		if err != nil {
			fatal("reading file %s: %v", filePath, err)
		}
		if mimeType == "text/plain" {
			mimeType = mimeFromExt(filePath)
		}
		data = base64.StdEncoding.EncodeToString(raw)
		// Wrap as a simple JSON envelope so receivers know it's a file payload
		envelope := fmt.Sprintf(`{"encoding":"base64","filename":%q,"size":%d,"data":%q}`,
			filepath.Base(filePath), len(raw), data)
		r, err := roundtrip(map[string]any{"op": "pub", "topic": topic, "mime": mimeType, "data": envelope})
		must(err)
		seq, _ := r["seq"].(float64)
		fmt.Printf("%s %s  %s  %s  seq=%d\n",
			g("✓"), c(topic), d(mimeType),
			d(fmt.Sprintf("%d bytes → base64", len(raw))), int(seq))
		return
	}

	if len(rest) > 0 {
		data = strings.Join(rest, " ")
	} else {
		// read from stdin
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatal("reading stdin: %v", err)
		}
		data = string(b)
	}

	r, err := roundtrip(map[string]any{"op": "pub", "topic": topic, "mime": mimeType, "data": data})
	must(err)
	seq, _ := r["seq"].(float64)
	fmt.Printf("%s %s  %s seq=%d\n", g("✓"), c(topic), d(mimeType), int(seq))
}

// tryDecodeFilePayload attempts to decode a base64 file envelope published
// by --file.  Returns raw bytes + filename if successful, otherwise nil.
func tryDecodeFilePayload(data string) (raw []byte, filename string, ok bool) {
	var env struct {
		Encoding string `json:"encoding"`
		Filename string `json:"filename"`
		Size     int    `json:"size"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return nil, "", false
	}
	if env.Encoding != "base64" || env.Data == "" {
		return nil, "", false
	}
	raw, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil {
		return nil, "", false
	}
	return raw, env.Filename, true
}

func cmdGet(args []string) {
	if len(args) < 1 {
		fatal("usage: kb get <topic> [--after N] [--json] [--out path]")
	}
	topic := args[0]
	jsonOut := false
	afterSeq := -1
	outPath := ""

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--after":
			if i+1 < len(args) {
				n, _ := strconv.Atoi(args[i+1])
				afterSeq = n
				i++
			}
		case "--out":
			if i+1 < len(args) {
				outPath = args[i+1]
				i++
			}
		}
	}

	req := map[string]any{"op": "get", "topic": topic}
	if afterSeq >= 0 {
		req["after_seq"] = afterSeq
	}

	r, err := roundtrip(req)
	must(err)

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(r)
		return
	}

	if afterSeq >= 0 {
		msgs, _ := r["messages"].([]any)
		if len(msgs) == 0 {
			fmt.Println(d("(no messages after seq " + strconv.Itoa(afterSeq) + ")"))
			return
		}
		for _, m := range msgs {
			printMsg(m.(map[string]any), outPath)
		}
		return
	}

	data, _ := r["data"].(string)
	if data == "" {
		fmt.Println(d("(empty)"))
		return
	}
	printMsg(r, outPath)
}

// printMsg prints a message envelope.  If outPath is set and the data is a
// base64 file envelope, the file is saved there (or to its original filename
// in the cwd if outPath is a directory).
func printMsg(r map[string]any, outPath string) {
	seq, _ := r["seq"].(float64)
	mimeType, _ := r["mime"].(string)
	data, _ := r["data"].(string)
	sender, _ := r["sender"].(string)

	// Attempt to decode a file payload
	if raw, filename, ok := tryDecodeFilePayload(data); ok {
		dest := outPath
		if dest == "" {
			dest = filename // default: write to cwd using original name
		} else {
			// If dest is a directory, append the original filename
			if info, err := os.Stat(dest); err == nil && info.IsDir() {
				dest = filepath.Join(dest, filename)
			}
		}
		if err := os.WriteFile(dest, raw, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "%s writing file: %v\n", y("✗"), err)
			return
		}
		fmt.Printf("%s %s  %s  %s  seq=%d\n",
			g("✓ saved"),
			c(r["topic"].(string)),
			d(mimeType),
			g(dest),
			int(seq),
		)
		return
	}

	// Plain text / non-file message
	fmt.Printf("%s %s  %s  %s\n%s\n",
		d(fmt.Sprintf("seq=%-4d", int(seq))),
		c(r["topic"].(string)),
		d(mimeType),
		d("sender="+sender),
		data,
	)
}

func cmdSub(args []string) {
	if len(args) < 1 {
		fatal("usage: kb sub <topic> [--pattern <glob>] [--topics a,b,c] [--json] [--data-only] [--out dir]")
	}
	topic := ""
	pattern := ""
	var topics []string
	jsonOut := false
	dataOnly := false
	outPath := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--data-only":
			dataOnly = true
		case "--out":
			if i+1 < len(args) {
				outPath = args[i+1]
				i++
			}
		case "--pattern":
			if i+1 < len(args) {
				pattern = args[i+1]
				i++
			}
		case "--topics":
			if i+1 < len(args) {
				topics = strings.Split(args[i+1], ",")
				i++
			}
		default:
			if topic == "" && !strings.HasPrefix(args[i], "--") {
				topic = args[i]
			}
		}
	}

	conn, err := connect()
	must(err)
	defer conn.Close()

	var subReq map[string]any
	switch {
	case pattern != "":
		subReq = map[string]any{"op": "sub", "pattern": pattern}
	case len(topics) > 0:
		subReq = map[string]any{"op": "sub", "topics": topics}
	default:
		subReq = map[string]any{"op": "sub", "topic": topic}
	}
	must(json.NewEncoder(conn).Encode(subReq))

	subDesc := topic
	if pattern != "" {
		subDesc = "pattern=" + pattern
	} else if len(topics) > 0 {
		subDesc = "topics=" + strings.Join(topics, ",")
	}
	fmt.Fprintf(os.Stderr, "%s %s  %s\n", d("⏳"), c(subDesc), d("waiting for messages (Ctrl-C to stop)"))

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1 MiB — handles large file payloads
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 { continue }

		if jsonOut {
			fmt.Println(string(line))
			continue
		}

		var r map[string]any
		if err := json.Unmarshal(line, &r); err != nil { continue }
		if ok, _ := r["ok"].(bool); !ok { continue }

		if dataOnly {
			data, _ := r["data"].(string)
			fmt.Println(data)
		} else {
			printMsg(r, outPath)
		}
	}
}

func cmdTopics(args []string) {
	r, err := roundtrip(map[string]any{"op": "topics"})
	must(err)
	topics, _ := r["topics"].([]any)
	if len(topics) == 0 {
		fmt.Println(d("(no topics)"))
		return
	}
	fmt.Printf("%s %s\n", b("Topics"), d(fmt.Sprintf("(%d)", len(topics))))
	for _, t := range topics {
		fmt.Printf("  %s %s\n", g("▸"), c(t.(string)))
	}
}

func cmdCreateTopic(args []string) {
	if len(args) < 1 {
		fatal("usage: kb create-topic <topic> [--ring N]")
	}
	topic := args[0]
	ringSize := 0
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--ring":
			if i+1 < len(args) {
				ringSize, _ = strconv.Atoi(args[i+1])
				i++
			}
		}
	}
	req := map[string]any{"op": "create_topic", "topic": topic}
	if ringSize > 0 {
		req["ring_size"] = ringSize
	}
	r, err := roundtrip(req)
	must(err)
	created, _ := r["created"].(bool)
	rs, _ := r["ring_size"].(float64)
	if created {
		fmt.Printf("%s created topic %s with ring size %d\n", g("✓"), c(topic), int(rs))
	} else {
		fmt.Printf("%s topic %s already exists (ring size %d)\n", d("○"), c(topic), int(rs))
	}
}

func cmdManifest() {
	r, err := roundtrip(map[string]any{"op": "manifest"})
	must(err)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(r)
}


func cmdStart() {
	busd, err := exec.LookPath("kernclip-busd")
	if err != nil {
		// try relative
		busd = "./kernclip-busd"
	}
	cmd := exec.Command(busd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fatal("could not start kernclip-busd: %v\nMake sure kernclip-busd is in your PATH or current directory.", err)
	}
	fmt.Printf("%s kernclip-busd started (pid %d)\n%s %s\n",
		g("✓"), cmd.Process.Pid,
		d("socket:"), c(sockPath()),
	)
	cmd.Wait()
}

func cmdStatus() {
	r, err := roundtrip(map[string]any{"op": "topics"})
	if err != nil {
		fmt.Printf("%s kernclip-busd not running\n%s %s\n", y("✗"), d("start with:"), g("kb start"))
		return
	}
	topics, _ := r["topics"].([]any)
	fmt.Printf("%s kernclip-busd running\n%s %s\n%s %d active topic(s)\n",
		g("✓"),
		d("socket:"), c(sockPath()),
		d("      "), len(topics),
	)
}

// cmdTail prints the last N messages from a topic's ring buffer and exits.
func cmdTail(args []string) {
	topic := ""
	n := 10
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "-n":
			if i+1 < len(args) {
				n, _ = strconv.Atoi(args[i+1])
				i++
			}
		default:
			if topic == "" {
				topic = args[i]
			}
		}
	}
	if topic == "" {
		fatal("usage: kb tail <topic> [-n N] [--json]")
	}
	// Fetch all messages since seq 0 (i.e. everything in the ring buffer)
	r, err := roundtrip(map[string]any{"op": "get", "topic": topic, "after_seq": uint64(0)})
	must(err)
	msgs, _ := r["messages"].([]any)
	if len(msgs) == 0 {
		fmt.Println(d("(no messages)"))
		return
	}
	// Trim to last N
	if len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		for _, m := range msgs {
			enc.Encode(m)
		}
		return
	}
	for _, m := range msgs {
		printMsg(m.(map[string]any), "")
	}
}

// cmdWait blocks until a new message arrives on a topic, then prints and exits.
func cmdWait(args []string) {
	topic := ""
	timeoutMs := 0 // 0 = wait forever
	dataOnly := false
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--data-only":
			dataOnly = true
		case "--json":
			jsonOut = true
		case "--timeout":
			if i+1 < len(args) {
				timeoutMs, _ = strconv.Atoi(args[i+1])
				i++
			}
		default:
			if topic == "" {
				topic = args[i]
			}
		}
	}
	if topic == "" {
		fatal("usage: kb wait <topic> [--timeout ms] [--data-only] [--json]")
	}

	// First check if there's already a message on the topic — return it immediately.
	existing, err := roundtrip(map[string]any{"op": "get", "topic": topic})
	must(err)
	if data, _ := existing["data"].(string); data != "" {
		// Topic has a current value — return it now without blocking.
		if jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			enc.Encode(existing)
			return
		}
		if dataOnly {
			fmt.Println(data)
			return
		}
		printMsg(existing, "")
		return
	}

	// Topic is empty — block until a message arrives.
	conn, err := connect()
	must(err)
	defer conn.Close()

	pollMs := timeoutMs
	if pollMs <= 0 {
		pollMs = 86400000 // 24 hours — effectively forever
	}
	req := map[string]any{
		"op":         "poll",
		"topic":      topic,
		"after_seq":  uint64(0),
		"limit":      1,
		"timeout_ms": pollMs,
	}
	must(json.NewEncoder(conn).Encode(req))

	var resp map[string]any
	must(json.NewDecoder(conn).Decode(&resp))

	msgs, _ := resp["messages"].([]any)
	if len(msgs) == 0 {
		fmt.Fprintln(os.Stderr, y("✗")+" timed out waiting for message on "+c(topic))
		os.Exit(1)
	}
	m := msgs[0].(map[string]any)
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(m)
		return
	}
	if dataOnly {
		data, _ := m["data"].(string)
		fmt.Println(data)
		return
	}
	printMsg(m, "")
}

// cmdCopy reads stdin, inline text, or a file and publishes to kernclip.clipboard.
func cmdCopy(args []string) {
	filePath := ""
	var textArgs []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--file" && i+1 < len(args) {
			filePath = args[i+1]
			i++
		} else {
			textArgs = append(textArgs, args[i])
		}
	}

	if filePath != "" {
		// File mode — base64 envelope, auto-detect MIME, same as pub --file
		raw, err := os.ReadFile(filePath)
		if err != nil {
			fatal("reading file: %v", err)
		}
		mimeType := mimeFromExt(filePath)
		envelope := fmt.Sprintf(`{"encoding":"base64","filename":%q,"size":%d,"data":%q}`,
			filepath.Base(filePath), len(raw), base64.StdEncoding.EncodeToString(raw))
		r, err := roundtrip(map[string]any{
			"op": "pub", "topic": "kernclip.clipboard",
			"mime": mimeType, "data": envelope,
		})
		must(err)
		seq, _ := r["seq"].(float64)
		fmt.Printf("%s %s  %s  %s  seq=%d\n",
			g("✓ copied"), d(mimeType),
			d(fmt.Sprintf("%d bytes → base64", len(raw))),
			c("kernclip.clipboard"), int(seq))
		return
	}

	// Text mode — inline args or stdin
	var data string
	if len(textArgs) > 0 {
		data = strings.Join(textArgs, " ")
	} else {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatal("reading stdin: %v", err)
		}
		data = string(b)
	}
	r, err := roundtrip(map[string]any{
		"op": "pub", "topic": "kernclip.clipboard",
		"mime": "text/plain", "data": data,
	})
	must(err)
	seq, _ := r["seq"].(float64)
	fmt.Printf("%s %s  %s  seq=%d\n",
		g("✓ copied"), d(fmt.Sprintf("(%d bytes)", len(data))),
		c("kernclip.clipboard"), int(seq))
}

// cmdPaste prints or saves the latest value from kernclip.clipboard.
func cmdPaste(args []string) {
	outPath := ""
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--out":
			if i+1 < len(args) {
				outPath = args[i+1]
				i++
			}
		}
	}
	r, err := roundtrip(map[string]any{"op": "get", "topic": "kernclip.clipboard"})
	must(err)
	data, _ := r["data"].(string)
	if data == "" {
		fmt.Fprintln(os.Stderr, d("(clipboard empty)"))
		return
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(r)
		return
	}
	// If it's a file envelope, printMsg handles saving/display
	if _, _, ok := tryDecodeFilePayload(data); ok {
		printMsg(r, outPath)
		return
	}
	// Plain text — print to stdout
	fmt.Print(data)
}

// cmdPasteOut implements: kb paste > /tmp/gtt-universal.pipe | jq -R '{operation: "typeText", text: .}'
func cmdPasteOut(args []string) {
	pipePath := "/tmp/gtt-universal.pipe"
	for i := 0; i < len(args); i++ {
		if args[i] == "--pipe" && i+1 < len(args) {
			pipePath = args[i+1]
			i++
		}
	}

	r, err := roundtrip(map[string]any{"op": "get", "topic": "kernclip.clipboard"})
	must(err)
	data, _ := r["data"].(string)
	if data == "" {
		fmt.Fprintln(os.Stderr, d("(clipboard empty)"))
		return
	}

	if _, _, ok := tryDecodeFilePayload(data); ok {
		fatal("clipboard contains a binary file — only text can be typed")
	}

	// Write to pipe (> /tmp/gtt-universal.pipe)
	f, err := os.OpenFile(pipePath, os.O_WRONLY, os.ModeNamedPipe)
	if err != nil {
		fatal("opening pipe %s: %v", pipePath, err)
	}
	defer f.Close()

	// jq -R '{operation: "typeText", text: .}' — one JSON object per line
	// also print to terminal exactly as the original pipeline does
	enc := json.NewEncoder(f)
	jqEnc := json.NewEncoder(os.Stdout)
	jqEnc.SetIndent("", "  ")
	for _, line := range strings.Split(data, "\n") {
		payload := map[string]string{"operation": "typeText", "text": line}
		enc.Encode(payload)
		jqEnc.Encode(payload)
	}

	fmt.Printf("%s %s\n", g("✓"), d(fmt.Sprintf("paste-out → %s", pipePath)))
}

// stripANSI removes ANSI escape sequences from a string.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

// cmdPasteFile saves the clipboard to ~/kcpaste/pasteNNNN.txt with auto-increment,
// stripping ANSI escape codes so the file is clean human-readable text.
func cmdPasteFile(args []string) {
	dir := filepath.Join(os.Getenv("HOME"), "kcpaste")
	if err := os.MkdirAll(dir, 0755); err != nil {
		fatal("creating directory %s: %v", dir, err)
	}

	// Find next available number
	n := 1
	for {
		candidate := filepath.Join(dir, fmt.Sprintf("paste%04d.txt", n))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			break
		}
		n++
	}
	outPath := filepath.Join(dir, fmt.Sprintf("paste%04d.txt", n))

	r, err := roundtrip(map[string]any{"op": "get", "topic": "kernclip.clipboard"})
	must(err)
	data, _ := r["data"].(string)
	if data == "" {
		fmt.Fprintln(os.Stderr, d("(clipboard empty)"))
		return
	}

	if _, _, ok := tryDecodeFilePayload(data); ok {
		fatal("clipboard contains a binary file — use get --out for files")
	}

	clean := stripANSI(data)
	if err := os.WriteFile(outPath, []byte(clean), 0644); err != nil {
		fatal("writing file: %v", err)
	}

	fmt.Printf("%s %s  %s\n",
		g("✓ saved"), d(fmt.Sprintf("(%d bytes)", len(clean))),
		g(outPath))
}

// cmdPasteIt runs: gttuni --type "$(kb paste)" with ANSI stripped
func cmdPasteIt(args []string) {
	r, err := roundtrip(map[string]any{"op": "get", "topic": "kernclip.clipboard"})
	must(err)
	data, _ := r["data"].(string)
	if data == "" {
		fmt.Fprintln(os.Stderr, d("(clipboard empty)"))
		return
	}
	if _, _, ok := tryDecodeFilePayload(data); ok {
		fatal("clipboard contains a binary file — only text can be typed")
	}
	clean := stripANSI(data)
	cmd := exec.Command("gttuni", "--type", clean)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatal("gttuni: %v", err)
	}
	fmt.Printf("%s %s\n", g("✓ paste-it"), d(fmt.Sprintf("(%d bytes)", len(clean))))
}

// cmdStop sends SIGTERM to the running kernclip-busd process.
func cmdStop() {
	// Find PID via the socket — if it's listening, it's running.
	// We use fuser or lsof; fall back to pkill.
	out, err := exec.Command("fuser", sockPath()).Output()
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		pid := strings.TrimSpace(string(out))
		if _, err2 := exec.Command("kill", "-TERM", pid).Output(); err2 == nil {
			fmt.Printf("%s kernclip-busd stopped (pid %s)\n", g("✓"), pid)
			return
		}
	}
	// Fallback: pkill by binary name
	if err := exec.Command("pkill", "-TERM", "-x", "kernclip-busd").Run(); err == nil {
		fmt.Printf("%s kernclip-busd stopped\n", g("✓"))
		return
	}
	fmt.Printf("%s kernclip-busd not running\n", y("✗"))
}

// ── Help ──────────────────────────────────────────────────────────────────────

func showHelp() {
	l := func(s string) { fmt.Println(s) }
	lf := func(f string, a ...any) { fmt.Printf(f+"\n", a...) }

	l("")
	lf("%s — Local IPC message bus for any process, any language, any AI model", b("⬡ kb"))
	l(d("KernClip Message Bus | zero deps | any language | any AI"))
	l("")
	l(b("COMMANDS"))
	lf("  %-40s %s", g("kb pub <topic> <data>"),          d("publish a message to a topic"))
	lf("  %-40s %s", g("echo data | kb pub <topic>"),     d("publish from stdin"))
	lf("  %-40s %s", g("kb pub <topic> --file <path>"),   d("publish a file (image, pdf, binary…)"))
	lf("  %-40s %s", g("kb get <topic>"),                 d("read the latest message"))
	lf("  %-40s %s", g("kb get <topic> --out <path>"),    d("save a received file to disk"))
	lf("  %-40s %s", g("kb sub <topic>"),                 d("stream all new messages (blocking)"))
	lf("  %-40s %s", g("kb sub <topic> --out <dir>"),     d("stream + auto-save incoming files"))
	lf("  %-40s %s", g("kb tail <topic>"),                d("print last 10 messages and exit"))
	lf("  %-40s %s", g("kb wait <topic>"),                d("block until next message arrives"))
	lf("  %-40s %s", g("kb copy <text>"),                 d("copy text to the bus clipboard"))
	lf("  %-40s %s", g("kb paste"),                       d("print the current clipboard value"))
	lf("  %-40s %s", g("kb paste-out"),                   d("type clipboard into active window via gtt pipe"))
	lf("  %-40s %s", g("kb paste-it"),                    d("type clipboard into active window via gttuni (true paste)"))
	lf("  %-40s %s", g("kb pfile"),                       d("save clipboard to ~/kcpaste/pasteNNNN.txt (auto-increment, ANSI-stripped)"))
	lf("  %-40s %s", g("kb topics"),                      d("list all active topics"))
	lf("  %-40s %s", g("kb status"),                      d("check if daemon is running"))
	lf("  %-40s %s", g("kb start"),                       d("start the bus daemon"))
	lf("  %-40s %s", g("kb stop"),                        d("stop the bus daemon"))
	l("")
	l(b("FLAGS"))
	lf("  %-22s %s", g("--file <path>"),  d("publish a file instead of text  (use with pub)"))
	lf("  %-22s %s", g("--mime <type>"),  d("override MIME type  (default: auto-detected)"))
	lf("  %-22s %s", g("--out <path>"),   d("save received file to this path or directory  (use with get, sub)"))
	lf("  %-22s %s", g("--after <N>"),    d("get all messages with seq > N  (use with get)"))
	lf("  %-22s %s", g("-n <N>"),         d("how many messages to show  (use with tail, default: 10)"))
	lf("  %-22s %s", g("--timeout <ms>"), d("give up waiting after N ms  (use with wait)"))
	lf("  %-22s %s", g("--pipe <path>"),  d("gttd pipe path  (use with type, default: /tmp/gtt-universal.pipe)"))
	lf("  %-22s %s", g("--data-only"),    d("print only the message data, no metadata"))
	lf("  %-22s %s", g("--json"),         d("raw JSON output"))
	l("")
	l(b("QUICK START"))
	l("  " + p("kb start"))
	l(`  ` + p(`kb pub greetings "hello world"`))
	l("  " + p("kb get greetings"))
	l("  " + p("kb sub greetings"))
	l("")
	l(b("FILES & IMAGES"))
	l("  " + d("# Send any file — MIME type is detected automatically from the extension:"))
	l("  " + p(`kb pub screenshots --file /tmp/screen.png`))
	l("  " + p(`kb pub documents  --file ~/report.pdf`))
	l("  " + p(`kb pub audio      --file recording.wav`))
	l("")
	l("  " + d("# Receive — saves to the original filename in the current directory:"))
	l("  " + p(`kb get screenshots`))
	l("")
	l("  " + d("# Receive — save to a specific folder:"))
	l("  " + p(`kb get screenshots --out ~/Downloads/`))
	l("")
	l("  " + d("# Watch a topic and auto-save every incoming file as it arrives:"))
	l("  " + p(`kb sub screenshots --out ~/incoming/`))
	l("")
	l(b("CLIPBOARD"))
	l("  " + d("# Copy text to the bus clipboard (replaces any previous value):"))
	l(`  ` + p(`kb copy "meeting at 3pm"`))
	l("  " + p(`echo "some text" | kb copy`))
	l(`  ` + p(`kb copy --file notes.txt`))
	l("")
	l("  " + d("# Paste — prints whatever is currently on the clipboard:"))
	l("  " + p(`kb paste`))
	l("  " + p(`kb paste > output.txt          # redirect to file`))
	l("")
	l("  " + d("# Type — injects clipboard into the active window via gttd (no system clipboard):"))
	l("  " + p(`kb type`))
	l("  " + p(`kb type --pipe /tmp/gtt-wlroots.pipe  # alternate pipe`))
	l("  " + d("# Typical workflow — copy something then type it anywhere:"))
	l(`  ` + p(`kb copy "some text" && kb type`))
	l("")
	l(b("HISTORY & WAITING"))
	l("  " + d("# See the last 10 messages on a topic (like unix tail):"))
	l("  " + p(`kb tail events`))
	l("  " + p(`kb tail events -n 25           # last 25`))
	l("")
	l("  " + d("# Block a script until something publishes to a topic:"))
	l("  " + p(`kb wait job.done               # waits forever`))
	l("  " + p(`kb wait job.done --timeout 5000  # give up after 5 seconds`))
	l("  " + p(`result=$(kb wait job.done --data-only)  # capture output in a script`))
	l("")
	l(b("SHELL — zero deps, no install"))
	l("  " + p(`source ~/kernclip/bus/sdk/shell/kernclip-bus.sh`))
	l("  " + p(`kc_pub "events" "something happened"`))
	l("  " + p(`kc_sub "events"                    # streaming`))
	l("  " + p(`kc_pub_file "screenshots" ~/screen.png   # send a file`))
	l("  " + p(`kc_get_file "screenshots" ~/Downloads/   # save received file`))
	l("  " + p(`kc_paste                           # get clipboard value`))
	l("")
	l(b("PYTHON — stdlib only"))
	l("  " + d("# 3 lines to integrate:"))
	l("  " + p(`import sys; sys.path.insert(0, "~/kernclip/bus/sdk/python")`))
	l("  " + p(`from kernclip_bus import Bus; bus = Bus()`))
	l("  " + p(`bus.pub("my-topic", "hello from python")`))
	l("  " + p(`msg = bus.get("my-topic"); print(msg.data)`))
	l("")
	l("  " + d("# send a file:"))
	l("  " + p(`bus.pub_file("screenshots", "/tmp/screen.png")`))
	l("  " + d("# receive a file (saves to disk, returns path):"))
	l("  " + p(`path = bus.get_file("screenshots", dest="~/Downloads/")`))
	l("")
	l("  " + d("# subscribe — blocking iterator:"))
	l("  " + p(`for msg in bus.sub("my-topic"):`))
	l("  " + p(`    print(msg.seq, msg.data)`))
	l("")
	l("  " + d("# async background subscription:"))
	l("  " + p(`bus.sub_async("my-topic", lambda msg: print(msg.data))`))
	l("")
	l(b("NODE.JS — stdlib only"))
	l("  " + d("# require, no npm install:"))
	l("  " + p(`const { Bus } = require('~/kernclip/bus/sdk/node/kernclip-bus');`))
	l("  " + p(`const bus = new Bus();`))
	l("  " + p(`await bus.pub('my-topic', 'hello from node');`))
	l("  " + p(`const msg = await bus.get('my-topic');`))
	l("  " + p(`const unsub = bus.sub('my-topic', msg => console.log(msg.data));`))
	l("")
	l("  " + d("# send a file:"))
	l("  " + p(`await bus.pubFile('screenshots', '/tmp/screen.png');`))
	l("  " + d("# receive a file (saves to disk, returns path):"))
	l("  " + p(`const savedPath = await bus.getFile('screenshots', '~/Downloads/');`))
	l("")
	l(b("GO — stdlib only"))
	l("  " + p(`bus := kernclip_bus.New()`))
	l("  " + p(`bus.Pub("my-topic", "hello", "text/plain")`))
	l("  " + p(`msg, _ := bus.Get("my-topic")`))
	l("  " + p(`bus.Sub(ctx, "my-topic", func(m Message) { fmt.Println(m.Data) })`))
	l("")
	l(b("RAW — any language that can open a socket"))
	l("  " + d(`# netcat:`))
	l("  " + p(`echo '{"op":"pub","topic":"x","data":"hi"}' | nc -U $XDG_RUNTIME_DIR/kernclip-bus.sock`))
	l("  " + d(`# Python one-liner:`))
	l("  " + p(`python3 -c "import socket,json,os; s=socket.socket(socket.AF_UNIX); s.connect(f'/run/user/{os.getuid()}/kernclip-bus.sock'); s.sendall(json.dumps({'op':'get','topic':'x'}).encode()+b'\n'); print(s.recv(4096).decode())"`))
	l("")
	l(b("AI MODEL COLLABORATION"))
	l("  " + d("# Any model, any SDK — agree on topic names, that's the whole contract:"))
	l("  " + p(`bus.pub("ai.session.001.input",  task)         # orchestrator posts task`))
	l("  " + p(`bus.pub("ai.session.001.output", call_claude(msg.data))  # Claude agent`))
	l("  " + p(`bus.pub("ai.session.001.output", call_gemini(msg.data))  # Gemini agent`))
	l("  " + p(`for msg in bus.sub("ai.session.001.done"):     # wait for final result`))
	l("")
	l("  " + d("# Run the full demo:"))
	l("  " + p(`python3 ~/kernclip/bus/examples/ai-collab/demo.py`))
	l("")
	l(b("BUILT-IN TOPICS"))
	lf("  %-40s %s", c("kernclip.clipboard"),             d("system clipboard — read/write with copy and paste commands"))
	l("")
	l(b("SYSTEM MONITOR TOPICS") + d("  (published by daemon every 2s, zero config)"))
	lf("  %-40s %s", c("gtt.system.metrics"),             d("CPU %, RAM, swap, disk read/write speed, net dl/ul, process count"))
	lf("  %-40s %s", c("gtt.system.top_disk_writers"),    d("top 5 processes by disk write speed (JSON array)"))
	l("")
	l(b("COMMAND TOPICS") + d("  (publish to execute — ACK arrives on gtt.system.ack.*)"))
	lf("  %-40s %s", c("gtt.settings.brightness"), d(`data="50"               → set screen brightness 0-100%`))
	lf("  %-40s %s", c("gtt.settings.volume"),     d(`data="40"               → set audio volume 0-150% (wpctl/pactl/amixer)`))
	lf("  %-40s %s", c("gtt.settings.nightlight"), d(`data="true|false"       → toggle GNOME Night Light`))
	lf("  %-40s %s", c("gtt.settings.dconf"),      d(`data="/path/key value"  → dconf write`))
	lf("  %-40s %s", c("gtt.settings.gsettings"),  d(`data="schema key value" → gsettings set`))
	lf("  %-40s %s", c("gtt.system.kill"),          d(`data="1234"             → kill -9 <pid>`))
	lf("  %-40s %s", c("gtt.system.killname"),      d(`data="chrome"           → pkill -9 <name>`))
	lf("  %-40s %s", c("gtt.system.service"),        d(`data="restart:nginx"   → systemctl <action>:<service>`))
	lf("  %-40s %s", c("gtt.system.notify"),         d(`data="Title|Body"      → desktop notification (notify-send)`))
	lf("  %-40s %s", c("gtt.system.exec"),            d(`data="bash cmd"        → execute shell command`))
	l("")
	l("  " + d("Examples:"))
	l("  " + p(`kb pub gtt.settings.brightness 50`))
	l("  " + p(`kb pub gtt.settings.volume 30`))
	l("  " + p(`kb pub gtt.settings.nightlight true`))
	l("  " + p(`kb pub gtt.system.service "restart:nginx"`))
	l("  " + p(`kb pub gtt.system.notify "Alert|CPU is above 90%"`))
	l("  " + p(`kb sub gtt.system.metrics --data-only | jq .cpu_percent`))
	l("  " + p(`kb wait gtt.system.ack.settings_brightness --data-only`))
	l("")
	l(b("SUBSCRIPTIONS"))
	l("  " + d("Three subscription modes:"))
	l("  " + p(`kb sub <topic>                    `) + d("exact topic (original)"))
	l("  " + p(`kb sub --pattern "agent.*"       `) + d("wildcard — * matches one segment"))
	l("  " + p(`kb sub --pattern "agent.#"       `) + d("wildcard — # matches one+ segments (terminal)"))
	l("  " + p(`kb sub --topics a,b,c            `) + d("multi-topic fan-in stream"))
	l("")
	l(b("TOPIC MANAGEMENT"))
	l("  " + p(`kb create-topic <topic>          `) + d("create with default ring size"))
	l("  " + p(`kb create-topic <topic> --ring 4096`) + d("create with explicit ring size"))
	l("  " + p(`kb manifest                      `) + d("full daemon schema + topic inventory"))
	l("")
	l(b("WIRE PROTOCOL"))
	l("  " + d("Newline-delimited JSON over Unix socket at /run/user/$UID/kernclip-bus.sock"))
	lf("  %-12s %s", g("pub"),         d(`{"op":"pub","topic":"t","mime":"text/plain","data":"..."}`))
	lf("  %-12s %s", g("get"),         d(`{"op":"get","topic":"t"}`))
	lf("  %-12s %s", g("sub"),         d(`{"op":"sub","topic":"t"}  → streaming, one line per message`))
	lf("  %-12s %s", g("sub(pattern)"),d(`{"op":"sub","pattern":"agent.*"}  → wildcard subscription`))
	lf("  %-12s %s", g("sub(topics)"), d(`{"op":"sub","topics":["a","b"]}  → multi-topic fan-in`))
	lf("  %-12s %s", g("poll"),        d(`{"op":"poll","topic":"t","after_seq":N,"timeout_ms":2000}`))
	lf("  %-12s %s", g("topics"),      d(`{"op":"topics"}`))
	lf("  %-12s %s", g("create_topic"),d(`{"op":"create_topic","topic":"t","ring_size":4096}`))
	lf("  %-12s %s", g("manifest"),    d(`{"op":"manifest"}  → self-describing schema of all ops`))
	l("")
}


// cmdTermCtx fetches terminal.command and terminal.output in one shot.
func cmdTermCtx(args []string) {
	jsonOut := false
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		}
	}

	cmdR, cmdErr := roundtrip(map[string]any{"op": "get", "topic": "terminal.command"})
	outR, outErr := roundtrip(map[string]any{"op": "get", "topic": "terminal.output"})

	if jsonOut {
		result := map[string]any{}
		if cmdErr == nil {
			result["command"] = cmdR["data"]
			result["command_seq"] = cmdR["seq"]
		} else {
			result["command"] = ""
		}
		if outErr == nil {
			result["output"] = outR["data"]
			result["output_seq"] = outR["seq"]
		} else {
			result["output"] = ""
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(result)
		return
	}

	cmd := d("(none)")
	if cmdErr == nil {
		if s, _ := cmdR["data"].(string); s != "" {
			cmd = s
		}
	}
	out := d("(none)")
	if outErr == nil {
		if s, _ := outR["data"].(string); s != "" {
			out = s
		}
	}

	fmt.Printf("%s %s\n%s\n\n%s %s\n%s\n",
		b("command:"), c(cmd),
		"",
		b("output:"), "",
		out,
	)
}

// ── Entry ─────────────────────────────────────────────────────────────────────

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		showHelp()
		return
	}

	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "pub":
		cmdPub(rest)
	case "get":
		cmdGet(rest)
	case "sub":
		cmdSub(rest)
	case "tail":
		cmdTail(rest)
	case "wait":
		cmdWait(rest)
	case "copy":
		cmdCopy(rest)
	case "paste":
		cmdPaste(rest)
	case "paste-out":
		cmdPasteOut(rest)
	case "paste-it":
		cmdPasteIt(rest)
	case "pfile":
		cmdPasteFile(rest)
	case "topics":
		cmdTopics(rest)
	case "status":
		cmdStatus()
	case "start":
		cmdStart()
	case "stop":
		cmdStop()
	case "create-topic":
		cmdCreateTopic(rest)
	case "manifest":
		cmdManifest()
	case "tctx":
		cmdTermCtx(rest)

	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\nRun %s for usage.\n", cmd, g("kb --help"))
		os.Exit(1)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", y("✗"), err)
		os.Exit(1)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, y("✗")+" "+format+"\n", args...)
	os.Exit(1)
}
