package manifest

import "strings"

// StripBlockComments removes /* ... */ comments, which may span lines.
// Exported for sibling parsers (e.g. the workspace file) that share the
// go.mod lexical conventions.
func StripBlockComments(data []byte) []byte { return stripBlockComments(data) }

// TokenizeLine splits one line into tokens, honoring double-quoted and
// back-quoted strings, and returns the trailing line comment content.
// ok is false when a quoted string is unterminated.
func TokenizeLine(line string) (toks []string, comment string, ok bool) {
	return tokenizeLine(line)
}

// stripBlockComments removes /* ... */ comments, which may span lines.
// go.mod files do not nest block comments.
func stripBlockComments(data []byte) []byte {
	s := string(data)
	var b strings.Builder
	for {
		i := strings.Index(s, "/*")
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		j := strings.Index(s[i+2:], "*/")
		if j < 0 {
			// Unterminated block comment: drop the remainder; the caller's
			// block balance checks still apply to real directives.
			break
		}
		s = s[i+2+j+2:]
	}
	return []byte(b.String())
}

// tokenizeLine splits one line into tokens, honoring double-quoted and
// back-quoted strings, and returns the trailing line comment content.
// ok is false when a quoted string is unterminated.
func tokenizeLine(line string) (toks []string, comment string, ok bool) {
	i := 0
	n := len(line)
	for i < n {
		c := line[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '/' && i+1 < n && line[i+1] == '/':
			comment = strings.TrimSpace(line[i+2:])
			return toks, comment, true
		case c == '"' || c == '`':
			val, ni, good := readString(line, i, c)
			if !good {
				return nil, "", false
			}
			toks = append(toks, val)
			i = ni
		default:
			start := i
			for i < n {
				ch := line[i]
				if ch == ' ' || ch == '\t' || ch == '\r' {
					break
				}
				if ch == '/' && i+1 < n && line[i+1] == '/' {
					break
				}
				i++
			}
			toks = append(toks, line[start:i])
		}
	}
	return toks, comment, true
}

func readString(line string, start int, quote byte) (string, int, bool) {
	i := start + 1
	var b strings.Builder
	for i < len(line) {
		c := line[i]
		if quote == '`' {
			if c == '`' {
				return b.String(), i + 1, true
			}
			b.WriteByte(c)
			i++
			continue
		}
		// double quoted: handle a minimal set of escapes go.mod uses.
		if c == '"' {
			return b.String(), i + 1, true
		}
		if c == '\\' && i+1 < len(line) {
			next := line[i+1]
			switch next {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			default:
				b.WriteByte('\\')
				b.WriteByte(next)
			}
			i += 2
			continue
		}
		b.WriteByte(c)
		i++
	}
	return "", start, false
}
