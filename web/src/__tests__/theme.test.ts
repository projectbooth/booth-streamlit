import { afterEach, describe, expect, it, vi } from "vitest";
import { startThemeSync } from "../theme";

// A fake "window inside an iframe": its own document and listeners, and a distinct parent that
// records what it was sent.
function framedWindow(origin = "https://booth.example") {
  const listeners: ((e: MessageEvent) => void)[] = [];
  const parent = { postMessage: vi.fn() };
  const doc = document.implementation.createHTMLDocument("t");
  const win = {
    parent,
    location: { origin },
    document: doc,
    addEventListener: (_: string, fn: (e: MessageEvent) => void) => listeners.push(fn),
    removeEventListener: (_: string, fn: (e: MessageEvent) => void) => listeners.splice(listeners.indexOf(fn), 1),
  } as unknown as Window;
  const send = (data: unknown, opts: { origin?: string; source?: unknown } = {}) =>
    listeners.forEach((fn) => fn({ data, origin: opts.origin ?? origin, source: opts.source ?? parent } as MessageEvent));
  return { win, parent, doc, send, listeners };
}

describe("theme sync (ADR 0075)", () => {
  afterEach(() => vi.restoreAllMocks());

  it("announces readiness to the parent, same-origin only", () => {
    const { win, parent } = framedWindow();
    startThemeSync(win);
    expect(parent.postMessage).toHaveBeenCalledWith({ type: "booth:iframe-ready" }, "https://booth.example");
  });

  it("applies the shell's theme, and follows later changes", () => {
    const { win, doc, send } = framedWindow();
    startThemeSync(win);
    send({ type: "booth:theme", theme: "dark" });
    expect(doc.documentElement.dataset.theme).toBe("dark");
    send({ type: "booth:theme", theme: "light" });
    expect(doc.documentElement.dataset.theme).toBe("light");
  });

  it("ignores another origin, another sender, and malformed messages", () => {
    const { win, doc, send } = framedWindow();
    startThemeSync(win);
    send({ type: "booth:theme", theme: "dark" }, { origin: "https://evil.example" });
    send({ type: "booth:theme", theme: "dark" }, { source: {} });
    send({ type: "booth:theme", theme: "purple" });
    send(null);
    expect(doc.documentElement.dataset.theme).toBeUndefined();
  });

  it("stops listening when cleaned up", () => {
    const { win, listeners } = framedWindow();
    const stop = startThemeSync(win);
    expect(listeners).toHaveLength(1);
    stop();
    expect(listeners).toHaveLength(0);
  });
});
