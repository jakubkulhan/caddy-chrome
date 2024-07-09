function getHTML() {
    const innerHTML = this.getHTML({
        serializableShadowRoots: true,
    });

    let outerHTML = "<" + this.tagName.toLowerCase();
    for (const attr of this.attributes) {
        outerHTML += ` ${attr.name}="${attr.value}"`;
    }
    outerHTML += ">";
    outerHTML += innerHTML;
    outerHTML += `</${this.tagName.toLowerCase()}>`;

    return outerHTML;
}
