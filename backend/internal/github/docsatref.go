package github

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sync"
	"testing/fstest"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
)

const (
	// maxDocsAtRef bounds how many docs DocsAtRef fetches, one request each.
	maxDocsAtRef = 500
	// maxBlobCacheBytes bounds the blob contents the Client keeps in memory.
	maxBlobCacheBytes = 32 << 20
)

// blobCache holds blob contents by blob SHA, which is content-addressed, so a
// hit is always correct. When full it drops arbitrary entries to make room.
type blobCache struct {
	mu    sync.Mutex
	items map[string][]byte
	size  int
}

func (b *blobCache) get(sha string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.items[sha]
	return data, ok
}

func (b *blobCache) put(sha string, data []byte) {
	if len(data) > maxBlobCacheBytes {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.items[sha]; ok {
		return
	}
	if b.items == nil {
		b.items = map[string][]byte{}
	}
	for k, v := range b.items {
		if b.size+len(data) <= maxBlobCacheBytes {
			break
		}
		delete(b.items, k)
		b.size -= len(v)
	}
	b.items[sha] = data
	b.size += len(data)
}

// DocsAtRef returns the .md files under docs/ of owner/repo at ref as a file
// system rooted at the repo root. Files over docs.MaxDocBytes are left out. It
// errors when the docs tree is truncated, when the root tree is truncated without a docs entry, or holds more than maxDocsAtRef docs.
func (c *Client) DocsAtRef(ctx context.Context, installationID int64, owner, repo, ref string) (fs.FS, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("read docs of %s/%s at %s: %w", owner, repo, ref, err)
	}

	root, _, err := client.Git.GetTree(ctx, owner, repo, ref, false)
	if err != nil {
		return nil, fmt.Errorf("read docs of %s/%s at %s: get tree: %w", owner, repo, ref, err)
	}
	docsSHA := ""
	for _, e := range root.Entries {
		if e.GetPath() == "docs" && e.GetType() == "tree" {
			docsSHA = e.GetSHA()
			break
		}
	}
	files := fstest.MapFS{}
	if docsSHA == "" {
		if root.GetTruncated() {
			return nil, fmt.Errorf("read docs of %s/%s at %s: root tree is truncated and has no docs entry", owner, repo, ref)
		}
		return files, nil
	}

	tree, _, err := client.Git.GetTree(ctx, owner, repo, docsSHA, true)
	if err != nil {
		return nil, fmt.Errorf("read docs of %s/%s at %s: get docs tree: %w", owner, repo, ref, err)
	}
	if tree.GetTruncated() {
		return nil, fmt.Errorf("read docs of %s/%s at %s: docs tree is truncated", owner, repo, ref)
	}

	for _, e := range tree.Entries {
		p := e.GetPath()
		if e.GetType() != "blob" || e.GetMode() == "120000" || path.Ext(p) != ".md" || e.GetSize() > docs.MaxDocBytes {
			continue
		}
		p = "docs/" + p
		if len(files) == maxDocsAtRef {
			return nil, fmt.Errorf("read docs of %s/%s at %s: more than %d docs", owner, repo, ref, maxDocsAtRef)
		}
		src, ok := c.blobs.get(e.GetSHA())
		if !ok {
			src, _, err = client.Git.GetBlobRaw(ctx, owner, repo, e.GetSHA())
			if err != nil {
				return nil, fmt.Errorf("read docs of %s/%s at %s: get %s: %w", owner, repo, ref, p, err)
			}
			c.blobs.put(e.GetSHA(), src)
		}
		files[p] = &fstest.MapFile{Data: src}
	}
	return files, nil
}
