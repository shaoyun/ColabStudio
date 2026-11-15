//go:build headless

package main

import "os"

func main() {
	os.Exit(runWebServerCLI(os.Args[1:]))
}
