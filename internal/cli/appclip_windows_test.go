package cli

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"image/png"
	"testing"
)

// A bottom-up 32 bit DIB without alpha becomes an opaque PNG, top row first.
func TestDIBToPNG(t *testing.T) {
	const w, h = 2, 2
	b := make([]byte, 40+w*h*4)
	binary.LittleEndian.PutUint32(b[0:], 40)
	binary.LittleEndian.PutUint32(b[4:], w)
	binary.LittleEndian.PutUint32(b[8:], h)
	binary.LittleEndian.PutUint16(b[12:], 1)
	binary.LittleEndian.PutUint16(b[14:], 32)
	// rows are stored bottom first, pixels as BGRA
	px := [][4]byte{{0, 0, 255, 0}, {0, 255, 0, 0}, {255, 0, 0, 0}, {255, 255, 255, 0}}
	for i, p := range px {
		copy(b[40+i*4:], p[:])
	}
	out, err := dibToPNG(b)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	want := map[[2]int]color.NRGBA{
		{0, 1}: {255, 0, 0, 255}, {1, 1}: {0, 255, 0, 255},
		{0, 0}: {0, 0, 255, 255}, {1, 0}: {255, 255, 255, 255},
	}
	for xy, c := range want {
		if got := color.NRGBAModel.Convert(img.At(xy[0], xy[1])).(color.NRGBA); got != c {
			t.Errorf("pixel %v = %v, want %v", xy, got, c)
		}
	}
	if _, err := dibToPNG(b[:30]); err == nil {
		t.Error("short DIB accepted")
	}
}
