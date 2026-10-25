import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { USES_POLL_MS } from "../App";
import type { App as BoothApp, Role, UploadTarget, Use, WriteTarget } from "../api";

// ADR 0117: the Write targets panel (the app's side of the declaration), the disclosure every viewer
// gets around an app that can write (the header's Permissions control and the "Write allowed"
// prompt), and the plain-words confirmation.

const base: BoothApp = {
  id: "aaaa", workspace: "acme", name: "Finance", description: "", shared: true, desiredState: "running",
  suspended: false, status: { state: "running" }, owner: "o", writeTargets: [],
  createdBy: "o", createdAt: "2026-10-15T00:00:00Z", updatedBy: "o", updatedAt: "2026-10-15T00:00:00Z",
};

type Call = { method: string; url: string; body?: unknown };

/** What GET api/apps/aaaa/uses answers; tests change it between polls. */
let usesAnswer: { now: string; uses: Use[] } = { now: "2026-10-25T12:00:00Z", uses: [] };

function backend(role: Role, app: BoothApp, me: Record<string, unknown> = {}) {
  const calls: Call[] = [];
  const json = (status: number, data: unknown) => new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      const body = init?.body ? JSON.parse(String(init.body)) : undefined;
      calls.push({ method, url, body });
      const key = `${method} ${url}`;
      if (key === "GET api/me")
        return json(200, { subject: "o", workspace: "acme", role, canAuthor: role === "owner", dataAccess: true, writes: true, egressOpen: true, ...me });
      if (key === "GET api/apps") return json(200, { apps: [app] });
      if (key === "GET api/apps/aaaa") return json(200, { ...app, source: "import streamlit" });
      if (key === "GET api/apps/aaaa/uses") return json(200, usesAnswer);
      if (key === "PUT api/apps/aaaa/write-targets") {
        const b = body as { targets: WriteTarget[]; uploads: UploadTarget[] };
        return json(200, { ...app, writeTargets: b.targets, uploadTargets: b.uploads });
      }
      return json(404, { error: "not found" });
    }),
  );
  return calls;
}

async function openEditor() {
  render(<App />);
  const row = (await screen.findByText("Finance")).closest("li")!;
  await userEvent.click(within(row).getByRole("button", { name: "Edit" }));
  return screen.findByRole("heading", { name: "Write targets" });
}

describe("Write targets panel", () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => {
    vi.unstubAllGlobals();
    window.location.hash = "";
  });

  it("adds a target with insert only, needs the confirmation, and links to the API page's grant", async () => {
    const calls = backend("owner", base);
    await openEditor();
    expect(screen.getByText(/A target here is only half of it/)).toHaveTextContent("streamlit:acme:aaaa");
    expect(screen.getByText(/has no internet access/)).toBeInTheDocument();

    await userEvent.type(screen.getByRole("textbox", { name: "API slug" }), "household");
    await userEvent.click(screen.getByRole("button", { name: "Add" }));
    const target = screen.getByText("household").closest("li")!;
    expect(within(target).getByRole("checkbox", { name: "add" })).toBeChecked();
    expect(within(target).getByRole("checkbox", { name: "change" })).not.toBeChecked();
    expect(within(target).getByRole("checkbox", { name: "delete" })).not.toBeChecked();
    const link = within(target).getByRole("link", { name: "Grant on the API page" });
    expect(link).toHaveAttribute("href", "/apis?writeApi=household&writePrincipal=streamlit%3Aacme%3Aaaaa");
    expect(link).toHaveAttribute("target", "_top");

    const save = screen.getByRole("button", { name: "Save write targets" });
    expect(save).toBeDisabled(); // not confirmed yet
    const confirm = screen.getByRole("checkbox", { name: /Anyone who can open this app, including viewers, will be able to add rows through household/ });
    await userEvent.click(confirm);
    await userEvent.click(save);
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    expect(calls.find((c) => c.method === "PUT")!.body).toEqual({ targets: [{ api: "household", operations: ["insert"] }], uploads: [], confirmed: true });
    expect(await screen.findByRole("status")).toHaveTextContent("Without an owner's grant on the API page");
  });

  it("removing a target needs no confirmation", async () => {
    const calls = backend("owner", { ...base, writeTargets: [{ api: "household", operations: ["insert", "update"] }] });
    await openEditor();
    await userEvent.click(within(screen.getByText("household").closest("li")!).getByRole("button", { name: "Remove" }));
    expect(screen.queryByRole("checkbox", { name: /Anyone who can open this app/ })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Save write targets" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")?.body).toEqual({ targets: [], uploads: [], confirmed: false }));
  });

  it("adds an upload folder with the confirmation, and links to Storage's App uploads with the grant filled in", async () => {
    const calls = backend("owner", base, { uploads: true });
    await openEditor();
    expect(screen.getByText(/It can only add new files/)).toHaveTextContent("streamlit:acme:aaaa");

    await userEvent.type(screen.getByRole("textbox", { name: "Storage backend" }), "local");
    await userEvent.type(screen.getByRole("textbox", { name: "Folder" }), "/finance/inbox");
    await userEvent.click(screen.getByRole("button", { name: "Add folder" }));
    const target = screen.getByText("local:finance/inbox/").closest("li")!;
    const link = within(target).getByRole("link", { name: "Grant on the Storage page" });
    expect(link).toHaveAttribute("href", "/storage/admin?panel=upload-grants&principal=streamlit%3Aacme%3Aaaaa&backendId=local&prefix=finance%2Finbox%2F");
    expect(link).toHaveAttribute("target", "_top");

    await userEvent.click(screen.getByRole("checkbox", { name: /will be able to add files to local:finance\/inbox\// }));
    await userEvent.click(screen.getByRole("button", { name: "Save write targets" }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === "PUT")?.body).toEqual({ targets: [], uploads: [{ backend: "local", prefix: "finance/inbox/" }], confirmed: true }),
    );
  });

  it("shows only uploads when the install has uploads but not writes", async () => {
    backend("owner", base, { writes: false, uploads: true });
    await openEditor();
    expect(screen.getByRole("heading", { name: "Files, into Storage folders" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Tables, through the API module" })).toBeNull();
  });

  it("isn't shown when the install has no app writes", async () => {
    backend("owner", base, { writes: false });
    render(<App />);
    await userEvent.click(within((await screen.findByText("Finance")).closest("li")!).getByRole("button", { name: "Edit" }));
    await screen.findByRole("heading", { name: "Edit app" });
    expect(screen.queryByRole("heading", { name: "Write targets" })).toBeNull();
  });
});

describe("header disclosure", () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => {
    vi.unstubAllGlobals();
    window.location.hash = "";
  });

  const writing = { ...base, writeTargets: [{ api: "household", operations: ["insert", "update"] }] } as BoothApp;

  it("tells a viewer in the list, and opens a writing app under the header", async () => {
    backend("viewer", writing);
    render(<App />);
    const row = (await screen.findByText("Finance")).closest("li")!;
    expect(within(row).getByText("Can write to: household (add, change)")).toBeInTheDocument();
    expect(within(row).getByRole("link", { name: "Open" })).toHaveAttribute("href", "#app=aaaa");
  });

  it("an app that can't write opens directly, with no header", async () => {
    backend("viewer", base);
    render(<App />);
    const row = (await screen.findByText("Finance")).closest("li")!;
    expect(within(row).getByRole("link", { name: "Open" })).toHaveAttribute("href", "apps/aaaa/");
    expect(within(row).queryByText(/Can write to/)).toBeNull();
  });

  it("the framed view has a collapsed Permissions control outside the app's frame, not a banner", async () => {
    backend("viewer", writing);
    window.location.hash = "#app=aaaa";
    render(<App />);
    const button = await screen.findByRole("button", { name: "Permissions" });
    expect(button).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("note", { name: "What this app can change" })).toBeNull();
    await userEvent.click(button);
    expect(button).toHaveAttribute("aria-expanded", "true");
    const note = screen.getByRole("note", { name: "What this app can change" });
    expect(note).toHaveTextContent("This app can write to: household (add, change). Anyone who opens it can make these changes, as the app.");
    expect(screen.getByTitle("Finance")).toHaveAttribute("src", "apps/aaaa/");
    // The backend lets only this frame load the app without this page; it knows it by referrer.
    expect(screen.getByTitle("Finance")).toHaveAttribute("referrerpolicy", "same-origin");
  });

  it("an app that only uploads opens in the frame page too, whose Permissions name the folder", async () => {
    const uploading = { ...base, uploadTargets: [{ backend: "local", prefix: "finance/inbox/" }] } as BoothApp;
    backend("viewer", uploading);
    render(<App />);
    const row = (await screen.findByText("Finance")).closest("li")!;
    expect(within(row).getByText("Can write to: upload to local:finance/inbox/")).toBeInTheDocument();
    expect(within(row).getByRole("link", { name: "Open" })).toHaveAttribute("href", "#app=aaaa");
    window.location.hash = "#app=aaaa";
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "Permissions" }));
    expect(screen.getByRole("note", { name: "What this app can change" })).toHaveTextContent("This app can write to: upload to local:finance/inbox/.");
  });
});

describe("Write allowed prompt", () => {
  const both = {
    ...base,
    writeTargets: [{ api: "household", operations: ["insert", "update"] }],
    uploadTargets: [{ backend: "local", prefix: "finance/inbox/" }],
  } as BoothApp;
  const at = (s: number) => new Date(Date.parse("2026-10-25T12:00:00Z") + s * 1000).toISOString();

  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    sessionStorage.clear();
    usesAnswer = { now: at(0), uses: [] };
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
    window.location.hash = "";
  });

  const poll = () => act(() => vi.advanceTimersByTimeAsync(USES_POLL_MS));
  const usesCalls = (calls: { url: string }[]) => calls.filter((c) => c.url === "api/apps/aaaa/uses").length;

  it("shows the first time this session a target is used, once, and links to all permissions", async () => {
    // A use from before this session (another tab, an hour ago) isn't "just now".
    usesAnswer = { now: at(0), uses: [{ kind: "write", api: "household", at: at(-3600) }] };
    const calls = backend("viewer", both);
    window.location.hash = "#app=aaaa";
    const { unmount } = render(<App />);
    await screen.findByRole("button", { name: "Permissions" });
    await poll();
    expect(screen.queryByRole("dialog", { name: "Write allowed" })).toBeNull();

    usesAnswer = { now: at(5), uses: [{ kind: "write", api: "household", at: at(4) }] };
    await poll();
    const prompt = await screen.findByRole("dialog", { name: "Write allowed" });
    expect(prompt).toHaveTextContent("This app just saved a change to household. It can add, change rows there.");
    expect(prompt).toHaveTextContent("The change is made as the app, not as you. Anyone who opens this app can make these changes.");
    await userEvent.click(within(prompt).getByRole("button", { name: "OK" }));
    expect(screen.queryByRole("dialog", { name: "Write allowed" })).toBeNull();

    // The same target again: no prompt, also after a reload in the same session.
    await poll();
    expect(screen.queryByRole("dialog", { name: "Write allowed" })).toBeNull();
    unmount();
    render(<App />);
    await screen.findByRole("button", { name: "Permissions" });
    await poll();
    expect(screen.queryByRole("dialog", { name: "Write allowed" })).toBeNull();

    // The upload folder is a separate target; "See all permissions" opens the control.
    usesAnswer = { now: at(9), uses: [{ kind: "write", api: "household", at: at(8) }, { kind: "upload", backend: "local", prefix: "finance/inbox/", at: at(8) }] };
    await poll();
    const upload = await screen.findByRole("dialog", { name: "Write allowed" });
    expect(upload).toHaveTextContent("This app just added a file to local:finance/inbox/.");
    expect(upload).not.toHaveTextContent("household");
    await userEvent.click(within(upload).getByRole("button", { name: "See all permissions" }));
    expect(screen.getByRole("button", { name: "Permissions" })).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByRole("note", { name: "What this app can change" })).toHaveTextContent("household (add, change) · upload to local:finance/inbox/");

    // Every declared target has been prompted: it stops asking.
    const n = usesCalls(calls);
    await poll();
    await poll();
    expect(usesCalls(calls)).toBe(n);
  });

  it("a target the app doesn't declare isn't prompted, and a hidden tab isn't polled", async () => {
    const calls = backend("viewer", both);
    window.location.hash = "#app=aaaa";
    render(<App />);
    await screen.findByRole("button", { name: "Permissions" });
    usesAnswer = { now: at(5), uses: [{ kind: "write", api: "notes", at: at(4) }] };
    await poll();
    expect(screen.queryByRole("dialog", { name: "Write allowed" })).toBeNull();

    const hidden = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    const n = usesCalls(calls);
    await poll();
    expect(usesCalls(calls)).toBe(n);
    hidden.mockRestore();
  });
});
