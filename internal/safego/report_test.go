package safego

import (
	"bytes"
	"log"
	"testing"
)

func TestAPanicBeforeInstallIsNotWritten(t *testing.T) {
	saved := installed.Swap(nil)
	t.Cleanup(func() { installed.Store(saved) })
	var buf bytes.Buffer
	output := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(output) })

	func() {
		defer Recover("early")
		panic("15550100042@s.whatsapp.net")
	}()
	if buf.Len() != 0 {
		t.Fatalf("a panic recovered before Install reached the default logger: %q", buf.String())
	}
}
