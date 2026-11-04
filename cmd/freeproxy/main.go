// FreeProxy for Android/Termux. Made for hihellohejo.
package main

import (
	"os"

	"freeproxy/core"
)

func main() {
	if err := core.Run("", os.Stdin, os.Stdout, os.Args[1:], "0.0.0.0", 19132); err != nil {
		os.Exit(1)
	}
}
