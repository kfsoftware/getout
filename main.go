package main

import (
	"github.com/kfsoftware/getout/cmd"
	"os"
)

func main() {
	if err := cmd.NewCmdGetOut().Execute(); err != nil {
		os.Exit(1)
	}
}
