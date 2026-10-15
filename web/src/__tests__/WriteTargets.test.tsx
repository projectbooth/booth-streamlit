import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import type { App as BoothApp, Role, WriteTarget } from "../api";

// ADR 0117: the Write targets panel (the app's side of the declaration), the header every viewer
// sees above an app that can write, and the plain-words confirmation.

const base: BoothApp = {
  id: "aaaa", workspace: "acme", name: "Finance", description: "", shared: true, desiredState: "running",
  suspended: false, status: { state: "running" }, owner: "o", writeTargets: [],
  createdBy: "o", createdAt: "2026-10-15T00:00:00Z", updatedBy: "o", updatedAt: "2026-10-15T00:00:00Z",
};

type Call = { method: string; url: string; body?: unknown };

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
      if (key === "PUT api/apps/aaaa/write-targets") return json(200, { ...app, writeTargets: (body as { targets: WriteTarget[] }).targets });
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
    expect(link).toHaveAttribute("href", "/apis?api=household&grant=streamlit%3Aacme%3Aaaaa");
    expect(link).toHaveAttribute("target", "_top");

    const save = screen.getByRole("button", { name: "Save write targets" });
    expect(save).toBeDisabled(); // not confirmed yet
    const confirm = screen.getByRole("checkbox", { name: /Anyone who can open this app, including viewers, will be able to add rows through household/ });
    await userEvent.click(confirm);
    await userEvent.click(save);
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    expect(calls.find((c) => c.method === "PUT")!.body).toEqual({ targets: [{ api: "household", operations: ["insert"] }], confirmed: true });
    expect(await screen.findByRole("status")).toHaveTextContent("Without an owner's grant on the API page");
  });

  it("removing a target needs no confirmation", async () => {
    const calls = backend("owner", { ...base, writeTargets: [{ api: "household", operations: ["insert", "update"] }] });
    await openEditor();
    await userEvent.click(within(screen.getByText("household").closest("li")!).getByRole("button", { name: "Remove" }));
    expect(screen.queryByRole("checkbox", { name: /Anyone who can open this app/ })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Save write targets" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")?.body).toEqual({ targets: [], confirmed: false }));
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

  it("the framed view renders the header outside the app's frame", async () => {
    backend("viewer", writing);
    window.location.hash = "#app=aaaa";
    render(<App />);
    const note = await screen.findByRole("note", { name: "What this app can change" });
    expect(note).toHaveTextContent("This app can write to: household (add, change). Anyone who opens it can make these changes, as the app.");
    expect(screen.getByTitle("Finance")).toHaveAttribute("src", "apps/aaaa/");
  });
});
