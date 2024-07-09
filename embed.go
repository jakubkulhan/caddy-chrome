package caddy_chrome

import _ "embed"

var (
	//go:embed js/get_html.js
	getHTMLScript string

	//go:embed js/on_new_document.js
	onNewDocumentScript string
)
