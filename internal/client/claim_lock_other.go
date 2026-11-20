//go:build !linux && !darwin

package client

import (
	"fmt"
	"os"
)

func lockClaim(f *os.File) error {
	return fmt.Errorf("stopped pool claims require Linux or macOS file locking")
}
