package main

// QR verification code for cleared e-invoices (NRS MBS requirement: every
// issued invoice carries a QR containing the IRN + integrity signature).
//
// The QR encoder below is a dependency-free implementation (byte mode, EC
// level M, versions 1-6, mask 0) cross-validated against the reference
// python "qrcode" library (bit-exact matrix match for payloads of 5-106
// bytes); a golden-matrix regression test pins the behaviour
// (TestQRGoldenMatrix). Payloads are capped at 106 bytes by construction.
//
// REAL: payload signing uses HMAC-SHA256 with QR_HMAC_KEY (dev default below
// is for local development only; production injects a KMS-managed key).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

const qrDevKey = "meridian-dev-qr-key" // DEV ONLY — set QR_HMAC_KEY in prod

func qrKey() []byte {
	if k := os.Getenv("QR_HMAC_KEY"); k != "" {
		return []byte(k)
	}
	return []byte(qrDevKey)
}

// QRPayload builds the signed verification payload for an invoice:
//
//	NRS1|<IRN>|<supplierTIN>|<payableKobo>|<yyyymmddhhmmss>|<hmac12>
//
// ≤ 106 bytes so it always fits a version-6 (41x41) EC-M symbol.
func QRPayload(inv *CanonicalInvoice) (payload, signature string) {
	ts := strings.NewReplacer("-", "", ":", "", "T", "", "Z", "").Replace(inv.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	payload = fmt.Sprintf("NRS1|%s|%s|%d|%s", inv.IRN, inv.Supplier.TIN, inv.PayableKobo, ts)
	mac := hmac.New(sha256.New, qrKey())
	mac.Write([]byte(payload))
	signature = hex.EncodeToString(mac.Sum(nil))[:12]
	return payload, signature
}

// VerifyQRPayload re-computes the HMAC for a payload (without trailing sig).
func VerifyQRPayload(payload, signature string) bool {
	mac := hmac.New(sha256.New, qrKey())
	mac.Write([]byte(payload))
	return hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))[:12]), []byte(signature))
}

// ---------------------------------------------------------------------------
// Minimal QR encoder: byte mode, EC level M, versions 1-6, mask 0.
// ---------------------------------------------------------------------------

var gfExp [512]int
var gfLog [256]int

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = x
		gfLog[x] = i
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b int) int {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[gfLog[a]+gfLog[b]]
}

func rsGenPoly(n int) []int {
	p := []int{1}
	for i := 0; i < n; i++ {
		p2 := make([]int, len(p)+1)
		for j, c := range p {
			p2[j] ^= gfMul(c, gfExp[i])
			p2[j+1] ^= c
		}
		p = p2
	}
	return p
}

func rsEncode(data []int, n int) []int {
	g := rsGenPoly(n)
	// descending order (leading coefficient first)
	gd := make([]int, len(g))
	for i, c := range g {
		gd[len(g)-1-i] = c
	}
	res := make([]int, n)
	for _, d := range data {
		factor := d ^ res[0]
		copy(res, res[1:])
		res[n-1] = 0
		if factor != 0 {
			for j := 0; j < n; j++ {
				res[j] ^= gfMul(gd[j+1], factor)
			}
		}
	}
	return res
}

// RS block groups per version for EC level M: {count, totalCW, dataCW}.
var qrRSGroups = map[int][][3]int{
	1: {{1, 26, 16}},
	2: {{1, 44, 28}},
	3: {{1, 70, 44}},
	4: {{2, 50, 32}},
	5: {{2, 67, 43}},
	6: {{4, 43, 27}},
}

var qrAlign = map[int][]int{
	1: {}, 2: {6, 18}, 3: {6, 22}, 4: {6, 26}, 5: {6, 30}, 6: {6, 34},
}

func qrDataCodewords(v int) int {
	n := 0
	for _, g := range qrRSGroups[v] {
		n += g[0] * g[2]
	}
	return n
}

func qrEncodeData(payload []byte) (version int, codewords []int, err error) {
	for v := 1; v <= 6; v++ {
		if 2+len(payload) <= qrDataCodewords(v) {
			version = v
			break
		}
	}
	if version == 0 {
		return 0, nil, fmt.Errorf("payload too long (%d bytes > 106)", len(payload))
	}
	dcw := qrDataCodewords(version)
	var bits []int
	put := func(val, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, (val>>i)&1)
		}
	}
	put(0b0100, 4)
	put(len(payload), 8)
	for _, b := range payload {
		put(int(b), 8)
	}
	if rem := dcw*8 - len(bits); rem > 4 {
		put(0, 4)
	} else {
		put(0, rem)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, 0)
	}
	pads := []int{0xEC, 0x11}
	for i := 0; len(bits) < dcw*8; i++ {
		put(pads[i%2], 8)
	}
	data := make([]int, dcw)
	for j := 0; j < dcw; j++ {
		for k := 0; k < 8; k++ {
			data[j] |= bits[j*8+k] << (7 - k)
		}
	}
	// split into blocks, compute EC, interleave
	type block struct {
		data []int
		ec   int
	}
	var blocks []block
	off := 0
	for _, g := range qrRSGroups[version] {
		cnt, total, dlen := g[0], g[1], g[2]
		for i := 0; i < cnt; i++ {
			blocks = append(blocks, block{data[off : off+dlen], total - dlen})
			off += dlen
		}
	}
	maxD := 0
	for _, b := range blocks {
		if len(b.data) > maxD {
			maxD = len(b.data)
		}
	}
	for j := 0; j < maxD; j++ {
		for _, b := range blocks {
			if j < len(b.data) {
				codewords = append(codewords, b.data[j])
			}
		}
	}
	ecs := make([][]int, len(blocks))
	maxE := 0
	for i, b := range blocks {
		ecs[i] = rsEncode(b.data, b.ec)
		if b.ec > maxE {
			maxE = b.ec
		}
	}
	for j := 0; j < maxE; j++ {
		for _, e := range ecs {
			codewords = append(codewords, e[j])
		}
	}
	return version, codewords, nil
}

// QRMatrix renders the boolean module matrix (false = light) for a payload.
func QRMatrix(payload []byte) ([][]bool, error) {
	v, codewords, err := qrEncodeData(payload)
	if err != nil {
		return nil, err
	}
	n := 17 + 4*v
	m := make([][]bool, n)
	fn := make([][]bool, n)
	for i := range m {
		m[i] = make([]bool, n)
		fn[i] = make([]bool, n)
	}
	setF := func(r, c int, val bool) { m[r][c] = val; fn[r][c] = true }
	finder := func(r, c int) {
		for dr := -1; dr <= 7; dr++ {
			for dc := -1; dc <= 7; dc++ {
				rr, cc := r+dr, c+dc
				if rr < 0 || rr >= n || cc < 0 || cc >= n {
					continue
				}
				on := dr >= 0 && dr <= 6 && dc >= 0 && dc <= 6 &&
					(dr == 0 || dr == 6 || dc == 0 || dc == 6 ||
						(dr >= 2 && dr <= 4 && dc >= 2 && dc <= 4))
				setF(rr, cc, on)
			}
		}
	}
	finder(0, 0)
	finder(0, n-7)
	finder(n-7, 0)
	for i := 8; i < n-8; i++ {
		setF(6, i, i%2 == 0)
		setF(i, 6, i%2 == 0)
	}
	for _, r := range qrAlign[v] {
		for _, c := range qrAlign[v] {
			if fn[r][c] {
				continue
			}
			for dr := -2; dr <= 2; dr++ {
				for dc := -2; dc <= 2; dc++ {
					ad, ae := dr, dc
					if ad < 0 {
						ad = -ad
					}
					if ae < 0 {
						ae = -ae
					}
					mx := ad
					if ae > mx {
						mx = ae
					}
					setF(r+dr, c+dc, mx != 1)
				}
			}
		}
	}
	setF(n-8, 8, true) // dark module
	// reserve format areas
	for i := 0; i < 9; i++ {
		if !fn[8][i] {
			fn[8][i] = true
		}
		if !fn[i][8] {
			fn[i][8] = true
		}
	}
	for i := 0; i < 8; i++ {
		if !fn[8][n-1-i] {
			fn[8][n-1-i] = true
		}
		if !fn[n-1-i][8] {
			fn[n-1-i][8] = true
		}
	}
	// data placement, mask 0: (r+c)%2==0 -> invert
	var bits []int
	for _, b := range codewords {
		for i := 7; i >= 0; i-- {
			bits = append(bits, (b>>i)&1)
		}
	}
	i := 0
	upward := true
	for col := n - 1; col > 0; col -= 2 {
		if col == 6 {
			col--
		}
		for k := 0; k < n; k++ {
			r := k
			if upward {
				r = n - 1 - k
			}
			for _, c := range []int{col, col - 1} {
				if !fn[r][c] {
					bit := 0
					if i < len(bits) {
						bit = bits[i]
					}
					i++
					if (r+c)%2 == 0 {
						bit ^= 1
					}
					m[r][c] = bit == 1
				}
			}
		}
		upward = !upward
	}
	// format info: EC level M (00), mask 0 -> 0x5412 (BCH(15,5) precomputed)
	fb := 0x5412
	fbits := make([]int, 15)
	for j := 14; j >= 0; j-- {
		fbits[14-j] = (fb >> j) & 1
	}
	pos1 := [][2]int{{8, 0}, {8, 1}, {8, 2}, {8, 3}, {8, 4}, {8, 5}, {8, 7}, {8, 8}, {7, 8}, {5, 8}, {4, 8}, {3, 8}, {2, 8}, {1, 8}, {0, 8}}
	pos2 := [][2]int{{n - 1, 8}, {n - 2, 8}, {n - 3, 8}, {n - 4, 8}, {n - 5, 8}, {n - 6, 8}, {n - 7, 8}, {8, n - 8}, {8, n - 7}, {8, n - 6}, {8, n - 5}, {8, n - 4}, {8, n - 3}, {8, n - 2}, {8, n - 1}}
	for j, p := range pos1 {
		m[p[0]][p[1]] = fbits[j] == 1
	}
	for j, p := range pos2 {
		m[p[0]][p[1]] = fbits[j] == 1
	}
	return m, nil
}

// QRSVG renders the matrix as a compact SVG (scale px per module, 2-module quiet zone).
func QRSVG(m [][]bool, scale int) string {
	n := len(m)
	q := 2
	var b strings.Builder
	size := (n + 2*q) * scale
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d"><rect width="%d" height="%d" fill="#fff"/><g fill="#000">`, size, size, size, size, size, size)
	for r := 0; r < n; r++ {
		for c := 0; c < n; c++ {
			if m[r][c] {
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d"/>`, (c+q)*scale, (r+q)*scale, scale, scale)
			}
		}
	}
	b.WriteString(`</g></svg>`)
	return b.String()
}
