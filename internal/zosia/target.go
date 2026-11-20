package zosia

const DefaultURL = "https://zosiasec.com"

// DefaultKey is Zosia's current report key, "<key id>:<base64url X25519 public key>".
const DefaultKey = "2026a:hfnSgJQMX4rIOlXV4g6HWklM127-aH0Y-_JynucYVWk"

// Target resolves the URL and key, with overrides for tests and staging.
func Target(url, key string) (string, Key, error) {
	if url == "" {
		url = DefaultURL
	}
	if key == "" {
		key = DefaultKey
	}
	k, err := ParseKey(key)
	if err != nil {
		return "", Key{}, err
	}
	return url, k, nil
}
