package main

import "errors"

// encodeQR builds a small, standards-compliant QR code for byte-mode URLs.
// Versions 1 through 4 at error correction level L cover the local URL sizes
// used by this app, without a third-party package or an internet dependency.
func encodeQR(value string) ([][]bool, error) {
	b := []byte(value)
	dataSizes := []int{0, 19, 34, 55, 80}
	version := 0
	for v := 1; v < len(dataSizes); v++ {
		if 4+8+len(b)*8 <= dataSizes[v]*8 {
			version = v
			break
		}
	}
	if version == 0 || len(b) > 255 {
		return nil, errors.New("URL is too long for the built-in QR encoder")
	}

	bits := make([]bool, 0, dataSizes[version]*8)
	appendBits := func(value, count int) {
		for i := count - 1; i >= 0; i-- {
			bits = append(bits, (value>>i)&1 != 0)
		}
	}
	appendBits(0x4, 4) // byte mode
	appendBits(len(b), 8)
	for _, c := range b {
		appendBits(int(c), 8)
	}
	capacity := dataSizes[version] * 8
	for i := 0; i < 4 && len(bits) < capacity; i++ {
		bits = append(bits, false)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	data := make([]byte, 0, dataSizes[version])
	for i := 0; i < len(bits); i += 8 {
		var c byte
		for j := 0; j < 8; j++ {
			c <<= 1
			if bits[i+j] {
				c |= 1
			}
		}
		data = append(data, c)
	}
	for pad := byte(0xec); len(data) < dataSizes[version]; pad ^= 0xec ^ 0x11 {
		data = append(data, pad)
	}

	eccSize := []int{0, 7, 10, 15, 20}[version]
	all := append(data, reedSolomonRemainder(data, eccSize)...)
	return makeQRMatrix(version, all), nil
}

func reedSolomonRemainder(data []byte, degree int) []byte {
	poly := []byte{1}
	root := byte(1)
	for i := 0; i < degree; i++ {
		next := make([]byte, len(poly)+1)
		for j, coefficient := range poly {
			next[j] ^= coefficient
			next[j+1] ^= gfMultiply(coefficient, root)
		}
		poly = next
		root = gfMultiply(root, 2)
	}

	remainder := make([]byte, degree)
	for _, value := range data {
		factor := value ^ remainder[0]
		copy(remainder, remainder[1:])
		remainder[degree-1] = 0
		for i := range remainder {
			remainder[i] ^= gfMultiply(poly[i+1], factor)
		}
	}
	return remainder
}

func gfMultiply(x, y byte) byte {
	var z byte
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x1d)
		z ^= ((y >> i) & 1) * x
	}
	return z
}

func makeQRMatrix(version int, codewords []byte) [][]bool {
	size := 17 + version*4
	matrix := make([][]bool, size)
	reserved := make([][]bool, size)
	for y := range matrix {
		matrix[y] = make([]bool, size)
		reserved[y] = make([]bool, size)
	}
	set := func(x, y int, dark bool) {
		if x >= 0 && x < size && y >= 0 && y < size {
			matrix[y][x], reserved[y][x] = dark, true
		}
	}

	for _, origin := range [][2]int{{0, 0}, {size - 7, 0}, {0, size - 7}} {
		for dy := -1; dy <= 7; dy++ {
			for dx := -1; dx <= 7; dx++ {
				dark := false
				if dx >= 0 && dx < 7 && dy >= 0 && dy < 7 {
					distance := maxInt(absInt(dx-3), absInt(dy-3))
					dark = distance == 3 || distance <= 1
				}
				set(origin[0]+dx, origin[1]+dy, dark)
			}
		}
	}

	for i := 8; i < size-8; i++ {
		set(i, 6, i%2 == 0)
		set(6, i, i%2 == 0)
	}
	centers := []int{}
	switch version {
	case 2:
		centers = []int{6, 18}
	case 3:
		centers = []int{6, 22}
	case 4:
		centers = []int{6, 26}
	}
	for _, cy := range centers {
		for _, cx := range centers {
			if reserved[cy][cx] {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					set(cx+dx, cy+dy, maxInt(absInt(dx), absInt(dy)) != 1)
				}
			}
		}
	}
	placeFormatBits(matrix, reserved, 0)

	dataBits := make([]bool, 0, len(codewords)*8)
	for _, value := range codewords {
		for i := 7; i >= 0; i-- {
			dataBits = append(dataBits, (value>>i)&1 != 0)
		}
	}
	bitIndex := 0
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		upward := ((right + 1) & 2) == 0
		for vert := 0; vert < size; vert++ {
			y := vert
			if upward {
				y = size - 1 - vert
			}
			for offset := 0; offset < 2; offset++ {
				x := right - offset
				if reserved[y][x] {
					continue
				}
				dark := false
				if bitIndex < len(dataBits) {
					dark = dataBits[bitIndex]
				}
				bitIndex++
				if (x+y)%2 == 0 { // mask pattern 0
					dark = !dark
				}
				matrix[y][x] = dark
			}
		}
	}
	placeFormatBits(matrix, reserved, 0)
	return matrix
}

func placeFormatBits(matrix, reserved [][]bool, mask int) {
	size := len(matrix)
	// Error correction level L has format indicator bits 01.
	data := (1 << 3) | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := ((data << 10) | rem) ^ 0x5412
	set := func(x, y, bit int) {
		matrix[y][x] = ((bits >> bit) & 1) != 0
		reserved[y][x] = true
	}
	for i := 0; i <= 5; i++ {
		set(8, i, i)
	}
	set(8, 7, 6)
	set(8, 8, 7)
	set(7, 8, 8)
	for i := 9; i < 15; i++ {
		set(14-i, 8, i)
	}
	for i := 0; i < 8; i++ {
		set(size-1-i, 8, i)
	}
	for i := 8; i < 15; i++ {
		set(8, size-15+i, i)
	}
	matrix[size-8][8] = true // fixed dark module
	reserved[size-8][8] = true
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
