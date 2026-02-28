package scan

import "os"

// osGetenv is the default implementation of envLookup.
func osGetenv(key string) string {
	return os.Getenv(key)
}
