package main

import (
	"os"

	"github.com/Fahnoee/berth/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
