//go:build go1.16

package fiberi18n

import "embed"

type EmbedLoader struct {
	FS embed.FS
}

func (e *EmbedLoader) LoadMessage(path string) ([]byte, error) {
	return e.FS.ReadFile(path)
}
