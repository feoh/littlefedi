package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media/objectstore"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// MaxPreviewSize bounds client-supplied posters independently of the original
// media file. Browser-generated posters are only a few dozen kilobytes.
const MaxPreviewSize int64 = 2 << 20

// SniffUpload identifies an upload from its bytes and returns a reader that
// includes the sniffed prefix. The multipart Content-Type is only a fallback
// when the standard library cannot identify the file.
func SniffUpload(r io.Reader, stated string) (string, io.Reader, error) {
	stated = strings.ToLower(strings.TrimSpace(strings.SplitN(stated, ";", 2)[0]))
	head := make([]byte, 512)
	n, err := io.ReadFull(r, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", nil, err
	}
	if n == 0 {
		return "", nil, fmt.Errorf("empty file")
	}
	mimeType := http.DetectContentType(head[:n])
	if mimeType == "application/octet-stream" && stated != "" {
		mimeType = stated
	}
	if mimeType == "image/svg+xml" || strings.HasPrefix(stated, "image/svg+xml") {
		return "", nil, fmt.Errorf("SVG uploads are not supported")
	}
	if !supportedUploadMIME(mimeType) {
		return "", nil, fmt.Errorf("unsupported media type")
	}
	return mimeType, io.MultiReader(bytes.NewReader(head[:n]), r), nil
}

// StoreUpload stages an upload for validation/image processing, then commits
// the original and generated preview to the configured object store.
func StoreUpload(ctx context.Context, store objectstore.Store, r io.Reader, size int64, m *models.MediaAttachment, filename string, stripEXIF bool) error {
	mimeType, reader, err := SniffUpload(r, m.MIMEType)
	if err != nil {
		return err
	}
	m.MIMEType = mimeType
	r = reader
	dir, err := os.MkdirTemp("", "littlefedi-media-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	name := filepath.Base(filename)
	ext := mimeToExt(mimeType)
	if ext == "" {
		ext = ".bin"
	}
	name = strings.TrimSuffix(name, filepath.Ext(name)) + ext
	full := filepath.Join(dir, name)
	f, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r)
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	if size >= 0 && n != size {
		return io.ErrUnexpectedEOF
	}
	m.URL = "/media/" + m.ID + "/" + name
	if strings.HasPrefix(m.MIMEType, "image/") || strings.HasPrefix(m.MIMEType, "audio/") {
		m.PreviewURL = m.URL
	} else {
		m.PreviewURL = ""
	}
	if err = PrepareUploadedImage(full, m.MIMEType, stripEXIF, m, dir); err != nil {
		return err
	}
	key := "media/" + m.ID + "/" + name
	orig, err := os.Open(full)
	if err != nil {
		return err
	}
	info, _ := orig.Stat()
	_, err = store.Put(ctx, key, orig, info.Size(), objectstore.PutOptions{ContentType: m.MIMEType, CacheControl: "public, max-age=31536000, immutable"})
	orig.Close()
	if err != nil {
		return err
	}
	m.StorageBackend = store.ID()
	m.StorageKey = key
	m.URL = store.URL(key)
	m.FileSize = info.Size()
	previewPath := findThumbnail(full)
	if previewPath != "" && previewPath != full {
		pk := "media/" + m.ID + "/" + filepath.Base(previewPath)
		pf, e := os.Open(previewPath)
		if e != nil {
			return e
		}
		pi, _ := pf.Stat()
		_, e = store.Put(ctx, pk, pf, pi.Size(), objectstore.PutOptions{ContentType: previewMIME(previewPath, m.MIMEType), CacheControl: "public, max-age=31536000, immutable"})
		pf.Close()
		if e != nil {
			return e
		}
		m.PreviewStorageKey = pk
		m.PreviewURL = store.URL(pk)
	} else if strings.HasPrefix(m.MIMEType, "image/") || strings.HasPrefix(m.MIMEType, "audio/") {
		m.PreviewURL = m.URL
	} else {
		m.PreviewURL = ""
	}
	return nil
}

func supportedUploadMIME(mimeType string) bool {
	switch mimeType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return strings.HasPrefix(mimeType, "video/") || strings.HasPrefix(mimeType, "audio/")
	}
}

// StorePreview validates and stores a client-supplied poster using the existing
// bounded image pipeline. Video decoding stays in the browser/API client.
func StorePreview(ctx context.Context, store objectstore.Store, r io.Reader, size int64, m *models.MediaAttachment, stripEXIF bool) error {
	if size <= 0 || size > MaxPreviewSize {
		return fmt.Errorf("thumbnail is too large")
	}
	mimeType, reader, err := SniffUpload(r, "")
	if err != nil {
		return err
	}
	if mimeType != "image/jpeg" && mimeType != "image/png" && mimeType != "image/webp" {
		return fmt.Errorf("thumbnail must be JPEG, PNG, or WebP")
	}

	dir, err := os.MkdirTemp("", "littlefedi-preview-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ext := mimeToExt(mimeType)
	full := filepath.Join(dir, "thumbnail"+ext)
	f, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, reader)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != size {
		return io.ErrUnexpectedEOF
	}

	scratch := &models.MediaAttachment{ID: m.ID, MIMEType: mimeType, URL: m.URL}
	if err := PrepareUploadedImage(full, mimeType, stripEXIF, scratch, dir); err != nil {
		return err
	}
	preview, err := os.Open(full)
	if err != nil {
		return err
	}
	info, err := preview.Stat()
	if err != nil {
		preview.Close()
		return err
	}
	key := "media/" + m.ID + "/thumbnail" + ext
	_, err = store.Put(ctx, key, preview, info.Size(), objectstore.PutOptions{ContentType: mimeType, CacheControl: "public, max-age=31536000, immutable"})
	preview.Close()
	if err != nil {
		return err
	}
	oldKey := m.PreviewStorageKey
	m.PreviewStorageKey = key
	m.PreviewURL = store.URL(key)
	m.Blurhash = scratch.Blurhash
	// Keep an image's original dimensions; a video's poster is the box shown
	// before playback and is the only size available here.
	if m.Type == "video" || m.Meta == "" {
		m.Meta = scratch.Meta
	}
	if oldKey != "" && oldKey != m.StorageKey && oldKey != key {
		_ = store.Delete(ctx, oldKey)
	}
	return nil
}

func previewMIME(name, fallback string) string {
	if strings.HasSuffix(strings.ToLower(name), ".jpg") {
		return "image/jpeg"
	}
	return fallback
}
