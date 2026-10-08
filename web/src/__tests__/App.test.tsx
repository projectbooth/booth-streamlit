import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import type { App as BoothApp, Role } from "../api";

const shared: BoothApp = {
  id: "aaaa", workspace: "acme", name: "Sales", description: "by region", shared: true, desiredState: "stopped",
  createdBy: "o", createdAt: "2026-10-08T00:00:00Z", updatedBy: "o", updatedAt: "2026-10-08T00:00:00Z",
};

type Call = { method: string; url: string; body?: unknown };

// A fake backend: answers /api/me for the given role and records every request.
function backend(role: Role, apps: BoothApp[] = [shared], overrides: Record<string, () => Response> = {}) {
  const calls: Call[] = [];
  const json = (status: number, data: unknown) => new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined });
      const key = `${method} ${url}`;
      if (overrides[key]) return overrides[key]();
      if (key === "GET api/me") return json(200, { subject: "u", workspace: "acme", role, canAuthor: role === "owner" });
      if (key === "GET api/apps") return json(200, { apps });
      if (key === "POST api/apps") return json(201, { ...shared, id: "new" });
      if (key.startsWith("POST api/apps/")) return json(200, { ...shared, desiredState: "running" });
      return json(404, { error: "not found" });
    }),
  );
  return calls;
}

describe("app list", () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it("shows owners every authoring control and the trust note (ADR 0105)", async () => {
    backend("owner");
    render(<App />);
    expect(await screen.findByText("Sales")).toBeInTheDocument();
    expect(screen.getByRole("note")).toHaveTextContent("App code runs in your browser with access to your Booth session");
    expect(screen.getByRole("button", { name: "New app" })).toBeInTheDocument();
    const row = screen.getByText("Sales").closest("li")!;
    expect(within(row).getByRole("button", { name: "Edit" })).toBeInTheDocument();
    expect(within(row).getByRole("button", { name: "Start" })).toBeInTheDocument();
    expect(within(row).getByRole("link", { name: "Open" })).toHaveAttribute("href", "apps/aaaa/");
  });

  it.each(["editor", "viewer"] as Role[])("shows a %s only Open, the trust note, and why", async (role) => {
    backend(role);
    render(<App />);
    expect(await screen.findByText("Sales")).toBeInTheDocument();
    expect(screen.getByRole("note")).toBeInTheDocument();
    expect(screen.getByText(/Only workspace owners can create or change apps/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New app" })).toBeNull();
    const row = screen.getByText("Sales").closest("li")!;
    expect(within(row).queryByRole("button")).toBeNull();
    expect(within(row).getByRole("link", { name: "Open" })).toBeInTheDocument();
  });

  it("start calls the API and reloads", async () => {
    const calls = backend("owner");
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "Start" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.url === "api/apps/aaaa/start")).toBe(true));
  });

  it("explains a refusal from the backend in plain words", async () => {
    backend("owner", [shared], {
      "POST api/apps/aaaa/start": () => new Response(JSON.stringify({ error: "x" }), { status: 403 }),
    });
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "Start" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Only workspace owners can do that.");
  });
});

describe("editor", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("creates an app with JSON, shows the trust note, and returns to the list", async () => {
    const calls = backend("owner", []);
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "New app" }));
    expect(screen.getByRole("note")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Name"), "Revenue");
    await userEvent.click(screen.getByLabelText(/Shared with workspace members/));
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(screen.getByRole("heading", { name: "Streamlit apps" })).toBeInTheDocument());
    const post = calls.find((c) => c.method === "POST" && c.url === "api/apps")!;
    expect(post.body).toMatchObject({ name: "Revenue", shared: true });
    expect((post.body as { source: string }).source).toContain("booth_streamlit.user()");
  });

  it("deletes only on a second click, without a browser dialog", async () => {
    const confirmSpy = vi.spyOn(window, "confirm");
    const calls = backend("owner", [shared], {
      "GET api/apps/aaaa": () => new Response(JSON.stringify({ ...shared, source: "x" }), { status: 200 }),
      "DELETE api/apps/aaaa": () => new Response(null, { status: 204 }),
    });
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "Edit" }));
    await userEvent.click(await screen.findByRole("button", { name: "Delete" }));
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    await userEvent.click(screen.getByRole("button", { name: "Click again to delete" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url === "api/apps/aaaa")).toBe(true));
    expect(confirmSpy).not.toHaveBeenCalled();
  });
});
