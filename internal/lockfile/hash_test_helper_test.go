package lockfile

import (
	"crypto/sha256"
	"encoding/hex"
)

// integrityOf mirrors Marshal's payload integrity computation.
func integrityOf(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(sum[:])
}
