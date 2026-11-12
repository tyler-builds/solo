//go:build !js

package core

import (
	"bytes"
	"context"
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
	// maxImageFile and maxImagePixels bound an image before it is decoded to be scaled down: a file's
	// header can claim any size, and decoding allocates four bytes for each pixel it claims.
	maxImageFile   = 16 << 20
	maxImagePixels = 50_000_000
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
	if c.isAttachment(path) {
		if _, err := os.Stat(path); err != nil {
			return "", err
		}
		return path, nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.Size() > maxImageFile {
		return "", fmt.Errorf("%s is larger than %d MB", filepath.Base(path), maxImageFile>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return c.saveImageData(data, filepath.Base(path))
}

// SaveImageData stores an image file's contents as an attachment and returns its path. A large image
// is scaled down to fit Claude's limits.
func (c *Core) SaveImageData(_ context.Context, data []byte) (string, error) {
	return c.saveImageData(data, "The image")
}

// saveImageData checks that data is an image Claude takes, scales it down when it is too large, and
// stores it. name is what errors call it.
func (c *Core) saveImageData(data []byte, name string) (string, error) {
	if len(data) > maxImageFile {
		return "", fmt.Errorf("%s is larger than %d MB", name, maxImageFile>>20)
	}
	mt := http.DetectContentType(data)
	ext, ok := imageTypes[mt]
	if !ok {
		return "", fmt.Errorf("%s isn't a PNG, JPEG, GIF or WebP image", name)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxImagePixels/cfg.Height {
		return "", fmt.Errorf("%s has too many pixels to scale down", name)
	}
	if len(data) > maxImageBytes || cfg.Width > maxImageSide || cfg.Height > maxImageSide {
		err := decodeImage(data, func(img image.Image) (err error) {
			data, ext, err = encodeImage(img)
			return err
		})
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
	}
	return c.writeAttachment(data, ext)
}

// isAttachment reports whether path names a file directly in the attachments folder. Images reach
// Claude and the UI by path, and only these paths are read: a message can't name another file.
func (c *Core) isAttachment(path string) bool {
	// In its plain spelling only: an attachment is found again by its path (a thread's are removed
	// with it, unless another message has the same), and two spellings of one file would be two files.
	return filepath.IsAbs(path) && path == filepath.Clean(path) && filepath.Dir(path) == filepath.Clean(c.attachmentsDir())
}

// ReadImage returns the contents of an attachment, by the path SaveImage, SaveImageData or
// ImportImage returned. With maxSide above zero, an image with a longer side is scaled down to it,
// for a preview that costs less to send and to decode.
func (c *Core) ReadImage(_ context.Context, path string, maxSide int) ([]byte, error) {
	if !c.isAttachment(path) {
		return nil, errors.New("not an attachment")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("the image is gone")
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxImageFile {
		return nil, errors.New("not an attachment")
	}
	data, err := os.ReadFile(path)
	if err != nil || maxSide <= 0 {
		return data, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if cfg.Width <= maxSide && cfg.Height <= maxSide {
		return data, nil
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxImagePixels/cfg.Height {
		return nil, errors.New("the image has too many pixels to scale down")
	}
	var out []byte
	err = decodeImage(data, func(img image.Image) (err error) {
		out, err = encodePreview(scaleTo(img, maxSide))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return out, nil
}

// imageWork lets one image be decoded and scaled at a time. A decoded image is four bytes a pixel, up
// to 200 MB, and scaling needs more: several at once, as a device could ask for, would take the
// machine's memory.
var imageWork = make(chan struct{}, 1)

// decodeImage decodes data, one image at a time across Solo, and gives it to f while it holds the turn.
func decodeImage(data []byte, f func(image.Image) error) error {
	imageWork <- struct{}{}
	defer func() { <-imageWork }()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	return f(img)
}

// scaleTo returns img scaled so that its longer side is at most side. A small image is scaled with the
// better filter; a large one with the cheaper one, whose working memory doesn't grow with the source
// (the better filter's is 32 bytes for each pixel of the result's width times the source's height).
func scaleTo(img image.Image, side int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= side && h <= side {
		return img
	}
	scale := float64(side) / float64(max(w, h))
	dst := image.NewRGBA(image.Rect(0, 0, max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))))
	if int64(dst.Bounds().Dx())*int64(h) <= 2_000_000 {
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	} else {
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	}
	return dst
}

// encodePreview encodes a scaled image: as JPEG, or as PNG when it has transparency.
func encodePreview(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if o, ok := img.(interface{ Opaque() bool }); ok && !o.Opaque() {
		err := png.Encode(&buf, img)
		return buf.Bytes(), err
	}
	err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85})
	return buf.Bytes(), err
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, agent.NewUUID()+ext)
	if err := os.WriteFile(p, data, 0o600); err != nil {
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
	img = scaleTo(img, maxImageSide)
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
		if !c.isAttachment(p) {
			return nil, fmt.Errorf("image %s isn't an attachment", filepath.Base(p))
		}
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
