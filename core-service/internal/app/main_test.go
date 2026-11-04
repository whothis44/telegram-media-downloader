package app

import (
	"os"
	"testing"
	"time"
)

// Background maintenance passes act on whatever a test leaves in the
// catalog; tests that exercise them call the sweep directly.
func TestMain(m *testing.M) {
	integrityBootDelay = 24 * time.Hour
	diskRotatorStartDelay = 24 * time.Hour
	// Tests look at the figures right after changing the queue.
	ixStatusCacheTTL, ixApartCacheTTL = 0, 0
	os.Exit(m.Run())
}
