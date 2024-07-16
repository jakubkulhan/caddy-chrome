package caddy_chrome

import _ "embed"

var (
	//go:embed js/on_new_document.js
	onNewDocumentScript string
)
