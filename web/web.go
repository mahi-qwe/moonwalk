// Package web embeds the customer and kitchen pages into the binary.
package web

import "embed"

// Static is served under /static (style.css, common.js).
//
//go:embed style.css common.js
var Static embed.FS

//go:embed customer.html
var Customer []byte

//go:embed kitchen.html
var Kitchen []byte
