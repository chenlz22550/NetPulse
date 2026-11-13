package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// 托盘图标在运行时绘制：彩色圆底 + 白色心电图折线。
// Windows 需要 ICO（这里生成 32 位 BMP 格式的 ICO，兼容 Server 2008 R2），其他平台用 PNG。

var levelColors = []color.NRGBA{
	{0x2e, 0xa0, 0x43, 0xff}, // 正常：绿
	{0xf0, 0x9a, 0x1a, 0xff}, // 警告：橙
	{0xe0, 0x3b, 0x24, 0xff}, // 离线：红
	{0x8a, 0x8a, 0x8a, 0xff}, // 暂停：灰
}

var pulse = [][2]float64{{0.12, 0.55}, {0.34, 0.55}, {0.43, 0.30}, {0.55, 0.78}, {0.64, 0.45}, {0.70, 0.55}, {0.88, 0.55}}

func distSeg(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := ((px-ax)*dx + (py-ay)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	cx, cy := ax+t*dx-px, ay+t*dy-py
	return math.Sqrt(cx*cx + cy*cy)
}

// Draw 绘制 size×size 的图标。
func Draw(size, level int) *image.NRGBA {
	if level < 0 || level >= len(levelColors) {
		level = 0
	}
	c := levelColors[level]
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	fs := float64(size)
	lineW := math.Max(1.2, fs*0.085)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var cover, white float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := float64(x) + (float64(sx)+0.5)/ss
					py := float64(y) + (float64(sy)+0.5)/ss
					dx, dy := px-fs/2, py-fs/2
					if dx*dx+dy*dy > (fs/2-0.3)*(fs/2-0.3) {
						continue
					}
					cover++
					for i := 0; i+1 < len(pulse); i++ {
						if distSeg(px, py, pulse[i][0]*fs, pulse[i][1]*fs, pulse[i+1][0]*fs, pulse[i+1][1]*fs) <= lineW/2 {
							white++
							break
						}
					}
				}
			}
			if cover == 0 {
				continue
			}
			wf := white / cover
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(float64(c.R)*(1-wf) + 255*wf),
				G: uint8(float64(c.G)*(1-wf) + 255*wf),
				B: uint8(float64(c.B)*(1-wf) + 255*wf),
				A: uint8(255 * cover / (ss * ss)),
			})
		}
	}
	return img
}

// PNG 返回 PNG 编码的图标。
func PNG(size, level int) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, Draw(size, level))
	return buf.Bytes()
}

// ICO 返回包含 16/32/48 三种尺寸的 BMP 格式 ICO。
func ICO(level int) []byte {
	sizes := []int{16, 32, 48}
	var images [][]byte
	for _, sz := range sizes {
		images = append(images, bmpEntry(Draw(sz, level)))
	}
	var buf bytes.Buffer
	w := func(v interface{}) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	w(uint16(0))
	w(uint16(1))
	w(uint16(len(sizes)))
	offset := 6 + 16*len(sizes)
	for i, sz := range sizes {
		w(uint8(sz))
		w(uint8(sz))
		w(uint8(0))
		w(uint8(0))
		w(uint16(1))
		w(uint16(32))
		w(uint32(len(images[i])))
		w(uint32(offset))
		offset += len(images[i])
	}
	for _, b := range images {
		buf.Write(b)
	}
	return buf.Bytes()
}

func bmpEntry(img *image.NRGBA) []byte {
	sz := img.Bounds().Dx()
	var buf bytes.Buffer
	w := func(v interface{}) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	maskRow := ((sz + 31) / 32) * 4
	w(uint32(40))
	w(int32(sz))
	w(int32(sz * 2))
	w(uint16(1))
	w(uint16(32))
	w(uint32(0))
	w(uint32(sz*sz*4 + maskRow*sz))
	w(int32(0))
	w(int32(0))
	w(uint32(0))
	w(uint32(0))
	for y := sz - 1; y >= 0; y-- {
		for x := 0; x < sz; x++ {
			c := img.NRGBAAt(x, y)
			buf.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	mask := make([]byte, maskRow)
	for y := 0; y < sz; y++ {
		for i := range mask {
			mask[i] = 0
		}
		for x := 0; x < sz; x++ {
			if img.NRGBAAt(x, sz-1-y).A == 0 {
				mask[x/8] |= 0x80 >> (x % 8)
			}
		}
		buf.Write(mask)
	}
	return buf.Bytes()
}
