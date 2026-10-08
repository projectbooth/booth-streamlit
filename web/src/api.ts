// The module's own API (internal/api/apps.go). URLs are relative: this page is served under
// /iframe/streamlit/ and booth-core forwards the rest, attaching the viewer's identity, so there is
// no token to handle here. The backend decides every permission; the UI only hides what would be
// refused anyway.

export type Role = "owner" | "editor" | "viewer";

export interface Me {
  subject: string;
  workspace: string;
  role: Role;
  canAuthor: boolean;
}

export interface App {
  id: string;
  workspace: string;
  name: string;
  description: string;
  source?: string;
  shared: boolean;
  desiredState: "running" | "stopped";
  suspended: boolean;
  /** What the lifecycle observes (internal/lifecycle). */
  status: { state: "stopped" | "suspended" | "starting" | "running" | "failed"; reason?: string };
  createdBy: string;
  createdAt: string;
  updatedBy: string;
  updatedAt: string;
}

export interface AppInput {
  name: string;
  description: string;
  source: string;
  shared: boolean;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return undefined as T;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, (data as { error?: string }).error ?? `Request failed (${res.status})`);
  return data as T;
}

export const api = {
  me: () => call<Me>("GET", "api/me"),
  list: () => call<{ apps: App[] }>("GET", "api/apps").then((r) => r.apps),
  get: (id: string) => call<App>("GET", `api/apps/${encodeURIComponent(id)}`),
  create: (input: AppInput) => call<App>("POST", "api/apps", input),
  update: (id: string, input: AppInput) => call<App>("PUT", `api/apps/${encodeURIComponent(id)}`, input),
  remove: (id: string) => call<void>("DELETE", `api/apps/${encodeURIComponent(id)}`),
  start: (id: string) => call<App>("POST", `api/apps/${encodeURIComponent(id)}/start`),
  stop: (id: string) => call<App>("POST", `api/apps/${encodeURIComponent(id)}/stop`),
};

/** Where an app opens: relative, so it stays under the module's iframe path. */
export const appURL = (id: string) => `apps/${encodeURIComponent(id)}/`;
