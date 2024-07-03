export default class MyComponent extends HTMLElement {
    static {
        customElements.define("my-component", this);
    }

    constructor() {
        super();
        this.attachShadow({mode: "open", serializable: true});
        const h1 = document.createElement("h1");
        h1.textContent = "Hello from Web Component";
        this.shadowRoot.appendChild(h1);
    }
}
