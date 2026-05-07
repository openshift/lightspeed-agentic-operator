package main

import (
	"os"

	"github.com/openshift/lightspeed-agentic-operator/cli"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

func main() {
	streams := genericclioptions.IOStreams{
		In:     os.Stdin,
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}
	cmd := cli.NewRootCmd(streams)
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
