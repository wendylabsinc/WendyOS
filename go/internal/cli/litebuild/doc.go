// Package litebuild configures and builds the Wendy Lite firmware.
//
// The configuration comes from the configurator master configuration of
// wendy-lite (master_config.yaml). This package is the Go port of the Python
// configurator (tools/configurator/conf_model.py and solve.py) and must
// give the same sdkconfig lines for the same input values. See
// wendy-lite/docs/configurator.md for the format.
//
// The Go configurator also asks the questions: it reads the "display"
// section of the master configuration (titles, labels, order), which the
// Python solver ignores. The questions follow the dependency order, offer
// only the answers that can lead to an allowed configuration, and skip a
// question that has only one allowed answer. An attribute that is not in
// the display section is internal: it is never shown, and it is an error
// if it needs a question.
package litebuild
