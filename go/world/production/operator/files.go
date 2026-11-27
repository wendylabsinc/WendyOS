package operator

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"strconv"
	"unicode/utf8"
)

const MaxConfigBytes = 4 << 20
const MaxKeyPEMBytes = 32 << 10

func readRegular(path string, max int, private bool) ([]byte, error) {
	if path == "" {
		return nil, errors.New("explicit input file required")
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("input must be a regular file, not a symlink or pipe")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("input file cannot be opened")
	}
	f := os.NewFile(uintptr(fd), "operator-input")
	defer f.Close()
	st, err = f.Stat()
	if err != nil || !st.Mode().IsRegular() || (private && st.Mode().Perm()&0077 != 0) {
		return nil, errors.New("credential configuration must be an owner-only regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil || len(b) > max {
		clear(b)
		return nil, errors.New("input exceeds its bound or cannot be read")
	}
	return b, nil
}

// ReadBodyFile loads one bounded read-query body, without path discovery.
func ReadBodyFile(path string) ([]byte, error) { return readRegular(path, 64<<10, false) }

// LoadTrustRoots loads explicit public trust, never the credential's peer chain.
func LoadTrustRoots(path string) ([][]byte, error) {
	b, err := readRegular(path, 128<<10, false)
	if err != nil {
		return nil, err
	}
	defer clear(b)
	certs, err := parsePEM(b, 8)
	if err != nil {
		return nil, err
	}
	roots := make([][]byte, len(certs))
	total := 0
	for i, c := range certs {
		if !c.IsCA || !c.BasicConstraintsValid || (c.KeyUsage != 0 && c.KeyUsage&x509.KeyUsageCertSign == 0) {
			return nil, errors.New("trust bundle contains a non-CA certificate")
		}
		total += len(c.Raw)
		if total > 64<<10 {
			return nil, errors.New("trust bundle exceeds decoded bound")
		}
		roots[i] = append([]byte(nil), c.Raw...)
	}
	return roots, nil
}

// The existing config includes unrelated settings and secrets. Only exact known
// fields are selected; duplicates anywhere are refused and nothing is rewritten.
func decodeConfigJSON(b []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(b) || !validStringEscapes(b) {
		return nil, errors.New("configuration is not UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := walk(d, 0); err != nil {
		return nil, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("configuration contains trailing JSON")
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(b, &root) != nil || root == nil {
		return nil, errors.New("configuration object required")
	}
	return root, nil
}
func walk(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("configuration nesting exceeds bound")
	}
	value, err := d.Token()
	if err != nil {
		return errors.New("invalid configuration JSON")
	}
	delimiter, ok := value.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return errors.New("invalid configuration key")
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return errors.New("duplicate configuration key")
			}
			seen[s] = true
			if err = walk(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid configuration object")
		}
	case '[':
		for d.More() {
			if err := walk(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid configuration array")
		}
	default:
		return errors.New("invalid configuration delimiter")
	}
	return nil
}

func validStringEscapes(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] != '"' {
			continue
		}
		i++
		for ; i < len(b) && b[i] != '"'; i++ {
			if b[i] < 0x20 {
				return false
			}
			if b[i] != '\\' {
				continue
			}
			i++
			if i >= len(b) {
				return false
			}
			if b[i] != 'u' {
				continue
			}
			if i+4 >= len(b) {
				return false
			}
			v, err := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
			if err != nil {
				return false
			}
			i += 4
			if v >= 0xdc00 && v <= 0xdfff {
				return false
			}
			if v >= 0xd800 && v <= 0xdbff {
				if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
					return false
				}
				low, err := strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
				if err != nil || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 6
			}
		}
		if i >= len(b) {
			return false
		}
	}
	return true
}
