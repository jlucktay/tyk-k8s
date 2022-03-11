package main

import (
	"flag"

	"go.jlucktay.dev/tyk-k8s/cmd"
)

func main() {
	flag.Parse()
	cmd.Execute()
}
