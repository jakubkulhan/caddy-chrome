// see https://github.com/webcomponents-cg/community-protocols/blob/main/proposals/pending-task.md
export class PendingTaskEvent extends Event {
    constructor(complete) {
        super("pending-task", {bubbles: true, composed: true});
        this.complete = complete;
    }
}
