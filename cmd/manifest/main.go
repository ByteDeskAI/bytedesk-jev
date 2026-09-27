package main

import (
	"encoding/json"
	"flag"
	jev "github.com/ByteDeskAI/bytedesk-jev/jevplugin"
	"log"
	"os"
)

func main() {
	output := flag.String("out", "", "Write the generated manifest to this path (default stdout)")
	flag.Parse()
	writer := os.Stdout
	if *output != "" {
		file, err := os.Create(*output)
		if err != nil {
			log.Fatal(err)
		}
		defer file.Close()
		writer = file
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(jev.New().Manifest()); err != nil {
		log.Fatal(err)
	}
}
