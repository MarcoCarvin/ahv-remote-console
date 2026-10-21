// Package plugins — in-tree plugin registration linkage.
//
// In-tree plugins register themselves via sdk.Register() in their init().
// To enable a plugin, blank-import its package below. Adding a new plugin is
// a one-line change here.
package plugins

import (
	_ "github.com/MarcoCarvin/ahv-remote-console/plugins/ahv"
)
