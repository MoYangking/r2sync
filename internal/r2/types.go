package r2

import (
	"context"
	"io"
	"time"
)

type Object struct {
	Key          string
	Exists       bool
	Size         int64
	SHA256       string
	ETag         string
	LastModified time.Time
	Metadata     map[string]string
}

type ObjectStore interface {
	Head(ctx context.Context, key string) (Object, bool, error)
	Download(ctx context.Context, key string, w io.Writer) (Object, error)
	Upload(ctx context.Context, key string, filePath string, metadata map[string]string) (Object, error)
	Delete(ctx context.Context, key string) error
}
