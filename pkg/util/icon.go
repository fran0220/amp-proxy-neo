package util

// Embedded PNG icons for the macOS menu bar.
// Simple 16x16 filled circles: green (running), red (error).
// Generated as minimal valid PNGs to avoid external asset files.

// iconGreen is a 16x16 green circle PNG.
var IconGreen = generateCirclePNG(0x34, 0xD3, 0x99) // #34D399 (emerald)

// iconRed is a 16x16 red circle PNG.
var IconRed = generateCirclePNG(0xF8, 0x71, 0x71) // #F87171 (red)

func generateCirclePNG(r, g, b byte) []byte {
	// Generate a 22x22 RGBA PNG with a filled circle (standard macOS menu bar icon size).
	const size = 22
	const centerX, centerY = 10.5, 10.5
	const radius = 8.0

	pixels := make([]byte, 0, size*(1+size*4))
	for y := 0; y < size; y++ {
		pixels = append(pixels, 0) // filter: none
		for x := 0; x < size; x++ {
			dx := float64(x) - centerX
			dy := float64(y) - centerY
			if dx*dx+dy*dy <= radius*radius {
				pixels = append(pixels, r, g, b, 0xFF)
			} else {
				pixels = append(pixels, 0, 0, 0, 0)
			}
		}
	}

	return encodePNG(size, size, pixels)
}

func encodePNG(width, height int, rawPixels []byte) []byte {
	// Minimal PNG encoder (IHDR + IDAT + IEND) without importing image/png.
	// This keeps the binary small and avoids init-time overhead.
	var buf []byte

	// PNG signature
	buf = append(buf, 0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n')

	// IHDR chunk
	ihdr := []byte{
		byte(width >> 24), byte(width >> 16), byte(width >> 8), byte(width),
		byte(height >> 24), byte(height >> 16), byte(height >> 8), byte(height),
		8, // bit depth
		6, // color type: RGBA
		0, // compression
		0, // filter
		0, // interlace
	}
	buf = appendChunk(buf, []byte("IHDR"), ihdr)

	// IDAT chunk: zlib-compress the raw pixel data
	compressed := zlibCompress(rawPixels)
	buf = appendChunk(buf, []byte("IDAT"), compressed)

	// IEND chunk
	buf = appendChunk(buf, []byte("IEND"), nil)

	return buf
}

func appendChunk(buf []byte, chunkType []byte, data []byte) []byte {
	length := len(data)
	// Length (4 bytes)
	buf = append(buf, byte(length>>24), byte(length>>16), byte(length>>8), byte(length))
	// Type (4 bytes)
	buf = append(buf, chunkType...)
	// Data
	buf = append(buf, data...)
	// CRC32 (type + data)
	crc := crc32Compute(append(chunkType, data...))
	buf = append(buf, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	return buf
}

func zlibCompress(data []byte) []byte {
	// Minimal zlib: header + stored blocks + adler32
	var out []byte
	out = append(out, 0x78, 0x01) // zlib header (deflate, no compression)

	// Split into 65535-byte blocks (max for stored blocks)
	for i := 0; i < len(data); {
		end := i + 65535
		if end > len(data) {
			end = len(data)
		}
		block := data[i:end]
		final := byte(0)
		if end == len(data) {
			final = 1
		}
		bLen := len(block)
		out = append(out, final)
		out = append(out, byte(bLen), byte(bLen>>8))
		out = append(out, byte(^bLen), byte((^bLen)>>8))
		out = append(out, block...)
		i = end
	}

	// Adler-32
	a := adler32Compute(data)
	out = append(out, byte(a>>24), byte(a>>16), byte(a>>8), byte(a))
	return out
}

func crc32Compute(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, b := range data {
		crc ^= uint32(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xEDB88320
			} else {
				crc >>= 1
			}
		}
	}
	return ^crc
}

func adler32Compute(data []byte) uint32 {
	a, b := uint32(1), uint32(0)
	for _, d := range data {
		a = (a + uint32(d)) % 65521
		b = (b + a) % 65521
	}
	return (b << 16) | a
}
