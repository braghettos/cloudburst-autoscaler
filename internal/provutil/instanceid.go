package provutil

import (
	"fmt"
	"strings"
)

// ParseInstanceID splits a provider-prefixed instance ID of the form
// "<prefix><region>/<id>" into its region and id components.
// prefix should include the trailing slash (e.g. "aws/", "hetzner/").
func ParseInstanceID(raw, prefix string) (region, id string, err error) {
	if !strings.HasPrefix(raw, prefix) {
		return "", "", fmt.Errorf("invalid instance ID format (expected prefix %q): %s", prefix, raw)
	}
	rest := strings.TrimPrefix(raw, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid instance ID format (expected %s<region>/<id>): %s", prefix, raw)
	}
	return parts[0], parts[1], nil
}
