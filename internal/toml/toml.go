// Package toml implements the well-defined subset of TOML that lastbeat
// configuration files use: comments, bare and quoted keys, basic and
// literal strings, integers, booleans, arrays (including multi-line),
// [tables] and [[arrays of tables]], with dotted names in headers.
//
// The subset is parsed with line-accurate error messages, because a config
// typo discovered at 3 a.m. must point at the exact line. Features outside
// the subset (dates, floats, inline tables, dotted keys in assignments)
// are rejected with an explicit "not supported" error rather than being
// silently misread. The full grammar is documented in docs/config.md.
package toml

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Value is one decoded TOML value: string, int64, bool, []Value, *Table
// (for [table]) or []*Table (for [[array of tables]]).
type Value interface{}

// Table is a decoded TOML table. Line records where each key was set so
// higher layers can produce positioned validation errors.
type Table struct {
	Values map[string]Value
	Line   map[string]int
}

func newTable() *Table {
	return &Table{Values: map[string]Value{}, Line: map[string]int{}}
}

// ParseError is a positioned syntax or structure error.
type ParseError struct {
	LineNo int
	Msg    string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("line %d: %s", e.LineNo, e.Msg)
}

func errAt(line int, format string, args ...interface{}) error {
	return &ParseError{LineNo: line, Msg: fmt.Sprintf(format, args...)}
}

// Parse decodes src into the root table.
func Parse(src string) (*Table, error) {
	p := &parser{root: newTable()}
	p.current = p.root
	lines := strings.Split(src, "\n")
	i := 0
	for i < len(lines) {
		lineNo := i + 1
		line := strings.TrimSpace(stripComment(lines[i]))
		i++
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "[["):
			if err := p.openArrayTable(line, lineNo); err != nil {
				return nil, err
			}
		case strings.HasPrefix(line, "["):
			if err := p.openTable(line, lineNo); err != nil {
				return nil, err
			}
		default:
			consumed, err := p.assign(line, lines[i:], lineNo)
			if err != nil {
				return nil, err
			}
			i += consumed
		}
	}
	return p.root, nil
}

type parser struct {
	root    *Table
	current *Table
}

// stripComment removes a trailing # comment, respecting quoted strings.
func stripComment(line string) string {
	inBasic, inLiteral := false, false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\\' && inBasic:
			i++ // skip the escaped character
		case c == '"' && !inLiteral:
			inBasic = !inBasic
		case c == '\'' && !inBasic:
			inLiteral = !inLiteral
		case c == '#' && !inBasic && !inLiteral:
			return line[:i]
		}
	}
	return line
}

func (p *parser) openTable(line string, lineNo int) error {
	if !strings.HasSuffix(line, "]") {
		return errAt(lineNo, "table header %q is missing the closing ]", line)
	}
	name := strings.TrimSpace(line[1 : len(line)-1])
	parts, err := splitHeader(name, lineNo)
	if err != nil {
		return err
	}
	t := p.root
	for i, part := range parts {
		last := i == len(parts)-1
		existing, ok := t.Values[part]
		if !ok {
			nt := newTable()
			t.Values[part] = nt
			t.Line[part] = lineNo
			t = nt
			continue
		}
		sub, isTable := existing.(*Table)
		if !isTable {
			if arr, isArr := existing.([]*Table); isArr && !last {
				t = arr[len(arr)-1]
				continue
			}
			return errAt(lineNo, "cannot redefine %q as a table (already set on line %d)", part, t.Line[part])
		}
		if last {
			return errAt(lineNo, "table [%s] already defined on line %d", name, t.Line[part])
		}
		t = sub
	}
	p.current = t
	return nil
}

func (p *parser) openArrayTable(line string, lineNo int) error {
	if !strings.HasSuffix(line, "]]") {
		return errAt(lineNo, "array-of-tables header %q is missing the closing ]]", line)
	}
	name := strings.TrimSpace(line[2 : len(line)-2])
	parts, err := splitHeader(name, lineNo)
	if err != nil {
		return err
	}
	t := p.root
	for i, part := range parts {
		last := i == len(parts)-1
		existing, ok := t.Values[part]
		if !ok {
			if last {
				nt := newTable()
				t.Values[part] = []*Table{nt}
				t.Line[part] = lineNo
				p.current = nt
				return nil
			}
			nt := newTable()
			t.Values[part] = nt
			t.Line[part] = lineNo
			t = nt
			continue
		}
		if last {
			arr, isArr := existing.([]*Table)
			if !isArr {
				return errAt(lineNo, "cannot append to %q: it is not an array of tables (set on line %d)", part, t.Line[part])
			}
			nt := newTable()
			t.Values[part] = append(arr, nt)
			p.current = nt
			return nil
		}
		switch v := existing.(type) {
		case *Table:
			t = v
		case []*Table:
			t = v[len(v)-1]
		default:
			return errAt(lineNo, "cannot descend into %q: it is not a table", part)
		}
	}
	return nil
}

// splitHeader splits a header name on dots, allowing quoted segments.
func splitHeader(name string, lineNo int) ([]string, error) {
	if name == "" {
		return nil, errAt(lineNo, "empty table name")
	}
	var parts []string
	rest := name
	for {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return nil, errAt(lineNo, "table name %q has an empty segment", name)
		}
		var part string
		if rest[0] == '"' || rest[0] == '\'' {
			s, remaining, err := scanString(rest, lineNo)
			if err != nil {
				return nil, err
			}
			part, rest = s, strings.TrimSpace(remaining)
		} else {
			end := strings.IndexByte(rest, '.')
			if end < 0 {
				part, rest = strings.TrimSpace(rest), ""
			} else {
				part, rest = strings.TrimSpace(rest[:end]), rest[end:]
			}
			if !isBareKey(part) {
				return nil, errAt(lineNo, "invalid table name segment %q", part)
			}
		}
		parts = append(parts, part)
		if rest == "" {
			return parts, nil
		}
		if rest[0] != '.' {
			return nil, errAt(lineNo, "unexpected %q in table name %q", rest, name)
		}
		rest = rest[1:]
	}
}

func isBareKey(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// assign parses `key = value`. Multi-line arrays consume following lines;
// the number of extra lines consumed is returned.
func (p *parser) assign(line string, following []string, lineNo int) (int, error) {
	key, rest, err := scanKey(line, lineNo)
	if err != nil {
		return 0, err
	}
	if _, exists := p.current.Values[key]; exists {
		return 0, errAt(lineNo, "key %q already set on line %d", key, p.current.Line[key])
	}
	val, consumed, err := parseValue(rest, following, lineNo)
	if err != nil {
		return 0, err
	}
	p.current.Values[key] = val
	p.current.Line[key] = lineNo
	return consumed, nil
}

func scanKey(line string, lineNo int) (key, rest string, err error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", errAt(lineNo, "expected a key")
	}
	if line[0] == '"' || line[0] == '\'' {
		key, rest, err = scanString(line, lineNo)
		if err != nil {
			return "", "", err
		}
	} else {
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return "", "", errAt(lineNo, "expected `key = value`, got %q", line)
		}
		key = strings.TrimSpace(line[:eq])
		rest = line[eq:]
		if strings.Contains(key, ".") {
			return "", "", errAt(lineNo, "dotted keys are not supported in assignments; use a [%s] table header", key[:strings.LastIndex(key, ".")])
		}
		if !isBareKey(key) {
			return "", "", errAt(lineNo, "invalid key %q", key)
		}
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "=") {
		return "", "", errAt(lineNo, "expected = after key %q", key)
	}
	return key, strings.TrimSpace(rest[1:]), nil
}

// parseValue decodes the value part of an assignment. following holds the
// source lines after the current one, for multi-line arrays.
func parseValue(rest string, following []string, lineNo int) (Value, int, error) {
	if rest == "" {
		return nil, 0, errAt(lineNo, "missing value after =")
	}
	if rest[0] == '[' {
		return parseArray(rest, following, lineNo)
	}
	v, remaining, err := parseScalar(rest, lineNo)
	if err != nil {
		return nil, 0, err
	}
	if strings.TrimSpace(remaining) != "" {
		return nil, 0, errAt(lineNo, "unexpected trailing %q after value", strings.TrimSpace(remaining))
	}
	return v, 0, nil
}

func parseScalar(rest string, lineNo int) (Value, string, error) {
	switch {
	case rest[0] == '"' || rest[0] == '\'':
		s, remaining, err := scanString(rest, lineNo)
		return s, remaining, err
	case strings.HasPrefix(rest, "true"):
		return true, rest[4:], nil
	case strings.HasPrefix(rest, "false"):
		return false, rest[5:], nil
	case rest[0] == '+' || rest[0] == '-' || (rest[0] >= '0' && rest[0] <= '9'):
		i := 0
		if rest[0] == '+' || rest[0] == '-' {
			i = 1
		}
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '_') {
			i++
		}
		lit := rest[:i]
		if i < len(rest) && (rest[i] == '.' || rest[i] == 'e' || rest[i] == 'E' || rest[i] == ':' || rest[i] == '-') {
			return nil, "", errAt(lineNo, "floats and dates are not supported (got %q); quote it if you meant a string", rest)
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(lit, "_", ""), 10, 64)
		if err != nil {
			return nil, "", errAt(lineNo, "invalid integer %q", lit)
		}
		return n, rest[i:], nil
	default:
		return nil, "", errAt(lineNo, "unrecognized value %q (inline tables and dates are not supported)", rest)
	}
}

// parseArray decodes `[ a, b, ... ]`, possibly spanning multiple lines.
func parseArray(rest string, following []string, lineNo int) (Value, int, error) {
	items := []Value{}
	src := rest[1:] // past the opening [
	consumed := 0
	curLine := lineNo
	needComma := false
	for {
		src = strings.TrimSpace(src)
		if src == "" {
			if consumed >= len(following) {
				return nil, 0, errAt(lineNo, "unterminated array")
			}
			src = strings.TrimSpace(stripComment(following[consumed]))
			consumed++
			curLine = lineNo + consumed
			continue
		}
		if src[0] == ']' {
			src = strings.TrimSpace(src[1:])
			if src != "" {
				return nil, 0, errAt(curLine, "unexpected trailing %q after array", src)
			}
			return items, consumed, nil
		}
		if needComma {
			if src[0] != ',' {
				return nil, 0, errAt(curLine, "expected , or ] in array, got %q", src)
			}
			src = src[1:]
			needComma = false
			continue
		}
		if src[0] == ',' {
			return nil, 0, errAt(curLine, "unexpected , in array")
		}
		if src[0] == '[' {
			return nil, 0, errAt(curLine, "nested arrays are not supported")
		}
		v, remaining, err := parseScalar(src, curLine)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, v)
		src = remaining
		needComma = true
	}
}

// scanString decodes a leading basic ("...") or literal ('...') string and
// returns the decoded text plus the unconsumed remainder of the line.
func scanString(src string, lineNo int) (string, string, error) {
	quote := src[0]
	if quote == '\'' {
		end := strings.IndexByte(src[1:], '\'')
		if end < 0 {
			return "", "", errAt(lineNo, "unterminated literal string")
		}
		return src[1 : 1+end], src[end+2:], nil
	}
	var b strings.Builder
	i := 1
	for i < len(src) {
		c := src[i]
		switch c {
		case '"':
			return b.String(), src[i+1:], nil
		case '\\':
			if i+1 >= len(src) {
				return "", "", errAt(lineNo, "dangling backslash in string")
			}
			i++
			switch e := src[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'u', 'U':
				n := 4
				if e == 'U' {
					n = 8
				}
				if i+n >= len(src) {
					return "", "", errAt(lineNo, "truncated \\%c escape", e)
				}
				code, err := strconv.ParseUint(src[i+1:i+1+n], 16, 32)
				if err != nil || !utf8.ValidRune(rune(code)) {
					return "", "", errAt(lineNo, "invalid \\%c escape %q", e, src[i+1:i+1+n])
				}
				b.WriteRune(rune(code))
				i += n
			default:
				return "", "", errAt(lineNo, "unsupported escape \\%c", e)
			}
		default:
			b.WriteByte(c)
		}
		i++
	}
	return "", "", errAt(lineNo, "unterminated string")
}
