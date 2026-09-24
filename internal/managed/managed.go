// Package managed names where the organisation's configuration lives.
//
// It is internal so a test in this module can point it at a temporary file,
// and nothing outside the module can move it.
package managed

// ConfigFile is the managed configuration. Every layer below it yields to it.
var ConfigFile = "/etc/abhed/config.json"
