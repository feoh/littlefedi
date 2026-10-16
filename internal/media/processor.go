// Package media provides image processing: thumbnail generation, blurhash computation,
// and remote media caching.
package media

import (
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bbrks/go-blurhash"
	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp" // register WebP with image.DecodeConfig/imaging

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// ThumbnailMaxWidth is the maximum width for preview thumbnails.
const ThumbnailMaxWidth = 600

// ThumbnailMaxHeight is the maximum height for preview thumbnails.
const ThumbnailMaxHeight = 600

// BlurhashComponentX is the X component count for blurhash.
const BlurhashComponentX = 4

// BlurhashComponentY is the Y component count for blurhash.
const BlurhashComponentY = 3

// Bound decoded memory even when compressed images fit within max_file_size.
const (
	defaultMaxImagePixels    int64 = 16_000_000
	defaultMaxImageDimension       = 0
)

var processingLimits = struct {
	sync.RWMutex
	maxPixels    int64
	maxDimension int
	slots        chan struct{}
}{
	maxPixels:    defaultMaxImagePixels,
	maxDimension: defaultMaxImageDimension,
	slots:        make(chan struct{}, 1),
}

// FullSizeJPEGQuality is the quality used when a stored JPEG is rewritten
// (EXIF strip and/or downscale). 88 is visually indistinguishable from 95 at
// normal viewing sizes and roughly halves the stored file.
const FullSizeJPEGQuality = 88

// maxStoredDimension caps the longest side of stored JPEG/PNG uploads. 0 keeps
// the original resolution. Set once at startup.
var maxStoredDimension int

// ConfigureStoredDimension sets the longest-side cap applied to stored uploads.
// A non-positive value stores originals untouched. It must be called during
// startup, before image work begins.
func ConfigureStoredDimension(px int) {
	maxStoredDimension = max(px, 0)
}

// resampleFilter is the filter used for thumbnail resizing. Lanczos is the
// highest quality and by far the most CPU-expensive filter in imaging; the
// low-power preset selects a faster one. Set once at startup.
var resampleFilter = imaging.Lanczos

// ConfigureResampleFilter selects the thumbnail resampling filter by name
// ("lanczos", "catmullrom", "linear", "box"). Empty or unknown names keep the
// Lanczos default. It must be called during startup, before image work begins.
func ConfigureResampleFilter(name string) {
	switch strings.ToLower(name) {
	case "", "lanczos":
		resampleFilter = imaging.Lanczos
	case "catmullrom":
		resampleFilter = imaging.CatmullRom
	case "linear":
		resampleFilter = imaging.Linear
	case "box":
		resampleFilter = imaging.Box
	default:
		slog.Warn("media: unknown resample_filter, keeping lanczos", "value", name)
		resampleFilter = imaging.Lanczos
	}
}

// ConfigureProcessingLimits sets process-wide decoded-image and concurrency
// limits. It must be called during startup, before image work begins.
// A non-positive maxPixels or maxDimension disables that bound.
func ConfigureProcessingLimits(maxPixels int64, maxDimension, concurrency int) {
	maxPixels = max(maxPixels, 0)
	maxDimension = max(maxDimension, 0)
	if concurrency <= 0 {
		concurrency = 1
	}
	processingLimits.Lock()
	processingLimits.maxPixels = maxPixels
	processingLimits.maxDimension = maxDimension
	processingLimits.slots = make(chan struct{}, concurrency)
	processingLimits.Unlock()
}

func acquireProcessingSlot() func() {
	processingLimits.RLock()
	slots := processingLimits.slots
	processingLimits.RUnlock()
	slots <- struct{}{}
	return func() { <-slots }
}

func validateImageDimensions(path string) (image.Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return image.Config{}, fmt.Errorf("open image header: %w", err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return image.Config{}, fmt.Errorf("decode image header: %w", err)
	}
	processingLimits.RLock()
	maxPixels, maxDimension := processingLimits.maxPixels, processingLimits.maxDimension
	processingLimits.RUnlock()
	// A zero bound is disabled; the other one still applies.
	if cfg.Width <= 0 || cfg.Height <= 0 ||
		(maxDimension > 0 && (cfg.Width > maxDimension || cfg.Height > maxDimension)) ||
		(maxPixels > 0 && int64(cfg.Width) > maxPixels/int64(cfg.Height)) {
		limits := "0"
		switch {
		case maxPixels > 0 && maxDimension > 0:
			limits = fmt.Sprintf("%d pixels, %d per side", maxPixels, maxDimension)
		case maxPixels > 0:
			limits = fmt.Sprintf("%d pixels", maxPixels)
		case maxDimension > 0:
			limits = fmt.Sprintf("%d per side", maxDimension)
		}
		return image.Config{}, fmt.Errorf("image dimensions %dx%d exceed limits (%s)",
			cfg.Width, cfg.Height, limits)
	}
	return cfg, nil
}

// generateThumbnail creates a resized preview image next to the original.
// Returns the URL path to the thumbnail, or an error.
func generateThumbnail(img image.Image, media *models.MediaAttachment, fullPath, mediaPath string) (string, error) {
	// Resize to fit within max dimensions while maintaining aspect ratio
	thumb := imaging.Fit(img, ThumbnailMaxWidth, ThumbnailMaxHeight, resampleFilter)

	// Determine output filename
	ext := filepath.Ext(fullPath)
	base := strings.TrimSuffix(fullPath, ext)
	thumbFilename := base + "_small" + ext

	// Encode based on original format (default to JPEG for unknown)
	switch strings.ToLower(ext) {
	case ".png":
		if err := imaging.Save(thumb, thumbFilename, imaging.PNGCompressionLevel(png.DefaultCompression)); err != nil {
			return "", fmt.Errorf("save png thumbnail: %w", err)
		}
	case ".gif":
		// For GIFs, save as JPEG thumbnail (GIF encoding is lossy anyway)
		thumbFilename = base + "_small.jpg"
		if err := imaging.Save(thumb, thumbFilename, imaging.JPEGQuality(85)); err != nil {
			return "", fmt.Errorf("save gif thumbnail: %w", err)
		}
	case ".webp":
		// imaging can decode WebP via x/image/webp, but it has no WebP encoder.
		thumbFilename = base + "_small.jpg"
		if err := imaging.Save(thumb, thumbFilename, imaging.JPEGQuality(85)); err != nil {
			return "", fmt.Errorf("save webp thumbnail: %w", err)
		}
	default:
		// JPEG or others
		if err := imaging.Save(thumb, thumbFilename, imaging.JPEGQuality(85)); err != nil {
			return "", fmt.Errorf("save jpeg thumbnail: %w", err)
		}
	}

	// Compute the URL path from the thumbnail filename
	relPath, err := filepath.Rel(mediaPath, thumbFilename)
	if err != nil {
		return "", fmt.Errorf("compute thumbnail rel path: %w", err)
	}

	return "/media/" + filepath.ToSlash(relPath), nil
}

// generateBlurhash computes a blurhash string from an image.
func generateBlurhash(img image.Image) (string, error) {
	bounds := img.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()

	// For very large images, downsample before computing blurhash to keep it
	// fast. Box is enough here: blurhash output is a handful of DCT
	// components, perceptually insensitive to resampling quality.
	src := img
	if w > 200 || h > 200 {
		src = imaging.Fit(img, 200, 200, imaging.Box)
	}

	hash, err := blurhash.Encode(BlurhashComponentX, BlurhashComponentY, src)
	if err != nil {
		return "", fmt.Errorf("encode blurhash: %w", err)
	}

	return hash, nil
}

// AverageColour decodes a blurhash down to the single pixel it averages to,
// formatted as a CSS hex colour. It is what a media box is painted with while
// the picture itself is still on the wire: a blurred placeholder would cost a
// data URI per attachment on every page, and the average tone gets most of the
// way there for six characters. An unusable hash returns false, and the caller
// is expected to leave the box alone rather than invent a colour.
func AverageColour(hash string) (string, bool) {
	if hash == "" {
		return "", false
	}
	// Decoding to a single pixel would sample the basis functions at one
	// corner rather than average them, which comes out noticeably brighter
	// than the picture. A 4x4 decode averaged by hand costs the same handful
	// of microseconds and lands on the colour the eye expects.
	const grid = 4
	img, err := blurhash.Decode(hash, grid, grid, 1)
	if err != nil {
		return "", false
	}
	var sr, sg, sb uint64
	for y := 0; y < grid; y++ {
		for x := 0; x < grid; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			sr, sg, sb = sr+uint64(r), sg+uint64(g), sb+uint64(b)
		}
	}
	// 16-bit channels out of image.Image, and CSS wants 8.
	const pixels = grid * grid
	return fmt.Sprintf("#%02x%02x%02x", sr/pixels>>8, sg/pixels>>8, sb/pixels>>8), true
}

// isImageMIME returns true if the MIME type represents a processable image.
func isImageMIME(mimeType string) bool {
	return strings.HasPrefix(mimeType, "image/") &&
		!strings.HasPrefix(mimeType, "image/svg") &&
		!strings.HasPrefix(mimeType, "image/gif")
}

func isRasterImageMIME(mimeType string) bool {
	return strings.HasPrefix(mimeType, "image/") && !strings.HasPrefix(mimeType, "image/svg")
}

// PrepareUploadedImage runs the full image-ingest pipeline (dimension
// validation, optional downscale to the stored-dimension cap, optional EXIF
// strip, thumbnail and blurhash) around a single decode and a single
// processing slot. GIFs are validated but left untouched so animation is
// preserved.
// Thumbnail and blurhash failures are non-fatal; validation and decode
// failures are returned.
func PrepareUploadedImage(fullPath, mimeType string, stripEXIF bool, media *models.MediaAttachment, mediaPath string) error {
	if !isRasterImageMIME(mimeType) {
		return nil
	}
	release := acquireProcessingSlot()
	defer release()

	cfg, err := validateImageDimensions(fullPath)
	if err != nil {
		return err
	}
	if !isImageMIME(mimeType) {
		media.Meta = fmt.Sprintf(`{"width":%d,"height":%d}`, cfg.Width, cfg.Height)
		return nil
	}

	// Single decode, with the EXIF orientation tag baked into the pixel data.
	src, err := imaging.Open(fullPath, imaging.AutoOrientation(true))
	if err != nil {
		return fmt.Errorf("open image: %w", err)
	}

	// Downscale before the rewrite so both share one encode. imaging.Fit is a
	// no-op when the image already fits, so only oversized uploads pay for it.
	bounds := src.Bounds()
	resized := false
	if maxStoredDimension > 0 && rewritableFormat(fullPath) &&
		(bounds.Dx() > maxStoredDimension || bounds.Dy() > maxStoredDimension) {
		src = imaging.Fit(src, maxStoredDimension, maxStoredDimension, resampleFilter)
		resized = true
	}

	if resized || stripEXIF {
		if err := rewriteStoredImage(src, fullPath); err != nil {
			// The original file is still intact (the rewrite is atomic), so keep
			// describing it rather than the version that failed to land.
			// Thumbnail and blurhash still come from the downscaled pixels; only
			// the reported dimensions must match what is actually stored.
			slog.Warn("media: stored image rewrite failed", "path", fullPath, "resized", resized, "error", err)
		} else {
			bounds = src.Bounds()
		}
	}

	if thumbnailPath, err := generateThumbnail(src, media, fullPath, mediaPath); err != nil {
		slog.Warn("media: thumbnail generation failed", "media_id", media.ID, "error", err)
		media.PreviewURL = media.URL
	} else {
		media.PreviewURL = thumbnailPath
	}
	if blur, err := generateBlurhash(src); err != nil {
		slog.Warn("media: blurhash generation failed", "media_id", media.ID, "error", err)
	} else {
		media.Blurhash = blur
	}
	media.Meta = fmt.Sprintf(`{"width":%d,"height":%d}`, bounds.Dx(), bounds.Dy())
	return nil
}

// rewritableFormat reports whether the stored file can be re-encoded in place.
// Only JPEG and PNG qualify: imaging has no WebP encoder, and re-encoding a GIF
// would flatten the animation.
func rewritableFormat(fullPath string) bool {
	switch strings.ToLower(filepath.Ext(fullPath)) {
	case ".png", ".jpg", ".jpeg":
		return true
	default:
		return false
	}
}

// rewriteStoredImage writes the processed image back over the stored file,
// carrying any downscale and dropping all metadata (Go's encoders carry no
// EXIF; orientation was already baked into the pixels by the AutoOrientation
// decode). Formats it cannot re-encode are left alone.
//
// The encode goes to a sibling temp file and is renamed into place, so a failed
// or partial write leaves the original upload intact instead of truncated.
func rewriteStoredImage(src image.Image, fullPath string) error {
	if !rewritableFormat(fullPath) {
		return nil
	}
	// The extension has to survive: imaging picks the encoder from it.
	ext := filepath.Ext(fullPath)
	tmp := strings.TrimSuffix(fullPath, ext) + ".rewrite" + ext
	var err error
	if strings.EqualFold(ext, ".png") {
		err = imaging.Save(src, tmp, imaging.PNGCompressionLevel(png.DefaultCompression))
	} else {
		err = imaging.Save(src, tmp, imaging.JPEGQuality(FullSizeJPEGQuality))
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, fullPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// MediaFilename returns a generic filename for a stored media file based on MIME type.
// If rename is false, it returns the sanitized original filename.
// If rename is true, it returns a clean generic name like "image.jpg" or "video.mp4".
func MediaFilename(originalName, mimeType string, rename bool) string {
	if !rename {
		// Sanitize the original filename (same logic as before)
		cleanName := filepath.Base(originalName)
		cleanName = strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
				return r
			}
			return '-'
		}, cleanName)
		if cleanName == "." || cleanName == "" {
			cleanName = "upload"
		}
		return cleanName
	}

	// Generate a generic name based on MIME type
	ext := mimeToExt(mimeType)
	if ext == "" {
		ext = ".bin"
	}

	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "image" + ext
	case strings.HasPrefix(mimeType, "video/"):
		return "video" + ext
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio" + ext
	default:
		return "file" + ext
	}
}
