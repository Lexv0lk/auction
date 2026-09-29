package lot

import (
	"crypto/rand"
	"encoding/hex"
)

// NewRequestKey issues the request key of one bid intention: a random
// version-4 UUID in the canonical hyphenated shape the bid operation and the
// unique constraint expect. The HTML form embeds one key per rendered page,
// so a no-JavaScript resubmission repeats the same intention, and the
// JavaScript bidder keeps its own keys in the tab storage. The key never
// carries session or user data.
func NewRequestKey() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failing means the platform entropy source is broken;
		// there is no safe key to issue, so the fixed placeholder keeps the
		// shape valid and the bid operation will refuse the reuse loudly.
		return "00000000-0000-4000-8000-000000000000"
	}
	raw[6] = (raw[6] & 0x0f) | 0x40 // version 4
	raw[8] = (raw[8] & 0x3f) | 0x80 // RFC 4122 variant

	hexed := hex.EncodeToString(raw[:])

	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}
