(function caddyChromeSerializeDOM() {
    const VOID_ELEMENTS = new Set([
        "area", "base", "br", "col", "embed", "hr", "img", "input",
        "link", "meta", "param", "source", "track", "wbr",
    ]);

    const escape = (s) => s
        .replace(/&/g, "&amp;")
        .replace(/</g, "&lt;")
        .replace(/>/g, "&gt;")
        .replace(/"/g, "&#34;")
        .replace(/'/g, "&#39;");

    const serializeChildren = (parent, parts) => {
        for (const child of parent.childNodes) {
            serializeNode(child, parts);
        }
    };

    const serializeElement = (el, parts) => {
        const local = el.localName;
        parts.push("<", local);
        for (const attr of el.attributes) {
            parts.push(" ", attr.name);
            if (attr.value !== "") {
                parts.push('="', escape(attr.value), '"');
            }
        }
        if (VOID_ELEMENTS.has(local)) {
            parts.push(" />");
            return;
        }
        parts.push(">");

        // Real shadow root attached by JS or by the parser (declarative DSD).
        if (el.shadowRoot) {
            parts.push('<template shadowrootmode="', escape(el.shadowRoot.mode), '">');
            serializeChildren(el.shadowRoot, parts);
            parts.push("</template>");
        }

        // <template shadowrootmode="..."> that the parser left as a regular
        // template element (e.g. when the implementation has not yet
        // implemented declarative shadow DOM). Emit its content as a
        // shadowrootmode template too so the client can adopt it.
        if (local === "template") {
            const mode = el.getAttribute("shadowrootmode");
            const content = el.content;
            if (mode && content) {
                // The element itself is the shadowrootmode template; its
                // content fragment is the shadow root contents.
                serializeChildren(content, parts);
                parts.push("</", local, ">");
                return;
            }
            if (content) {
                serializeChildren(content, parts);
                parts.push("</", local, ">");
                return;
            }
        }

        if (local === "script" || local === "style") {
            for (const child of el.childNodes) {
                if (child.nodeType === 3) {
                    parts.push(child.nodeValue);
                } else {
                    serializeNode(child, parts);
                }
            }
        } else {
            serializeChildren(el, parts);
        }
        parts.push("</", local, ">");
    };

    const serializeNode = (node, parts) => {
        switch (node.nodeType) {
            case 1: // ELEMENT_NODE
                serializeElement(node, parts);
                return;
            case 3: // TEXT_NODE
                parts.push(escape(node.nodeValue));
                return;
            case 8: // COMMENT_NODE
                parts.push("<!--", node.nodeValue, "-->");
                return;
            case 9: // DOCUMENT_NODE
                serializeChildren(node, parts);
                return;
            case 10: // DOCUMENT_TYPE_NODE: skip; we always emit <!DOCTYPE html>.
                return;
            case 11: // DOCUMENT_FRAGMENT_NODE
                serializeChildren(node, parts);
                return;
        }
    };

    const parts = ["<!DOCTYPE html>"];
    serializeChildren(document, parts);
    return parts.join("");
})();
