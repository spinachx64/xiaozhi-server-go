package configs

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ResolveAPIKey resolves credentials only at runtime. Callers must not persist
// the result into the stored server/provider configuration.
// env_key and ${NAME} are explicit references and fail if unset or empty.
// A bare name resolves when the environment variable exists; otherwise it is
// preserved as a literal key for compatibility with existing configurations.
func ResolveAPIKey(value, envKey string) (string, error) {
	value, envKey = strings.TrimSpace(value), strings.TrimSpace(envKey)
	if envKey != "" {
		return requiredEnvironment(envKey)
	}
	if strings.HasPrefix(value, "${") {
		if !strings.HasSuffix(value, "}") {
			return "", fmt.Errorf("invalid API key environment reference; use ${NAME}")
		}
		return requiredEnvironment(value[2 : len(value)-1])
	}
	if environmentName.MatchString(value) {
		if resolved, exists := os.LookupEnv(value); exists {
			if resolved = strings.TrimSpace(resolved); resolved == "" {
				return "", fmt.Errorf("API key environment variable %s is empty", value)
			}
			return resolved, nil
		}
	}
	return value, nil
}

func requiredEnvironment(name string) (string, error) {
	if !environmentName.MatchString(name) {
		return "", fmt.Errorf("invalid API key environment variable name")
	}
	value, exists := os.LookupEnv(name)
	if !exists || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("API key environment variable %s is not set or empty", name)
	}
	return strings.TrimSpace(value), nil
}
