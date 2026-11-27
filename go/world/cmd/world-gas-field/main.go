// world-gas-field evaluates bounded, simulation-only concentration observations.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"io"
	"os"
)

func serve(input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 65536), 256<<10)
	encoder := json.NewEncoder(output)
	session := new(gasfield.Session)
	for scanner.Scan() {
		var req gasfield.Request
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&req)
		if err == nil {
			if e := decoder.Decode(new(any)); !errors.Is(e, io.EOF) {
				err = errors.New("one request object required")
			}
		}
		var result any
		if err == nil {
			result, err = session.Step(req)
		}
		if err != nil {
			result = map[string]string{"error": err.Error()}
		}
		if err = encoder.Encode(result); err != nil {
			return err
		}
	}
	return scanner.Err()
}
func main() {
	if err := serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
