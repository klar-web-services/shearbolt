package compose

import (
	"errors"
	"fmt"
	"strings"
)

// Env looks up an environment variable.
type Env func(name string) (string, bool)

// missingVar is a variable without a value: unset, or empty under a ":" form, with no default.
type missingVar struct {
	name    string
	message string // Compose's own ?message, if any
}

// interpolate applies Compose's variable syntax to s (design section 3). When missing is not
// empty, out is not usable. A malformed or nested reference is an error.
func interpolate(s string, env Env) (out string, missing []missingVar, err error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 == len(s) {
			return "", nil, fmt.Errorf("%q ends with a lone $ (write $$ for a literal $)", s)
		}
		switch n := s[i+1]; {
		case n == '$':
			b.WriteByte('$')
			i++
		case n == '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", nil, fmt.Errorf("%q has an unclosed ${", s)
			}
			v, miss, err := braced(s[i+2:i+2+end], env)
			if err != nil {
				return "", nil, fmt.Errorf("%q: %v", s, err)
			}
			if miss != nil {
				missing = append(missing, *miss)
			}
			b.WriteString(v)
			i += 2 + end
		case isNameStart(n):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			if v, ok := env(s[i+1 : j]); ok {
				b.WriteString(v)
			} else {
				missing = append(missing, missingVar{name: s[i+1 : j]})
			}
			i = j - 1
		default:
			return "", nil, fmt.Errorf("%q has a $ that starts no variable (write $$ for a literal $)", s)
		}
	}
	return b.String(), missing, nil
}

// braced resolves the body of one ${...} reference.
func braced(body string, env Env) (string, *missingVar, error) {
	j := 0
	for j < len(body) && isNameChar(body[j]) {
		j++
	}
	name, rest := body[:j], body[j:]
	if name == "" || !isNameStart(name[0]) {
		return "", nil, fmt.Errorf("${%s} does not start with a variable name", body)
	}
	if strings.Contains(rest, "$") {
		return "", nil, errors.New("a default or alternative value may not contain $; nested variable references are not resolved")
	}
	op, arg := "", ""
	for _, o := range []string{":-", ":?", ":+", "-", "?", "+"} {
		if strings.HasPrefix(rest, o) {
			op, arg = o, rest[len(o):]
			break
		}
	}
	if op == "" && rest != "" {
		return "", nil, fmt.Errorf("${%s} is not a variable reference Compose understands", body)
	}
	v, set := env(name)
	usable := set && (v != "" || !strings.HasPrefix(op, ":"))
	switch op {
	case "":
		if !set {
			return "", &missingVar{name: name}, nil
		}
		return v, nil, nil
	case ":-", "-":
		if usable {
			return v, nil, nil
		}
		return arg, nil, nil
	case ":?", "?":
		if usable {
			return v, nil, nil
		}
		return "", &missingVar{name: name, message: arg}, nil
	default: // ":+", "+"
		if usable {
			return arg, nil, nil
		}
		return "", nil, nil
	}
}

func isNameStart(c byte) bool { return c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }
func isNameChar(c byte) bool  { return isNameStart(c) || '0' <= c && c <= '9' }
