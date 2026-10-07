//go:build embedocr && linux && amd64

package ocrbin

import _ "embed"

//go:embed bin/linux_amd64/ocr.gz
var embeddedGz []byte

//go:embed bin/linux_amd64/ocr.sha256
var embeddedSum string

func init() { compressed, sum = embeddedGz, embeddedSum }
