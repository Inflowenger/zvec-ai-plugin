package main

import (
	_ "embed"
	"log"
)

// manualText is the plugin's page on the platform — what the node is, how to
// point it at storage, and what each action does. It is embedded rather than
// read at run time so the binary is the whole deployment.
//
//go:embed manual.md
var manualText string

func manual() string {
	if manualText == "" {
		log.Println("zvec plugin: the manual is empty")
	}
	return manualText
}
