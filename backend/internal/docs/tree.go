package docs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"

	"github.com/bmatcuk/doublestar/v4"
)

const maxDocBytes = 1 << 20

// Problem is a doc that failed to parse, with the cause.
type Problem struct {
	Path string
	Err  error
}

// Tree is a repo's docs/ folder parsed into docs and the files that failed.
type Tree struct {
	Docs     []Doc
	Problems []Problem
}

// Parse walks "docs" under fsys, which is rooted at the repo root, and parses
// every .md file found at any depth. A missing docs folder yields an empty
// Tree, not an error; per-file parse failures become Problems so one bad doc
// doesn't stop the rest. Only regular files are read, and a file over 1 MiB is
// a Problem; the tree is untrusted PR content.
func Parse(fsys fs.FS) (Tree, error) {
	if _, err := fs.Stat(fsys, "docs"); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Tree{}, nil
		}

		return Tree{}, fmt.Errorf("stat docs folder: %w", err)
	}

	var tree Tree

	err := fs.WalkDir(fsys, "docs", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", p, err)
		}
		if !d.Type().IsRegular() || path.Ext(p) != ".md" {
			return nil
		}

		src, tooLarge, err := readDoc(fsys, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		if tooLarge {
			tree.Problems = append(tree.Problems, Problem{Path: p, Err: fmt.Errorf("%s: larger than 1 MiB", p)})

			return nil
		}

		tree.add(p, src)

		return nil
	})
	if err != nil {
		return Tree{}, fmt.Errorf("parse docs tree: %w", err)
	}

	sort.Slice(tree.Docs, func(i, j int) bool { return tree.Docs[i].Path < tree.Docs[j].Path })
	sort.Slice(tree.Problems, func(i, j int) bool { return tree.Problems[i].Path < tree.Problems[j].Path })

	return tree, nil
}

func readDoc(fsys fs.FS, p string) (src []byte, tooLarge bool, err error) {
	f, err := fsys.Open(p)
	if err != nil {
		return nil, false, fmt.Errorf("open: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			src, tooLarge, err = nil, false, fmt.Errorf("close: %w", cerr)
		}
	}()

	src, err = io.ReadAll(io.LimitReader(f, maxDocBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read: %w", err)
	}

	return src, len(src) > maxDocBytes, nil
}

func (t *Tree) add(path string, src []byte) {
	doc, err := ParseDoc(path, src)
	if err != nil {
		t.Problems = append(t.Problems, Problem{Path: path, Err: err})

		return
	}

	t.Docs = append(t.Docs, doc)
}

// Match returns the paths of docs whose covers include a glob matching any
// of changed, in path order.
func (t Tree) Match(changed []string) []string {
	var paths []string
	for _, doc := range t.Docs {
		if coversAny(doc.Covers, changed) {
			paths = append(paths, doc.Path)
		}
	}

	return paths
}

// coversAny relies on ParseDoc having validated every glob.
func coversAny(globs, files []string) bool {
	for _, glob := range globs {
		for _, file := range files {
			if doublestar.MatchUnvalidated(glob, file) {
				return true
			}
		}
	}

	return false
}
