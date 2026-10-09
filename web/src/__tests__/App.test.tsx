import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import type { App as BoothApp, Role } from "../api";

const shared: BoothApp = {
  id: "aaaa", workspace: "acme", name: "Sales", description: "by region", shared: true, desiredState: "stopped",
  suspended: false, status: { state: "stopped" }, owner: "o",
  createdBy: "o", createdAt: "2026-10-08T00:00:00Z", updatedBy: "o", updatedAt: "2026-10-08T00:00:00Z",
};

type Call = { method: string; url: string; body?: unknown };

// A fake backend: answers /api/me for the given role and records every request.
function backend(role: Role, apps: BoothApp[] = [shared], overrides: Record<string, () => Response> = {}, dataAccess = false) {
  const calls: Call[] = [];
  const json = (status: number, data: unknown) => new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined });
      const key = `${method} ${url}`;
      if (overrides[key]) return overrides[key]();
      if (key === "GET api/me") return json(200, { subject: "u", workspace: "acme", role, canAuthor: role === "owner", dataAccess });
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

describe("status", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows the observed state, and a failure's reason to owners only", async () => {
    const failed = { ...shared, desiredState: "running" as const, status: { state: "failed" as const, reason: "streamlit: CrashLoopBackOff" } };
    const sleeping = { ...shared, id: "bbbb", name: "Ops", desiredState: "running" as const, suspended: true, status: { state: "suspended" as const } };
    backend("owner", [failed, sleeping]);
    const { unmount } = render(<App />);
    expect(await screen.findByText("Failed to start: streamlit: CrashLoopBackOff")).toBeInTheDocument();
    expect(within(screen.getByText("Ops").closest("li")!).getByText("Sleeping")).toBeInTheDocument();
    // A running (or sleeping) app offers Stop, not Start.
    expect(within(screen.getByText("Ops").closest("li")!).getByRole("button", { name: "Stop" })).toBeInTheDocument();
    unmount();

    vi.unstubAllGlobals();
    backend("viewer", [failed]);
    render(<App />);
    expect(await screen.findByText("Failed")).toBeInTheDocument();
    expect(screen.queryByText(/CrashLoopBackOff/)).toBeNull();
  });

  it("shows installing packages, and a failed install's pip output to owners", async () => {
    const installing = { ...shared, desiredState: "running" as const, status: { state: "installing" as const } };
    const pipFailed = {
      ...shared,
      id: "bbbb",
      name: "Ops",
      desiredState: "running" as const,
      status: { state: "failed" as const, reason: "pip install failed (exit 1):\nERROR: No matching distribution found for nope==9" },
    };
    backend("owner", [installing, pipFailed]);
    render(<App />);
    expect(await screen.findByText("Installing packages")).toBeInTheDocument();
    const failure = within(screen.getByText("Ops").closest("li")!).getByText(/No matching distribution found for nope==9/);
    expect(failure).toHaveTextContent("Failed to start: pip install failed (exit 1):");
    expect(failure.className).toContain("whitespace-pre-wrap");
  });

  it("shows the backend's reason when Start is refused at the cap", async () => {
    backend("owner", [shared], {
      "POST api/apps/aaaa/start": () => new Response(JSON.stringify({ error: "too many apps are running; stop one first" }), { status: 409 }),
    });
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "Start" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("too many apps are running; stop one first");
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
    await userEvent.type(screen.getByLabelText(/requirements.txt/), "humanize==4.12.0");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(screen.getByRole("heading", { name: "Streamlit apps" })).toBeInTheDocument());
    const post = calls.find((c) => c.method === "POST" && c.url === "api/apps")!;
    expect(post.body).toMatchObject({ name: "Revenue", shared: true, requirements: "humanize==4.12.0" });
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

describe("data access (ADR 0104/0107)", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("says whose access a shared app reads with, and what it can read", async () => {
    backend("viewer", [shared], {}, true);
    render(<App />);
    expect(await screen.findByText(/Reads data as o/)).toBeInTheDocument();
    expect(screen.getByText(/This app can read any data you can read in this workspace\./)).toBeInTheDocument();
  });

  it("says nothing about data when the install has no data access", async () => {
    backend("viewer", [shared], {}, false);
    render(<App />);
    expect(await screen.findByText("Sales")).toBeInTheDocument();
    expect(screen.queryByText(/Reads data as/)).toBeNull();
  });

  it("shows a paused app's reason to everyone and Take ownership to owners only", async () => {
    const paused = { ...shared, dataPausedReason: "the app's owner no longer has access to this workspace" };
    const calls = backend("owner", [paused], {}, true);
    const { unmount } = render(<App />);
    expect(await screen.findByText("Data access paused")).toBeInTheDocument();
    expect(screen.getByText(/the app's owner no longer has access/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Take ownership" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.url === "api/apps/aaaa/take-ownership")).toBe(true));
    unmount();

    vi.unstubAllGlobals();
    backend("viewer", [paused], {}, true);
    render(<App />);
    expect(await screen.findByText("Data access paused")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Take ownership" })).toBeNull();
  });

  it("the editor shows the wording when an app is shared", async () => {
    backend("owner", [], {}, true);
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "New app" }));
    expect(screen.queryByText(/can read any data you can read/)).toBeNull();
    await userEvent.click(screen.getByLabelText(/Shared with workspace members/));
    expect(screen.getByText(/This app can read any data you can read in this workspace\./)).toBeInTheDocument();
  });
});
