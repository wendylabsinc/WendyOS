// world-people projects actual image-inference results and tracks anonymous
// people in simulation time. Input and output are bounded newline-delimited JSON.
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

	"github.com/wendylabsinc/wendy/go/world/internal/people"
)

func serve(input io.Reader, output io.Writer) error { return serveCatalog(input, output, nil) }
func serveCatalog(input io.Reader, output io.Writer, catalog *catalogMonitor) error {
	scan := bufio.NewScanner(input)
	scan.Buffer(make([]byte, 65536), 2<<20)
	encode := json.NewEncoder(output)
	session := new(people.Session)
	var history people.Output
	for scan.Scan() {
		var req people.Request
		d := json.NewDecoder(bytes.NewReader(scan.Bytes()))
		d.DisallowUnknownFields()
		err := d.Decode(&req)
		if err == nil {
			if e := d.Decode(new(any)); !errors.Is(e, io.EOF) {
				err = errors.New("one request object required")
			}
		}
		var result any
		if err == nil {
			if catalog != nil {
				err = catalog.bind(&req)
				if err == nil && (req.NowNS < 0 || req.NowNS < int64(history.NowNS) || req.NowNS > int64(24*time.Hour)) {
					err = errors.New("invalid catalog stream time")
				}
			}
			if err == nil {
				if catalog != nil && !catalog.check() {
					suppressedHistory(&history)
					history.ParticipantID = req.ParticipantID
					history.NowNS = req.NowNS
					result = catalogOutput(history, catalog)
				} else {
					var out people.Output
					out, err = session.Step(req)
					if err == nil && catalog != nil && !catalog.verifyPinned() {
						suppressedHistory(&history)
						history.ParticipantID = req.ParticipantID
						history.NowNS = req.NowNS
						result = catalogOutput(history, catalog)
					} else if err == nil {
						history = out
						if catalog != nil {
							result = catalogOutput(out, catalog)
						} else {
							result = out
						}
					}
				}
			}
		}
		if err != nil {
			result = map[string]any{"error": err.Error(), "tracks": []any{}}
		}
		if err = encode.Encode(result); err != nil {
			return err
		}
	}
	return scan.Err()
}
func main() {
	credentials := flag.String("zone-credentials", "", "read credentials for selected catalog replica")
	trust := flag.String("zone-trust", "", "pinned root and reloadable current revocations")
	digest := flag.String("zone-sha256", "", "exact zone revision")
	source := flag.String("zone-source", "", "exact source identity JSON")
	flag.Parse()
	var catalog *catalogMonitor
	var err error
	if flag.NArg() != 0 {
		err = errors.New("unexpected arguments")
	}
	if *credentials != "" || *trust != "" || *digest != "" || *source != "" {
		if *credentials == "" || *trust == "" || *digest == "" || *source == "" {
			err = errors.New("all four zone selection flags are required")
		} else {
			catalog, err = openCatalog(*credentials, *trust, *digest, *source)
		}
	}
	if err == nil {
		err = serveCatalog(os.Stdin, os.Stdout, catalog)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
