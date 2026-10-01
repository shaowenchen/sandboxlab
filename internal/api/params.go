package api

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseTTL reads a duration from a request body. Empty means "no expiry
// requested", which the service reads as "use the default" at create time and
// as "remove the expiry" at renew time.
//
// It is a duration string rather than a number of seconds because the value is
// written by a person at least as often as by a program — `ttl: 2h` needs no
// explanation, and `ttl: 7200` does.
func parseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("ttl %q is not a duration (try \"90m\" or \"2h\")", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("ttl cannot be negative")
	}
	return d, nil
}

func parsePositiveInt(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return n, nil
}
