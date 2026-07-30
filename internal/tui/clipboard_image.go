package tui

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gigovich/aigem/internal/llm"
)

const (
	maxClipboardImageBytes = 20 * 1024 * 1024
	imageMarkerPrefix      = "[image:"
)

var imageMarkerRE = regexp.MustCompile(`\s*\[image:\d+\]\s*`)

type imagePasteMsg struct {
	image llm.Image
	err   error
}

func pasteImageFromClipboardCmd() tea.Cmd {
	return func() tea.Msg {
		img, err := readClipboardImage()
		return imagePasteMsg{image: img, err: err}
	}
}

func readClipboardImage() (llm.Image, error) {
	data, mediaType, err := clipboardImageBytes()
	if err != nil {
		return llm.Image{}, err
	}
	if len(data) == 0 {
		return llm.Image{}, errors.New("clipboard does not contain an image")
	}
	if len(data) > maxClipboardImageBytes {
		return llm.Image{}, fmt.Errorf("clipboard image is too large (%d MB, max %d MB)", len(data)/(1024*1024), maxClipboardImageBytes/(1024*1024))
	}
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}
	if !isSupportedImageType(mediaType) {
		return llm.Image{}, fmt.Errorf("clipboard image type %q is not supported", mediaType)
	}
	return llm.Image{MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(data)}, nil
}

func clipboardImageBytes() ([]byte, string, error) {
	switch runtime.GOOS {
	case "darwin":
		return runFirstClipboardCommand([]clipboardCommand{
			{name: "pngpaste", args: []string{"-"}, mediaType: "image/png"},
			{name: "osascript", args: []string{"-l", "JavaScript", "-e", macOSClipboardImageScript}, mediaType: "image/png"},
		})
	case "linux":
		return runFirstClipboardCommand([]clipboardCommand{
			{name: "wl-paste", args: []string{"--no-newline", "--type", "image/png"}, mediaType: "image/png"},
			{name: "xclip", args: []string{"-selection", "clipboard", "-t", "image/png", "-o"}, mediaType: "image/png"},
		})
	case "windows":
		return runFirstClipboardCommand([]clipboardCommand{
			{name: "powershell", args: []string{"-NoProfile", "-STA", "-Command", powershellClipboardImageScript}, mediaType: "image/png"},
			{name: "pwsh", args: []string{"-NoProfile", "-STA", "-Command", powershellClipboardImageScript}, mediaType: "image/png"},
		})
	default:
		return nil, "", fmt.Errorf("image clipboard is not supported on %s", runtime.GOOS)
	}
}

type clipboardCommand struct {
	name      string
	args      []string
	mediaType string
}

func runFirstClipboardCommand(cmds []clipboardCommand) ([]byte, string, error) {
	var lastErr error
	for _, c := range cmds {
		if _, err := exec.LookPath(c.name); err != nil {
			lastErr = err
			continue
		}
		out, err := exec.Command(c.name, c.args...).Output()
		if err != nil {
			lastErr = err
			continue
		}
		if len(out) == 0 {
			lastErr = errors.New("clipboard command returned no image data")
			continue
		}
		return out, c.mediaType, nil
	}
	if lastErr != nil {
		return nil, "", fmt.Errorf("could not read image clipboard: %w", lastErr)
	}
	return nil, "", errors.New("no image clipboard command is available")
}

func isSupportedImageType(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

const macOSClipboardImageScript = `ObjC.import('AppKit'); const pb = $.NSPasteboard.generalPasteboard; let data = pb.dataForType('public.png'); if (!data || data.isNil()) { const image = $.NSImage.alloc.initWithPasteboard(pb); if (!image || image.isNil()) { $.exit(2); } const tiff = image.TIFFRepresentation; if (!tiff || tiff.isNil()) { $.exit(2); } const rep = $.NSBitmapImageRep.imageRepWithData(tiff); if (!rep || rep.isNil()) { $.exit(2); } data = rep.representationUsingTypeProperties($.NSBitmapImageFileTypePNG, $()); } if (!data || data.isNil() || data.length === 0) { $.exit(2); } $.NSFileHandle.fileHandleWithStandardOutput.writeData(data);`

const powershellClipboardImageScript = `Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; $img=[Windows.Forms.Clipboard]::GetImage(); if ($null -eq $img) { exit 2 }; $ms=New-Object System.IO.MemoryStream; $img.Save($ms,[System.Drawing.Imaging.ImageFormat]::Png); $bytes=$ms.ToArray(); [Console]::OpenStandardOutput().Write($bytes,0,$bytes.Length)`

func imagesLabel(n int) string {
	if n == 1 {
		return "1 image"
	}
	return fmt.Sprintf("%d images", n)
}

func imageMarker(n int) string {
	return fmt.Sprintf("%s%d]", imageMarkerPrefix, n)
}

func stripImageMarkers(text string) string {
	return strings.TrimSpace(imageMarkerRE.ReplaceAllString(text, " "))
}

func userTextWithImages(text string, n int) string {
	if n <= 0 || text != "" {
		return text
	}
	return "[" + imagesLabel(n) + "]"
}
