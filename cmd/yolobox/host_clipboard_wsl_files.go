package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif" // register decoders for image.Decode
	_ "image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// maxPNGDimension caps the long side of a converted image. Claude Code
// downscales pasted images well below this anyway, and encoding a full-size
// camera photo as PNG is slow and large.
const maxPNGDimension = 4096

// imageMIMEForPath returns the image type of a copied file from its
// extension, or "" when it is not an image yolobox can serve.
func imageMIMEForPath(windowsPath string) string {
	switch strings.ToLower(path.Ext(strings.ReplaceAll(windowsPath, `\`, "/"))) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	case ".webp":
		return "image/webp"
	}
	return ""
}

var windowsDrivePath = regexp.MustCompile(`^([A-Za-z]):[\\/](.*)$`)

// windowsPathToWSL maps C:\dir\file to /mnt/c/dir/file. It is the fallback
// for when wslpath is unavailable, and assumes WSL's default automount root.
func windowsPathToWSL(windowsPath string) (string, error) {
	m := windowsDrivePath.FindStringSubmatch(windowsPath)
	if m == nil {
		return "", fmt.Errorf("not a drive path: %q", windowsPath)
	}
	return "/mnt/" + strings.ToLower(m[1]) + "/" + strings.ReplaceAll(m[2], `\`, "/"), nil
}

// wslpathToLinux asks wslpath, which honours a custom automount root, and
// falls back to the default /mnt/<drive> mapping.
func wslpathToLinux(ctx context.Context, windowsPath string) (string, error) {
	if out, err := exec.CommandContext(ctx, "wslpath", "-u", windowsPath).Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			return p, nil
		}
	}
	return windowsPathToWSL(windowsPath)
}

// readWindowsFile reads a copied file directly through the WSL mount when
// possible. Paths WSL cannot see (network shares, cloud placeholders) are read
// by powershell.exe instead, which is slower but needs no mapping.
func (c *wslClipboard) readWindowsFile(ctx context.Context, windowsPath string) ([]byte, error) {
	toLinux := c.toLinuxPath
	if toLinux == nil {
		toLinux = wslpathToLinux
	}
	if linuxPath, err := toLinux(ctx, windowsPath); err == nil {
		if info, err := os.Stat(linuxPath); err == nil && info.Mode().IsRegular() {
			if info.Size() > maxClipboardImageBytes {
				return nil, fmt.Errorf("copied file is %d MB, over the %d MB limit", info.Size()>>20, maxClipboardImageBytes>>20)
			}
			return os.ReadFile(linuxPath)
		}
	}

	payload := []byte(base64.StdEncoding.EncodeToString([]byte(windowsPath)))
	out, err := c.run(ctx, psReadFileScript, payload)
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, fmt.Errorf("powershell.exe returned an invalid file: %w", err)
	}
	if len(data) > maxClipboardImageBytes {
		return nil, fmt.Errorf("copied file is %d MB, over the %d MB limit", len(data)>>20, maxClipboardImageBytes>>20)
	}
	return data, nil
}

// convertImageToPNG decodes PNG, JPEG, GIF, BMP or WebP data and encodes it
// as PNG, scaling it down so its long side is at most maxPNGDimension.
func convertImageToPNG(data []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("cannot decode the copied image: %w", err)
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if long := max(w, h); long > maxPNGDimension {
		w = max(1, w*maxPNGDimension/long)
		h = max(1, h*maxPNGDimension/long)
		scaled := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), img, bounds, draw.Src, nil)
		img = scaled
	}

	var buf bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("cannot encode PNG: %w", err)
	}
	return buf.Bytes(), nil
}
