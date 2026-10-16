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
// Only explicit references are resolved: a non-empty env_key, or an api_key of
// the form ${NAME}. Both fail if the variable is unset or empty. Any other value
// is a literal key, so a key that merely looks like a variable name is never
// swapped for process environment.
//
// Only call this for operator-controlled configuration (config file, admin
// provider tables). Values from agents or other user input must stay literal.
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
