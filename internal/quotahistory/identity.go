package quotahistory

import (
	"crypto/sha256"
	"encoding/hex"
)

// Identity never publishes provider account claims or credential material. A
// proven account claim survives token rotation; an unproven replacement does not.
func Identity(provider, accountClaim, credentialFingerprint string) string {
	kind, value := "account", accountClaim
	if value == "" {
		kind, value = "credential", credentialFingerprint
	}
	digest := sha256.Sum256([]byte(provider + "\x00" + kind + "\x00" + value))
	return provider + ":" + hex.EncodeToString(digest[:16])
}
