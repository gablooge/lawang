package record

import (
	"errors"
	"fmt"
	"strings"
)

// MaxScopeID is the longest scope id, in bytes. A scope id is ASCII, so bytes and characters are
// the same count.
const MaxScopeID = 512

// ErrBadScopeID reports parts that cannot make a scope id, or a string that is not one. The
// message never quotes the input.
var ErrBadScopeID = errors.New("record: bad scope id")

// Scope is a scope id taken apart.
type Scope struct {
	// Provider is the internal provider key, never a sink's wire name.
	Provider string
	// ContainerKind is the provider's word for the kind of container: channel, list, mailbox.
	ContainerKind string
	// ContainerID is the provider's id of the container, decoded: exactly the bytes the
	// provider uses.
	ContainerID string
}

// ScopeID builds the scope id of a container:
//
//	{provider}:{container_kind}:{container_id}
//
// It is the one thing access is decided on, and it is a join key: a record carries it in
// Visibility.Scope, and the membership that access sync pushes for the same container carries
// the same string. Both sides therefore build it here, from the same three parts, and never by
// hand (ADR 3).
//
// provider is the internal provider key, not a wire name: a wire name is per sink and may
// change, and a scope id that changed with it would cut every delivered record off from its
// members. provider and containerKind are a lowercase letter followed by up to 31 of a-z, 0-9
// and underscore. containerID is the provider's id byte for byte, never case-folded or trimmed.
// The characters A-Z a-z 0-9 . _ ~ - stay as they are, every other byte becomes %XX with
// uppercase hex (so a Microsoft Graph id keeps its ':', '/', '=' and '+' and the result still
// has exactly two colons), and an id that is empty or holds a control character is refused. The
// result is ASCII and at most MaxScopeID bytes. The tenant is deliberately not part of it.
func ScopeID(provider, containerKind, containerID string) (string, error) {
	if !ValidProviderKey(provider) {
		return "", fmt.Errorf("%w: the provider key", ErrBadScopeID)
	}
	if !validName(containerKind, maxKind, false) {
		return "", fmt.Errorf("%w: the container kind", ErrBadScopeID)
	}
	if containerID == "" {
		return "", fmt.Errorf("%w: the container id is empty", ErrBadScopeID)
	}
	var b strings.Builder
	b.Grow(len(provider) + len(containerKind) + len(containerID) + 2)
	b.WriteString(provider)
	b.WriteByte(':')
	b.WriteString(containerKind)
	b.WriteByte(':')
	for i := range len(containerID) {
		c := containerID[i]
		switch {
		case unreserved(c):
			b.WriteByte(c)
		case control(c):
			return "", fmt.Errorf("%w: the container id holds a control character", ErrBadScopeID)
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0F])
		}
		if b.Len() > MaxScopeID {
			return "", fmt.Errorf("%w: longer than %d bytes", ErrBadScopeID, MaxScopeID)
		}
	}
	return b.String(), nil
}

// ParseScopeID takes a scope id apart. It accepts exactly the strings ScopeID produces, so
// ScopeID(ParseScopeID(s)) is s again, and no two different strings name one scope: an escape of
// a character that needs none, a lowercase hex digit and an escaped control character are all
// refused.
//
// Lawang parses its own scope ids. A sink does not need to: to a sink a scope id is opaque,
// and two of them name the same scope exactly when they are equal byte for byte.
func ParseScopeID(s string) (Scope, error) {
	if err := checkScopeID(s); err != nil {
		return Scope{}, err
	}
	first := strings.IndexByte(s, ':')
	second := first + 1 + strings.IndexByte(s[first+1:], ':')
	enc := s[second+1:]

	id := make([]byte, 0, len(enc))
	for i := 0; i < len(enc); i++ {
		if enc[i] == '%' {
			id = append(id, unhex(enc[i+1])<<4|unhex(enc[i+2]))
			i += 2
			continue
		}
		id = append(id, enc[i])
	}
	return Scope{Provider: s[:first], ContainerKind: s[first+1 : second], ContainerID: string(id)}, nil
}

// checkScopeID is the whole grammar, without allocating. Validate runs it for every record.
func checkScopeID(s string) error {
	if len(s) > MaxScopeID {
		return fmt.Errorf("%w: longer than %d bytes", ErrBadScopeID, MaxScopeID)
	}
	first := strings.IndexByte(s, ':')
	if first < 0 || !ValidProviderKey(s[:first]) {
		return fmt.Errorf("%w: the provider key", ErrBadScopeID)
	}
	rest := s[first+1:]
	second := strings.IndexByte(rest, ':')
	if second < 0 || !validName(rest[:second], maxKind, false) {
		return fmt.Errorf("%w: the container kind", ErrBadScopeID)
	}
	enc := rest[second+1:]
	if enc == "" {
		return fmt.Errorf("%w: the container id is empty", ErrBadScopeID)
	}
	for i := 0; i < len(enc); i++ {
		c := enc[i]
		if unreserved(c) {
			continue
		}
		if c != '%' || i+2 >= len(enc) || !isUpperHex(enc[i+1]) || !isUpperHex(enc[i+2]) {
			return fmt.Errorf("%w: the container id holds a character that is not escaped", ErrBadScopeID)
		}
		decoded := unhex(enc[i+1])<<4 | unhex(enc[i+2])
		if unreserved(decoded) || control(decoded) {
			return fmt.Errorf("%w: the container id holds an escape the format does not write", ErrBadScopeID)
		}
		i += 2
	}
	return nil
}

const upperHex = "0123456789ABCDEF"

// unreserved is the set RFC 3986 calls unreserved: the bytes a scope id carries as they are.
func unreserved(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
		c == '.' || c == '_' || c == '~' || c == '-'
}

func control(c byte) bool { return c < 0x20 || c == 0x7F }

func isUpperHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'A' && c <= 'F' }

func unhex(c byte) byte {
	if c >= 'A' {
		return c - 'A' + 10
	}
	return c - '0'
}
