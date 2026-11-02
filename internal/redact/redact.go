// Package redact provides high-throughput credential and secret token
// redaction for EVOLV's ledger, conversation memory, event bus, and logs.
package redact

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"unicode"
)

// Mask is the standard replacement string for redacted credentials.
const Mask = "[REDACTED]"

var (
	// Token patterns targeting known provider key formats
	reAnthropic = regexp.MustCompile(`(?i)sk-ant-[a-zA-Z0-9_\-]{20,}`)
	reOpenAI    = regexp.MustCompile(`(?i)sk-[a-zA-Z0-9_\-]{20,}`)
	reGitHub    = regexp.MustCompile(`(?i)(?:gh[pousr]_[a-zA-Z0-9]{20,}|github_pat_[a-zA-Z0-9_]{20,})`)
	reAWS       = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	reGoogle    = regexp.MustCompile(`\bAIza[0-9A-Za-z\-_]{35}\b`)
	reSlack     = regexp.MustCompile(`\bxox[baprs](?:-[0-9a-zA-Z]+){2,4}\b`)

	// Private key blocks (PEM / OpenSSH / PGP)
	rePrivateKey = regexp.MustCompile(`-----BEGIN (?:[A-Z0-9 ]+)?PRIVATE KEY-----[\s\S]*?-----END (?:[A-Z0-9 ]+)?PRIVATE KEY-----`)

	// Explicit Authorization header assignments across common HTTP schemes
	reAuthHeaderDouble = regexp.MustCompile(`(?i)(\bauthorization"?\s*[:=]\s*"(?:(?:bearer|basic|token|digest|apikey|api-key)\s+)?)([^"\r\n]+)(")`)
	reAuthHeaderSingle = regexp.MustCompile(`(?i)(\bauthorization'?\s*[:=]\s*'(?:(?:bearer|basic|token|digest|apikey|api-key)\s+)?)([^'\r\n]+)(')`)
	reAuthHeaderPlain  = regexp.MustCompile(`(?i)(\bauthorization["']?\s*[:=]\s*(?:(?:bearer|basic|token|digest|apikey|api-key)\s+))([^\s"',;}\r\n]+)`)

	// Standalone Bearer / Basic scheme values outside an Authorization header key
	reStandaloneBearer = regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9_\-\.~+/]+=*)`)
	reStandaloneBasic  = regexp.MustCompile(`\b(Basic\s+)([A-Za-z0-9+/]{8,}={0,2})\b`)

	// Key-value credential prefix matcher; value boundaries are parsed quote-aware in Go.
	reKeyPrefix = regexp.MustCompile(`(?i)((?:\\"|\\'|["'])?((?:[a-zA-Z0-9_-]*)(?:api[_-]?key|access[_-]?token|auth[_-]?token|refresh[_-]?token|client[_-]?secret|secret[_-]?key|secret[_-]?access[_-]?key|private[_-]?key|password|passwd|pwd|token|secret))(?:\\"|\\'|["'])?\s*([:=])\s*)`)

	// List of direct replacement patterns
	directPatterns = []*regexp.Regexp{
		reAnthropic,
		reOpenAI,
		reGitHub,
		reAWS,
		reGoogle,
		reSlack,
		rePrivateKey,
	}

	bearerProseStopwords = map[string]struct{}{
		"token": {}, "tokens": {}, "auth": {}, "authentication": {},
		"authorization": {}, "credential": {}, "credentials": {},
		"header": {}, "headers": {}, "scheme": {}, "schemes": {},
		"value": {}, "values": {}, "key": {}, "keys": {}, "only": {},
	}

	unquotedNonSecrets = map[string]struct{}{
		"null": {}, "nil": {}, "true": {}, "false": {}, "none": {}, "undefined": {},
		"the": {}, "this": {}, "that": {}, "from": {}, "with": {}, "must": {}, "will": {},
		"string": {}, "number": {}, "boolean": {}, "bool": {}, "any": {}, "unknown": {},
		"never": {}, "void": {}, "object": {}, "symbol": {}, "bigint": {}, "int": {},
		"int32": {}, "int64": {}, "float32": {}, "float64": {}, "byte": {}, "rune": {},
		"error": {},
	}

	sensitiveKeyNames = map[string]struct{}{
		"api_key": {}, "api-key": {}, "apikey": {},
		"access_token": {}, "access-token": {}, "accesstoken": {},
		"auth_token": {}, "auth-token": {}, "authtoken": {},
		"refresh_token": {}, "refresh-token": {}, "refreshtoken": {},
		"client_secret": {}, "client-secret": {}, "clientsecret": {},
		"secret_key": {}, "secret-key": {}, "secretkey": {},
		"secret_access_key": {}, "secret-access-key": {}, "secretaccesskey": {},
		"private_key": {}, "private-key": {}, "privatekey": {},
		"password": {}, "passwd": {}, "pwd": {},
		"token": {}, "secret": {}, "authorization": {},
	}

	byteSliceType = reflect.TypeOf([]byte(nil))
)

// ContainsCredential reports whether s contains any known credential or API key token shape.
func ContainsCredential(s string) bool {
	if s == "" {
		return false
	}
	for _, p := range directPatterns {
		if p.MatchString(s) {
			return true
		}
	}
	if _, changed := redactAuth(s); changed {
		return true
	}
	if _, changed := redactKeyValues(s); changed {
		return true
	}
	return false
}

// ContainsCredentialBytes reports whether b contains any known credential token shape.
func ContainsCredentialBytes(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	return ContainsCredential(string(b))
}

// Redact replaces recognized credential tokens in s with [REDACTED].
func Redact(s string) string {
	if s == "" {
		return ""
	}

	out := s

	// Direct token patterns
	for _, p := range directPatterns {
		out = p.ReplaceAllString(out, Mask)
	}

	// Authorization header schemes and standalone Bearer/Basic tokens
	out, _ = redactAuth(out)

	// Quote-aware key-value secret assignments
	out, _ = redactKeyValues(out)

	return out
}

func redactAuth(s string) (string, bool) {
	changed := false
	out := s

	applyThreeGroup := func(re *regexp.Regexp, in string) string {
		return re.ReplaceAllStringFunc(in, func(m string) string {
			sub := re.FindStringSubmatch(m)
			if len(sub) != 4 {
				return m
			}
			val := strings.TrimSpace(sub[2])
			if val == "" || val == Mask {
				return m
			}
			changed = true
			return sub[1] + Mask + sub[3]
		})
	}

	out = applyThreeGroup(reAuthHeaderDouble, out)
	out = applyThreeGroup(reAuthHeaderSingle, out)

	out = reAuthHeaderPlain.ReplaceAllStringFunc(out, func(m string) string {
		sub := reAuthHeaderPlain.FindStringSubmatch(m)
		if len(sub) != 3 {
			return m
		}
		val := strings.TrimSpace(sub[2])
		if val == "" || val == Mask {
			return m
		}
		changed = true
		return sub[1] + Mask
	})

	out = reStandaloneBasic.ReplaceAllStringFunc(out, func(m string) string {
		sub := reStandaloneBasic.FindStringSubmatch(m)
		if len(sub) != 3 || sub[2] == Mask {
			return m
		}
		changed = true
		return sub[1] + Mask
	})

	out = reStandaloneBearer.ReplaceAllStringFunc(out, func(m string) string {
		sub := reStandaloneBearer.FindStringSubmatch(m)
		if len(sub) != 3 {
			return m
		}
		if !isLikelyBearerToken(sub[1], sub[2]) {
			return m
		}
		changed = true
		return sub[1] + Mask
	})

	return out, changed
}

func isLikelyBearerToken(prefix, tok string) bool {
	if tok == "" || tok == Mask || tok == "REDACTED" {
		return false
	}
	if _, stop := bearerProseStopwords[strings.ToLower(tok)]; stop {
		return false
	}
	if len(tok) >= 16 {
		return true
	}
	if len(tok) < 4 {
		return false
	}
	if strings.HasPrefix(prefix, "Bearer") || strings.HasPrefix(prefix, "BEARER") {
		return true
	}
	for _, r := range tok {
		if !unicode.IsLower(r) {
			return true
		}
	}
	return false
}

func isIdentChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

func redactKeyValues(s string) (string, bool) {
	if !strings.ContainsAny(s, ":=") {
		return s, false
	}

	var b strings.Builder
	changed := false
	cursor := 0

	for cursor < len(s) {
		loc := reKeyPrefix.FindStringSubmatchIndex(s[cursor:])
		if loc == nil {
			b.WriteString(s[cursor:])
			break
		}

		prefixStart := cursor + loc[2]
		prefixEnd := cursor + loc[3]
		keyStart := cursor + loc[4]
		keyEnd := cursor + loc[5]
		op := s[cursor+loc[6] : cursor+loc[7]]

		// Ensure key starts on an identifier boundary (do not match suffix of unrelated word).
		if keyStart > 0 && isIdentChar(s[keyStart-1]) {
			b.WriteString(s[cursor:keyEnd])
			cursor = keyEnd
			continue
		}

		valStart := prefixEnd
		if valStart >= len(s) {
			b.WriteString(s[cursor:])
			break
		}

		// Case 1: Escaped quote pair (\"...\" or \'...\')
		if valStart+1 < len(s) && s[valStart] == '\\' && (s[valStart+1] == '"' || s[valStart+1] == '\'') {
			q := s[valStart+1]
			innerStart := valStart + 2
			closeIdx := findEscapedQuoteClose(s, innerStart, q)
			if closeIdx == -1 {
				lineEnd := len(s)
				if idx := strings.IndexAny(s[innerStart:], "\r\n"); idx != -1 {
					lineEnd = innerStart + idx
				}
				inner := s[innerStart:lineEnd]
				b.WriteString(s[cursor:innerStart])
				if strings.TrimSpace(inner) != "" && inner != Mask {
					b.WriteString(Mask)
					changed = true
				} else {
					b.WriteString(inner)
				}
				cursor = lineEnd
				continue
			}
			inner := s[innerStart:closeIdx]
			b.WriteString(s[cursor:innerStart])
			if strings.TrimSpace(inner) != "" && inner != Mask {
				b.WriteString(Mask)
				changed = true
			} else {
				b.WriteString(inner)
			}
			b.WriteString(s[closeIdx : closeIdx+2])
			cursor = closeIdx + 2
			continue
		}

		// Case 2: Standard quoted value ("..." or '...')
		if s[valStart] == '"' || s[valStart] == '\'' {
			q := s[valStart]
			innerStart := valStart + 1
			closeIdx := findQuoteClose(s, innerStart, q)
			if closeIdx == -1 {
				lineEnd := len(s)
				if idx := strings.IndexAny(s[innerStart:], "\r\n"); idx != -1 {
					lineEnd = innerStart + idx
				}
				inner := s[innerStart:lineEnd]
				b.WriteString(s[cursor:innerStart])
				if strings.TrimSpace(inner) != "" && inner != Mask {
					b.WriteString(Mask)
					changed = true
				} else {
					b.WriteString(inner)
				}
				cursor = lineEnd
				continue
			}
			inner := s[innerStart:closeIdx]
			b.WriteString(s[cursor:innerStart])
			if strings.TrimSpace(inner) != "" && inner != Mask {
				b.WriteString(Mask)
				changed = true
			} else {
				b.WriteString(inner)
			}
			b.WriteByte(q)
			cursor = closeIdx + 1
			continue
		}

		// Case 3: Unquoted scalar assignment (e.g. client_secret=highentropy1234567890)
		if strings.HasPrefix(s[valStart:], Mask) {
			b.WriteString(s[cursor : valStart+len(Mask)])
			cursor = valStart + len(Mask)
			continue
		}
		if s[valStart] == '{' || s[valStart] == '[' {
			b.WriteString(s[cursor:prefixEnd])
			cursor = prefixEnd
			continue
		}

		valEnd := scanUnquotedValueEnd(s, valStart)
		val := s[valStart:valEnd]
		if !shouldMaskUnquotedValue(op, val) {
			_ = prefixStart
			b.WriteString(s[cursor:valEnd])
			cursor = valEnd
			continue
		}

		b.WriteString(s[cursor:valStart])
		b.WriteString(Mask)
		changed = true
		cursor = valEnd
	}

	if !changed {
		return s, false
	}
	return b.String(), true
}

func findQuoteClose(s string, start int, quote byte) int {
	for i := start; i < len(s); i++ {
		if s[i] == '\r' || s[i] == '\n' {
			return -1
		}
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == quote {
			return i
		}
	}
	return -1
}

func findEscapedQuoteClose(s string, start int, quote byte) int {
	for i := start; i+1 < len(s); i++ {
		if s[i] == '\r' || s[i] == '\n' {
			return -1
		}
		if s[i] == '\\' && s[i+1] == quote {
			return i
		}
	}
	return -1
}

func scanUnquotedValueEnd(s string, start int) int {
	i := start
	for i < len(s) {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' ||
			c == ',' || c == ';' || c == '}' || c == ')' || c == ']' ||
			c == '&' || c == '"' || c == '\'' {
			break
		}
		i++
	}
	return i
}

func shouldMaskUnquotedValue(op, val string) bool {
	if val == "" || val == Mask {
		return false
	}
	if val[0] == '=' {
		return false
	}
	if _, skip := unquotedNonSecrets[strings.ToLower(val)]; skip {
		return false
	}
	if op == ":" && len(val) < 4 {
		return false
	}
	return true
}

func isSensitiveMapKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return false
	}
	if _, ok := sensitiveKeyNames[k]; ok {
		return true
	}
	for sep := range []byte{'_', '-'} {
		_ = sep
	}
	if idx := strings.LastIndexAny(k, "_-"); idx != -1 && idx+1 < len(k) {
		suffix := k[idx+1:]
		if suffix == "password" || suffix == "passwd" || suffix == "pwd" || suffix == "token" || suffix == "secret" || suffix == "apikey" || suffix == "authorization" {
			return true
		}
	}
	for _, compound := range []string{"_api_key", "-api-key", "_access_token", "-access-token", "_auth_token", "-auth-token", "_refresh_token", "-refresh-token", "_client_secret", "-client-secret", "_secret_key", "-secret-key", "_private_key", "-private-key"} {
		if strings.HasSuffix(k, compound) {
			return true
		}
	}
	return false
}

func redactSensitiveKeyString(key, val string) string {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" || trimmed == Mask {
		return val
	}
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "authorization" || strings.HasSuffix(k, "_authorization") || strings.HasSuffix(k, "-authorization") {
		if idx := strings.IndexByte(trimmed, ' '); idx > 0 {
			scheme := trimmed[:idx]
			switch strings.ToLower(scheme) {
			case "bearer", "basic", "token", "digest", "apikey", "api-key":
				return scheme + " " + Mask
			}
		}
	}
	return Mask
}

// RedactBytes returns a copy of b with sensitive credentials masked.
func RedactBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return []byte(Redact(string(b)))
}

// RedactAny recursively traverses v and redacts any strings contained within,
// including typed nested slices, arrays, maps, pointers, and structs.
// Containers are cloned to avoid mutating input references.
func RedactAny(v any) any {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case string:
		return Redact(val)
	case []byte:
		return RedactBytes(val)
	case map[string]any:
		return RedactMap(val)
	case map[string]string:
		if val == nil {
			return map[string]string(nil)
		}
		out := make(map[string]string, len(val))
		for k, item := range val {
			if isSensitiveMapKey(k) {
				out[k] = redactSensitiveKeyString(k, item)
			} else {
				out[k] = Redact(item)
			}
		}
		return out
	case []any:
		if val == nil {
			return []any(nil)
		}
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = RedactAny(item)
		}
		return out
	case []string:
		if val == nil {
			return []string(nil)
		}
		out := make([]string, len(val))
		for i, item := range val {
			out[i] = Redact(item)
		}
		return out
	default:
		rv := reflect.ValueOf(v)
		return redactReflect(rv).Interface()
	}
}

func redactReflect(rv reflect.Value) reflect.Value {
	if !rv.IsValid() {
		return rv
	}

	if rv.Type() == reflect.TypeOf(json.RawMessage(nil)) {
		if rv.IsNil() {
			return rv
		}
		rawBytes := rv.Bytes()
		var decoded any
		if err := json.Unmarshal(rawBytes, &decoded); err == nil {
			redacted := RedactAny(decoded)
			if encoded, err := json.Marshal(redacted); err == nil {
				out := reflect.New(rv.Type()).Elem()
				out.SetBytes(encoded)
				return out
			}
		}
		out := reflect.New(rv.Type()).Elem()
		out.SetBytes(RedactBytes(rawBytes))
		return out
	}

	if rv.Type() == byteSliceType {
		if rv.IsNil() {
			return rv
		}
		return reflect.ValueOf(RedactBytes(rv.Bytes()))
	}

	switch rv.Kind() {
	case reflect.String:
		redacted := Redact(rv.String())
		out := reflect.New(rv.Type()).Elem()
		out.SetString(redacted)
		return out

	case reflect.Interface:
		if rv.IsNil() {
			return rv
		}
		elem := RedactAny(rv.Elem().Interface())
		if elem == nil {
			return rv
		}
		ev := reflect.ValueOf(elem)
		if ev.Type().AssignableTo(rv.Type()) {
			out := reflect.New(rv.Type()).Elem()
			out.Set(ev)
			return out
		}
		return rv

	case reflect.Slice:
		if rv.IsNil() {
			return rv
		}
		out := reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out.Index(i).Set(redactReflect(rv.Index(i)))
		}
		return out

	case reflect.Array:
		out := reflect.New(rv.Type()).Elem()
		for i := 0; i < rv.Len(); i++ {
			out.Index(i).Set(redactReflect(rv.Index(i)))
		}
		return out

	case reflect.Map:
		if rv.IsNil() {
			return rv
		}
		out := reflect.MakeMapWithSize(rv.Type(), rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			mk := iter.Key()
			mv := iter.Value()
			if mk.Kind() == reflect.String && isSensitiveMapKey(mk.String()) {
				out.SetMapIndex(mk, redactSensitiveReflectValue(mk.String(), mv))
				continue
			}
			out.SetMapIndex(mk, redactReflect(mv))
		}
		return out

	case reflect.Ptr:
		if rv.IsNil() {
			return rv
		}
		elem := redactReflect(rv.Elem())
		out := reflect.New(rv.Type().Elem())
		out.Elem().Set(elem)
		return out

	case reflect.Struct:
		if rv.Type().PkgPath() == "time" && rv.Type().Name() == "Time" {
			return rv
		}
		out := reflect.New(rv.Type()).Elem()
		out.Set(rv)
		for i := 0; i < out.NumField(); i++ {
			f := out.Field(i)
			if f.CanSet() {
				f.Set(redactReflect(f))
			}
		}
		return out

	default:
		return rv
	}
}

func redactSensitiveReflectValue(key string, mv reflect.Value) reflect.Value {
	if !mv.IsValid() {
		return mv
	}
	if mv.Kind() == reflect.Interface && !mv.IsNil() {
		inner := mv.Elem()
		if inner.Kind() == reflect.String {
			masked := redactSensitiveKeyString(key, inner.String())
			out := reflect.New(mv.Type()).Elem()
			strVal := reflect.New(inner.Type()).Elem()
			strVal.SetString(masked)
			out.Set(strVal)
			return out
		}
		if inner.Type() == byteSliceType && !inner.IsNil() {
			masked := []byte(redactSensitiveKeyString(key, string(inner.Bytes())))
			out := reflect.New(mv.Type()).Elem()
			out.Set(reflect.ValueOf(masked))
			return out
		}
	}
	if mv.Kind() == reflect.String {
		masked := redactSensitiveKeyString(key, mv.String())
		out := reflect.New(mv.Type()).Elem()
		out.SetString(masked)
		return out
	}
	if mv.Type() == byteSliceType && !mv.IsNil() {
		return reflect.ValueOf([]byte(redactSensitiveKeyString(key, string(mv.Bytes()))))
	}
	return redactReflect(mv)
}

// RedactMap recursively creates a sanitized copy of m with all string values redacted.
func RedactMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if isSensitiveMapKey(k) {
			switch tv := v.(type) {
			case string:
				out[k] = redactSensitiveKeyString(k, tv)
				continue
			case []byte:
				if len(tv) == 0 {
					out[k] = tv
				} else {
					out[k] = []byte(redactSensitiveKeyString(k, string(tv)))
				}
				continue
			}
		}
		out[k] = RedactAny(v)
	}
	return out
}

// SanitizeLog cleans raw log lines for terminal or file output.
func SanitizeLog(line []byte) []byte {
	if !ContainsCredentialBytes(line) {
		return line
	}
	return RedactBytes(line)
}
