package agent

import (
	"os"
	"testing"

	"github.com/gigovich/aigem/internal/testenv"
)

func TestMain(m *testing.M) {
	os.Exit(testenv.Run(m))
}
