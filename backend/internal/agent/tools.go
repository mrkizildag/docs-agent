package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
)

const (
	readFileToolName = "read_file"
	grepToolName     = "grep"
	listDirToolName  = "list_dir"

	maxReadBytes    = 64 << 10
	maxGrepMatches  = 200
	maxGrepBytes    = 32 << 10
	maxGrepFileSize = 1 << 20
	maxGrepLineLen  = 300
	binarySniffLen  = 8000
)

// readTools describes the file tools offered to the model.
func readTools() []llm.Tool {
	return []llm.Tool{
		{
			Name:        readFileToolName,
			Description: "Read a repo-relative file's contents (truncated at 64 KB).",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {"path": {"type": "string", "description": "Repo-relative file path."}},
				"required": ["path"]
			}`),
		},
		{
			Name:        grepToolName,
			Description: "Search files with a Go regular expression. Returns path:line: text lines, capped at 200 matches.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pattern": {"type": "string", "description": "Go regexp."},
					"path": {"type": "string", "description": "Repo-relative file or directory to search; defaults to the repo root."}
				},
				"required": ["pattern"]
			}`),
		},
		{
			Name:        listDirToolName,
			Description: "List a repo-relative directory's entries; directories end in /.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {"path": {"type": "string", "description": "Repo-relative directory path; \".\" is the repo root."}},
				"required": ["path"]
			}`),
		},
	}
}

type pathArgs struct {
	Path string `json:"path"`
}

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

// callTool runs call against root, returning a ToolResult that never panics:
// an unknown tool name, malformed arguments, or a path outside root becomes an
// error result, not a stopped loop.
func callTool(root *os.Root, call llm.ToolCall) llm.ToolResult {
	var (
		content string
		err     error
	)
	switch call.Name {
	case readFileToolName:
		content, err = readFile(root, call.Args)
	case grepToolName:
		content, err = grep(root, call.Args)
	case listDirToolName:
		content, err = listDir(root, call.Args)
	default:
		return llm.ToolResult{CallID: call.ID, Content: fmt.Sprintf("unknown tool %q", call.Name), IsError: true}
	}
	if err != nil {
		return llm.ToolResult{CallID: call.ID, Content: fmt.Sprintf("%s: %v", call.Name, err), IsError: true}
	}
	return llm.ToolResult{CallID: call.ID, Content: content}
}

// scopedPath validates p as a slash-separated path inside the root, not under
// .git. An empty p means the root.
func scopedPath(p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	if !fs.ValidPath(p) {
		return "", fmt.Errorf("path %q is not a repo-relative path inside the repo", p)
	}
	if isGitPath(p) {
		return "", fmt.Errorf("path %q refused: under .git", p)
	}
	return p, nil
}

func readFile(root *os.Root, raw json.RawMessage) (string, error) {
	var args pathArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	p, err := scopedPath(args.Path)
	if err != nil {
		return "", err
	}

	f, err := root.Open(p)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", p, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %q: %w", p, err)
	}
	if len(data) > maxReadBytes {
		return string(data[:maxReadBytes]) + fmt.Sprintf("\n[truncated at %d bytes]", maxReadBytes), nil
	}
	return string(data), nil
}

func listDir(root *os.Root, raw json.RawMessage) (string, error) {
	var args pathArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	p, err := scopedPath(args.Path)
	if err != nil {
		return "", err
	}

	entries, err := fs.ReadDir(root.FS(), p)
	if err != nil {
		return "", fmt.Errorf("list %q: %w", p, err)
	}
	var b strings.Builder
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		b.WriteString(e.Name())
		if e.IsDir() {
			b.WriteByte('/')
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func grep(root *os.Root, raw json.RawMessage) (string, error) {
	var args grepArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}
	start, err := scopedPath(args.Path)
	if err != nil {
		return "", err
	}

	fsys := root.FS()
	var (
		out       strings.Builder
		matches   int
		skipped   int
		truncated bool
	)
	walk := func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if p == start {
				return walkErr
			}
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := readSearchable(fsys, p)
		if err != nil {
			if !errors.Is(err, errBinary) {
				skipped++
			}
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			if len(line) > maxGrepLineLen {
				line = line[:maxGrepLineLen] + "..."
			}
			entry := fmt.Sprintf("%s:%d: %s\n", p, i+1, line)
			if matches >= maxGrepMatches || out.Len()+len(entry) > maxGrepBytes {
				truncated = true
				return fs.SkipAll
			}
			out.WriteString(entry)
			matches++
		}
		return nil
	}
	if err := fs.WalkDir(fsys, start, walk); err != nil {
		return "", fmt.Errorf("search %q: %w", start, err)
	}
	if truncated {
		out.WriteString("[truncated: refine the pattern or path]\n")
	}
	if skipped > 0 {
		fmt.Fprintf(&out, "(skipped %d unreadable files)\n", skipped)
	}
	return out.String(), nil
}

var errBinary = errors.New("binary file")

// readSearchable returns the file's leading maxGrepFileSize bytes, or
// errBinary when they look binary.
func readSearchable(fsys fs.FS, p string) ([]byte, error) {
	f, err := fsys.Open(p)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", p, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxGrepFileSize))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", p, err)
	}
	if bytes.IndexByte(data[:min(len(data), binarySniffLen)], 0) >= 0 {
		return nil, errBinary
	}
	return data, nil
}

// isGitPath reports whether p has a ".git" path component; os.Root already
// confines p inside the clone, but .git holds history the model has no
// business reading.
func isGitPath(p string) bool {
	for _, part := range strings.Split(path.Clean(p), "/") {
		if part == ".git" {
			return true
		}
	}
	return false
}
