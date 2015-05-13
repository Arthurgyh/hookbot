package github

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
)

func Sha1HMAC(key string, payload []byte) string {
	mac := hmac.New(sha1.New, []byte(key))
	_, _ = mac.Write(payload)
	return fmt.Sprintf("%x", mac.Sum(nil))
}

func SecureEqual(x, y string) bool {
	if subtle.ConstantTimeCompare([]byte(x), []byte(y)) == 1 {
		return true
	}
	return false
}

func IsValidGithubSignature(secret string, message []byte) bool {

	type GithubMessage struct {
		Signature string
		Payload   []byte
	}

	var m GithubMessage

	err := json.Unmarshal(message, &m)
	if err != nil {
		log.Printf("Failed to unmarshal message in IsValidGithubSignature: %v",
			err)
		return false
	}

	expected := m.Signature
	got := fmt.Sprintf("sha1=%v", Sha1HMAC(secret, m.Payload))

	log.Printf("Expected = %v got = %v", expected, got)

	return SecureEqual(got, expected)
}
