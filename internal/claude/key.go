package claude

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// KeyFileEnv names the local key file (mode 0600). ANTHROPIC_API_KEY is never read: the env
// file reaches every unit, the credential only the units that call Claude.
const KeyFileEnv = "LEADHOUND_ANTHROPIC_KEY_FILE"

// CredentialName is the systemd LoadCredential name the units that call Claude get the key as.
const CredentialName = "anthropic-key" //nolint:gosec // G101: a systemd credential name, not a credential

var errNoKey = errors.New("claude: no Anthropic key: set " + KeyFileEnv + " or the " + CredentialName + " credential")

// LoadKey finds the Anthropic key: the systemd credential first (read without a mode check,
// because systemd hands it over group-readable with an ACL for the service user), then
// KeyFileEnv with the mode check. The value is trimmed; an empty file is an error.
func LoadKey(getenv func(string) string) (config.Secret, error) {
	if dir := getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		path := filepath.Join(dir, CredentialName)
		if _, err := os.Stat(path); err == nil {
			return readKey(path, false)
		}
	}
	if path := getenv(KeyFileEnv); path != "" {
		return readKey(path, true)
	}
	return "", errNoKey
}

// readKey reads a key file. With checkMode it refuses a file that group or other can read
// (skipped on Windows, which has no such bits). Errors never carry the file's content.
func readKey(path string, checkMode bool) (config.Secret, error) {
	if checkMode && runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("anthropic key: %w", err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("anthropic key %s is readable by group or other (mode %v); run chmod 600 on it", path, fi.Mode().Perm())
		}
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the operator names the key file
	if err != nil {
		return "", fmt.Errorf("anthropic key: %w", err)
	}
	defer clear(b)
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("anthropic key %s is empty", path)
	}
	return config.Secret(k), nil
}
