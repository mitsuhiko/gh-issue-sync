// Package termcolor provides ANSI color utilities with automatic fallback
// for terminals that don't support truecolor (24-bit) mode.
package termcolor

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ColorMode represents the terminal's color capability.
type ColorMode int

const (
	// ColorModeBasic is 16-color mode (not really used, we fall back to 256).
	ColorModeBasic ColorMode = iota
	// ColorMode256 is 256-color mode (xterm-256color).
	ColorMode256
	// ColorModeTruecolor is 24-bit RGB mode.
	ColorModeTruecolor
)

// Color represents an RGB color.
type Color struct {
	R, G, B uint8
}

// ANSI escape codes
const (
	reset         = "\x1b[0m"
	fgReset       = "\x1b[39m"
	bgReset       = "\x1b[49m"
	bold          = "\x1b[1m"
	dim           = "\x1b[2m"
	italic        = "\x1b[3m"
	underline     = "\x1b[4m"
	strikethrough = "\x1b[9m"
	boldOff       = "\x1b[22m"
	italicOff     = "\x1b[23m"
	underlineOff  = "\x1b[24m"
	strikeOff     = "\x1b[29m"
)

// The 6x6x6 color cube channel values (indices 0-5)
var cubeValues = [6]uint8{0, 95, 135, 175, 215, 255}

// Grayscale ramp values (indices 232-255, 24 grays from 8 to 238)
var grayValues [24]uint8

func init() {
	for i := 0; i < 24; i++ {
		grayValues[i] = uint8(8 + i*10)
	}
}

// DetectColorMode returns the terminal's color capability based on environment.
func DetectColorMode() ColorMode {
	// Check COLORTERM first (most reliable)
	colorterm := os.Getenv("COLORTERM")
	if colorterm == "truecolor" || colorterm == "24bit" {
		return ColorModeTruecolor
	}

	// Windows Terminal supports truecolor
	if os.Getenv("WT_SESSION") != "" {
		return ColorModeTruecolor
	}

	// iTerm2 supports truecolor
	if os.Getenv("TERM_PROGRAM") == "iTerm.app" {
		return ColorModeTruecolor
	}

	// VS Code terminal supports truecolor
	if os.Getenv("TERM_PROGRAM") == "vscode" {
		return ColorModeTruecolor
	}

	// Check TERM for 256color support
	term := os.Getenv("TERM")
	if strings.Contains(term, "256color") || strings.Contains(term, "truecolor") {
		// Many modern terminals advertise 256color but actually support truecolor
		// We could be more aggressive here, but 256 is a safe fallback
		return ColorMode256
	}

	// Default to 256 colors (most terminals support this)
	return ColorMode256
}

// ParseHex parses a hex color string (with or without #) into a Color.
func ParseHex(hex string) (Color, error) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) == 3 {
		// Expand shorthand: "abc" -> "aabbcc"
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	}
	if len(hex) != 6 {
		return Color{}, fmt.Errorf("invalid hex color: %s", hex)
	}
	r, err := strconv.ParseUint(hex[0:2], 16, 8)
	if err != nil {
		return Color{}, fmt.Errorf("invalid hex color: %s", hex)
	}
	g, err := strconv.ParseUint(hex[2:4], 16, 8)
	if err != nil {
		return Color{}, fmt.Errorf("invalid hex color: %s", hex)
	}
	b, err := strconv.ParseUint(hex[4:6], 16, 8)
	if err != nil {
		return Color{}, fmt.Errorf("invalid hex color: %s", hex)
	}
	return Color{R: uint8(r), G: uint8(g), B: uint8(b)}, nil
}

// MustParseHex parses a hex color string, panicking on error.
func MustParseHex(hex string) Color {
	c, err := ParseHex(hex)
	if err != nil {
		panic(err)
	}
	return c
}

// RGB creates a Color from RGB values.
func RGB(r, g, b uint8) Color {
	return Color{R: r, G: g, B: b}
}

// Hex returns the color as a hex string with # prefix.
func (c Color) Hex() string {
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

// findClosestCubeIndex finds the closest index in the 6x6x6 color cube for a value.
func findClosestCubeIndex(value uint8) int {
	minDist := 256
	minIdx := 0
	for i, cv := range cubeValues {
		dist := int(value) - int(cv)
		if dist < 0 {
			dist = -dist
		}
		if dist < minDist {
			minDist = dist
			minIdx = i
		}
	}
	return minIdx
}

// findClosestGrayIndex finds the closest grayscale index for a value.
func findClosestGrayIndex(gray uint8) int {
	minDist := 256
	minIdx := 0
	for i, gv := range grayValues {
		dist := int(gray) - int(gv)
		if dist < 0 {
			dist = -dist
		}
		if dist < minDist {
			minDist = dist
			minIdx = i
		}
	}
	return minIdx
}

// colorDistance calculates weighted Euclidean distance (human eye is more sensitive to green).
func colorDistance(r1, g1, b1, r2, g2, b2 uint8) float64 {
	dr := float64(int(r1) - int(r2))
	dg := float64(int(g1) - int(g2))
	db := float64(int(b1) - int(b2))
	return dr*dr*0.299 + dg*dg*0.587 + db*db*0.114
}

// To256 converts an RGB color to the closest 256-color palette index.
func (c Color) To256() uint8 {
	// Find closest color in the 6x6x6 cube (indices 16-231)
	rIdx := findClosestCubeIndex(c.R)
	gIdx := findClosestCubeIndex(c.G)
	bIdx := findClosestCubeIndex(c.B)
	cubeR := cubeValues[rIdx]
	cubeG := cubeValues[gIdx]
	cubeB := cubeValues[bIdx]
	cubeIndex := uint8(16 + 36*rIdx + 6*gIdx + bIdx)
	cubeDist := colorDistance(c.R, c.G, c.B, cubeR, cubeG, cubeB)

	// Find closest grayscale (indices 232-255)
	gray := uint8(float64(c.R)*0.299 + float64(c.G)*0.587 + float64(c.B)*0.114)
	grayIdx := findClosestGrayIndex(gray)
	grayValue := grayValues[grayIdx]
	grayIndex := uint8(232 + grayIdx)
	grayDist := colorDistance(c.R, c.G, c.B, grayValue, grayValue, grayValue)

	// Check if color has noticeable saturation
	maxC := c.R
	if c.G > maxC {
		maxC = c.G
	}
	if c.B > maxC {
		maxC = c.B
	}
	minC := c.R
	if c.G < minC {
		minC = c.G
	}
	if c.B < minC {
		minC = c.B
	}
	spread := int(maxC) - int(minC)

	// Only consider grayscale if color is nearly neutral (spread < 10) AND grayscale is closer
	if spread < 10 && grayDist < cubeDist {
		return grayIndex
	}

	return cubeIndex
}

// Styler provides methods for styling text with ANSI colors.
type Styler struct {
	mode ColorMode
}

// NewStyler creates a new Styler with the given color mode.
func NewStyler(mode ColorMode) *Styler {
	return &Styler{mode: mode}
}

// DefaultStyler creates a Styler with auto-detected color mode.
func DefaultStyler() *Styler {
	return NewStyler(DetectColorMode())
}

// fgAnsi returns the ANSI escape sequence for a foreground color.
func (s *Styler) fgAnsi(c Color) string {
	if s.mode == ColorModeTruecolor {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.R, c.G, c.B)
	}
	return fmt.Sprintf("\x1b[38;5;%dm", c.To256())
}

// bgAnsi returns the ANSI escape sequence for a background color.
func (s *Styler) bgAnsi(c Color) string {
	if s.mode == ColorModeTruecolor {
		return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", c.R, c.G, c.B)
	}
	return fmt.Sprintf("\x1b[48;5;%dm", c.To256())
}

// Fg returns text with the given foreground color.
func (s *Styler) Fg(c Color, text string) string {
	return s.fgAnsi(c) + text + fgReset
}

// Bg returns text with the given background color.
func (s *Styler) Bg(c Color, text string) string {
	return s.bgAnsi(c) + text + bgReset
}

// FgBg returns text with both foreground and background colors.
func (s *Styler) FgBg(fg, bg Color, text string) string {
	return s.fgAnsi(fg) + s.bgAnsi(bg) + text + reset
}

// FgHex returns text with a foreground color from a hex string.
func (s *Styler) FgHex(hex string, text string) string {
	c, err := ParseHex(hex)
	if err != nil {
		return text
	}
	return s.Fg(c, text)
}

// BgHex returns text with a background color from a hex string.
func (s *Styler) BgHex(hex string, text string) string {
	c, err := ParseHex(hex)
	if err != nil {
		return text
	}
	return s.Bg(c, text)
}

// Bold returns text with bold styling.
func (s *Styler) Bold(text string) string {
	return bold + text + boldOff
}

// Dim returns text with dim styling.
func (s *Styler) Dim(text string) string {
	return dim + text + boldOff
}

// Italic returns text with italic styling.
func (s *Styler) Italic(text string) string {
	return italic + text + italicOff
}

// Underline returns text with underline styling.
func (s *Styler) Underline(text string) string {
	return underline + text + underlineOff
}

// Strikethrough returns text with strikethrough styling.
func (s *Styler) Strikethrough(text string) string {
	return strikethrough + text + strikeOff
}

// FgStrikethrough returns text with foreground color and strikethrough.
func (s *Styler) FgStrikethrough(c Color, text string) string {
	return s.fgAnsi(c) + strikethrough + text + strikeOff + fgReset
}

// FgUnderline returns text with foreground color and underline.
func (s *Styler) FgUnderline(c Color, text string) string {
	return s.fgAnsi(c) + underline + text + underlineOff + fgReset
}

// Reset returns the ANSI reset sequence.
func (s *Styler) Reset() string {
	return reset
}

// Mode returns the detected color mode.
func (s *Styler) Mode() ColorMode {
	return s.mode
}

// IsTruecolor returns true if the terminal supports 24-bit color.
func (s *Styler) IsTruecolor() bool {
	return s.mode == ColorModeTruecolor
}
