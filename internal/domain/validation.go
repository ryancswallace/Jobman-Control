package domain

// ValidIdempotencyKey reports whether a key is bounded visible ASCII. The
// exact bytes remain significant within their principal/namespace scope.
func ValidIdempotencyKey(value string) bool {
	if len(value) < 1 || len(value) > 200 {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}

	return true
}
