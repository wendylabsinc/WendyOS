package go2project

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"golang.org/x/sys/unix"
)

func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func hexDigest(s string) bool { return lowerHex(s, 64) }

var tokenRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func token(s string) bool { return tokenRE.MatchString(s) }

func strict(data []byte, out any, limit int) error {
	if !utf8.Valid(data) {
		return errors.New("JSON must be UTF-8")
	}
	if err := authority.DecodeStrictLimit(data, out, limit); err != nil {
		return err
	}
	return exactFields(data, reflect.TypeOf(out).Elem())
}
func exactFields(data []byte, t reflect.Type) error {
	if t == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields == nil {
			return errors.New("JSON object required")
		}
		allowed := map[string]reflect.Type{}
		required := map[string]bool{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" {
				name = f.Name
			}
			if name != "-" {
				allowed[name] = f.Type
				required[name] = !strings.Contains(f.Tag.Get("json"), ",omitempty")
			}
		}
		for name, needed := range required {
			if _, ok := fields[name]; needed && !ok {
				return errors.New("required JSON field missing")
			}
		}
		for name, raw := range fields {
			ft, ok := allowed[name]
			if !ok {
				return errors.New("unknown or case-aliased JSON field")
			}
			if bytes.Equal(raw, []byte("null")) && ft.Kind() == reflect.Pointer {
				continue
			}
			if err := exactFields(raw, ft); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var values []json.RawMessage
		if json.Unmarshal(data, &values) != nil {
			return errors.New("JSON array required")
		}
		for _, v := range values {
			if err := exactFields(v, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReadFile rejects symlinks in every path component and refuses blocking special
// files. Parent directories remain a trusted local commissioning boundary.
func ReadFile(path string, limit int) ([]byte, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(absolute, current), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, e := os.Lstat(current)
		if e != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("input path unavailable or aliased")
		}
	}
	fd, err := unix.Open(absolute, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("input unavailable")
	}
	f := os.NewFile(uintptr(fd), "go2-project-input")
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > int64(limit) {
		return nil, errors.New("input must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	after, e := f.Stat()
	if err != nil || e != nil || len(data) > limit || int64(len(data)) != before.Size() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("input incomplete or changed")
	}
	return data, nil
}

func WriteNew(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > 2<<20 {
		return errors.New("binding report exceeds limit")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent := filepath.Dir(absolute)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent {
		return errors.New("output parent must exist without aliases")
	}
	f, err := os.OpenFile(absolute, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("output must be a new file")
	}
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	dir, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
