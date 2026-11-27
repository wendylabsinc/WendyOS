// world-collab-demo is a JSON-lines coordinator for a synthetic local simulation.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/wendylabsinc/wendy/go/world/internal/collabdemo"
)

func run(path string, input io.Reader, output io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if stat.Size() > 8<<20 {
		return fmt.Errorf("scene exceeds 8 MiB")
	}
	var scene collabdemo.Scene
	decoder := json.NewDecoder(io.LimitReader(file, 8<<20))
	if err = decoder.Decode(&scene); err != nil {
		return err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return fmt.Errorf("scene must contain exactly one JSON document")
	}
	coordinator, err := collabdemo.New(scene)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	writer := bufio.NewWriter(output)
	encoder := json.NewEncoder(writer)
	for scanner.Scan() {
		var in collabdemo.Input
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&in)
		if err == nil {
			var extra any
			if decoder.Decode(&extra) != io.EOF {
				err = fmt.Errorf("one JSON input is required per line")
			}
		}
		var result collabdemo.Output
		if err != nil {
			result = coordinator.Refused(collabdemo.Input{}, "malformed control input")
		} else {
			result = coordinator.Tick(in)
		}
		if err = encoder.Encode(result); err != nil {
			return err
		}
		if err = writer.Flush(); err != nil {
			return err
		}
	}
	return scanner.Err()
}
func main() {
	path := flag.String("scene", "", "synthetic scene configuration JSON")
	flag.Parse()
	if *path == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "--scene is required")
		os.Exit(2)
	}
	if err := run(*path, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
