// The shell's live theme-sync channel (ADR 0075, contracts/ui-integration.md). The embedded page
// announces itself with `booth:iframe-ready`; the shell answers, and again on every later theme
// change, with `booth:theme`. Both sides are same-origin by construction (the iframe URL is a
// relative /iframe/<id>/... path), so anything from another origin, or from anything but our own
// parent frame, is ignored.

export type Theme = "dark" | "light";

export function applyTheme(theme: Theme, root: HTMLElement = document.documentElement): void {
  root.dataset.theme = theme;
}

/**
 * Starts listening for the shell's theme and announces readiness. Returns a function that stops
 * listening. Outside an iframe (local `npm run dev`) the page follows the OS preference instead.
 */
export function startThemeSync(win: Window = window): () => void {
  if (win.parent === win) {
    applyTheme(win.matchMedia?.("(prefers-color-scheme: dark)").matches ? "dark" : "light", win.document.documentElement);
    return () => {};
  }

  const onMessage = (event: MessageEvent) => {
    if (event.origin !== win.location.origin || event.source !== win.parent) return;
    const data = event.data as { type?: unknown; theme?: unknown } | null;
    if (data?.type === "booth:theme" && (data.theme === "dark" || data.theme === "light")) {
      applyTheme(data.theme, win.document.documentElement);
    }
  };
  win.addEventListener("message", onMessage);
  win.parent.postMessage({ type: "booth:iframe-ready" }, win.location.origin);
  return () => win.removeEventListener("message", onMessage);
}
