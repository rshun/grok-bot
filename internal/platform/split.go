package platform

// Split breaks text into chunks of at most limit runes. Empty text becomes a
// single space so a platform that rejects empty messages still receives one.
func Split(text string, limit int) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return []string{" "}
	}
	if limit < 1 {
		limit = 4000
	}
	var parts []string
	for len(runes) > limit {
		cut := limit
		for i := limit; i > limit/2; i-- {
			if runes[i-1] == '\n' {
				cut = i - 1
				break
			}
		}
		if cut < 1 {
			cut = limit
		}
		parts = append(parts, string(runes[:cut]))
		runes = runes[cut:]
		for len(runes) > 0 && runes[0] == '\n' {
			runes = runes[1:]
		}
	}
	if len(runes) > 0 {
		parts = append(parts, string(runes))
	}
	if len(parts) == 0 {
		return []string{text}
	}
	return parts
}
