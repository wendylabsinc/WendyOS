// world-fusion-view consumes delivered reports from fixed simulation gateways.
// It has no network, credentials or publication privileges.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusionview"
)

const maxLine = 16 << 20

type request struct {
	Now     time.Time           `json:"now"`
	Reports []fusionview.Report `json:"reports"`
}

func decode(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if e := d.Decode(new(any)); !errors.Is(e, io.EOF) {
		return errors.New("one JSON object required")
	}
	return nil
}
func serve(input io.Reader, output io.Writer, cfg fusionview.Config) error {
	selector, err := fusionview.New(cfg)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 65536), maxLine)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		var req request
		err := decode(scanner.Bytes(), &req)
		var view fusionview.View
		if err == nil {
			view, err = selector.Update(req.Now, req.Reports)
		}
		if err != nil {
			if err = encoder.Encode(map[string]string{"error": err.Error()}); err != nil {
				return err
			}
		} else if err = encoder.Encode(view); err != nil {
			return err
		}
	}
	return scanner.Err()
}
func main() {
	path := flag.String("config", "", "fixed selector scope and gateway catalog")
	flag.Parse()
	if *path == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "--config required")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*path)
	var cfg fusionview.Config
	if err == nil && len(raw) > 32<<10 {
		err = errors.New("config exceeds bound")
	}
	if err == nil {
		err = decode(raw, &cfg)
	}
	if err == nil {
		err = serve(os.Stdin, os.Stdout, cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
