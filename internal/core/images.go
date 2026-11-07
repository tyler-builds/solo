package core

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // decoders for ImportImage
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/tyler-builds/solo/internal/agent"
)

// Limits for images sent to Claude. The API takes up to 5 MB of base64 per image (about 3.75 MB of
// file) and scales anything past about 1568 px down itself, so larger images only cost upload time.
const (
	maxImageBytes = 3_500_000
	maxImageSide  = 2000
	maxImages     = 20 // per message
)

var imageTypes = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}

// attachmentsDir holds the images sent in messages, so a message keeps its images when the originals
// move.
func (c *Core) attachmentsDir() string { return filepath.Join(c.cfg.DataDir, "attachments") }

// SaveImage stores an image, such as one pasted from the clipboard, as an attachment and returns its
// path.
func (c *Core) SaveImage(img image.Image) (string, error) {
	data, ext, err := encodeImage(img)
	if err != nil {
		return "", err
	}
	return c.writeAttachment(data, ext)
}

// ImportImage stores a copy of an image file as an attachment and returns its path. An image already
// among the attachments is used as it is. A large image is scaled down to fit Claude's limits.
func (c *Core) ImportImage(path string) (string, error) {
	path = filepath.Clean(path)
	if filepath.Dir(path) == c.attachmentsDir() {
		if _, err := os.Stat(path); err != nil {
			return "", err
		}
		return path, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	mt := http.DetectContentType(data)
	ext, ok := imageTypes[mt]
	if !ok {
		return "", fmt.Errorf("%s isn't a PNG, JPEG, GIF or WebP image", filepath.Base(path))
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if len(data) > maxImageBytes || cfg.Width > maxImageSide || cfg.Height > maxImageSide {
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return "", fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		if data, ext, err = encodeImage(img); err != nil {
			return "", err
		}
	}
	return c.writeAttachment(data, ext)
}

// ImportImages imports each path (see ImportImage).
func (c *Core) ImportImages(paths []string) ([]string, error) {
	if len(paths) > maxImages {
		return nil, fmt.Errorf("at most %d images per message", maxImages)
	}
	var out []string
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		a, err := c.ImportImage(p)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (c *Core) writeAttachment(data []byte, ext string) (string, error) {
	dir := c.attachmentsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, agent.NewUUID()+ext)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// encodeImage scales img to fit maxImageSide and encodes it as PNG, or as JPEG when the PNG is too big
// (photos).
func encodeImage(img image.Image) ([]byte, string, error) {
	if img == nil {
		return nil, "", errors.New("no image")
	}
	b := img.Bounds()
	if w, h := b.Dx(), b.Dy(); w > maxImageSide || h > maxImageSide {
		scale := float64(maxImageSide) / float64(max(w, h))
		dst := image.NewRGBA(image.Rect(0, 0, max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
		img = dst
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, "", err
	}
	if buf.Len() <= maxImageBytes {
		return buf.Bytes(), ".png", nil
	}
	for _, q := range []int{90, 80, 65} {
		buf.Reset()
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return nil, "", err
		}
		if buf.Len() <= maxImageBytes {
			break
		}
	}
	return buf.Bytes(), ".jpg", nil
}

// loadImages reads image files as Claude takes them.
func (c *Core) loadImages(paths []string) ([]agent.Image, error) {
	if len(paths) > maxImages {
		return nil, fmt.Errorf("at most %d images per message", maxImages)
	}
	var out []agent.Image
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("image %s: %w", filepath.Base(p), err)
		}
		mt := http.DetectContentType(data)
		if _, ok := imageTypes[mt]; !ok {
			return nil, fmt.Errorf("%s isn't a PNG, JPEG, GIF or WebP image", filepath.Base(p))
		}
		if len(data) > maxImageBytes {
			return nil, fmt.Errorf("%s is larger than Claude takes", filepath.Base(p))
		}
		out = append(out, agent.Image{MediaType: mt, Data: base64.StdEncoding.EncodeToString(data)})
	}
	return out, nil
}
