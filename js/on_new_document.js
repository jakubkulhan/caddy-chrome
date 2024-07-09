window.CaddyChrome = {
    pendingTask: Promise.resolve(),
    events: 0,
    pending: 0,
    failed: 0,
    resolve: null,
    reject: null,
};

// see https://github.com/webcomponents-cg/community-protocols/blob/main/proposals/pending-task.md
document.addEventListener("pending-task", async function (ev) {
    window.CaddyChrome.events++;

    if (window.CaddyChrome.pending === 0) {
        window.CaddyChrome.pendingTask = new Promise((resolve, reject) => {
            window.CaddyChrome.resolve = resolve;
            window.CaddyChrome.reject = reject;
        });
    }

    window.CaddyChrome.pending++;
    try {
        await ev.complete;
    } catch (e) {
        window.CaddyChrome.failed++;
    } finally {
        window.CaddyChrome.pending--;
    }

    if (window.CaddyChrome.pending === 0) {
        if (window.CaddyChrome.failed === 0) {
            window.CaddyChrome.resolve();
        } else {
            window.CaddyChrome.reject();
        }
        window.CaddyChrome.failed = 0;
        window.CaddyChrome.resolve = null;
        window.CaddyChrome.reject = null;
        window.CaddyChrome.pendingTask = Promise.resolve();
    }
});
