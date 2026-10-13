package main

import (
	"fmt"
	"os"
)

var version, publicKey, defaultUpdateURL string

const crashExitCode = 3

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(version)
		return
	}
	fmt.Fprintln(os.Stderr, "badrelease: dying on start, as every start of this release does")
	os.Exit(crashExitCode)
}
