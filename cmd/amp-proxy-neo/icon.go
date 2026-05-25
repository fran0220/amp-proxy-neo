package main

// iconPurple is a generated 22x22 purple circle PNG for the macOS menu bar.
var iconPurple = generateCirclePNG(0xA7, 0x55, 0xF7) // #A755F7
var iconRed = generateCirclePNG(0xFF, 0x5A, 0x5F)    // #FF5A5F
var iconYellow = generateCirclePNG(0xFF, 0xC8, 0x47) // degraded / updating

func generateCirclePNG(r, g, b byte) []byte {
	const size = 22
	const centerX, centerY = 10.5, 10.5
	const radius = 8.0

	pixels := make([]byte, 0, size*(1+size*4))
	for y := 0; y < size; y++ {
		pixels = append(pixels, 0)
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
	var buf []byte
	buf = append(buf, 0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n')
	ihdr := []byte{
		byte(width >> 24), byte(width >> 16), byte(width >> 8), byte(width),
		byte(height >> 24), byte(height >> 16), byte(height >> 8), byte(height),
		8, 6, 0, 0, 0,
	}
	buf = appendChunk(buf, []byte("IHDR"), ihdr)
	buf = appendChunk(buf, []byte("IDAT"), zlibCompress(rawPixels))
	buf = appendChunk(buf, []byte("IEND"), nil)
	return buf
}

func appendChunk(buf []byte, chunkType []byte, data []byte) []byte {
	length := len(data)
	buf = append(buf, byte(length>>24), byte(length>>16), byte(length>>8), byte(length))
	buf = append(buf, chunkType...)
	buf = append(buf, data...)
	crc := crc32Compute(append(chunkType, data...))
	buf = append(buf, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	return buf
}

func zlibCompress(data []byte) []byte {
	var out []byte
	out = append(out, 0x78, 0x01)
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
		out = append(out, final, byte(bLen), byte(bLen>>8), byte(^bLen), byte((^bLen)>>8))
		out = append(out, block...)
		i = end
	}
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
