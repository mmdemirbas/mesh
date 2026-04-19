package filesync

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	deviceIDBytes = 6
	deviceIDChars = 10
)

// crockfordEnc is Crockford's base32 alphabet: digits 0-9 and uppercase
// letters with I, L, O, U removed. Case-insensitive on input; emits
// uppercase on output.
var crockfordEnc = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// generateDeviceID returns a new random device identifier in canonical
// form: 10 uppercase Crockford base32 characters, no separator.
func generateDeviceID() string {
	var b [deviceIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("filesync: crypto/rand: %v", err))
	}
	return crockfordEnc.EncodeToString(b[:])
}

// formatDeviceID returns the display form of a canonical device ID, with
// a single dash between the two halves (XXXXX-XXXXX). Non-canonical
// input is returned unchanged.
func formatDeviceID(id string) string {
	if len(id) != deviceIDChars {
		return id
	}
	return id[:deviceIDChars/2] + "-" + id[deviceIDChars/2:]
}

// parseDeviceID normalizes user input into canonical form: dashes and
// whitespace are ignored, lowercase is upper-cased. Returns an error if
// the result is not a valid device ID.
func parseDeviceID(s string) (string, error) {
	var b strings.Builder
	b.Grow(deviceIDChars)
	for _, r := range s {
		switch {
		case r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r':
			continue
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) != deviceIDChars {
		return "", fmt.Errorf("device id must decode to %d chars, got %d", deviceIDChars, len(out))
	}
	if _, err := crockfordEnc.DecodeString(out); err != nil {
		return "", fmt.Errorf("device id: %w", err)
	}
	return out, nil
}

// loadOrCreateDeviceID reads the persisted device ID from
// <dir>/device-id, creating a new one atomically if the file does not
// exist. Mode 0600 — the ID is stable identity, not a secret but not
// meant to be world-readable either.
func loadOrCreateDeviceID(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("ensure filesync dir: %w", err)
	}
	path := filepath.Join(dir, "device-id")
	data, err := os.ReadFile(path)
	if err == nil {
		id, perr := parseDeviceID(string(data))
		if perr != nil {
			return "", fmt.Errorf("device id at %s: %w", path, perr)
		}
		return id, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read device id: %w", err)
	}
	id := generateDeviceID()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(id), 0o600); err != nil {
		return "", fmt.Errorf("write device id tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename device id: %w", err)
	}
	return id, nil
}
