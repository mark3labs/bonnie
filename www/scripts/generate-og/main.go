// Run from www with: go run ./scripts/generate-og
// Generate the shared social card from the BONNIE logo without external fonts.
package main

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"os"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

func main() {
	if err := generate(); err != nil {
		log.Fatal(err)
	}
}

func generate() error {
	file, err := os.Open("public/logo.png")
	if err != nil {
		return err
	}
	logo, err := png.Decode(file)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	card := image.NewRGBA(image.Rect(0, 0, 1200, 630))
	navy := color.RGBA{8, 11, 32, 255}
	cyan := color.RGBA{0, 203, 234, 255}
	pink := color.RGBA{237, 33, 170, 255}
	white := color.RGBA{244, 245, 255, 255}
	draw.Draw(card, card.Bounds(), image.NewUniform(navy), image.Point{}, draw.Src)
	for y := 0; y < 630; y += 42 {
		draw.Draw(card, image.Rect(0, y, 1200, y+1), image.NewUniform(color.RGBA{16, 21, 45, 255}), image.Point{}, draw.Src)
	}
	draw.Draw(card, image.Rect(0, 0, 1200, 8), image.NewUniform(cyan), image.Point{}, draw.Src)
	draw.Draw(card, image.Rect(0, 622, 1200, 630), image.NewUniform(pink), image.Point{}, draw.Src)
	// Nearest-neighbor scaling keeps the logo's pixel artwork sharp.
	for y := range 500 {
		for x := range 500 {
			c := logo.At(logo.Bounds().Min.X+x*logo.Bounds().Dx()/500, logo.Bounds().Min.Y+y*logo.Bounds().Dy()/500)
			draw.Draw(card, image.Rect(36+x, 65+y, 37+x, 66+y), image.NewUniform(c), image.Point{}, draw.Over)
		}
	}
	for _, line := range []struct {
		text string
		size float64
		y    int
		bold bool
		ink  color.Color
	}{
		{"BONNIE", 76, 220, true, white},
		{"Durable agent runs for Go.", 34, 285, false, cyan},
		{"Survive a crash.", 30, 370, false, white},
		{"Wait days for a human.", 30, 415, false, white},
		{"go-bonnie.dev", 26, 520, true, pink},
	} {
		data := goregular.TTF
		if line.bold {
			data = gobold.TTF
		}
		parsed, err := opentype.Parse(data)
		if err != nil {
			return err
		}
		face, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: line.size, DPI: 72, Hinting: font.HintingFull})
		if err != nil {
			return err
		}
		d := font.Drawer{Dst: card, Src: image.NewUniform(line.ink), Face: face, Dot: fixed.P(575, line.y)}
		d.DrawString(line.text)
		if err := face.Close(); err != nil {
			return err
		}
	}
	out, err := os.Create("public/og.png")
	if err != nil {
		return err
	}
	err = png.Encode(out, card)
	closeErr = out.Close()
	if err != nil {
		return err
	}
	return closeErr
}
